// Package domain defines the core Domain-Driven Design (DDD) entities, value objects,
// and Hexagonal Architecture (Ports & Adapters) interfaces for Library MCP.
//
// This package has zero dependencies on any infrastructure or adapter package.
package domain

// ParsedBook represents a parsed book aggregate with its hierarchical chapters,
// parent sections (~3,200 chars), and child search chunks (~650 chars).
type ParsedBook struct {
	ID             string          `json:"id"`
	FileHash       string          `json:"file_hash"`
	FileName       string          `json:"file_name"`
	RelPath        string          `json:"rel_path,omitempty"`
	Format         string          `json:"format,omitempty"`
	Title          string          `json:"title"`
	Subtitle       string          `json:"subtitle,omitempty"`
	Author         string          `json:"author"`
	Publisher      string          `json:"publisher"`
	PublishedYear  string          `json:"published_year,omitempty"`
	ISBN           string          `json:"isbn,omitempty"`
	PageCount      int             `json:"page_count,omitempty"`
	Language       string          `json:"language"`
	Description    string          `json:"description"`
	Subjects       []string        `json:"subjects"`
	Category       string          `json:"category,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	CoverURL       string          `json:"cover_url,omitempty"`
	WebURL         string          `json:"web_url,omitempty"`
	MetadataSource string          `json:"metadata_source,omitempty"`
	Chapters       []ParsedChapter `json:"chapters"`
	Parents        []ParentSection `json:"parents"`
	Chunks         []ParsedChunk   `json:"chunks"`
}

// ParsedChapter represents a full chapter in a book's reading spine.
type ParsedChapter struct {
	ID           string `json:"id"`
	BookID       string `json:"book_id"`
	ChapterIndex int    `json:"chapter_index"`
	Title        string `json:"title"`
	Href         string `json:"href"`
	Content      string `json:"content"`
}

// ParentSection represents a complete structural section (~2000-4000 chars)
// returned to the user or AI agent during Parent-Child RAG promotion.
type ParentSection struct {
	ID           string `json:"id"`
	BookID       string `json:"book_id"`
	ChapterID    string `json:"chapter_id"`
	BookTitle    string `json:"book_title"`
	Author       string `json:"author"`
	ChapterTitle string `json:"chapter_title"`
	SectionTitle string `json:"section_title"`
	ParentIndex  int    `json:"parent_index"`
	HasCode      bool   `json:"has_code"`
	Content      string `json:"content"`
}

// ParsedChunk represents a fine-grained child chunk (~650 chars) indexed in the
// vector database and full-text index, linked to its ParentSection via ParentID.
type ParsedChunk struct {
	ID           string `json:"id"`
	PointUUID    string `json:"point_uuid"`
	ParentID     string `json:"parent_id"`
	BookID       string `json:"book_id"`
	ChapterID    string `json:"chapter_id"`
	BookTitle    string `json:"book_title"`
	Author       string `json:"author"`
	ChapterTitle string `json:"chapter_title"`
	SectionTitle string `json:"section_title"`
	ChunkIndex   int    `json:"chunk_index"`
	HasCode      bool   `json:"has_code"`
	Text         string `json:"text"`
}

// BookRecord is the persisted catalog representation of an indexed book.
type BookRecord struct {
	ID             string   `json:"id"`
	FileHash       string   `json:"file_hash"`
	FileName       string   `json:"file_name"`
	RelPath        string   `json:"rel_path,omitempty"`
	Format         string   `json:"format,omitempty"`
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle,omitempty"`
	Author         string   `json:"author"`
	Publisher      string   `json:"publisher"`
	PublishedYear  string   `json:"published_year,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	PageCount      int      `json:"page_count,omitempty"`
	Language       string   `json:"language"`
	Description    string   `json:"description"`
	Subjects       []string `json:"subjects"`
	Category       string   `json:"category"`
	Tags           []string `json:"tags"`
	CoverURL       string   `json:"cover_url,omitempty"`
	WebURL         string   `json:"web_url,omitempty"`
	MetadataSource string   `json:"metadata_source,omitempty"`
	ChapterCount   int      `json:"chapter_count"`
	ParentCount    int      `json:"parent_count"`
	ChunkCount     int      `json:"chunk_count"`
	IndexedAt      string   `json:"indexed_at"`
}

// BookMetadata holds bibliographic metadata, category, and tags for a book.
type BookMetadata struct {
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle,omitempty"`
	Author         string   `json:"author"`
	Publisher      string   `json:"publisher,omitempty"`
	PublishedYear  string   `json:"published_year,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	PageCount      int      `json:"page_count,omitempty"`
	Language       string   `json:"language,omitempty"`
	Description    string   `json:"description,omitempty"`
	Subjects       []string `json:"subjects,omitempty"`
	Category       string   `json:"category"`
	Tags           []string `json:"tags"`
	CoverURL       string   `json:"cover_url,omitempty"`
	WebURL         string   `json:"web_url,omitempty"`
	MetadataSource string   `json:"metadata_source"`
}

// SearchOptions configures filtered and diversity-aware Parent-Child search.
type SearchOptions struct {
	Query      string `json:"query"`
	Mode       string `json:"mode"`
	BookFilter string `json:"book_filter,omitempty"`
	Category   string `json:"category,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Author     string `json:"author,omitempty"`
	YearMin    int    `json:"year_min,omitempty"`
	MaxPerBook int    `json:"max_per_book,omitempty"`
	CodeOnly   bool   `json:"code_only,omitempty"`
	TopK       int    `json:"top_k,omitempty"`
}

// SearchResult represents a hierarchical retrieval result containing both the
// matched child chunk and its promoted ParentSection.
type SearchResult struct {
	Chunk        ParsedChunk `json:"chunk"`
	ParentID     string      `json:"parent_id"`
	ParentText   string      `json:"parent_text"`
	ChildHits    int         `json:"child_hits"`
	Score        float64     `json:"score"`
	BM25Score    float64     `json:"bm25_score"`
	VectorScore  float64     `json:"vector_score"`
	MatchSources []string    `json:"match_sources"`
}

// NoteRecord stores a persistent user or AI-agent annotation, bookmark, or takeaway.
type NoteRecord struct {
	ID           string   `json:"id"`
	BookID       string   `json:"book_id,omitempty"`
	BookTitle    string   `json:"book_title,omitempty"`
	Author       string   `json:"author,omitempty"`
	ChapterID    string   `json:"chapter_id,omitempty"`
	ChapterTitle string   `json:"chapter_title,omitempty"`
	ParentID     string   `json:"parent_id,omitempty"`
	SectionTitle string   `json:"section_title,omitempty"`
	Title        string   `json:"title"`
	Content      string   `json:"content"`
	Tags         []string `json:"tags,omitempty"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// TOCSection represents a ParentSection entry inside a book's Table of Contents.
type TOCSection struct {
	ParentID     string `json:"parent_id"`
	SectionIndex int    `json:"section_index"`
	Title        string `json:"title"`
	CharLength   int    `json:"char_length"`
}

// TOCChapter represents a chapter and its nested ParentSections in a book's Table of Contents.
type TOCChapter struct {
	ChapterID    string       `json:"chapter_id"`
	ChapterIndex int          `json:"chapter_index"`
	Title        string       `json:"title"`
	CharLength   int          `json:"char_length"`
	Sections     []TOCSection `json:"sections,omitempty"`
}

// BookTOC holds a book's bibliographic record and full hierarchical Table of Contents.
type BookTOC struct {
	Book     BookRecord   `json:"book"`
	Chapters []TOCChapter `json:"chapters"`
}

// ChapterSearchHit represents a matched chapter or parent section heading.
type ChapterSearchHit struct {
	BookID       string  `json:"book_id"`
	BookTitle    string  `json:"book_title"`
	Author       string  `json:"author"`
	Category     string  `json:"category"`
	ChapterID    string  `json:"chapter_id"`
	ChapterIndex int     `json:"chapter_index"`
	ChapterTitle string  `json:"chapter_title"`
	ParentID     string  `json:"parent_id,omitempty"`
	SectionTitle string  `json:"section_title,omitempty"`
	Snippet      string  `json:"snippet"`
	Score        float64 `json:"score"`
}

// DuplicateGroup represents a group of duplicate or near-duplicate editions of the same book.
type DuplicateGroup struct {
	Key        string       `json:"key"`
	Reason     string       `json:"reason"`
	Keep       BookRecord   `json:"keep"`
	Duplicates []BookRecord `json:"duplicates"`
}

// DedupReport summarizes duplicate book detection and optional pruning.
type DedupReport struct {
	Groups       []DuplicateGroup `json:"groups"`
	RemovedBooks []BookRecord     `json:"removed_books,omitempty"`
	FreedChunks  int              `json:"freed_chunks"`
	Applied      bool             `json:"applied"`
}

// IngestReport summarizes a batch or single-file book ingestion run.
type IngestReport struct {
	ScannedDirs  int          `json:"scanned_dirs"`
	ScannedFiles int          `json:"scanned_files"`
	IndexedBooks int          `json:"indexed_books"`
	SkippedBooks int          `json:"skipped_books"`
	TotalParents int          `json:"total_parents"`
	TotalChunks  int          `json:"total_chunks"`
	Embedded     bool         `json:"embedded"`
	Books        []BookRecord `json:"books,omitempty"`
	Errors       []string     `json:"errors"`
	DurationMs   int64        `json:"duration_ms"`
}

// AskResponse holds a grounded LLM response and its Parent-Child source citations.
type AskResponse struct {
	Question   string         `json:"question"`
	Answer     string         `json:"answer"`
	Model      string         `json:"model"`
	Sources    []SearchResult `json:"sources"`
	DurationMs int64          `json:"duration_ms"`
}

// WatcherEvent records a live folder-watcher auto-ingestion event.
type WatcherEvent struct {
	Timestamp string `json:"timestamp"`
	File      string `json:"file"`
	BookTitle string `json:"book_title,omitempty"`
	Chunks    int    `json:"chunks,omitempty"`
	Status    string `json:"status"`
}

// ModelInfo describes an installed AI model in the local model server.
type ModelInfo struct {
	Name       string `json:"name"`
	ModifiedAt string `json:"modified_at"`
	Size       int64  `json:"size"`
}

// ChatMessage represents a single message in a grounded RAG conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SparseVector represents a sparse vector of sorted token hash indices and term weights.
type SparseVector struct {
	Indices []uint32  `json:"indices"`
	Values  []float32 `json:"values"`
}

// ScoredPoint represents a matched vector point and its child chunk payload.
type ScoredPoint struct {
	ID      string      `json:"id"`
	Score   float64     `json:"score"`
	Payload ParsedChunk `json:"payload"`
}
