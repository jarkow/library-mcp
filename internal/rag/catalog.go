package rag

import (
	"context"
	"strconv"
	"strings"

	"library-mcp/internal/domain"
)

// Catalog use cases: thin application-layer operations over domain.CatalogRepository,
// so that CLI, MCP and Web driving adapters never touch the store directly.

// ListBooks lists indexed books, applying category/tag (repository) and author/year (application) filters.
func (e *Engine) ListBooks(f domain.BookFilter) []domain.BookRecord {
	books := e.store.ListBooksFiltered(f.Category, f.Tag)
	author := strings.ToLower(strings.TrimSpace(f.Author))
	if author == "" && f.YearMin <= 0 {
		return books
	}
	out := make([]domain.BookRecord, 0, len(books))
	for _, b := range books {
		if author != "" && !strings.Contains(strings.ToLower(b.Author), author) {
			continue
		}
		if f.YearMin > 0 {
			yr, _ := strconv.Atoi(strings.TrimSpace(b.PublishedYear))
			if yr < f.YearMin {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// GetBookRecord returns a single book's catalog record.
func (e *Engine) GetBookRecord(bookID string) (domain.BookRecord, bool) {
	return e.store.GetBookRecord(bookID)
}

// GetBook returns a book record with its chapters.
func (e *Engine) GetBook(bookID string) (domain.BookRecord, []domain.ParsedChapter, bool) {
	return e.store.GetBook(bookID)
}

// GetBookTOC returns a book's hierarchical table of contents.
func (e *Engine) GetBookTOC(idOrQuery string) (*domain.BookTOC, error) {
	return e.store.GetBookTOC(idOrQuery)
}

// SearchChapters searches chapter and section headings.
func (e *Engine) SearchChapters(query, bookFilter, category, tag string, topK int) []domain.ChapterSearchHit {
	return e.store.SearchChapters(query, bookFilter, category, tag, topK)
}

// GetChapter returns a full chapter.
func (e *Engine) GetChapter(chapterID string) (domain.ParsedChapter, bool) {
	return e.store.GetChapter(chapterID)
}

// GetParentSection returns a single parent section.
func (e *Engine) GetParentSection(parentID string) (domain.ParentSection, bool) {
	return e.store.GetParentSection(parentID)
}

// ListCategoriesAndTags returns the catalog taxonomy.
func (e *Engine) ListCategoriesAndTags() ([]string, []string) {
	return e.store.ListCategoriesAndTags()
}

// CatalogOverview aggregates catalog statistics, taxonomy, notes and duplicate counts.
func (e *Engine) CatalogOverview() domain.CatalogOverview {
	b, ch, p, ck, v := e.store.Stats()
	cats, tags := e.store.ListCategoriesAndTags()
	return domain.CatalogOverview{
		Books:           b,
		Chapters:        ch,
		Parents:         p,
		Chunks:          ck,
		Vectors:         v,
		Categories:      cats,
		Tags:            tags,
		NotesCount:      len(e.store.ListNotes("", "", "")),
		DuplicatesCount: len(e.store.FindDuplicates()),
	}
}

// SaveNote persists a note.
func (e *Engine) SaveNote(note domain.NoteRecord) (domain.NoteRecord, error) {
	return e.store.SaveNote(note)
}

// ListNotes lists or searches notes.
func (e *Engine) ListNotes(query, bookFilter, tag string) []domain.NoteRecord {
	return e.store.ListNotes(query, bookFilter, tag)
}

// DeleteNote deletes a note.
func (e *Engine) DeleteNote(noteID string) (domain.NoteRecord, error) {
	return e.store.DeleteNote(noteID)
}

// FindDuplicates reports duplicate book editions.
func (e *Engine) FindDuplicates() []domain.DuplicateGroup {
	return e.store.FindDuplicates()
}

// DeduplicateBooks analyzes (and optionally prunes) duplicate editions.
func (e *Engine) DeduplicateBooks(ctx context.Context, apply bool) (*domain.DedupReport, error) {
	return e.store.DeduplicateBooks(ctx, apply)
}

// RemoveBook removes one book by ID, title, ISBN or filename.
func (e *Engine) RemoveBook(ctx context.Context, idOrQuery string) (domain.BookRecord, error) {
	return e.store.RemoveBook(ctx, idOrQuery)
}

// RemoveAllBooks removes every indexed book and returns how many were removed.
func (e *Engine) RemoveAllBooks(ctx context.Context) (int, error) {
	n := 0
	for _, b := range e.store.ListBooks() {
		if _, err := e.store.RemoveBook(ctx, b.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SyncToQdrant pushes all cataloged chunks and vectors into Qdrant.
func (e *Engine) SyncToQdrant(ctx context.Context) error {
	return e.store.SyncAllToQdrant(ctx)
}
