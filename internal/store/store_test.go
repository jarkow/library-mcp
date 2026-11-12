package store

import (
	"context"
	"testing"

	"library-mcp/internal/epub"
)

func TestParentChildPromotionAndHybridFallback(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := Open(tmpDir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	book := &epub.ParsedBook{
		ID:       "book01",
		FileHash: "0123456789abcdef0123456789abcdef",
		FileName: "ddia.epub",
		Title:    "Designing Data-Intensive Applications",
		Author:   "Martin Kleppmann",
		Chapters: []epub.ParsedChapter{
			{
				ID:           "book01_ch001",
				BookID:       "book01",
				ChapterIndex: 1,
				Title:        "Storage and Retrieval",
				Content:      "Full chapter content comparing B-Trees and LSM-Trees.",
			},
		},
		Parents: []epub.ParentSection{
			{
				ID:           "book01_sec0001",
				BookID:       "book01",
				ChapterID:    "book01_ch001",
				BookTitle:    "Designing Data-Intensive Applications",
				Author:       "Martin Kleppmann",
				ChapterTitle: "Storage and Retrieval",
				SectionTitle: "LSM-Trees vs B-Trees",
				ParentIndex:  1,
				Content:      "COMPLETE PARENT SECTION: LSM-trees are typically able to sustain higher write throughput than B-trees because they sequentially write SSTables.",
			},
		},
		Chunks: []epub.ParsedChunk{
			{
				ID:           "book01_ck00001",
				PointUUID:    epub.DeterministicUUID("book01_ck00001"),
				ParentID:     "book01_sec0001",
				BookID:       "book01",
				ChapterID:    "book01_ch001",
				BookTitle:    "Designing Data-Intensive Applications",
				Author:       "Martin Kleppmann",
				ChapterTitle: "Storage and Retrieval",
				SectionTitle: "LSM-Trees vs B-Trees",
				ChunkIndex:   1,
				Text:         "LSM-trees sustain higher write throughput using SSTables.",
			},
			{
				ID:           "book01_ck00002",
				PointUUID:    epub.DeterministicUUID("book01_ck00002"),
				ParentID:     "book01_sec0001",
				BookID:       "book01",
				ChapterID:    "book01_ch001",
				BookTitle:    "Designing Data-Intensive Applications",
				Author:       "Martin Kleppmann",
				ChapterTitle: "Storage and Retrieval",
				SectionTitle: "LSM-Trees vs B-Trees",
				ChunkIndex:   2,
				Text:         "B-trees write pages in-place with write amplification.",
			},
		},
	}

	vecs := [][]float32{
		{0.9, 0.1, 0.0, 0.0},
		{0.8, 0.2, 0.0, 0.0},
	}

	if err := st.UpsertBook(context.Background(), book, vecs); err != nil {
		t.Fatalf("UpsertBook failed: %v", err)
	}

	results := st.Search(context.Background(), "LSM-trees write throughput B-trees", []float32{0.85, 0.15, 0.0, 0.0}, "hybrid", "", 5)
	if len(results) != 1 {
		t.Fatalf("expected 1 deduplicated ParentSection result, got %d", len(results))
	}
	if results[0].ParentID != "book01_sec0001" {
		t.Errorf("unexpected ParentID: %s", results[0].ParentID)
	}
	if results[0].ChildHits != 2 {
		t.Errorf("expected 2 child hits promoted to same parent, got %d", results[0].ChildHits)
	}
	if results[0].ParentText != book.Parents[0].Content {
		t.Errorf("expected promoted ParentText to match full ParentSection content")
	}

	removed, err := st.RemoveBook(context.Background(), "Designing Data-Intensive")
	if err != nil {
		t.Fatalf("RemoveBook by title substring failed: %v", err)
	}
	if removed.ID != "book01" {
		t.Errorf("expected removed book ID 'book01', got %q", removed.ID)
	}
	if len(st.ListBooks()) != 0 {
		t.Errorf("expected 0 books after RemoveBook, got %d", len(st.ListBooks()))
	}
}

func TestAdvancedStoreFeatures(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := Open(tmpDir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer st.Close()

	b1 := &epub.ParsedBook{
		ID:            "b1",
		FileHash:      "h1",
		FileName:      "clean_arch.epub",
		Format:        "epub",
		Title:         "Clean Architecture",
		Author:        "Robert Martin",
		PublishedYear: "2018",
		ISBN:          "9780134494166",
		Category:      "Software Architecture & Design",
		Tags:          []string{"clean-architecture", "golang"},
		Chapters: []epub.ParsedChapter{
			{ID: "b1_ch1", BookID: "b1", ChapterIndex: 1, Title: "Dependency Rule", Content: "func NewService() { return }"},
		},
		Parents: []epub.ParentSection{
			{ID: "b1_p1", BookID: "b1", ChapterID: "b1_ch1", BookTitle: "Clean Architecture", Author: "Robert Martin", ChapterTitle: "Dependency Rule", SectionTitle: "Go Service Wiring", ParentIndex: 1, Content: "```go\nfunc NewService() *Service {\n    return &Service{}\n}\n```"},
		},
		Chunks: []epub.ParsedChunk{
			{ID: "b1_ck1", ParentID: "b1_p1", BookID: "b1", ChapterID: "b1_ch1", BookTitle: "Clean Architecture", Author: "Robert Martin", ChapterTitle: "Dependency Rule", SectionTitle: "Go Service Wiring", Text: "```go\nfunc NewService() *Service {\n    return &Service{}\n}\n```"},
		},
	}

	// Duplicate edition (PDF copy with same ISBN/title)
	b2 := &epub.ParsedBook{
		ID:            "b2",
		FileHash:      "h2",
		FileName:      "clean_arch(1).pdf",
		Format:        "pdf",
		Title:         "Clean Architecture (1)",
		Author:        "Robert Martin",
		PublishedYear: "2018",
		ISBN:          "9780134494166",
		Category:      "Software Architecture & Design",
		Tags:          []string{"clean-architecture"},
		Chapters: []epub.ParsedChapter{
			{ID: "b2_ch1", BookID: "b2", ChapterIndex: 1, Title: "Dependency Rule", Content: "Duplicate PDF text"},
		},
		Parents: []epub.ParentSection{
			{ID: "b2_p1", BookID: "b2", ChapterID: "b2_ch1", BookTitle: "Clean Architecture (1)", Author: "Robert Martin", ChapterTitle: "Dependency Rule", SectionTitle: "Intro", ParentIndex: 1, Content: "Duplicate PDF text"},
		},
		Chunks: []epub.ParsedChunk{
			{ID: "b2_ck1", ParentID: "b2_p1", BookID: "b2", ChapterID: "b2_ch1", BookTitle: "Clean Architecture (1)", Author: "Robert Martin", ChapterTitle: "Dependency Rule", SectionTitle: "Intro", Text: "Duplicate PDF text"},
		},
	}

	if err := st.UpsertBook(context.Background(), b1, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBook(context.Background(), b2, nil); err != nil {
		t.Fatal(err)
	}

	// Test TOC
	toc, err := st.GetBookTOC("b1")
	if err != nil || len(toc.Chapters) != 1 || len(toc.Chapters[0].Sections) != 1 {
		t.Fatalf("GetBookTOC failed: err=%v, toc=%+v", err, toc)
	}

	// Test Chapter Search
	chapHits := st.SearchChapters("Go Service Wiring", "", "Software Architecture", "", 5)
	if len(chapHits) == 0 || chapHits[0].ParentID != "b1_p1" {
		t.Fatalf("SearchChapters failed: %+v", chapHits)
	}

	// Test CodeOnly Search
	codeHits := st.SearchWithOptions(context.Background(), nil, SearchOptions{
		Query:    "NewService",
		CodeOnly: true,
		TopK:     5,
	})
	if len(codeHits) != 1 || codeHits[0].ParentID != "b1_p1" {
		t.Fatalf("expected 1 code-only hit for b1_p1, got %+v", codeHits)
	}

	// Test Notes CRUD
	savedNote, err := st.SaveNote(NoteRecord{
		Title:   "Dependency Rule Takeaway",
		Content: "Source code dependencies must point only inward toward higher-level policies.",
		BookID:  "b1",
		Tags:    []string{"architecture", "solid"},
	})
	if err != nil || savedNote.ID == "" {
		t.Fatalf("SaveNote failed: %v", err)
	}
	notes := st.ListNotes("inward", "b1", "solid")
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(notes))
	}

	// Test Deduplication (prefers EPUB b1 over PDF b2)
	dedupRep, err := st.DeduplicateBooks(context.Background(), true)
	if err != nil {
		t.Fatalf("DeduplicateBooks failed: %v", err)
	}
	if len(dedupRep.RemovedBooks) != 1 || dedupRep.RemovedBooks[0].ID != "b2" {
		t.Fatalf("expected duplicate b2 to be removed, got %+v", dedupRep.RemovedBooks)
	}

	// Test Fast Reopen from SQLite library.db
	_ = st.Close()
	st2, err := Open(tmpDir, nil)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer st2.Close()
	if bCount, _, _, _, _ := st2.Stats(); bCount != 1 {
		t.Fatalf("expected 1 book after reopen, got %d", bCount)
	}
	if len(st2.ListNotes("", "", "")) != 1 {
		t.Fatalf("expected 1 persisted note after reopen")
	}
}
