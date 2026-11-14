package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"library-mcp/internal/config"
	"library-mcp/internal/domain"
	"library-mcp/internal/rag"
	"library-mcp/internal/store"
)

type stubAI struct{}

func (stubAI) ListModels(context.Context) ([]domain.ModelInfo, error) {
	return nil, errors.New("offline")
}

func (stubAI) EmbedBatch(context.Context, string, []string) ([][]float32, error) {
	return nil, errors.New("offline")
}

func (stubAI) Chat(context.Context, string, []domain.ChatMessage) (string, error) {
	return "", errors.New("offline")
}

type stubMeta struct{}

func (stubMeta) Enrich(_ context.Context, seed domain.BookMetadata, _ []string) domain.BookMetadata {
	return seed
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(dataDir, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})

	cfg := config.Default()
	cfg.DataDir = dataDir
	eng := rag.NewEngineWithPorts(cfg, st, stubAI{}, nil, stubMeta{})
	return NewServer(eng), st
}

func seedTestBook(t *testing.T, st *store.Store) {
	t.Helper()
	book := &domain.ParsedBook{
		ID:            "book-1",
		FileHash:      "hash-1",
		FileName:      "clean_architecture.epub",
		Format:        "epub",
		Title:         "Clean Architecture",
		Subtitle:      "A Craftsman's Guide to Software Structure and Design",
		Author:        "Robert C. Martin",
		Publisher:     "Prentice Hall",
		PublishedYear: "2017",
		ISBN:          "9780134494166",
		Language:      "en",
		Category:      "Software Architecture & Design",
		Tags:          []string{"clean-architecture", "hexagonal-architecture"},
		Chapters: []domain.ParsedChapter{
			{
				ID:           "chap-1",
				BookID:       "book-1",
				ChapterIndex: 1,
				Title:        "The Dependency Rule",
				Href:         "ch01.xhtml",
				Content:      "Source code dependencies must point only inward toward higher-level policies.",
			},
		},
		Parents: []domain.ParentSection{
			{
				ID:           "parent-1",
				BookID:       "book-1",
				ChapterID:    "chap-1",
				BookTitle:    "Clean Architecture",
				Author:       "Robert C. Martin",
				ChapterTitle: "The Dependency Rule",
				SectionTitle: "Boundaries and Ports",
				ParentIndex:  1,
				HasCode:      true,
				Content:      "Source code dependencies must point only inward toward higher-level policies.\n```go\ntype Port interface {\n    Execute()\n}\nfunc NewPort() Port {\n    return nil\n}\n```",
			},
		},
		Chunks: []domain.ParsedChunk{
			{
				ID:           "chunk-1",
				PointUUID:    "00000000-0000-0000-0000-000000000001",
				ParentID:     "parent-1",
				BookID:       "book-1",
				ChapterID:    "chap-1",
				BookTitle:    "Clean Architecture",
				Author:       "Robert C. Martin",
				ChapterTitle: "The Dependency Rule",
				SectionTitle: "Boundaries and Ports",
				ChunkIndex:   1,
				HasCode:      true,
				Text:         "Source code dependencies must point only inward toward higher-level policies. func NewPort() Port { return nil }",
			},
		},
	}
	if err := st.UpsertBook(context.Background(), book, nil); err != nil {
		t.Fatalf("upsert book: %v", err)
	}
}

func callRPC(t *testing.T, srv *Server, method string, params any) map[string]any {
	t.Helper()
	rawParams, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	reqLine, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  method,
		Params:  rawParams,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var out bytes.Buffer
	if err := srv.ServeStdio(context.Background(), strings.NewReader(string(reqLine)+"\n"), &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	var resp struct {
		JSONRPC string         `json:"jsonrpc"`
		Result  map[string]any `json:"result"`
		Error   *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", resp.Error)
	}
	return resp.Result
}

func callToolJSON(t *testing.T, srv *Server, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res := callRPC(t, srv, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	isErr, _ := res["isError"].(bool)
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected 1 content item for %s, got %#v", name, res["content"])
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected content item shape: %#v", content[0])
	}
	text, _ := item["text"].(string)

	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool %s did not return valid JSON: %v\nraw text:\n%s", name, err, text)
	}
	return payload, isErr
}

func TestListBooksEmptyReturnsStructuredJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	payload, isErr := callToolJSON(t, srv, "list_books", nil)
	if isErr {
		t.Fatalf("expected isError=false, got payload=%v", payload)
	}
	if count, _ := payload["count"].(float64); count != 0 {
		t.Fatalf("expected count=0, got %v", payload["count"])
	}
	books, ok := payload["books"].([]any)
	if !ok {
		t.Fatalf("expected books to be JSON array, got %T (%v)", payload["books"], payload["books"])
	}
	if len(books) != 0 {
		t.Fatalf("expected empty books slice, got %d", len(books))
	}
}

func TestToolsReturnStructuredJSON(t *testing.T) {
	srv, st := newTestServer(t)
	seedTestBook(t, st)

	// 1. list_books
	booksPayload, isErr := callToolJSON(t, srv, "list_books", nil)
	if isErr {
		t.Fatalf("list_books failed: %v", booksPayload)
	}
	if count, _ := booksPayload["count"].(float64); count != 1 {
		t.Fatalf("expected count=1, got %v", booksPayload["count"])
	}
	booksList := booksPayload["books"].([]any)
	firstBook := booksList[0].(map[string]any)
	if firstBook["id"] != "book-1" || firstBook["title"] != "Clean Architecture" {
		t.Fatalf("unexpected book item: %#v", firstBook)
	}

	// 2. search_books
	searchPayload, isErr := callToolJSON(t, srv, "search_books", map[string]any{"query": "inward dependencies"})
	if isErr {
		t.Fatalf("search_books failed: %v", searchPayload)
	}
	if searchPayload["query"] != "inward dependencies" {
		t.Fatalf("unexpected query echo: %v", searchPayload["query"])
	}
	if count, _ := searchPayload["count"].(float64); count != 1 {
		t.Fatalf("expected 1 search result, got %v", searchPayload["count"])
	}
	results := searchPayload["results"].([]any)
	firstRes := results[0].(map[string]any)
	if firstRes["book_id"] != "book-1" || firstRes["parent_id"] != "parent-1" {
		t.Fatalf("unexpected search result item: %#v", firstRes)
	}
	if _, hasContent := firstRes["content"]; hasContent {
		t.Fatalf("search_books should not return full 'content', got %#v", firstRes["content"])
	}
	if snippet, _ := firstRes["snippet"].(string); snippet == "" {
		t.Fatalf("search_books should return non-empty 'snippet', got %#v", firstRes)
	}

	// 2a. get_section
	secPayload, isErr := callToolJSON(t, srv, "get_section", map[string]any{"parent_id": "parent-1"})
	if isErr {
		t.Fatalf("get_section failed: %v", secPayload)
	}
	if secPayload["parent_id"] != "parent-1" || secPayload["section_title"] != "Boundaries and Ports" {
		t.Fatalf("unexpected get_section payload: %#v", secPayload)
	}
	if secContent, _ := secPayload["content"].(string); !strings.Contains(secContent, "type Port interface") {
		t.Fatalf("expected full section content with code block, got %q", secContent)
	}
	if hasCode, _ := secPayload["has_code"].(bool); !hasCode {
		t.Fatalf("expected has_code=true on get_section payload, got %#v", secPayload["has_code"])
	}

	// 2b. extract_code_examples & compare_books_on_topic
	codePayload, isErr := callToolJSON(t, srv, "extract_code_examples", map[string]any{"query": "inward dependencies"})
	if isErr {
		t.Fatalf("extract_code_examples failed: %v", codePayload)
	}
	if count, _ := codePayload["count"].(float64); count != 1 {
		t.Fatalf("expected 1 code result, got %v", codePayload["count"])
	}
	comparePayload, isErr := callToolJSON(t, srv, "compare_books_on_topic", map[string]any{"topic": "inward dependencies"})
	if isErr {
		t.Fatalf("compare_books_on_topic failed: %v", comparePayload)
	}
	if compared, _ := comparePayload["books_compared"].(float64); compared != 1 {
		t.Fatalf("expected 1 compared book, got %v", comparePayload["books_compared"])
	}

	// 3. get_book_toc
	tocPayload, isErr := callToolJSON(t, srv, "get_book_toc", map[string]any{"book_id": "book-1"})
	if isErr {
		t.Fatalf("get_book_toc failed: %v", tocPayload)
	}
	chapters, ok := tocPayload["chapters"].([]any)
	if !ok || len(chapters) != 1 {
		t.Fatalf("expected 1 chapter in TOC JSON, got %#v", tocPayload["chapters"])
	}

	// 4. search_chapters
	chapSearchPayload, isErr := callToolJSON(t, srv, "search_chapters", map[string]any{"query": "Dependency"})
	if isErr {
		t.Fatalf("search_chapters failed: %v", chapSearchPayload)
	}
	if count, _ := chapSearchPayload["count"].(float64); count != 1 {
		t.Fatalf("expected 1 chapter hit, got %v", chapSearchPayload["count"])
	}

	// 5. get_chapter
	chapPayload, isErr := callToolJSON(t, srv, "get_chapter", map[string]any{"chapter_id": "chap-1"})
	if isErr {
		t.Fatalf("get_chapter failed: %v", chapPayload)
	}
	if chapPayload["chapter_id"] != "chap-1" || chapPayload["title"] != "The Dependency Rule" {
		t.Fatalf("unexpected chapter payload: %#v", chapPayload)
	}

	// 6. save_note, list_notes, delete_note
	savePayload, isErr := callToolJSON(t, srv, "save_note", map[string]any{
		"title":   "Inward Dependencies",
		"content": "Core domain must never import adapters.",
		"book_id": "book-1",
		"tags":    []string{"architecture"},
	})
	if isErr || savePayload["status"] != "saved" {
		t.Fatalf("save_note failed: %#v", savePayload)
	}
	savedNote := savePayload["note"].(map[string]any)
	noteID := savedNote["id"].(string)

	notesPayload, isErr := callToolJSON(t, srv, "list_notes", nil)
	if isErr {
		t.Fatalf("list_notes failed: %v", notesPayload)
	}
	if count, _ := notesPayload["count"].(float64); count != 1 {
		t.Fatalf("expected 1 note, got %v", notesPayload["count"])
	}

	delPayload, isErr := callToolJSON(t, srv, "delete_note", map[string]any{"note_id": noteID})
	if isErr || delPayload["status"] != "deleted" {
		t.Fatalf("delete_note failed: %#v", delPayload)
	}

	// 7. Error payload is also JSON
	errPayload, isErr := callToolJSON(t, srv, "search_books", map[string]any{"query": ""})
	if !isErr {
		t.Fatalf("expected isError=true for empty query")
	}
	if _, hasErr := errPayload["error"].(string); !hasErr {
		t.Fatalf("expected error field in JSON error response, got %#v", errPayload)
	}

	// 8. Resources return application/json
	for _, uri := range []string{
		"library://catalog/overview",
		"library://catalog/notes",
		"library://catalog/duplicates",
		"library://category/software-architecture-design",
	} {
		res := callRPC(t, srv, "resources/read", map[string]any{"uri": uri})
		contents := res["contents"].([]any)
		entry := contents[0].(map[string]any)
		if entry["mimeType"] != "application/json" {
			t.Fatalf("resource %s expected mimeType application/json, got %v", uri, entry["mimeType"])
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(entry["text"].(string)), &obj); err != nil {
			t.Fatalf("resource %s did not return valid JSON: %v", uri, err)
		}
	}
}

type stubEmbedAI struct{}

func (stubEmbedAI) ListModels(context.Context) ([]domain.ModelInfo, error) {
	return nil, nil
}

func (stubEmbedAI) EmbedBatch(_ context.Context, _ string, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i := range texts {
		vecs[i] = []float32{0.6, 0.8}
	}
	return vecs, nil
}

func (stubEmbedAI) Chat(context.Context, string, []domain.ChatMessage) (string, error) {
	return "", errors.New("unused")
}

type qdrantQueryCall struct {
	queryText  string
	hasDense   bool
	mode       string
	bookFilter string
	limit      int
}

type recordingVectorIndex struct {
	calls  []qdrantQueryCall
	points []domain.ScoredPoint
}

func (r *recordingVectorIndex) HealthCheck(context.Context) error            { return nil }
func (r *recordingVectorIndex) EnsureCollection(context.Context, int) error  { return nil }
func (r *recordingVectorIndex) DeleteByBookID(context.Context, string) error { return nil }
func (r *recordingVectorIndex) UpsertChunks(context.Context, []domain.ParsedChunk, [][]float32) error {
	return nil
}
func (r *recordingVectorIndex) QueryHybrid(_ context.Context, queryText string, denseVec []float32, mode string, bookID string, limit int) ([]domain.ScoredPoint, error) {
	r.calls = append(r.calls, qdrantQueryCall{
		queryText:  queryText,
		hasDense:   len(denseVec) > 0,
		mode:       mode,
		bookFilter: bookID,
		limit:      limit,
	})
	return r.points, nil
}

func TestFilteredAndCodeSearchAlwaysUseQdrant(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	recVec := &recordingVectorIndex{
		points: []domain.ScoredPoint{
			{
				ID:    "00000000-0000-0000-0000-000000000001",
				Score: 0.91,
				Payload: domain.ParsedChunk{
					ID:           "chunk-1",
					PointUUID:    "00000000-0000-0000-0000-000000000001",
					ParentID:     "parent-1",
					BookID:       "book-1",
					ChapterID:    "chap-1",
					BookTitle:    "Clean Architecture",
					Author:       "Robert C. Martin",
					ChapterTitle: "The Dependency Rule",
					SectionTitle: "Boundaries and Ports",
					ChunkIndex:   1,
					HasCode:      true,
					Text:         "Source code dependencies must point only inward toward higher-level policies.",
				},
			},
		},
	}

	st, err := store.Open(dataDir, recVec)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	seedTestBook(t, st)

	cfg := config.Default()
	cfg.DataDir = dataDir
	eng := rag.NewEngineWithPorts(cfg, st, stubEmbedAI{}, recVec, stubMeta{})
	srv := NewServer(eng)

	// Filtered search with category, tag, year_min, and code_only must still route through Qdrant.
	searchPayload, isErr := callToolJSON(t, srv, "search_books", map[string]any{
		"query":     "ports and boundaries",
		"category":  "Software Architecture",
		"tag":       "hexagonal-architecture",
		"year_min":  2017,
		"code_only": true,
	})
	if isErr {
		t.Fatalf("filtered search_books failed: %v", searchPayload)
	}
	if len(recVec.calls) != 1 {
		t.Fatalf("expected Qdrant QueryHybrid to be called once for filtered search, got %d", len(recVec.calls))
	}
	call := recVec.calls[0]
	if !call.hasDense || call.mode != "hybrid" || call.bookFilter != "book_ids:book-1" {
		t.Fatalf("unexpected Qdrant query call: %+v", call)
	}
	results := searchPayload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}
	hit := results[0].(map[string]any)
	if _, hasContent := hit["content"]; hasContent {
		t.Fatalf("search_books should not include 'content'")
	}
	sources := hit["match_sources"].([]any)
	if len(sources) != 1 || sources[0] != "qdrant:dense+sparse_rrf" {
		t.Fatalf("expected qdrant:dense+sparse_rrf match source, got %#v", sources)
	}

	// extract_code_examples must also route through Qdrant.
	codePayload, isErr := callToolJSON(t, srv, "extract_code_examples", map[string]any{
		"query":    "port interface",
		"category": "Software Architecture",
	})
	if isErr {
		t.Fatalf("extract_code_examples failed: %v", codePayload)
	}
	if len(recVec.calls) != 2 {
		t.Fatalf("expected Qdrant QueryHybrid to be called twice, got %d", len(recVec.calls))
	}
}
