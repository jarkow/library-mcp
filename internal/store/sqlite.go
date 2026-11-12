package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"library-mcp/internal/epub"

	_ "modernc.org/sqlite"
)

func (s *Store) dbPath() string {
	return filepath.Join(s.dataDir, "library.db")
}

func (s *Store) initSQLiteLocked() error {
	dsn := s.dbPath() + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open sqlite db: %w", err)
	}
	db.SetMaxOpenConns(1)
	s.db = db

	schema := `
CREATE TABLE IF NOT EXISTS books (
	id TEXT PRIMARY KEY,
	file_hash TEXT,
	file_name TEXT,
	rel_path TEXT,
	format TEXT,
	title TEXT,
	subtitle TEXT,
	author TEXT,
	publisher TEXT,
	published_year TEXT,
	isbn TEXT,
	page_count INTEGER,
	language TEXT,
	description TEXT,
	subjects_json TEXT,
	category TEXT,
	tags_json TEXT,
	cover_url TEXT,
	web_url TEXT,
	metadata_source TEXT,
	chapter_count INTEGER,
	parent_count INTEGER,
	chunk_count INTEGER,
	indexed_at TEXT
);

CREATE TABLE IF NOT EXISTS chapters (
	id TEXT PRIMARY KEY,
	book_id TEXT,
	chapter_index INTEGER,
	title TEXT,
	content TEXT,
	char_len INTEGER
);
CREATE INDEX IF NOT EXISTS idx_chapters_book ON chapters(book_id, chapter_index);

CREATE TABLE IF NOT EXISTS parents (
	id TEXT PRIMARY KEY,
	book_id TEXT,
	chapter_id TEXT,
	book_title TEXT,
	author TEXT,
	chapter_title TEXT,
	section_title TEXT,
	parent_index INTEGER,
	content TEXT,
	char_len INTEGER
);
CREATE INDEX IF NOT EXISTS idx_parents_book ON parents(book_id, chapter_id, parent_index);

CREATE TABLE IF NOT EXISTS chunks (
	id TEXT PRIMARY KEY,
	point_uuid TEXT,
	parent_id TEXT,
	book_id TEXT,
	chapter_id TEXT,
	book_title TEXT,
	author TEXT,
	chapter_title TEXT,
	section_title TEXT,
	chunk_index INTEGER,
	text TEXT,
	embedding BLOB
);
CREATE INDEX IF NOT EXISTS idx_chunks_book ON chunks(book_id);
CREATE INDEX IF NOT EXISTS idx_chunks_parent ON chunks(parent_id);

CREATE TABLE IF NOT EXISTS notes (
	id TEXT PRIMARY KEY,
	book_id TEXT,
	book_title TEXT,
	author TEXT,
	chapter_id TEXT,
	chapter_title TEXT,
	parent_id TEXT,
	section_title TEXT,
	title TEXT,
	content TEXT,
	tags_json TEXT,
	created_at TEXT,
	updated_at TEXT
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("failed to initialize sqlite schema: %w", err)
	}

	// Enable FTS5 full-text index on chunks if available in modernc.org/sqlite
	_, ftsErr := s.db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(chunk_id UNINDEXED, book_id UNINDEXED, parent_id UNINDEXED, text);`)
	s.ftsEnabled = (ftsErr == nil)

	if err := s.loadBooksAndNotesFromSQLiteLocked(); err != nil {
		return err
	}

	// If SQLite is empty but legacy corpus.gob or library_index.json exists, migrate automatically
	if len(s.books) == 0 {
		if _, errGob := os.Stat(s.corpusPath()); errGob == nil {
			if err := s.migrateLegacyToSQLiteLocked(); err != nil {
				return err
			}
		} else if _, errJSON := os.Stat(s.indexPath()); errJSON == nil {
			if err := s.migrateLegacyToSQLiteLocked(); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Store) loadBooksAndNotesFromSQLiteLocked() error {
	rows, err := s.db.Query(`SELECT id, file_hash, file_name, rel_path, format, title, subtitle, author, publisher,
		published_year, isbn, page_count, language, description, subjects_json, category, tags_json,
		cover_url, web_url, metadata_source, chapter_count, parent_count, chunk_count, indexed_at FROM books`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.books = make(map[string]BookRecord)
	for rows.Next() {
		var b BookRecord
		var subjJSON, tagsJSON string
		if err := rows.Scan(
			&b.ID, &b.FileHash, &b.FileName, &b.RelPath, &b.Format, &b.Title, &b.Subtitle, &b.Author, &b.Publisher,
			&b.PublishedYear, &b.ISBN, &b.PageCount, &b.Language, &b.Description, &subjJSON, &b.Category, &tagsJSON,
			&b.CoverURL, &b.WebURL, &b.MetadataSource, &b.ChapterCount, &b.ParentCount, &b.ChunkCount, &b.IndexedAt,
		); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(subjJSON), &b.Subjects)
		_ = json.Unmarshal([]byte(tagsJSON), &b.Tags)
		s.books[b.ID] = b
	}

	nRows, err := s.db.Query(`SELECT id, book_id, book_title, author, chapter_id, chapter_title, parent_id, section_title, title, content, tags_json, created_at, updated_at FROM notes`)
	if err == nil {
		defer nRows.Close()
		s.notes = make(map[string]NoteRecord)
		for nRows.Next() {
			var n NoteRecord
			var tagsJSON string
			if err := nRows.Scan(&n.ID, &n.BookID, &n.BookTitle, &n.Author, &n.ChapterID, &n.ChapterTitle, &n.ParentID, &n.SectionTitle, &n.Title, &n.Content, &tagsJSON, &n.CreatedAt, &n.UpdatedAt); err == nil {
				_ = json.Unmarshal([]byte(tagsJSON), &n.Tags)
				s.notes[n.ID] = n
			}
		}
	}

	s.updateCachedCountsLocked()
	return nil
}

func (s *Store) migrateLegacyToSQLiteLocked() error {
	// Load legacy meta + corpus into memory once and bulk-insert into SQLite
	if metaBytes, err := os.ReadFile(s.metaPath()); err == nil {
		var meta catalogMetaFile
		if json.Unmarshal(metaBytes, &meta) == nil {
			if meta.Books != nil {
				s.books = meta.Books
			}
			if meta.Notes != nil {
				s.notes = meta.Notes
			}
		}
	}
	if err := s.loadFullCorpusLocked(); err != nil {
		return err
	}
	if len(s.books) == 0 {
		return nil
	}

	if err := s.syncMemoryToSQLiteLocked(); err != nil {
		return err
	}
	// Clean up redundant legacy 724MB corpus.gob and 850MB library_index.json after SQLite migration
	_ = os.Remove(s.corpusPath())
	_ = os.Remove(s.indexPath())
	_ = os.Remove(s.metaPath())
	return nil
}

func (s *Store) syncMemoryToSQLiteLocked() error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, cmd := range []string{
		`DELETE FROM books`,
		`DELETE FROM chapters`,
		`DELETE FROM parents`,
		`DELETE FROM chunks`,
		`DELETE FROM notes`,
	} {
		if _, err := tx.Exec(cmd); err != nil {
			return err
		}
	}
	if s.ftsEnabled {
		_, _ = tx.Exec(`DELETE FROM chunks_fts`)
	}

	bookStmt, err := tx.Prepare(`INSERT INTO books VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer bookStmt.Close()
	for _, b := range s.books {
		subjJSON, _ := json.Marshal(b.Subjects)
		tagsJSON, _ := json.Marshal(b.Tags)
		if _, err := bookStmt.Exec(
			b.ID, b.FileHash, b.FileName, b.RelPath, b.Format, b.Title, b.Subtitle, b.Author, b.Publisher,
			b.PublishedYear, b.ISBN, b.PageCount, b.Language, b.Description, string(subjJSON), b.Category, string(tagsJSON),
			b.CoverURL, b.WebURL, b.MetadataSource, b.ChapterCount, b.ParentCount, b.ChunkCount, b.IndexedAt,
		); err != nil {
			return err
		}
	}

	chapStmt, err := tx.Prepare(`INSERT INTO chapters VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer chapStmt.Close()
	for _, ch := range s.chapters {
		if _, err := chapStmt.Exec(ch.ID, ch.BookID, ch.ChapterIndex, ch.Title, ch.Content, len(ch.Content)); err != nil {
			return err
		}
	}

	parentStmt, err := tx.Prepare(`INSERT INTO parents VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer parentStmt.Close()
	for _, p := range s.parents {
		if _, err := parentStmt.Exec(p.ID, p.BookID, p.ChapterID, p.BookTitle, p.Author, p.ChapterTitle, p.SectionTitle, p.ParentIndex, p.Content, len(p.Content)); err != nil {
			return err
		}
	}

	chunkStmt, err := tx.Prepare(`INSERT INTO chunks VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer chunkStmt.Close()

	var ftsStmt *sql.Stmt
	if s.ftsEnabled {
		ftsStmt, _ = tx.Prepare(`INSERT INTO chunks_fts (chunk_id, book_id, parent_id, text) VALUES (?,?,?,?)`)
		if ftsStmt != nil {
			defer ftsStmt.Close()
		}
	}

	for i, ck := range s.chunks {
		var vecBlob []byte
		if i < len(s.vectors) && len(s.vectors[i]) > 0 {
			vecBlob = encodeFloat32Slice(s.vectors[i])
		}
		if _, err := chunkStmt.Exec(ck.ID, ck.PointUUID, ck.ParentID, ck.BookID, ck.ChapterID, ck.BookTitle, ck.Author, ck.ChapterTitle, ck.SectionTitle, ck.ChunkIndex, ck.Text, vecBlob); err != nil {
			return err
		}
		if ftsStmt != nil {
			_, _ = ftsStmt.Exec(ck.ID, ck.BookID, ck.ParentID, ck.Text)
		}
	}

	noteStmt, err := tx.Prepare(`INSERT INTO notes VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer noteStmt.Close()
	for _, n := range s.notes {
		tagsJSON, _ := json.Marshal(n.Tags)
		if _, err := noteStmt.Exec(n.ID, n.BookID, n.BookTitle, n.Author, n.ChapterID, n.ChapterTitle, n.ParentID, n.SectionTitle, n.Title, n.Content, string(tagsJSON), n.CreatedAt, n.UpdatedAt); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return nil
}

func (s *Store) upsertBookSQLiteLocked(rec BookRecord, book *epub.ParsedBook, normEmbeddings [][]float32) error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteBookFromSQLiteTx(tx, book.ID, s.ftsEnabled); err != nil {
		return err
	}

	subjJSON, _ := json.Marshal(rec.Subjects)
	tagsJSON, _ := json.Marshal(rec.Tags)
	if _, err := tx.Exec(`INSERT INTO books VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.FileHash, rec.FileName, rec.RelPath, rec.Format, rec.Title, rec.Subtitle, rec.Author, rec.Publisher,
		rec.PublishedYear, rec.ISBN, rec.PageCount, rec.Language, rec.Description, string(subjJSON), rec.Category, string(tagsJSON),
		rec.CoverURL, rec.WebURL, rec.MetadataSource, rec.ChapterCount, rec.ParentCount, rec.ChunkCount, rec.IndexedAt,
	); err != nil {
		return err
	}

	chapStmt, err := tx.Prepare(`INSERT INTO chapters VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer chapStmt.Close()
	for _, ch := range book.Chapters {
		if _, err := chapStmt.Exec(ch.ID, ch.BookID, ch.ChapterIndex, ch.Title, ch.Content, len(ch.Content)); err != nil {
			return err
		}
	}

	parentStmt, err := tx.Prepare(`INSERT INTO parents VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer parentStmt.Close()
	for _, p := range book.Parents {
		if _, err := parentStmt.Exec(p.ID, p.BookID, p.ChapterID, p.BookTitle, p.Author, p.ChapterTitle, p.SectionTitle, p.ParentIndex, p.Content, len(p.Content)); err != nil {
			return err
		}
	}

	chunkStmt, err := tx.Prepare(`INSERT INTO chunks VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer chunkStmt.Close()

	var ftsStmt *sql.Stmt
	if s.ftsEnabled {
		ftsStmt, _ = tx.Prepare(`INSERT INTO chunks_fts (chunk_id, book_id, parent_id, text) VALUES (?,?,?,?)`)
		if ftsStmt != nil {
			defer ftsStmt.Close()
		}
	}

	for i, ck := range book.Chunks {
		var vecBlob []byte
		if i < len(normEmbeddings) && len(normEmbeddings[i]) > 0 {
			vecBlob = encodeFloat32Slice(normEmbeddings[i])
		}
		if _, err := chunkStmt.Exec(ck.ID, ck.PointUUID, ck.ParentID, ck.BookID, ck.ChapterID, ck.BookTitle, ck.Author, ck.ChapterTitle, ck.SectionTitle, ck.ChunkIndex, ck.Text, vecBlob); err != nil {
			return err
		}
		if ftsStmt != nil {
			_, _ = ftsStmt.Exec(ck.ID, ck.BookID, ck.ParentID, ck.Text)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return nil
}

func deleteBookFromSQLiteTx(tx *sql.Tx, bookID string, ftsEnabled bool) error {
	for _, tbl := range []string{"books", "chapters", "parents", "chunks"} {
		col := "book_id"
		if tbl == "books" {
			col = "id"
		}
		if _, err := tx.Exec("DELETE FROM "+tbl+" WHERE "+col+" = ?", bookID); err != nil {
			return err
		}
	}
	if ftsEnabled {
		_, _ = tx.Exec(`DELETE FROM chunks_fts WHERE book_id = ?`, bookID)
	}
	return nil
}

func (s *Store) deleteBookSQLiteLocked(bookID string) error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteBookFromSQLiteTx(tx, bookID, s.ftsEnabled); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return nil
}

func (s *Store) updateBookMetadataSQLiteLocked(rec BookRecord) error {
	if s.db == nil {
		return nil
	}
	subjJSON, _ := json.Marshal(rec.Subjects)
	tagsJSON, _ := json.Marshal(rec.Tags)
	_, err := s.db.Exec(`UPDATE books SET title=?, subtitle=?, author=?, publisher=?, published_year=?, isbn=?,
		page_count=?, language=?, description=?, subjects_json=?, category=?, tags_json=?, cover_url=?, web_url=?, metadata_source=?
		WHERE id=?`,
		rec.Title, rec.Subtitle, rec.Author, rec.Publisher, rec.PublishedYear, rec.ISBN,
		rec.PageCount, rec.Language, rec.Description, string(subjJSON), rec.Category, string(tagsJSON),
		rec.CoverURL, rec.WebURL, rec.MetadataSource, rec.ID,
	)
	return err
}

func (s *Store) saveNoteSQLiteLocked(n NoteRecord) error {
	if s.db == nil {
		return nil
	}
	tagsJSON, _ := json.Marshal(n.Tags)
	_, err := s.db.Exec(`INSERT OR REPLACE INTO notes VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.BookID, n.BookTitle, n.Author, n.ChapterID, n.ChapterTitle, n.ParentID, n.SectionTitle,
		n.Title, n.Content, string(tagsJSON), n.CreatedAt, n.UpdatedAt,
	)
	return err
}

func (s *Store) deleteNoteSQLiteLocked(noteID string) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM notes WHERE id = ?`, noteID)
	return err
}

func (s *Store) getChaptersMetadataFromSQLite(bookID string) ([]epub.ParsedChapter, bool) {
	if s.db == nil {
		return nil, false
	}
	rows, err := s.db.Query(`SELECT id, book_id, chapter_index, title FROM chapters WHERE book_id = ? ORDER BY chapter_index ASC`, bookID)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var chaps []epub.ParsedChapter
	for rows.Next() {
		var ch epub.ParsedChapter
		if err := rows.Scan(&ch.ID, &ch.BookID, &ch.ChapterIndex, &ch.Title); err == nil {
			chaps = append(chaps, ch)
		}
	}
	return chaps, true
}

func (s *Store) getChapterFromSQLite(chapterID string) (epub.ParsedChapter, bool) {
	if s.db == nil {
		return epub.ParsedChapter{}, false
	}
	var ch epub.ParsedChapter
	err := s.db.QueryRow(`SELECT id, book_id, chapter_index, title, content FROM chapters WHERE id = ?`, chapterID).
		Scan(&ch.ID, &ch.BookID, &ch.ChapterIndex, &ch.Title, &ch.Content)
	if err != nil {
		return epub.ParsedChapter{}, false
	}
	return ch, true
}

func (s *Store) getParentFromSQLite(parentID string) (epub.ParentSection, bool) {
	if s.db == nil {
		return epub.ParentSection{}, false
	}
	var p epub.ParentSection
	err := s.db.QueryRow(`SELECT id, book_id, chapter_id, book_title, author, chapter_title, section_title, parent_index, content FROM parents WHERE id = ?`, parentID).
		Scan(&p.ID, &p.BookID, &p.ChapterID, &p.BookTitle, &p.Author, &p.ChapterTitle, &p.SectionTitle, &p.ParentIndex, &p.Content)
	if err != nil {
		return epub.ParentSection{}, false
	}
	return p, true
}

func (s *Store) getBookTOCFromSQLite(rec BookRecord) (*BookTOC, bool) {
	if s.db == nil {
		return nil, false
	}
	cRows, err := s.db.Query(`SELECT id, chapter_index, title, char_len FROM chapters WHERE book_id = ? ORDER BY chapter_index ASC`, rec.ID)
	if err != nil {
		return nil, false
	}
	defer cRows.Close()

	var chaps []TOCChapter
	chapMap := make(map[string]int)
	for cRows.Next() {
		var ch TOCChapter
		if err := cRows.Scan(&ch.ChapterID, &ch.ChapterIndex, &ch.Title, &ch.CharLength); err == nil {
			chapMap[ch.ChapterID] = len(chaps)
			chaps = append(chaps, ch)
		}
	}

	pRows, err := s.db.Query(`SELECT id, chapter_id, parent_index, section_title, char_len FROM parents WHERE book_id = ? ORDER BY parent_index ASC`, rec.ID)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var sec TOCSection
			var chapID string
			if err := pRows.Scan(&sec.ParentID, &chapID, &sec.SectionIndex, &sec.Title, &sec.CharLength); err == nil {
				if idx, ok := chapMap[chapID]; ok {
					chaps[idx].Sections = append(chaps[idx].Sections, sec)
				}
			}
		}
	}

	return &BookTOC{
		Book:     rec,
		Chapters: chaps,
	}, true
}

func (s *Store) loadCorpusFromSQLiteLocked() error {
	if s.db == nil {
		return nil
	}
	s.chapters = make(map[string]epub.ParsedChapter)
	cRows, err := s.db.Query(`SELECT id, book_id, chapter_index, title, content FROM chapters`)
	if err == nil {
		for cRows.Next() {
			var ch epub.ParsedChapter
			if cRows.Scan(&ch.ID, &ch.BookID, &ch.ChapterIndex, &ch.Title, &ch.Content) == nil {
				s.chapters[ch.ID] = ch
			}
		}
		cRows.Close()
	}

	s.parents = make(map[string]epub.ParentSection)
	pRows, err := s.db.Query(`SELECT id, book_id, chapter_id, book_title, author, chapter_title, section_title, parent_index, content FROM parents`)
	if err == nil {
		for pRows.Next() {
			var p epub.ParentSection
			if pRows.Scan(&p.ID, &p.BookID, &p.ChapterID, &p.BookTitle, &p.Author, &p.ChapterTitle, &p.SectionTitle, &p.ParentIndex, &p.Content) == nil {
				s.parents[p.ID] = p
			}
		}
		pRows.Close()
	}

	s.chunks = nil
	s.vectors = nil
	ckRows, err := s.db.Query(`SELECT id, point_uuid, parent_id, book_id, chapter_id, book_title, author, chapter_title, section_title, chunk_index, text, embedding FROM chunks`)
	if err == nil {
		for ckRows.Next() {
			var ck epub.ParsedChunk
			var vecBlob []byte
			if ckRows.Scan(&ck.ID, &ck.PointUUID, &ck.ParentID, &ck.BookID, &ck.ChapterID, &ck.BookTitle, &ck.Author, &ck.ChapterTitle, &ck.SectionTitle, &ck.ChunkIndex, &ck.Text, &vecBlob) == nil {
				s.chunks = append(s.chunks, ck)
				s.vectors = append(s.vectors, decodeFloat32Slice(vecBlob))
			}
		}
		ckRows.Close()
	}

	s.rebuildBM25Locked()
	s.corpusLoaded = true
	s.updateCachedCountsLocked()
	return nil
}

// searchFTS5 executes a fast SQLite FTS5 BM25 query when no dense query vector is active.
func (s *Store) searchFTS5(opts SearchOptions, limit int) ([]SearchResult, bool) {
	if s.db == nil || !s.ftsEnabled {
		return nil, false
	}
	tokens := tokenizeTechnical(opts.Query)
	if len(tokens) == 0 {
		return nil, false
	}

	s.mu.RLock()
	allowedBooks := s.matchingBookIDsLocked(opts)
	s.mu.RUnlock()
	if allowedBooks != nil && len(allowedBooks) == 0 {
		return nil, true
	}

	// Build safe FTS5 OR expression quoting each token
	quoted := make([]string, 0, len(tokens))
	for _, t := range tokens {
		clean := strings.ReplaceAll(t, `"`, `""`)
		quoted = append(quoted, `"`+clean+`"`)
	}
	ftsExpr := strings.Join(quoted, " OR ")

	rows, err := s.db.Query(`
		SELECT c.id, c.point_uuid, c.parent_id, c.book_id, c.chapter_id, c.book_title, c.author, c.chapter_title, c.section_title, c.chunk_index, c.text, - bm25(chunks_fts) AS score
		FROM chunks_fts
		JOIN chunks c ON c.id = chunks_fts.chunk_id
		WHERE chunks_fts MATCH ?
		ORDER BY score DESC
		LIMIT ?`, ftsExpr, limit*3)
	if err != nil {
		return nil, false
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var ck epub.ParsedChunk
		var score float64
		if err := rows.Scan(&ck.ID, &ck.PointUUID, &ck.ParentID, &ck.BookID, &ck.ChapterID, &ck.BookTitle, &ck.Author, &ck.ChapterTitle, &ck.SectionTitle, &ck.ChunkIndex, &ck.Text, &score); err != nil {
			continue
		}
		if allowedBooks != nil && !allowedBooks[ck.BookID] {
			continue
		}
		if opts.CodeOnly {
			cb := scoreCodeDensity(ck.Text)
			if cb <= 0 {
				continue
			}
			score *= (1.0 + 0.15*cb)
		}
		results = append(results, SearchResult{
			Chunk:        ck,
			ParentID:     ck.ParentID,
			Score:        score,
			BM25Score:    score,
			MatchSources: []string{"sqlite:fts5"},
		})
		if len(results) >= limit {
			break
		}
	}
	return results, true
}

func encodeFloat32Slice(vec []float32) []byte {
	if len(vec) == 0 {
		return nil
	}
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decodeFloat32Slice(buf []byte) []float32 {
	if len(buf) == 0 || len(buf)%4 != 0 {
		return nil
	}
	out := make([]float32, len(buf)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out
}

// Close closes the underlying SQLite database handle if open.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

var _ = context.Background
