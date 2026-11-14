package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"library-mcp/internal/domain"
	"library-mcp/internal/rag"
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
			return toJSONError("'query' parameter is required.")
		}
		opts := domain.SearchOptions{
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
		items := s.toMCPSearchResults(results)
		return toJSON(map[string]any{
			"query":   query,
			"count":   len(items),
			"results": items,
		})

	case "list_books":
		books := s.engine.ListBooks(domain.BookFilter{
			Category: stringArg(args, "category"),
			Tag:      stringArg(args, "tag"),
			Author:   stringArg(args, "author"),
			YearMin:  intArg(args, "year_min", 0),
		})
		items := toMCPBookItems(books)
		return toJSON(map[string]any{
			"count": len(items),
			"books": items,
		})

	case "get_book_toc":
		bookID := stringArg(args, "book_id")
		if bookID == "" {
			return toJSONError("'book_id' parameter is required.")
		}
		toc, err := s.engine.GetBookTOC(bookID)
		if err != nil {
			return toJSONError(fmt.Sprintf("Error retrieving TOC: %v", err))
		}
		if toc.Book.Tags == nil {
			toc.Book.Tags = []string{}
		}
		if toc.Book.Subjects == nil {
			toc.Book.Subjects = []string{}
		}
		if toc.Chapters == nil {
			toc.Chapters = []domain.TOCChapter{}
		}
		return toJSON(toc)

	case "search_chapters":
		query := stringArg(args, "query")
		if query == "" {
			return toJSONError("'query' parameter is required.")
		}
		hits := s.engine.SearchChapters(
			query,
			stringArg(args, "book_filter"),
			stringArg(args, "category"),
			stringArg(args, "tag"),
			intArg(args, "top_k", 15),
		)
		if hits == nil {
			hits = []domain.ChapterSearchHit{}
		}
		return toJSON(map[string]any{
			"query":    query,
			"count":    len(hits),
			"chapters": hits,
		})

	case "get_chapter":
		chapID := stringArg(args, "chapter_id")
		if chapID == "" {
			return toJSONError("'chapter_id' parameter is required.")
		}
		chap, ok := s.engine.GetChapter(chapID)
		if !ok {
			return toJSONError(fmt.Sprintf("Chapter %q not found.", chapID))
		}
		return toJSON(map[string]any{
			"chapter_id":    chap.ID,
			"book_id":       chap.BookID,
			"chapter_index": chap.ChapterIndex,
			"title":         chap.Title,
			"href":          chap.Href,
			"char_length":   len(chap.Content),
			"content":       chap.Content,
		})

	case "compare_books_on_topic":
		topic := stringArg(args, "topic")
		if topic == "" {
			return toJSONError("'topic' parameter is required.")
		}
		results, markdown := s.engine.CompareBooksOnTopic(
			ctx,
			topic,
			stringArg(args, "category"),
			stringArg(args, "tag"),
			intArg(args, "max_books", 5),
		)
		items := s.toMCPSearchResults(results)
		return toJSON(map[string]any{
			"topic":            topic,
			"books_compared":   len(items),
			"perspectives":     items,
			"markdown_summary": markdown,
		})

	case "extract_code_examples":
		query := stringArg(args, "query")
		if query == "" {
			return toJSONError("'query' parameter is required.")
		}
		lang := stringArg(args, "language")
		results := s.engine.ExtractCodeExamples(
			ctx,
			query,
			lang,
			stringArg(args, "book_filter"),
			stringArg(args, "category"),
			intArg(args, "top_k", 5),
		)
		items := s.toMCPSearchResults(results)
		return toJSON(map[string]any{
			"query":    query,
			"language": lang,
			"count":    len(items),
			"results":  items,
		})

	case "deduplicate_books":
		apply := boolArg(args, "apply")
		rep, err := s.engine.DeduplicateBooks(ctx, apply)
		if err != nil {
			return toJSONError(fmt.Sprintf("Deduplication failed: %v", err))
		}
		if rep.Groups == nil {
			rep.Groups = []domain.DuplicateGroup{}
		}
		return toJSON(rep)

	case "save_note":
		content := stringArg(args, "content")
		if content == "" {
			return toJSONError("'content' parameter is required.")
		}
		var tags []string
		if rawTags, ok := args["tags"].([]any); ok {
			for _, t := range rawTags {
				if s, okS := t.(string); okS && strings.TrimSpace(s) != "" {
					tags = append(tags, strings.TrimSpace(s))
				}
			}
		}
		saved, err := s.engine.SaveNote(domain.NoteRecord{
			Title:     stringArg(args, "title"),
			Content:   content,
			BookID:    stringArg(args, "book_id"),
			ChapterID: stringArg(args, "chapter_id"),
			ParentID:  stringArg(args, "parent_id"),
			Tags:      tags,
		})
		if err != nil {
			return toJSONError(fmt.Sprintf("Error saving note: %v", err))
		}
		if saved.Tags == nil {
			saved.Tags = []string{}
		}
		return toJSON(map[string]any{
			"status": "saved",
			"note":   saved,
		})

	case "list_notes":
		notes := s.engine.ListNotes(
			stringArg(args, "query"),
			stringArg(args, "book_filter"),
			stringArg(args, "tag"),
		)
		if notes == nil {
			notes = []domain.NoteRecord{}
		}
		return toJSON(map[string]any{
			"count": len(notes),
			"notes": notes,
		})

	case "delete_note":
		noteID := stringArg(args, "note_id")
		if noteID == "" {
			return toJSONError("'note_id' parameter is required.")
		}
		deleted, err := s.engine.DeleteNote(noteID)
		if err != nil {
			return toJSONError(fmt.Sprintf("Error deleting note: %v", err))
		}
		return toJSON(map[string]any{
			"status": "deleted",
			"note":   deleted,
		})

	case "export_dossier":
		topic := stringArg(args, "topic")
		if topic == "" {
			return toJSONError("'topic' parameter is required.")
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
			return toJSONError(fmt.Sprintf("Error exporting dossier: %v", err))
		}
		return toJSON(map[string]any{
			"topic":      topic,
			"saved_path": savedPath,
			"markdown":   md,
		})

	case "enrich_books":
		bookID := stringArg(args, "book_id")
		force := boolArg(args, "force")
		if bookID != "" {
			rec, err := s.engine.EnrichBook(ctx, bookID)
			if err != nil {
				return toJSONError(fmt.Sprintf("Error enriching book %q: %v", bookID, err))
			}
			items := toMCPBookItems([]domain.BookRecord{rec})
			return toJSON(map[string]any{
				"count": len(items),
				"books": items,
			})
		}
		books, err := s.engine.EnrichAllBooks(ctx, force)
		if err != nil {
			return toJSONError(fmt.Sprintf("Error enriching books: %v", err))
		}
		items := toMCPBookItems(books)
		return toJSON(map[string]any{
			"count": len(items),
			"books": items,
		})

	case "remove_book":
		target := stringArg(args, "book_id")
		if target == "" {
			return toJSONError("'book_id' parameter is required.")
		}
		removed, err := s.engine.RemoveBook(ctx, target)
		if err != nil {
			return toJSONError(fmt.Sprintf("Error removing book: %v", err))
		}
		return toJSON(map[string]any{
			"status": "removed",
			"book":   toMCPBookItem(removed),
		})

	default:
		return toJSONError(fmt.Sprintf("Unknown tool: %s", name))
	}
}

func (s *Server) formatSearchResults(results []domain.SearchResult) string {
	var sb strings.Builder
	for i, res := range results {
		ck := res.Chunk
		parentContent := res.ParentText
		if parentContent == "" {
			parentContent = ck.Text
		}
		bookRec, hasBook := s.engine.GetBookRecord(ck.BookID)
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
	cats, _ := s.engine.ListCategoriesAndTags()
	res := []map[string]any{
		{
			"uri":         "library://catalog/overview",
			"name":        "Library Catalog Overview & Taxonomy",
			"description": "Summary statistics, categories, and tags across all indexed technical books.",
			"mimeType":    "application/json",
		},
		{
			"uri":         "library://catalog/notes",
			"name":        "Saved Library Notes & Bookmarks",
			"description": "All saved annotations, highlights, and architectural notes linked to books and chapters.",
			"mimeType":    "application/json",
		},
		{
			"uri":         "library://catalog/duplicates",
			"name":        "Duplicate Books Report",
			"description": "Analysis of duplicate book editions detected across the library.",
			"mimeType":    "application/json",
		},
	}
	for _, c := range cats {
		slug := slugifyCategory(c)
		res = append(res, map[string]any{
			"uri":         "library://category/" + slug,
			"name":        "Category: " + c,
			"description": fmt.Sprintf("All indexed books in category %q", c),
			"mimeType":    "application/json",
		})
	}
	return res
}

func (s *Server) readResource(uri string) (string, string, error) {
	switch uri {
	case "library://catalog/overview":
		ov := s.engine.CatalogOverview()
		if ov.Categories == nil {
			ov.Categories = []string{}
		}
		if ov.Tags == nil {
			ov.Tags = []string{}
		}
		out, _ := toJSON(ov)
		return out, "application/json", nil

	case "library://catalog/notes":
		out, _ := s.executeTool(context.Background(), "list_notes", nil)
		return out, "application/json", nil

	case "library://catalog/duplicates":
		out, _ := s.executeTool(context.Background(), "deduplicate_books", map[string]any{"apply": false})
		return out, "application/json", nil
	}

	if strings.HasPrefix(uri, "library://category/") {
		wantSlug := strings.TrimPrefix(uri, "library://category/")
		cats, _ := s.engine.ListCategoriesAndTags()
		for _, c := range cats {
			if slugifyCategory(c) == wantSlug {
				books := s.engine.ListBooks(domain.BookFilter{Category: c})
				items := toMCPBookItems(books)
				out, _ := toJSON(map[string]any{
					"category": c,
					"count":    len(items),
					"books":    items,
				})
				return out, "application/json", nil
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

	results := s.engine.SearchWithOptions(ctx, domain.SearchOptions{
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

type mcpBookItem struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle,omitempty"`
	Author         string   `json:"author"`
	Category       string   `json:"category"`
	Tags           []string `json:"tags"`
	Publisher      string   `json:"publisher,omitempty"`
	PublishedYear  string   `json:"published_year,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	PageCount      int      `json:"page_count,omitempty"`
	Language       string   `json:"language,omitempty"`
	Format         string   `json:"format,omitempty"`
	FileName       string   `json:"file_name,omitempty"`
	MetadataSource string   `json:"metadata_source,omitempty"`
	WebURL         string   `json:"web_url,omitempty"`
	ChapterCount   int      `json:"chapter_count"`
	ParentCount    int      `json:"parent_count"`
	ChunkCount     int      `json:"chunk_count"`
	IndexedAt      string   `json:"indexed_at,omitempty"`
}

type mcpSearchResult struct {
	Rank         int      `json:"rank"`
	Score        float64  `json:"score"`
	BM25Score    float64  `json:"bm25_score,omitempty"`
	VectorScore  float64  `json:"vector_score,omitempty"`
	ChildHits    int      `json:"child_hits"`
	MatchSources []string `json:"match_sources"`
	BookID       string   `json:"book_id"`
	BookTitle    string   `json:"book_title"`
	Author       string   `json:"author"`
	Category     string   `json:"category,omitempty"`
	Tags         []string `json:"tags"`
	ChapterID    string   `json:"chapter_id"`
	ChapterTitle string   `json:"chapter_title"`
	ParentID     string   `json:"parent_id"`
	SectionTitle string   `json:"section_title"`
	HasCode      bool     `json:"has_code"`
	Content      string   `json:"content"`
}

func toJSON(v any) (string, bool) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toJSONError(fmt.Sprintf("failed to serialize JSON response: %v", err))
	}
	return string(b), false
}

func toJSONError(msg string) (string, bool) {
	b, err := json.MarshalIndent(map[string]string{"error": msg}, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, msg), true
	}
	return string(b), true
}

func toMCPBookItem(b domain.BookRecord) mcpBookItem {
	category := b.Category
	if category == "" {
		category = "Uncategorized"
	}
	tags := make([]string, len(b.Tags))
	copy(tags, b.Tags)
	return mcpBookItem{
		ID:             b.ID,
		Title:          b.Title,
		Subtitle:       b.Subtitle,
		Author:         b.Author,
		Category:       category,
		Tags:           tags,
		Publisher:      b.Publisher,
		PublishedYear:  b.PublishedYear,
		ISBN:           b.ISBN,
		PageCount:      b.PageCount,
		Language:       b.Language,
		Format:         b.Format,
		FileName:       b.FileName,
		MetadataSource: b.MetadataSource,
		WebURL:         b.WebURL,
		ChapterCount:   b.ChapterCount,
		ParentCount:    b.ParentCount,
		ChunkCount:     b.ChunkCount,
		IndexedAt:      b.IndexedAt,
	}
}

func toMCPBookItems(books []domain.BookRecord) []mcpBookItem {
	items := make([]mcpBookItem, 0, len(books))
	for _, b := range books {
		items = append(items, toMCPBookItem(b))
	}
	return items
}

func (s *Server) toMCPSearchResults(results []domain.SearchResult) []mcpSearchResult {
	items := make([]mcpSearchResult, 0, len(results))
	for i, res := range results {
		ck := res.Chunk
		parentContent := res.ParentText
		if parentContent == "" {
			parentContent = ck.Text
		}
		var category string
		tags := []string{}
		if bookRec, hasBook := s.engine.GetBookRecord(ck.BookID); hasBook {
			category = bookRec.Category
			if len(bookRec.Tags) > 0 {
				tags = append([]string(nil), bookRec.Tags...)
			}
		}
		sources := []string{}
		if len(res.MatchSources) > 0 {
			sources = append([]string(nil), res.MatchSources...)
		}
		items = append(items, mcpSearchResult{
			Rank:         i + 1,
			Score:        res.Score,
			BM25Score:    res.BM25Score,
			VectorScore:  res.VectorScore,
			ChildHits:    res.ChildHits,
			MatchSources: sources,
			BookID:       ck.BookID,
			BookTitle:    ck.BookTitle,
			Author:       ck.Author,
			Category:     category,
			Tags:         tags,
			ChapterID:    ck.ChapterID,
			ChapterTitle: ck.ChapterTitle,
			ParentID:     res.ParentID,
			SectionTitle: ck.SectionTitle,
			HasCode:      ck.HasCode,
			Content:      parentContent,
		})
	}
	return items
}
