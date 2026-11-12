package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"library-mcp/internal/rag"
	"library-mcp/internal/store"
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
					"name":    "library-mcp",
					"version": "1.1.0",
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
						"description": "Search the user's technical EPUB library using Qdrant Hybrid Search (Dense + Sparse RRF) and Parent-Child Hierarchical Retrieval. Supports filtering by book title/author/ID, category, or tag.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"query": map[string]any{
									"type":        "string",
									"description": "Technical search query, concept, API name, or question.",
								},
								"book_filter": map[string]any{
									"type":        "string",
									"description": "Optional book title, author, book ID, category, or tag filter.",
								},
								"category": map[string]any{
									"type":        "string",
									"description": "Optional category filter (e.g. 'AI & Machine Learning', 'Security & Cryptography').",
								},
								"tag": map[string]any{
									"type":        "string",
									"description": "Optional tag filter (e.g. 'rag', 'xss', 'domain-driven-design').",
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
						"description": "List all technical EPUB books currently indexed in Library MCP with their web metadata, category, and tags. Supports optional category and tag filters.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"category": map[string]any{
									"type":        "string",
									"description": "Optional category substring filter.",
								},
								"tag": map[string]any{
									"type":        "string",
									"description": "Optional tag filter.",
								},
							},
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
					{
						"name":        "enrich_books",
						"description": "Fetch bibliographic metadata from the web (Google Books & Open Library) and automatically categorize and tag indexed books.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"book_id": map[string]any{
									"type":        "string",
									"description": "Optional book ID to enrich. If omitted, enriches all indexed books.",
								},
								"force": map[string]any{
									"type":        "boolean",
									"description": "Force re-fetching web metadata even if already enriched.",
								},
							},
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
		if cat, _ := args["category"].(string); strings.TrimSpace(cat) != "" && bookFilter == "" {
			bookFilter = cat
		}
		if tag, _ := args["tag"].(string); strings.TrimSpace(tag) != "" && bookFilter == "" {
			bookFilter = tag
		}
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
			bookRec, _, hasBook := s.engine.Store().GetBook(ck.BookID)
			catTagLine := ""
			if hasBook && (bookRec.Category != "" || len(bookRec.Tags) > 0) {
				catTagLine = fmt.Sprintf("**Category:** %s | **Tags:** `%s`\n", bookRec.Category, strings.Join(bookRec.Tags, "`, `"))
			}
			sb.WriteString(fmt.Sprintf(
				"### Result %d (Score: %.4f | Child Hits: %d | Chapter ID: %s | Parent ID: %s)\n**Book:** %s (%s)\n%s**Chapter:** %s > %s\n\n%s\n\n---\n\n",
				i+1, res.Score, res.ChildHits, ck.ChapterID, res.ParentID, ck.BookTitle, ck.Author, catTagLine, ck.ChapterTitle, ck.SectionTitle, parentContent,
			))
		}
		return sb.String(), false

	case "list_books":
		catFilter, _ := args["category"].(string)
		tagFilter, _ := args["tag"].(string)
		books := s.engine.Store().ListBooksFiltered(catFilter, tagFilter)
		if len(books) == 0 {
			return "No matching books are currently indexed in Library MCP.", false
		}
		return formatBooksList(books), false

	case "get_chapter":
		chapID, _ := args["chapter_id"].(string)
		chap, ok := s.engine.Store().GetChapter(strings.TrimSpace(chapID))
		if !ok {
			return fmt.Sprintf("Chapter %q not found.", chapID), true
		}
		return fmt.Sprintf("# %s (Chapter ID: %s)\n\n%s", chap.Title, chap.ID, chap.Content), false

	case "enrich_books":
		bookID, _ := args["book_id"].(string)
		force, _ := args["force"].(bool)
		if strings.TrimSpace(bookID) != "" {
			rec, err := s.engine.EnrichBook(ctx, strings.TrimSpace(bookID))
			if err != nil {
				return fmt.Sprintf("Error enriching book %q: %v", bookID, err), true
			}
			return formatBooksList([]store.BookRecord{rec}), false
		}
		books, err := s.engine.EnrichAllBooks(ctx, force)
		if err != nil {
			return fmt.Sprintf("Error enriching books: %v", err), true
		}
		return formatBooksList(books), false

	default:
		return fmt.Sprintf("Unknown tool: %s", name), true
	}
}

func formatBooksList(books []store.BookRecord) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Indexed Technical Books (%d):\n\n", len(books)))
	for _, b := range books {
		titleLine := fmt.Sprintf("- **%s**", b.Title)
		if b.Subtitle != "" {
			titleLine += fmt.Sprintf(": *%s*", b.Subtitle)
		}
		titleLine += fmt.Sprintf(" by %s (`id`: `%s`)\n", b.Author, b.ID)
		sb.WriteString(titleLine)

		category := b.Category
		if category == "" {
			category = "Uncategorized"
		}
		tagsStr := "none"
		if len(b.Tags) > 0 {
			tagsStr = "`" + strings.Join(b.Tags, "`, `") + "`"
		}
		sb.WriteString(fmt.Sprintf("  - **Category:** %s | **Tags:** %s\n", category, tagsStr))

		var bibParts []string
		if b.Publisher != "" {
			bibParts = append(bibParts, "Publisher: "+b.Publisher)
		}
		if b.PublishedYear != "" {
			bibParts = append(bibParts, "Year: "+b.PublishedYear)
		}
		if b.ISBN != "" {
			bibParts = append(bibParts, "ISBN: "+b.ISBN)
		}
		if b.MetadataSource != "" {
			bibParts = append(bibParts, "Source: "+b.MetadataSource)
		}
		if len(bibParts) > 0 {
			sb.WriteString(fmt.Sprintf("  - **Metadata:** %s\n", strings.Join(bibParts, " | ")))
		}
		sb.WriteString(fmt.Sprintf("  - **Structure:** %d chapters, %d parent sections, %d child chunks\n",
			b.ChapterCount, b.ParentCount, b.ChunkCount))
	}
	return sb.String()
}
