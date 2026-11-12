package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
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
					"tools":     map[string]any{},
					"resources": map[string]any{},
					"prompts":   map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "library-mcp",
					"version": "1.2.0",
				},
			},
		}

	case "tools/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": s.toolDefinitions(),
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

	case "resources/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"resources": s.listResources(),
			},
		}

	case "resources/read":
		var readParams struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &readParams); err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid resource read params"},
			}
		}
		content, mimeType, err := s.readResource(readParams.URI)
		if err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32002, Message: err.Error()},
			}
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"contents": []map[string]any{
					{
						"uri":      readParams.URI,
						"mimeType": mimeType,
						"text":     content,
					},
				},
			},
		}

	case "prompts/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"prompts": s.listPrompts(),
			},
		}

	case "prompts/get":
		var getParams struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &getParams); err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid prompt arguments"},
			}
		}
		desc, messages, err := s.getPrompt(ctx, getParams.Name, getParams.Arguments)
		if err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: err.Error()},
			}
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"description": desc,
				"messages":    messages,
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

func (s *Server) toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "search_books",
			"description": "Search the user's technical book library using Hybrid RRF (Dense + Sparse BM25), Parent-Child Hierarchical Retrieval, and MMR cross-book diversity. Supports filtering by book, category, tag, author, minimum year, per-book cap, and code-only sections.",
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
						"description": "Optional category filter (e.g. 'AI & Machine Learning', 'Security & Cryptography', 'Software Architecture & Design').",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag filter (e.g. 'rag', 'xss', 'golang', 'domain-driven-design').",
					},
					"author": map[string]any{
						"type":        "string",
						"description": "Optional author name substring filter.",
					},
					"year_min": map[string]any{
						"type":        "integer",
						"description": "Optional minimum publication year filter (e.g. 2020).",
					},
					"max_per_book": map[string]any{
						"type":        "integer",
						"description": "Max sections to return from any single book for cross-book diversity (default 2 across library).",
					},
					"code_only": map[string]any{
						"type":        "boolean",
						"description": "When true, only return sections containing code blocks, CLI commands, or technical syntax.",
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
			"description": "List technical books currently indexed in Library MCP with their web metadata, category, and tags. Supports optional category, tag, author, and minimum year filters.",
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
					"author": map[string]any{
						"type":        "string",
						"description": "Optional author substring filter.",
					},
					"year_min": map[string]any{
						"type":        "integer",
						"description": "Optional minimum publication year filter.",
					},
				},
			},
		},
		{
			"name":        "get_book_toc",
			"description": "Inspect a book's full hierarchical Table of Contents (all chapters and nested parent section headings with their IDs and character lengths) before pulling full chapter text.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"book_id": map[string]any{
						"type":        "string",
						"description": "Book ID, title, ISBN, or filename.",
					},
				},
				"required": []string{"book_id"},
			},
		},
		{
			"name":        "search_chapters",
			"description": "Search specifically across all chapter titles and parent section headings in the library to locate relevant chapters before reading their full text.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Heading keywords or topic to match against chapter and section titles.",
					},
					"book_filter": map[string]any{
						"type":        "string",
						"description": "Optional book ID, title, or author filter.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Optional category filter.",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag filter.",
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "Maximum number of matching chapters/sections to return (default 15).",
					},
				},
				"required": []string{"query"},
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
						"description": "The chapter_id returned by search_books, get_book_toc, or search_chapters.",
					},
				},
				"required": []string{"chapter_id"},
			},
		},
		{
			"name":        "compare_books_on_topic",
			"description": "Retrieve and compare the top Parent Section from distinct books (1 per book with MMR diversity) on a technical topic to synthesize contrasting author perspectives.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"topic": map[string]any{
						"type":        "string",
						"description": "Technical topic or architectural trade-off to compare across books.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Optional category filter.",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag filter.",
					},
					"max_books": map[string]any{
						"type":        "integer",
						"description": "Number of distinct books to compare (default 5).",
					},
				},
				"required": []string{"topic"},
			},
		},
		{
			"name":        "extract_code_examples",
			"description": "Search and filter specifically for book sections containing source code examples, CLI commands, SQL queries, or implementation patterns.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Concept, API, algorithm, or pattern to find code examples for.",
					},
					"language": map[string]any{
						"type":        "string",
						"description": "Optional programming language or tool filter (e.g. 'golang', 'python', 'rust', 'sql', 'kubernetes').",
					},
					"book_filter": map[string]any{
						"type":        "string",
						"description": "Optional book filter.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Optional category filter.",
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "Number of code-rich sections to return (default 5).",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "deduplicate_books",
			"description": "Detect duplicate or redundant book editions across the library (by ISBN or canonical Title + Author) and optionally prune lower-quality duplicates.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"apply": map[string]any{
						"type":        "boolean",
						"description": "If false (default), runs a dry-run analysis reporting duplicate groups. If true, removes the duplicate copies and keeps the highest-quality edition of each book.",
					},
				},
			},
		},
		{
			"name":        "save_note",
			"description": "Save a persistent bookmark, highlight, study takeaway, or architectural decision note linked to a book, chapter, or parent section. Saved notes are automatically included in future RAG context.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "Short title or headline for the note.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Markdown content of the note, takeaway, or highlight.",
					},
					"book_id": map[string]any{
						"type":        "string",
						"description": "Optional book ID or book title to link this note to.",
					},
					"chapter_id": map[string]any{
						"type":        "string",
						"description": "Optional chapter_id to link this note to.",
					},
					"parent_id": map[string]any{
						"type":        "string",
						"description": "Optional parent_id section to link this note to.",
					},
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional list of tags for organizing the note.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			"name":        "list_notes",
			"description": "List or search saved bookmarks, highlights, and agent notes across the library.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Optional search query to filter notes.",
					},
					"book_filter": map[string]any{
						"type":        "string",
						"description": "Optional book ID or title filter.",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag filter.",
					},
				},
			},
		},
		{
			"name":        "delete_note",
			"description": "Delete a saved note or bookmark by note_id.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"note_id": map[string]any{
						"type":        "string",
						"description": "The note ID (e.g. 'note_...') to delete.",
					},
				},
				"required": []string{"note_id"},
			},
		},
		{
			"name":        "export_dossier",
			"description": "Compile a comprehensive Markdown Study Guide / Technical Dossier on a topic across all relevant books, combining multi-author excerpts, code examples, recommended chapters, and saved notes.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"topic": map[string]any{
						"type":        "string",
						"description": "Technical topic for the study dossier.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Optional category filter.",
					},
					"tag": map[string]any{
						"type":        "string",
						"description": "Optional tag filter.",
					},
					"output_path": map[string]any{
						"type":        "string",
						"description": "Optional file path to save the compiled Markdown dossier to disk.",
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "Number of core sections to include (default 6).",
					},
				},
				"required": []string{"topic"},
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
		{
			"name":        "remove_book",
			"description": "Remove an indexed book (and all its chapters, parent sections, and chunks) from Library MCP by book ID, title, ISBN, or filename.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"book_id": map[string]any{
						"type":        "string",
						"description": "Book ID, title, ISBN, or filename of the book to remove.",
					},
				},
				"required": []string{"book_id"},
			},
		},
	}
}

func (s *Server) executeTool(ctx context.Context, name string, args map[string]any) (string, bool) {
	switch name {
	case "search_books":
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return "Error: 'query' parameter is required.", true
		}
		opts := store.SearchOptions{
			Query:      query,
			Mode:       "hybrid",
			BookFilter: stringArg(args, "book_filter"),
			Category:   stringArg(args, "category"),
			Tag:        stringArg(args, "tag"),
			Author:     stringArg(args, "author"),
			YearMin:    intArg(args, "year_min", 0),
			MaxPerBook: intArg(args, "max_per_book", 0),
			CodeOnly:   boolArg(args, "code_only"),
			TopK:       intArg(args, "top_k", 5),
		}

		results := s.engine.SearchWithOptions(ctx, opts)
		if len(results) == 0 {
			return "No matching passages found in the library.", false
		}
		return s.formatSearchResults(results), false

	case "list_books":
		catFilter := stringArg(args, "category")
		tagFilter := stringArg(args, "tag")
		authorFilter := strings.ToLower(stringArg(args, "author"))
		yearMin := intArg(args, "year_min", 0)

		books := s.engine.Store().ListBooksFiltered(catFilter, tagFilter)
		if authorFilter != "" || yearMin > 0 {
			filtered := make([]store.BookRecord, 0, len(books))
			for _, b := range books {
				if authorFilter != "" && !strings.Contains(strings.ToLower(b.Author), authorFilter) {
					continue
				}
				if yearMin > 0 {
					yr, _ := strconv.Atoi(strings.TrimSpace(b.PublishedYear))
					if yr < yearMin {
						continue
					}
				}
				filtered = append(filtered, b)
			}
			books = filtered
		}
		if len(books) == 0 {
			return "No matching books are currently indexed in Library MCP.", false
		}
		return formatBooksList(books), false

	case "get_book_toc":
		bookID := stringArg(args, "book_id")
		if bookID == "" {
			return "Error: 'book_id' parameter is required.", true
		}
		toc, err := s.engine.Store().GetBookTOC(bookID)
		if err != nil {
			return fmt.Sprintf("Error retrieving TOC: %v", err), true
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# Table of Contents: %s\n", toc.Book.Title))
		sb.WriteString(fmt.Sprintf("- **Author:** %s | **Book ID:** `%s` | **Category:** %s\n", toc.Book.Author, toc.Book.ID, toc.Book.Category))
		sb.WriteString(fmt.Sprintf("- **Chapters:** %d | **Parent Sections:** %d | **Child Chunks:** %d\n\n",
			toc.Book.ChapterCount, toc.Book.ParentCount, toc.Book.ChunkCount))

		for _, ch := range toc.Chapters {
			sb.WriteString(fmt.Sprintf("## %d. %s (`chapter_id`: `%s`, %d chars)\n", ch.ChapterIndex, ch.Title, ch.ChapterID, ch.CharLength))
			for _, sec := range ch.Sections {
				if sec.Title != "" && !strings.EqualFold(sec.Title, ch.Title) {
					sb.WriteString(fmt.Sprintf("  - %s (`parent_id`: `%s`, %d chars)\n", sec.Title, sec.ParentID, sec.CharLength))
				}
			}
		}
		return sb.String(), false

	case "search_chapters":
		query := stringArg(args, "query")
		if query == "" {
			return "Error: 'query' parameter is required.", true
		}
		hits := s.engine.Store().SearchChapters(
			query,
			stringArg(args, "book_filter"),
			stringArg(args, "category"),
			stringArg(args, "tag"),
			intArg(args, "top_k", 15),
		)
		if len(hits) == 0 {
			return "No matching chapters or section headings found.", false
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# Matching Chapters & Sections (%d)\n\n", len(hits)))
		for i, h := range hits {
			sb.WriteString(fmt.Sprintf("%d. **%s** (%s) — **%s > %s**\n", i+1, h.BookTitle, h.Author, h.ChapterTitle, h.SectionTitle))
			sb.WriteString(fmt.Sprintf("   - `chapter_id`: `%s` | `parent_id`: `%s` | Category: %s | Score: `%.1f`\n",
				h.ChapterID, h.ParentID, h.Category, h.Score))
			if h.Snippet != "" {
				sb.WriteString(fmt.Sprintf("   - *Preview:* %s\n", h.Snippet))
			}
		}
		return sb.String(), false

	case "get_chapter":
		chapID := stringArg(args, "chapter_id")
		chap, ok := s.engine.Store().GetChapter(chapID)
		if !ok {
			return fmt.Sprintf("Chapter %q not found.", chapID), true
		}
		return fmt.Sprintf("# %s (Chapter ID: %s)\n\n%s", chap.Title, chap.ID, chap.Content), false

	case "compare_books_on_topic":
		topic := stringArg(args, "topic")
		if topic == "" {
			return "Error: 'topic' parameter is required.", true
		}
		_, markdown := s.engine.CompareBooksOnTopic(
			ctx,
			topic,
			stringArg(args, "category"),
			stringArg(args, "tag"),
			intArg(args, "max_books", 5),
		)
		return markdown, false

	case "extract_code_examples":
		query := stringArg(args, "query")
		if query == "" {
			return "Error: 'query' parameter is required.", true
		}
		results := s.engine.ExtractCodeExamples(
			ctx,
			query,
			stringArg(args, "language"),
			stringArg(args, "book_filter"),
			stringArg(args, "category"),
			intArg(args, "top_k", 5),
		)
		if len(results) == 0 {
			return "No code-heavy sections matched your query.", false
		}
		return s.formatSearchResults(results), false

	case "deduplicate_books":
		apply := boolArg(args, "apply")
		rep, err := s.engine.Store().DeduplicateBooks(ctx, apply)
		if err != nil {
			return fmt.Sprintf("Deduplication failed: %v", err), true
		}
		if len(rep.Groups) == 0 {
			return "No duplicate books detected in the catalog.", false
		}
		var sb strings.Builder
		if apply {
			sb.WriteString(fmt.Sprintf("# Deduplication Applied: Removed %d Duplicate Books (%d chunks freed)\n\n", len(rep.RemovedBooks), rep.FreedChunks))
		} else {
			sb.WriteString(fmt.Sprintf("# Duplicate Books Analysis (Dry Run): %d Duplicate Groups (%d chunks can be freed)\n*Call `deduplicate_books` with `apply: true` to remove duplicates.*\n\n", len(rep.Groups), rep.FreedChunks))
		}
		for i, g := range rep.Groups {
			sb.WriteString(fmt.Sprintf("%d. **%s** by %s — *%s*\n", i+1, g.Keep.Title, g.Keep.Author, g.Reason))
			sb.WriteString(fmt.Sprintf("   - **Keep:** `[%s]` `%s` (%d chunks)\n", g.Keep.ID, g.Keep.FileName, g.Keep.ChunkCount))
			for _, d := range g.Duplicates {
				sb.WriteString(fmt.Sprintf("   - **Duplicate:** `[%s]` `%s` (%d chunks)\n", d.ID, d.FileName, d.ChunkCount))
			}
		}
		return sb.String(), false

	case "save_note":
		content := stringArg(args, "content")
		if content == "" {
			return "Error: 'content' parameter is required.", true
		}
		var tags []string
		if rawTags, ok := args["tags"].([]any); ok {
			for _, t := range rawTags {
				if s, okS := t.(string); okS && strings.TrimSpace(s) != "" {
					tags = append(tags, strings.TrimSpace(s))
				}
			}
		}
		saved, err := s.engine.Store().SaveNote(store.NoteRecord{
			Title:     stringArg(args, "title"),
			Content:   content,
			BookID:    stringArg(args, "book_id"),
			ChapterID: stringArg(args, "chapter_id"),
			ParentID:  stringArg(args, "parent_id"),
			Tags:      tags,
		})
		if err != nil {
			return fmt.Sprintf("Error saving note: %v", err), true
		}
		return fmt.Sprintf("Saved note **%s** (`id`: `%s`, linked book: `%s`, chapter: `%s`).",
			saved.Title, saved.ID, saved.BookTitle, saved.ChapterTitle), false

	case "list_notes":
		notes := s.engine.Store().ListNotes(
			stringArg(args, "query"),
			stringArg(args, "book_filter"),
			stringArg(args, "tag"),
		)
		if len(notes) == 0 {
			return "No saved notes found.", false
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# Saved Library Notes (%d)\n\n", len(notes)))
		for _, n := range notes {
			sb.WriteString(fmt.Sprintf("## %s (`id`: `%s`)\n", n.Title, n.ID))
			if n.BookTitle != "" {
				sb.WriteString(fmt.Sprintf("- **Book:** %s (`%s`)", n.BookTitle, n.BookID))
				if n.ChapterTitle != "" {
					sb.WriteString(fmt.Sprintf(" | **Chapter:** %s (`%s`)", n.ChapterTitle, n.ChapterID))
				}
				sb.WriteString("\n")
			}
			if len(n.Tags) > 0 {
				sb.WriteString(fmt.Sprintf("- **Tags:** `%s`\n", strings.Join(n.Tags, "`, `")))
			}
			sb.WriteString(fmt.Sprintf("- **Updated:** %s\n\n%s\n\n---\n\n", n.UpdatedAt, n.Content))
		}
		return sb.String(), false

	case "delete_note":
		noteID := stringArg(args, "note_id")
		if noteID == "" {
			return "Error: 'note_id' parameter is required.", true
		}
		deleted, err := s.engine.Store().DeleteNote(noteID)
		if err != nil {
			return fmt.Sprintf("Error deleting note: %v", err), true
		}
		return fmt.Sprintf("Deleted note **%s** (`%s`).", deleted.Title, deleted.ID), false

	case "export_dossier":
		topic := stringArg(args, "topic")
		if topic == "" {
			return "Error: 'topic' parameter is required.", true
		}
		md, savedPath, err := s.engine.ExportTopicDossier(
			ctx,
			topic,
			stringArg(args, "category"),
			stringArg(args, "tag"),
			stringArg(args, "output_path"),
			intArg(args, "top_k", 6),
		)
		if err != nil {
			return fmt.Sprintf("Error exporting dossier: %v", err), true
		}
		if savedPath != "" {
			return fmt.Sprintf("Dossier saved to `%s`.\n\n%s", savedPath, md), false
		}
		return md, false

	case "enrich_books":
		bookID := stringArg(args, "book_id")
		force := boolArg(args, "force")
		if bookID != "" {
			rec, err := s.engine.EnrichBook(ctx, bookID)
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

	case "remove_book":
		target := stringArg(args, "book_id")
		if target == "" {
			return "Error: 'book_id' parameter is required.", true
		}
		removed, err := s.engine.Store().RemoveBook(ctx, target)
		if err != nil {
			return fmt.Sprintf("Error removing book: %v", err), true
		}
		return fmt.Sprintf("Removed book **%s** by %s (`id`: `%s`, %d chapters, %d parent sections, %d child chunks).",
			removed.Title, removed.Author, removed.ID, removed.ChapterCount, removed.ParentCount, removed.ChunkCount), false

	default:
		return fmt.Sprintf("Unknown tool: %s", name), true
	}
}

func (s *Server) formatSearchResults(results []store.SearchResult) string {
	var sb strings.Builder
	for i, res := range results {
		ck := res.Chunk
		parentContent := res.ParentText
		if parentContent == "" {
			parentContent = ck.Text
		}
		bookRec, hasBook := s.engine.Store().GetBookRecord(ck.BookID)
		catTagLine := ""
		if hasBook && (bookRec.Category != "" || len(bookRec.Tags) > 0) {
			catTagLine = fmt.Sprintf("**Category:** %s | **Tags:** `%s`\n", bookRec.Category, strings.Join(bookRec.Tags, "`, `"))
		}
		sb.WriteString(fmt.Sprintf(
			"### Result %d (Score: %.4f | Child Hits: %d | Chapter ID: %s | Parent ID: %s)\n**Book:** %s (%s)\n%s**Chapter:** %s > %s\n\n%s\n\n---\n\n",
			i+1, res.Score, res.ChildHits, ck.ChapterID, res.ParentID, ck.BookTitle, ck.Author, catTagLine, ck.ChapterTitle, ck.SectionTitle, parentContent,
		))
	}
	return sb.String()
}

var nonSlugCharRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugifyCategory(cat string) string {
	return strings.Trim(nonSlugCharRe.ReplaceAllString(strings.ToLower(cat), "-"), "-")
}

func (s *Server) listResources() []map[string]any {
	cats, _ := s.engine.Store().ListCategoriesAndTags()
	res := []map[string]any{
		{
			"uri":         "library://catalog/overview",
			"name":        "Library Catalog Overview & Taxonomy",
			"description": "Summary statistics, categories, and tags across all indexed technical books.",
			"mimeType":    "text/markdown",
		},
		{
			"uri":         "library://catalog/notes",
			"name":        "Saved Library Notes & Bookmarks",
			"description": "All saved annotations, highlights, and architectural notes linked to books and chapters.",
			"mimeType":    "text/markdown",
		},
		{
			"uri":         "library://catalog/duplicates",
			"name":        "Duplicate Books Report",
			"description": "Analysis of duplicate book editions detected across the library.",
			"mimeType":    "text/markdown",
		},
	}
	for _, c := range cats {
		slug := slugifyCategory(c)
		res = append(res, map[string]any{
			"uri":         "library://category/" + slug,
			"name":        "Category: " + c,
			"description": fmt.Sprintf("All indexed books in category %q", c),
			"mimeType":    "text/markdown",
		})
	}
	return res
}

func (s *Server) readResource(uri string) (string, string, error) {
	switch uri {
	case "library://catalog/overview":
		bCount, chCount, pCount, ckCount, vCount := s.engine.Store().Stats()
		cats, tags := s.engine.Store().ListCategoriesAndTags()
		var sb strings.Builder
		sb.WriteString("# Library MCP Catalog Overview\n\n")
		sb.WriteString(fmt.Sprintf("- **Indexed Books:** %d\n- **Chapters:** %d\n- **Parent Sections:** %d\n- **Child Chunks:** %d (%d dense vectors)\n",
			bCount, chCount, pCount, ckCount, vCount))
		sb.WriteString(fmt.Sprintf("- **Categories (%d):** %s\n", len(cats), strings.Join(cats, " | ")))
		if len(tags) > 40 {
			sb.WriteString(fmt.Sprintf("- **Sample Tags (%d total):** `%s`\n", len(tags), strings.Join(tags[:40], "`, `")))
		} else if len(tags) > 0 {
			sb.WriteString(fmt.Sprintf("- **Tags (%d):** `%s`\n", len(tags), strings.Join(tags, "`, `")))
		}
		return sb.String(), "text/markdown", nil

	case "library://catalog/notes":
		out, _ := s.executeTool(context.Background(), "list_notes", nil)
		return out, "text/markdown", nil

	case "library://catalog/duplicates":
		out, _ := s.executeTool(context.Background(), "deduplicate_books", map[string]any{"apply": false})
		return out, "text/markdown", nil
	}

	if strings.HasPrefix(uri, "library://category/") {
		wantSlug := strings.TrimPrefix(uri, "library://category/")
		cats, _ := s.engine.Store().ListCategoriesAndTags()
		for _, c := range cats {
			if slugifyCategory(c) == wantSlug {
				books := s.engine.Store().ListBooksFiltered(c, "")
				return formatBooksList(books), "text/markdown", nil
			}
		}
	}

	return "", "", fmt.Errorf("resource %q not found", uri)
}

func (s *Server) listPrompts() []map[string]any {
	return []map[string]any{
		{
			"name":        "architecture-review",
			"description": "Ground an architectural review of code or system design in principles from your Software Architecture & Design books.",
			"arguments": []map[string]any{
				{
					"name":        "topic",
					"description": "Architectural pattern, service boundary, or design decision to review.",
					"required":    true,
				},
			},
		},
		{
			"name":        "security-audit",
			"description": "Ground a security audit or threat model in principles from your Security & Cryptography books.",
			"arguments": []map[string]any{
				{
					"name":        "topic",
					"description": "Component, authentication flow, or vulnerability class to audit.",
					"required":    true,
				},
			},
		},
		{
			"name":        "code-refactoring",
			"description": "Ground refactoring and code quality improvements in your Software Engineering & DevOps and Refactoring books.",
			"arguments": []map[string]any{
				{
					"name":        "topic",
					"description": "Code smell, legacy pattern, or testing challenge to refactor.",
					"required":    true,
				},
			},
		},
		{
			"name":        "topic-study-guide",
			"description": "Generate a multi-book technical study guide on any subject using diverse books from your library.",
			"arguments": []map[string]any{
				{
					"name":        "topic",
					"description": "Technical subject to study.",
					"required":    true,
				},
			},
		},
	}
}

func (s *Server) getPrompt(ctx context.Context, name string, args map[string]string) (string, []map[string]any, error) {
	topic := strings.TrimSpace(args["topic"])
	if topic == "" {
		topic = "software architecture and modularity"
	}

	var category string
	var instruction string
	switch name {
	case "architecture-review":
		category = "Software Architecture"
		instruction = fmt.Sprintf("Perform a rigorous architectural review regarding %q grounded in the following excerpts from the user's technical library. Cite specific books, authors, and chapters:", topic)
	case "security-audit":
		category = "Security"
		instruction = fmt.Sprintf("Perform a defense-in-depth security audit regarding %q grounded in the following security book excerpts. Cite specific books, authors, and chapters:", topic)
	case "code-refactoring":
		category = ""
		instruction = fmt.Sprintf("Provide concrete refactoring and testing recommendations for %q grounded in the following book excerpts. Cite specific books, authors, and chapters:", topic)
	case "topic-study-guide":
		_, md := s.engine.CompareBooksOnTopic(ctx, topic, "", "", 5)
		return "Multi-book study guide for " + topic, []map[string]any{
			{
				"role": "user",
				"content": map[string]any{
					"type": "text",
					"text": fmt.Sprintf("Synthesize a structured technical study guide for %q based on these multi-author perspectives from my library:\n\n%s", topic, md),
				},
			},
		}, nil
	default:
		return "", nil, fmt.Errorf("prompt %q not found", name)
	}

	results := s.engine.SearchWithOptions(ctx, store.SearchOptions{
		Query:      topic,
		Category:   category,
		MaxPerBook: 1,
		TopK:       4,
	})
	excerpts := s.formatSearchResults(results)

	return fmt.Sprintf("Grounded %s for %s", name, topic), []map[string]any{
		{
			"role": "user",
			"content": map[string]any{
				"type": "text",
				"text": fmt.Sprintf("%s\n\n%s", instruction, excerpts),
			},
		},
	}, nil
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func intArg(args map[string]any, key string, defVal int) int {
	if args == nil {
		return defVal
	}
	if f, ok := args[key].(float64); ok {
		return int(f)
	}
	if i, ok := args[key].(int); ok {
		return i
	}
	return defVal
}

func boolArg(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	b, _ := args[key].(bool)
	return b
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
