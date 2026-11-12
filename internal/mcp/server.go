package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"smart-library/internal/rag"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Server implements a JSON-RPC 2.0 Model Context Protocol (MCP) server over stdio.
type Server struct {
	engine *rag.Engine
}

// NewServer creates a new MCP server backed by the given RAG Engine.
func NewServer(engine *rag.Engine) *Server {
	return &Server{engine: engine}
}

// ServeStdio reads JSON-RPC 2.0 messages line-by-line from r and writes responses to w.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			// Notification message (e.g., notifications/initialized)
			continue
		}

		resp := s.handleRequest(ctx, req)
		b, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\n", string(b))
	}
	return scanner.Err()
}

func (s *Server) handleRequest(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "smart-library-mcp",
					"version": "1.0.0",
				},
			},
		}

	case "tools/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": []map[string]any{
					{
						"name":        "search_books",
						"description": "Search the user's technical EPUB library using Qdrant Hybrid Search (Dense + Sparse RRF) and Parent-Child Hierarchical Retrieval.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query": map[string]any{
									"type":        "string",
									"description": "Technical search query, concept, API name, or question.",
								},
								"book_filter": map[string]any{
									"type":        "string",
									"description": "Optional book title, author, or book ID filter.",
								},
								"top_k": map[string]any{
									"type":        "integer",
									"description": "Number of Parent Sections to return (default 5).",
								},
							},
							"required": []string{"query"},
						},
					},
					{
						"name":        "list_books",
						"description": "List all technical EPUB books currently indexed in the Smart Library.",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
					{
						"name":        "get_chapter",
						"description": "Retrieve the complete text of a specific book chapter by chapter_id.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"chapter_id": map[string]any{
									"type":        "string",
									"description": "The chapter_id returned by search_books.",
								},
							},
							"required": []string{"chapter_id"},
						},
					},
				},
			},
		}

	case "tools/call":
		var callParams struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid tool arguments"},
			}
		}
		textOut, isErr := s.executeTool(ctx, callParams.Name, callParams.Arguments)
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]any{
					{
						"type": "text",
						"text": textOut,
					},
				},
				"isError": isErr,
			},
		}

	default:
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "method not found"},
		}
	}
}

func (s *Server) executeTool(ctx context.Context, name string, args map[string]any) (string, bool) {
	switch name {
	case "search_books":
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return "Error: 'query' parameter is required.", true
		}
		bookFilter, _ := args["book_filter"].(string)
		topK := 5
		if kFloat, ok := args["top_k"].(float64); ok && int(kFloat) > 0 {
			topK = int(kFloat)
		}

		results := s.engine.Search(ctx, query, "hybrid", bookFilter, topK)
		if len(results) == 0 {
			return "No matching passages found in the library.", false
		}

		var sb strings.Builder
		for i, res := range results {
			ck := res.Chunk
			parentContent := res.ParentText
			if parentContent == "" {
				parentContent = ck.Text
			}
			sb.WriteString(fmt.Sprintf(
				"### Result %d (Score: %.4f | Child Hits: %d | Chapter ID: %s | Parent ID: %s)\n**Book:** %s (%s)\n**Chapter:** %s > %s\n\n%s\n\n---\n\n",
				i+1, res.Score, res.ChildHits, ck.ChapterID, res.ParentID, ck.BookTitle, ck.Author, ck.ChapterTitle, ck.SectionTitle, parentContent,
			))
		}
		return sb.String(), false

	case "list_books":
		books := s.engine.Store().ListBooks()
		if len(books) == 0 {
			return "No books are currently indexed in Smart Library.", false
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Indexed Technical Books (%d):\n\n", len(books)))
		for _, b := range books {
			sb.WriteString(fmt.Sprintf("- **%s** by %s (`id`: `%s`, %d chapters, %d parent sections, %d child chunks)\n",
				b.Title, b.Author, b.ID, b.ChapterCount, b.ParentCount, b.ChunkCount))
		}
		return sb.String(), false

	case "get_chapter":
		chapID, _ := args["chapter_id"].(string)
		chap, ok := s.engine.Store().GetChapter(strings.TrimSpace(chapID))
		if !ok {
			return fmt.Sprintf("Chapter %q not found.", chapID), true
		}
		return fmt.Sprintf("# %s (Chapter ID: %s)\n\n%s", chap.Title, chap.ID, chap.Content), false

	default:
		return fmt.Sprintf("Unknown tool: %s", name), true
	}
}

