package rag

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"library-mcp/internal/config"
	"library-mcp/internal/epub"
	"library-mcp/internal/metadata"
	"library-mcp/internal/ollama"
	"library-mcp/internal/qdrant"
	"library-mcp/internal/store"
)

// IngestReport summarizes a batch or single-file ingestion run.
type IngestReport struct {
	ScannedFiles int                `json:"scanned_files"`
	IndexedBooks int                `json:"indexed_books"`
	SkippedBooks int                `json:"skipped_books"`
	TotalParents int                `json:"total_parents"`
	TotalChunks  int                `json:"total_chunks"`
	Embedded     bool               `json:"embedded"`
	Books        []store.BookRecord `json:"books,omitempty"`
	Errors       []string           `json:"errors"`
	DurationMs   int64              `json:"duration_ms"`
}

// AskResponse holds the grounded LLM response and its Parent-Child source citations.
type AskResponse struct {
	Question   string               `json:"question"`
	Answer     string               `json:"answer"`
	Model      string               `json:"model"`
	Sources    []store.SearchResult `json:"sources"`
	DurationMs int64                `json:"duration_ms"`
}

// Engine coordinates EPUB ingestion, web metadata enrichment, Qdrant hybrid retrieval, Parent-Child promotion, and Ollama generation.
type Engine struct {
	mu       sync.RWMutex
	cfg      *config.Config
	store    *store.Store
	ollama   *ollama.Client
	qdrant   *qdrant.Client
	enricher *metadata.Enricher
}

// NewEngine creates a new RAG Engine for the given configuration and store.
func NewEngine(cfg *config.Config, st *store.Store) *Engine {
	qc := qdrant.NewClient(cfg.QdrantURL, cfg.QdrantCollection)
	st.SetQdrantClient(qc)
	return &Engine{
		cfg:      cfg,
		store:    st,
		ollama:   ollama.NewClient(cfg.OllamaURL),
		qdrant:   qc,
		enricher: metadata.NewEnricher(),
	}
}

// UpdateConfig updates the engine configuration and refreshes the Ollama and Qdrant clients.
func (e *Engine) UpdateConfig(cfg *config.Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg.Sanitize()
	e.cfg = cfg
	e.ollama = ollama.NewClient(cfg.OllamaURL)
	e.qdrant = qdrant.NewClient(cfg.QdrantURL, cfg.QdrantCollection)
	e.store.SetQdrantClient(e.qdrant)
}

// GetConfig returns a copy of the current configuration.
func (e *Engine) GetConfig() config.Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return *e.cfg
}

// Store returns the underlying hybrid Store.
func (e *Engine) Store() *store.Store {
	return e.store
}

// CheckQdrant checks if the Qdrant server is reachable.
func (e *Engine) CheckQdrant(ctx context.Context) bool {
	e.mu.RLock()
	qc := e.qdrant
	e.mu.RUnlock()
	if qc == nil {
		return false
	}
	return qc.HealthCheck(ctx) == nil
}

// IngestDir scans dirPath for .epub files and indexes them into Qdrant and the Parent-Child catalog.
func (e *Engine) IngestDir(ctx context.Context, dirPath string, force bool, progressFn func(file string, idx, total int)) (*IngestReport, error) {
	start := time.Now()
	cfg := e.GetConfig()

	targetDir := cfg.ResolvedBooksDir()
	if strings.TrimSpace(dirPath) != "" {
		targetDir = config.TranslatePath(dirPath)
	}

	info, err := os.Stat(targetDir)
	if err != nil {
		return nil, fmt.Errorf("books directory %q is not accessible: %w", targetDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", targetDir)
	}

	var epubFiles []string
	err = filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".epub") {
			if config.IsSafeSubpath(p, targetDir) {
				epubFiles = append(epubFiles, p)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	report := &IngestReport{
		ScannedFiles: len(epubFiles),
	}

	for i, filePath := range epubFiles {
		if progressFn != nil {
			progressFn(filepath.Base(filePath), i+1, len(epubFiles))
		}
		indexed, parentsCount, chunksCount, embedded, err := e.IngestFile(ctx, filePath, targetDir, force)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", filepath.Base(filePath), err))
			continue
		}
		if !indexed {
			report.SkippedBooks++
			continue
		}
		report.IndexedBooks++
		report.TotalParents += parentsCount
		report.TotalChunks += chunksCount
		if embedded {
			report.Embedded = true
		}
	}

	// Ensure any existing books lacking web metadata/category/tags are also enriched
	if enrichedBooks, err := e.EnrichAllBooks(ctx, false); err == nil {
		report.Books = enrichedBooks
	} else {
		report.Books = e.store.ListBooks()
	}

	report.DurationMs = time.Since(start).Milliseconds()
	return report, nil
}

// IngestFile parses, enriches web metadata & tags/category, and indexes a single EPUB file verified to reside inside allowedBaseDir.
func (e *Engine) IngestFile(ctx context.Context, filePath, allowedBaseDir string, force bool) (bool, int, int, bool, error) {
	if allowedBaseDir != "" && !config.IsSafeSubpath(filePath, allowedBaseDir) {
		return false, 0, 0, false, fmt.Errorf("security violation: path outside allowed directory boundary")
	}

	fileHash, _, err := epub.ValidateEPUBHeader(filePath)
	if err != nil {
		return false, 0, 0, false, err
	}

	if !force && e.store.HasFileHash(fileHash) {
		return false, 0, 0, false, nil
	}

	cfg := e.GetConfig()
	parsed, err := epub.ParseFile(filePath, cfg.ParentChunkSize, cfg.ChildChunkSize, cfg.ChildOverlap)
	if err != nil {
		return false, 0, 0, false, err
	}
	if len(parsed.Chunks) == 0 {
		return false, 0, 0, false, fmt.Errorf("no readable text chunks found in EPUB")
	}

	e.enrichParsedBook(ctx, parsed)

	embeddings, embeddedOK := e.embedChunksWithBatching(ctx, parsed.Chunks)
	if err := e.store.UpsertBook(ctx, parsed, embeddings); err != nil {
		return false, 0, 0, false, err
	}

	return true, len(parsed.Parents), len(parsed.Chunks), embeddedOK, nil
}

func (e *Engine) enrichParsedBook(ctx context.Context, parsed *epub.ParsedBook) {
	if e.enricher == nil || parsed == nil {
		return
	}
	chapTitles := make([]string, 0, len(parsed.Chapters))
	for _, ch := range parsed.Chapters {
		chapTitles = append(chapTitles, ch.Title)
	}
	seed := metadata.BookMetadata{
		Title:         parsed.Title,
		Subtitle:      parsed.Subtitle,
		Author:        parsed.Author,
		Publisher:     parsed.Publisher,
		PublishedYear: parsed.PublishedYear,
		ISBN:          parsed.ISBN,
		Language:      parsed.Language,
		Description:   parsed.Description,
		Subjects:      parsed.Subjects,
	}
	enriched := e.enricher.Enrich(ctx, seed, chapTitles)
	if enriched.Title != "" {
		parsed.Title = enriched.Title
	}
	parsed.Subtitle = enriched.Subtitle
	if enriched.Author != "" {
		parsed.Author = enriched.Author
	}
	if enriched.Publisher != "" {
		parsed.Publisher = enriched.Publisher
	}
	if enriched.PublishedYear != "" {
		parsed.PublishedYear = enriched.PublishedYear
	}
	if enriched.ISBN != "" {
		parsed.ISBN = enriched.ISBN
	}
	if enriched.PageCount > 0 {
		parsed.PageCount = enriched.PageCount
	}
	if enriched.Language != "" {
		parsed.Language = enriched.Language
	}
	if enriched.Description != "" {
		parsed.Description = enriched.Description
	}
	if len(enriched.Subjects) > 0 {
		parsed.Subjects = enriched.Subjects
	}
	parsed.Category = enriched.Category
	parsed.Tags = enriched.Tags
	parsed.CoverURL = enriched.CoverURL
	parsed.WebURL = enriched.WebURL
	parsed.MetadataSource = enriched.MetadataSource
}

// EnrichBook fetches web metadata (Google Books + Open Library) and categorizes/tags a single indexed book by ID.
func (e *Engine) EnrichBook(ctx context.Context, bookID string) (store.BookRecord, error) {
	rec, chaps, ok := e.store.GetBook(bookID)
	if !ok {
		return store.BookRecord{}, fmt.Errorf("book %q not found", bookID)
	}
	chapTitles := make([]string, 0, len(chaps))
	for _, ch := range chaps {
		chapTitles = append(chapTitles, ch.Title)
	}
	seed := metadata.BookMetadata{
		Title:          rec.Title,
		Subtitle:       rec.Subtitle,
		Author:         rec.Author,
		Publisher:      rec.Publisher,
		PublishedYear:  rec.PublishedYear,
		ISBN:           rec.ISBN,
		PageCount:      rec.PageCount,
		Language:       rec.Language,
		Description:    rec.Description,
		Subjects:       rec.Subjects,
		CoverURL:       rec.CoverURL,
		WebURL:         rec.WebURL,
		MetadataSource: rec.MetadataSource,
	}
	enriched := e.enricher.Enrich(ctx, seed, chapTitles)
	return e.store.UpdateBookMetadata(bookID, enriched)
}

// EnrichAllBooks enriches all indexed books (or only those missing web metadata/category/tags when force is false).
func (e *Engine) EnrichAllBooks(ctx context.Context, force bool) ([]store.BookRecord, error) {
	books := e.store.ListBooks()
	for _, b := range books {
		needsEnrich := force || b.Category == "" || len(b.Tags) == 0 || b.MetadataSource == "" || b.MetadataSource == "epub"
		if !needsEnrich {
			continue
		}
		if _, err := e.EnrichBook(ctx, b.ID); err != nil {
			log.Printf("Warning: could not enrich book %s (%s): %v", b.ID, b.Title, err)
		}
	}
	return e.store.ListBooks(), nil
}

func (e *Engine) embedChunksWithBatching(ctx context.Context, chunks []epub.ParsedChunk) ([][]float32, bool) {
	cfg := e.GetConfig()
	e.mu.RLock()
	client := e.ollama
	e.mu.RUnlock()

	const batchSize = 32
	allVecs := make([][]float32, 0, len(chunks))

	for start := 0; start < len(chunks); start += batchSize {
		end := start + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, end-start)
		for i := start; i < end; i++ {
			texts[i-start] = chunks[i].Text
		}

		vecs, err := client.EmbedBatch(ctx, cfg.EmbeddingModel, texts)
		if err != nil {
			log.Printf("Note: Ollama embedding (%s) unavailable, indexing sparse vectors & BM25: %v", cfg.EmbeddingModel, err)
			return nil, false
		}
		allVecs = append(allVecs, vecs...)
	}

	return allVecs, len(allVecs) == len(chunks)
}

// Search performs Qdrant Hybrid Search (Dense + Sparse RRF) and promotes child hits to ParentSections.
func (e *Engine) Search(ctx context.Context, query, mode, bookFilter string, topK int) []store.SearchResult {
	cfg := e.GetConfig()
	if topK <= 0 {
		topK = cfg.TopK
	}
	if mode == "" {
		mode = "hybrid"
	}

	var queryVec []float32
	if mode == "hybrid" || mode == "semantic" {
		e.mu.RLock()
		client := e.ollama
		e.mu.RUnlock()

		if vecs, err := client.EmbedBatch(ctx, cfg.EmbeddingModel, []string{query}); err == nil && len(vecs) == 1 {
			queryVec = vecs[0]
		}
	}

	return e.store.Search(ctx, query, queryVec, mode, bookFilter, topK)
}

// Ask retrieves top ParentSections via Parent-Child Hybrid RAG and generates a cited answer via local Ollama.
func (e *Engine) Ask(ctx context.Context, question, bookFilter string, topK int) (*AskResponse, error) {
	start := time.Now()
	cfg := e.GetConfig()
	if topK <= 0 {
		topK = cfg.TopK
	}

	sources := e.Search(ctx, question, "hybrid", bookFilter, topK)
	if len(sources) == 0 {
		return &AskResponse{
			Question:   question,
			Answer:     "No matching passages were found in your indexed library. Try indexing books from your configured books directory (`H:\\Books`) first.",
			Model:      cfg.ChatModel,
			Sources:    nil,
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var contextBuilder strings.Builder
	for i, src := range sources {
		ck := src.Chunk
		parentContent := src.ParentText
		if strings.TrimSpace(parentContent) == "" {
			parentContent = ck.Text
		}
		contextBuilder.WriteString(fmt.Sprintf(
			"--- SOURCE %d [Book: %s | Author: %s | Chapter: %s | Section: %s | Matched Children: %d] ---\n%s\n\n",
			i+1, ck.BookTitle, ck.Author, ck.ChapterTitle, ck.SectionTitle, src.ChildHits, parentContent,
		))
	}

	systemPrompt := `You are Smart Library AI, an expert technical research assistant backed by the user's personal EPUB library via Parent-Child Hierarchical RAG.
Follow these rules strictly:
1. Base your answer directly on the provided Parent Section excerpts from the user's books.
2. Always cite your sources inline using the format [Source N: Book Title — Chapter Title].
3. Preserve code snippets, CLI commands, and technical formulas accurately.
4. If the provided book excerpts do not contain enough information to fully answer the question, state clearly what is covered in the books and what is missing.`

	userPrompt := fmt.Sprintf("Retrieved Book Parent Sections:\n\n%s\nUser Question: %s\n\nProvide a clear, thorough technical answer with inline [Source N] citations:", contextBuilder.String(), question)

	e.mu.RLock()
	client := e.ollama
	e.mu.RUnlock()

	answer, err := client.Chat(ctx, cfg.ChatModel, []ollama.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	})
	if err != nil {
		answer = fmt.Sprintf(
			"⚠️ Could not reach local Ollama chat model (%q at %s): %v\n\nHowever, Qdrant Parent-Child retrieval found %d relevant parent sections from your books (shown in the Sources panel).",
			cfg.ChatModel, cfg.OllamaURL, err, len(sources),
		)
	}

	return &AskResponse{
		Question:   question,
		Answer:     answer,
		Model:      cfg.ChatModel,
		Sources:    sources,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// ListOllamaModels queries Ollama for installed local models.
func (e *Engine) ListOllamaModels(ctx context.Context) ([]ollama.ModelInfo, error) {
	e.mu.RLock()
	client := e.ollama
	e.mu.RUnlock()
	return client.ListModels(ctx)
}
