package store

import (
	"context"
	"testing"

	"smart-library/internal/epub"
)

func TestParentChildPromotionAndHybridFallback(t *testing.T) {
	tmpDir := t.TempDir()
	st, err := Open(tmpDir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

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
}

