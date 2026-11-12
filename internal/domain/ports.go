package domain

import (
	"context"
	"time"
)

// ============================================================================
// Inbound / Driving Port (Primary Port)
// ============================================================================

// LibraryService defines the primary application use-case port consumed by
// driving adapters (MCP stdio server, HTTP REST/Web UI server, and CLI).
type LibraryService interface {
	IngestDir(ctx context.Context, dirPath string, force bool, progressFn func(file string, idx, total int)) (*IngestReport, error)
	IngestFile(ctx context.Context, filePath, allowedBaseDir string, force bool) (bool, int, int, bool, error)
	EnrichBook(ctx context.Context, bookID string) (BookRecord, error)
	EnrichAllBooks(ctx context.Context, force bool) ([]BookRecord, error)
	Search(ctx context.Context, query, mode, bookFilter string, topK int) []SearchResult
	SearchWithOptions(ctx context.Context, opts SearchOptions) []SearchResult
	CompareBooksOnTopic(ctx context.Context, topic, category, tag string, maxBooks int) ([]SearchResult, string)
	ExtractCodeExamples(ctx context.Context, query, language, bookFilter, category string, topK int) []SearchResult
	ExportTopicDossier(ctx context.Context, topic, category, tag, outputPath string, topK int) (string, string, error)
	Ask(ctx context.Context, question, bookFilter string, topK int) (*AskResponse, error)
	StartWatcher(ctx context.Context, pollInterval time.Duration, onEvent func(ev WatcherEvent))
	RecentWatcherEvents() []WatcherEvent
	CheckQdrant(ctx context.Context) bool
	ListOllamaModels(ctx context.Context) ([]ModelInfo, error)
}

// ============================================================================
// Outbound / Driven Ports (Secondary / Infrastructure Ports)
// ============================================================================

// CatalogRepository defines the driven port for the local SQLite + Hybrid catalog store.
type CatalogRepository interface {
	HasFileHash(fileHash string) bool
	UpsertBook(ctx context.Context, book *ParsedBook, embeddings [][]float32) error
	UpdateBookMetadata(bookID string, meta BookMetadata) (BookRecord, error)
	RemoveBook(ctx context.Context, idOrQuery string) (BookRecord, error)
	DeleteBook(ctx context.Context, bookID string) error
	ListBooks() []BookRecord
	ListBooksFiltered(category, tag string) []BookRecord
	ResolveBook(idOrQuery string) (BookRecord, error)
	ListCategoriesAndTags() ([]string, []string)
	GetBook(bookID string) (BookRecord, []ParsedChapter, bool)
	GetBookRecord(bookID string) (BookRecord, bool)
	GetBookTOC(idOrQuery string) (*BookTOC, error)
	GetChapter(chapterID string) (ParsedChapter, bool)
	GetParentSection(parentID string) (ParentSection, bool)
	SearchChapters(query, bookFilter, category, tag string, topK int) []ChapterSearchHit
	Search(ctx context.Context, query string, queryVec []float32, mode string, bookFilter string, topK int) []SearchResult
	SearchWithOptions(ctx context.Context, queryVec []float32, opts SearchOptions) []SearchResult
	SaveNote(note NoteRecord) (NoteRecord, error)
	ListNotes(query, bookFilter, tag string) []NoteRecord
	DeleteNote(noteID string) (NoteRecord, error)
	FindDuplicates() []DuplicateGroup
	DeduplicateBooks(ctx context.Context, apply bool) (*DedupReport, error)
	SyncAllToQdrant(ctx context.Context) error
	Stats() (booksCount, chaptersCount, parentsCount, chunksCount, vectorsCount int)
	Flush() error
	Close() error
}

// VectorIndex defines the driven port for the vector database adapter (Qdrant).
type VectorIndex interface {
	HealthCheck(ctx context.Context) error
	EnsureCollection(ctx context.Context, denseDim int) error
	DeleteByBookID(ctx context.Context, bookID string) error
	UpsertChunks(ctx context.Context, chunks []ParsedChunk, denseVectors [][]float32) error
	QueryHybrid(ctx context.Context, queryText string, denseVec []float32, mode string, bookID string, limit int) ([]ScoredPoint, error)
}

// AIGateway defines the driven port for the embedding and LLM generation provider (Ollama).
type AIGateway interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
	EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, error)
	Chat(ctx context.Context, model string, messages []ChatMessage) (string, error)
}

// MetadataGateway defines the driven port for external bibliographic enrichment and taxonomy classification.
type MetadataGateway interface {
	Enrich(ctx context.Context, seed BookMetadata, chapterTitles []string) BookMetadata
}
