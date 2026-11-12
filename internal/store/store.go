package store

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"library-mcp/internal/epub"
	"library-mcp/internal/metadata"
	"library-mcp/internal/qdrant"
)

// BookRecord stores metadata, category, and tags for an indexed EPUB book.
type BookRecord struct {
	ID             string   `json:"id"`
	FileHash       string   `json:"file_hash"`
	FileName       string   `json:"file_name"`
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

// SearchResult represents a hierarchical retrieval result containing both the matched child chunk and its promoted ParentSection.
type SearchResult struct {
	Chunk        epub.ParsedChunk `json:"chunk"`
	ParentID     string           `json:"parent_id"`
	ParentText   string           `json:"parent_text"`
	ChildHits    int              `json:"child_hits"`
	Score        float64          `json:"score"`
	BM25Score    float64          `json:"bm25_score"`
	VectorScore  float64          `json:"vector_score"`
	MatchSources []string         `json:"match_sources"`
}

type persistedData struct {
	Books    map[string]BookRecord         `json:"books"`
	Chapters map[string]epub.ParsedChapter `json:"chapters"`
	Parents  map[string]epub.ParentSection `json:"parents"`
	Chunks   []epub.ParsedChunk            `json:"chunks"`
}

// Store coordinates Qdrant hybrid vector/sparse search with the local ParentSection & Chapter catalog.
type Store struct {
	mu      sync.RWMutex
	dataDir string
	qdrant  *qdrant.Client

	books    map[string]BookRecord
	chapters map[string]epub.ParsedChapter
	parents  map[string]epub.ParentSection
	chunks   []epub.ParsedChunk
	vectors  [][]float32

	// Local fallback BM25 structures when Qdrant container is offline
	postings  map[string]map[int]int
	docLen    []int
	avgDocLen float64
}

// Open initializes a Store backed by Qdrant and the local Parent/Chapter catalog in dataDir.
func Open(dataDir string, qdrantClient *qdrant.Client) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dataDir:  dataDir,
		qdrant:   qdrantClient,
		books:    make(map[string]BookRecord),
		chapters: make(map[string]epub.ParsedChapter),
		parents:  make(map[string]epub.ParentSection),
		postings: make(map[string]map[int]int),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetQdrantClient updates the active Qdrant client.
func (s *Store) SetQdrantClient(qc *qdrant.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.qdrant = qc
}

func (s *Store) indexPath() string {
	return filepath.Join(s.dataDir, "library_index.json")
}

func (s *Store) vectorsPath() string {
	return filepath.Join(s.dataDir, "vectors.bin")
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idxPath := s.indexPath()
	if b, err := os.ReadFile(idxPath); err == nil {
		var pd persistedData
		if err := json.Unmarshal(b, &pd); err != nil {
			return err
		}
		if pd.Books != nil {
			s.books = pd.Books
		}
		if pd.Chapters != nil {
			s.chapters = pd.Chapters
		}
		if pd.Parents != nil {
			s.parents = pd.Parents
		}
		s.chunks = pd.Chunks
	}

	s.vectors = make([][]float32, len(s.chunks))
	if f, err := os.Open(s.vectorsPath()); err == nil {
		defer f.Close()
		var count uint32
		if err := binary.Read(f, binary.LittleEndian, &count); err == nil {
			limit := int(count)
			if limit > len(s.chunks) {
				limit = len(s.chunks)
			}
			for i := 0; i < limit; i++ {
				var dim uint32
				if err := binary.Read(f, binary.LittleEndian, &dim); err != nil {
					break
				}
				if dim == 0 || dim > 16384 {
					continue
				}
				vec := make([]float32, dim)
				if err := binary.Read(f, binary.LittleEndian, vec); err != nil {
					break
				}
				s.vectors[i] = vec
			}
		}
	}

	s.rebuildBM25Locked()
	return nil
}

// Save writes the catalog and vectors atomically to disk.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	pd := persistedData{
		Books:    s.books,
		Chapters: s.chapters,
		Parents:  s.parents,
		Chunks:   s.chunks,
	}
	b, err := json.Marshal(pd)
	if err != nil {
		return err
	}

	tmpIdx := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmpIdx, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpIdx, s.indexPath()); err != nil {
		return err
	}

	tmpVec := s.vectorsPath() + ".tmp"
	vf, err := os.OpenFile(tmpVec, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	count := uint32(len(s.chunks))
	if err := binary.Write(vf, binary.LittleEndian, count); err != nil {
		vf.Close()
		return err
	}
	for i := 0; i < len(s.chunks); i++ {
		var vec []float32
		if i < len(s.vectors) {
			vec = s.vectors[i]
		}
		dim := uint32(len(vec))
		if err := binary.Write(vf, binary.LittleEndian, dim); err != nil {
			vf.Close()
			return err
		}
		if dim > 0 {
			if err := binary.Write(vf, binary.LittleEndian, vec); err != nil {
				vf.Close()
				return err
			}
		}
	}
	if err := vf.Close(); err != nil {
		return err
	}
	return os.Rename(tmpVec, s.vectorsPath())
}

// HasFileHash returns true if a book with the given SHA-256 hash is already indexed.
func (s *Store) HasFileHash(fileHash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.books {
		if b.FileHash == fileHash {
			return true
		}
	}
	return false
}

// UpsertBook indexes child chunks into Qdrant and saves ParentSections & Chapters in the local catalog.
func (s *Store) UpsertBook(ctx context.Context, book *epub.ParsedBook, embeddings [][]float32) error {
	normEmbeddings := make([][]float32, len(book.Chunks))
	for i := range book.Chunks {
		if i < len(embeddings) && len(embeddings[i]) > 0 {
			normEmbeddings[i] = normalizeVector(embeddings[i])
		}
	}

	s.mu.RLock()
	qc := s.qdrant
	s.mu.RUnlock()

	if qc != nil {
		_ = qc.DeleteByBookID(ctx, book.ID)
		if err := qc.UpsertChunks(ctx, book.Chunks, normEmbeddings); err != nil {
			log.Printf("Warning: Qdrant upsert failed (continuing with local catalog): %v", err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleteBookLocked(book.ID)

	rec := BookRecord{
		ID:             book.ID,
		FileHash:       book.FileHash,
		FileName:       book.FileName,
		Title:          book.Title,
		Subtitle:       book.Subtitle,
		Author:         book.Author,
		Publisher:      book.Publisher,
		PublishedYear:  book.PublishedYear,
		ISBN:           book.ISBN,
		PageCount:      book.PageCount,
		Language:       book.Language,
		Description:    book.Description,
		Subjects:       book.Subjects,
		Category:       book.Category,
		Tags:           book.Tags,
		CoverURL:       book.CoverURL,
		WebURL:         book.WebURL,
		MetadataSource: book.MetadataSource,
		ChapterCount:   len(book.Chapters),
		ParentCount:    len(book.Parents),
		ChunkCount:     len(book.Chunks),
		IndexedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	s.books[book.ID] = rec

	for _, ch := range book.Chapters {
		s.chapters[ch.ID] = ch
	}
	for _, p := range book.Parents {
		s.parents[p.ID] = p
	}
	for i, ck := range book.Chunks {
		s.chunks = append(s.chunks, ck)
		s.vectors = append(s.vectors, normEmbeddings[i])
	}

	s.rebuildBM25Locked()
	return s.saveLocked()
}

// SyncAllToQdrant pushes all locally cataloged chunks and vectors into Qdrant (useful if Qdrant was started after ingestion).
func (s *Store) SyncAllToQdrant(ctx context.Context) error {
	s.mu.RLock()
	qc := s.qdrant
	chunksCopy := append([]epub.ParsedChunk(nil), s.chunks...)
	vecsCopy := append([][]float32(nil), s.vectors...)
	s.mu.RUnlock()

	if qc == nil {
		return errors.New("qdrant client not configured")
	}
	return qc.UpsertChunks(ctx, chunksCopy, vecsCopy)
}

// DeleteBook removes a book from Qdrant and the local catalog by ID.
func (s *Store) DeleteBook(ctx context.Context, bookID string) error {
	s.mu.RLock()
	qc := s.qdrant
	_, exists := s.books[bookID]
	s.mu.RUnlock()

	if !exists {
		return errors.New("book not found")
	}
	if qc != nil {
		_ = qc.DeleteByBookID(ctx, bookID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleteBookLocked(bookID)
	s.rebuildBM25Locked()
	return s.saveLocked()
}

func (s *Store) deleteBookLocked(bookID string) {
	delete(s.books, bookID)
	for chID, ch := range s.chapters {
		if ch.BookID == bookID {
			delete(s.chapters, chID)
		}
	}
	for pID, p := range s.parents {
		if p.BookID == bookID {
			delete(s.parents, pID)
		}
	}
	newChunks := make([]epub.ParsedChunk, 0, len(s.chunks))
	newVectors := make([][]float32, 0, len(s.vectors))
	for i, ck := range s.chunks {
		if ck.BookID == bookID {
			continue
		}
		newChunks = append(newChunks, ck)
		if i < len(s.vectors) {
			newVectors = append(newVectors, s.vectors[i])
		} else {
			newVectors = append(newVectors, nil)
		}
	}
	s.chunks = newChunks
	s.vectors = newVectors
}

// ListBooks returns all indexed books sorted alphabetically by title.
func (s *Store) ListBooks() []BookRecord {
	return s.ListBooksFiltered("", "")
}

// ListBooksFiltered returns indexed books filtered by optional category and/or tag.
func (s *Store) ListBooksFiltered(category, tag string) []BookRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	catFilter := strings.ToLower(strings.TrimSpace(category))
	tagFilter := strings.ToLower(strings.TrimSpace(tag))

	out := make([]BookRecord, 0, len(s.books))
	for _, b := range s.books {
		if catFilter != "" && !strings.Contains(strings.ToLower(b.Category), catFilter) {
			continue
		}
		if tagFilter != "" {
			matchedTag := false
			for _, t := range b.Tags {
				if strings.EqualFold(t, tagFilter) || strings.Contains(strings.ToLower(t), tagFilter) {
					matchedTag = true
					break
				}
			}
			if !matchedTag {
				continue
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out
}

// ListCategoriesAndTags returns sorted unique categories and tags across all indexed books.
func (s *Store) ListCategoriesAndTags() ([]string, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	catSet := make(map[string]bool)
	tagSet := make(map[string]bool)
	for _, b := range s.books {
		if strings.TrimSpace(b.Category) != "" {
			catSet[b.Category] = true
		}
		for _, t := range b.Tags {
			if strings.TrimSpace(t) != "" {
				tagSet[t] = true
			}
		}
	}

	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return cats, tags
}

// UpdateBookMetadata updates a book's bibliographic metadata, category, and tags and persists the catalog.
func (s *Store) UpdateBookMetadata(bookID string, meta metadata.BookMetadata) (BookRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.books[bookID]
	if !ok {
		return BookRecord{}, errors.New("book not found")
	}
	if meta.Title != "" {
		rec.Title = meta.Title
	}
	if meta.Subtitle != "" {
		rec.Subtitle = meta.Subtitle
	}
	if meta.Author != "" {
		rec.Author = meta.Author
	}
	if meta.Publisher != "" {
		rec.Publisher = meta.Publisher
	}
	if meta.PublishedYear != "" {
		rec.PublishedYear = meta.PublishedYear
	}
	if meta.ISBN != "" {
		rec.ISBN = meta.ISBN
	}
	if meta.PageCount > 0 {
		rec.PageCount = meta.PageCount
	}
	if meta.Language != "" {
		rec.Language = meta.Language
	}
	if meta.Description != "" {
		rec.Description = meta.Description
	}
	if len(meta.Subjects) > 0 {
		rec.Subjects = meta.Subjects
	}
	if meta.Category != "" {
		rec.Category = meta.Category
	}
	if len(meta.Tags) > 0 {
		rec.Tags = meta.Tags
	}
	if meta.CoverURL != "" {
		rec.CoverURL = meta.CoverURL
	}
	if meta.WebURL != "" {
		rec.WebURL = meta.WebURL
	}
	if meta.MetadataSource != "" {
		rec.MetadataSource = meta.MetadataSource
	}

	s.books[bookID] = rec
	if err := s.saveLocked(); err != nil {
		return rec, err
	}
	return rec, nil
}

// GetBook returns a book record and its ordered chapters (without full content for fast listing).
func (s *Store) GetBook(bookID string) (BookRecord, []epub.ParsedChapter, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.books[bookID]
	if !ok {
		return BookRecord{}, nil, false
	}
	var chaps []epub.ParsedChapter
	for _, ch := range s.chapters {
		if ch.BookID == bookID {
			chCopy := ch
			chCopy.Content = ""
			chaps = append(chaps, chCopy)
		}
	}
	sort.Slice(chaps, func(i, j int) bool {
		return chaps[i].ChapterIndex < chaps[j].ChapterIndex
	})
	return b, chaps, true
}

// GetChapter returns the full chapter including content by chapter ID.
func (s *Store) GetChapter(chapterID string) (epub.ParsedChapter, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ch, ok := s.chapters[chapterID]
	return ch, ok
}

// GetParentSection returns the full ParentSection by parent ID.
func (s *Store) GetParentSection(parentID string) (epub.ParentSection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.parents[parentID]
	return p, ok
}

// Stats returns total counts of indexed books, chapters, parent sections, child chunks, and vectors.
func (s *Store) Stats() (booksCount, chaptersCount, parentsCount, chunksCount, vectorsCount int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	vCount := 0
	for _, v := range s.vectors {
		if len(v) > 0 {
			vCount++
		}
	}
	return len(s.books), len(s.chapters), len(s.parents), len(s.chunks), vCount
}

// Search executes Qdrant Hybrid Search (or local fallback) and promotes child chunk hits to deduplicated ParentSections.
func (s *Store) Search(ctx context.Context, query string, queryVec []float32, mode, bookFilter string, topK int) []SearchResult {
	if topK <= 0 {
		topK = 6
	}
	if mode == "" {
		mode = "hybrid"
	}

	var normQueryVec []float32
	if len(queryVec) > 0 {
		normQueryVec = normalizeVector(queryVec)
	}

	s.mu.RLock()
	qc := s.qdrant
	s.mu.RUnlock()

	// 1. Try Qdrant Universal Query API (Dense + Sparse RRF) first
	if qc != nil {
		qPoints, err := qc.QueryHybrid(ctx, query, normQueryVec, mode, bookFilter, topK*3)
		if err == nil && len(qPoints) > 0 {
			childResults := make([]SearchResult, 0, len(qPoints))
			for _, pt := range qPoints {
				childResults = append(childResults, SearchResult{
					Chunk:        pt.Payload,
					ParentID:     pt.Payload.ParentID,
					Score:        pt.Score,
					MatchSources: []string{"qdrant:" + mode},
				})
			}
			return s.promoteToParents(childResults, topK)
		}
	}

	// 2. Fallback to local in-memory hybrid search if Qdrant is offline or empty
	childResults := s.searchLocalFallback(query, normQueryVec, mode, bookFilter, topK*3)
	return s.promoteToParents(childResults, topK)
}

// promoteToParents maps matched child chunks to their ParentSections, deduplicating by ParentID and boosting multi-hit parents.
func (s *Store) promoteToParents(childHits []SearchResult, topK int) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(childHits) == 0 {
		return nil
	}

	byParent := make(map[string]*SearchResult)
	order := make([]string, 0, len(childHits))

	for _, hit := range childHits {
		pID := hit.Chunk.ParentID
		if pID == "" {
			pID = hit.Chunk.ID
		}

		existing, found := byParent[pID]
		if !found {
			parentText := hit.Chunk.Text
			if parentSec, ok := s.parents[pID]; ok && parentSec.Content != "" {
				parentText = parentSec.Content
			}
			copied := hit
			copied.ParentID = pID
			copied.ParentText = parentText
			copied.ChildHits = 1
			byParent[pID] = &copied
			order = append(order, pID)
		} else {
			existing.ChildHits++
			// Boost parent section when multiple fine-grained child chunks inside it match the query
			existing.Score += hit.Score * 0.20
			if hit.BM25Score > existing.BM25Score {
				existing.BM25Score = hit.BM25Score
			}
			if hit.VectorScore > existing.VectorScore {
				existing.VectorScore = hit.VectorScore
			}
		}
	}

	promoted := make([]SearchResult, 0, len(order))
	for _, pID := range order {
		promoted = append(promoted, *byParent[pID])
	}

	sort.Slice(promoted, func(i, j int) bool {
		return promoted[i].Score > promoted[j].Score
	})

	if len(promoted) > topK {
		promoted = promoted[:topK]
	}
	return promoted
}

func (s *Store) searchLocalFallback(query string, normQueryVec []float32, mode, bookFilter string, limit int) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.chunks) == 0 || limit <= 0 {
		return nil
	}

	candidateMask := s.buildFilterMaskLocked(bookFilter)

	var bm25Scores map[int]float64
	var bm25Ranks map[int]int
	if mode == "hybrid" || mode == "keyword" {
		bm25Scores, bm25Ranks = s.searchBM25Locked(query, candidateMask, limit*3)
	}

	var vecScores map[int]float64
	var vecRanks map[int]int
	if (mode == "hybrid" || mode == "semantic") && len(normQueryVec) > 0 {
		vecScores, vecRanks = s.searchVectorsLocked(normQueryVec, candidateMask, limit*3)
	}

	const rrfK = 60.0
	combinedIndices := make(map[int]bool)
	for idx := range bm25Ranks {
		combinedIndices[idx] = true
	}
	for idx := range vecRanks {
		combinedIndices[idx] = true
	}

	results := make([]SearchResult, 0, len(combinedIndices))
	for idx := range combinedIndices {
		var rrfScore float64
		var sources []string

		if r, ok := bm25Ranks[idx]; ok {
			rrfScore += 1.0 / (rrfK + float64(r))
			sources = append(sources, "bm25")
		}
		if r, ok := vecRanks[idx]; ok {
			rrfScore += 1.0 / (rrfK + float64(r))
			sources = append(sources, "vector")
		}

		finalScore := rrfScore
		if mode == "keyword" {
			finalScore = bm25Scores[idx]
		} else if mode == "semantic" {
			finalScore = vecScores[idx]
		}

		results = append(results, SearchResult{
			Chunk:        s.chunks[idx],
			ParentID:     s.chunks[idx].ParentID,
			Score:        finalScore,
			BM25Score:    bm25Scores[idx],
			VectorScore:  vecScores[idx],
			MatchSources: sources,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].BM25Score > results[j].BM25Score
		}
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func (s *Store) buildFilterMaskLocked(bookFilter string) []bool {
	filter := strings.ToLower(strings.TrimSpace(bookFilter))
	if filter == "" {
		return nil
	}
	matchingBooks := make(map[string]bool)
	for id, b := range s.books {
		if strings.ToLower(id) == filter ||
			strings.Contains(strings.ToLower(b.Title), filter) ||
			strings.Contains(strings.ToLower(b.Author), filter) ||
			strings.Contains(strings.ToLower(b.Category), filter) ||
			strings.EqualFold(b.ISBN, filter) {
			matchingBooks[id] = true
			continue
		}
		for _, t := range b.Tags {
			if strings.EqualFold(t, filter) || strings.Contains(strings.ToLower(t), filter) {
				matchingBooks[id] = true
				break
			}
		}
	}

	mask := make([]bool, len(s.chunks))
	for i, ck := range s.chunks {
		if matchingBooks[ck.BookID] ||
			strings.ToLower(ck.BookID) == filter ||
			strings.Contains(strings.ToLower(ck.BookTitle), filter) ||
			strings.Contains(strings.ToLower(ck.Author), filter) {
			mask[i] = true
		}
	}
	return mask
}

func (s *Store) rebuildBM25Locked() {
	s.postings = make(map[string]map[int]int)
	s.docLen = make([]int, len(s.chunks))
	totalTerms := 0

	for docID, ck := range s.chunks {
		tokens := tokenizeTechnical(ck.Text)
		s.docLen[docID] = len(tokens)
		totalTerms += len(tokens)
		for _, tok := range tokens {
			p, exists := s.postings[tok]
			if !exists {
				p = make(map[int]int)
				s.postings[tok] = p
			}
			p[docID]++
		}
	}

	if len(s.chunks) > 0 {
		s.avgDocLen = float64(totalTerms) / float64(len(s.chunks))
	} else {
		s.avgDocLen = 1.0
	}
}

func (s *Store) searchBM25Locked(query string, mask []bool, limit int) (map[int]float64, map[int]int) {
	qTokens := tokenizeTechnical(query)
	if len(qTokens) == 0 || len(s.chunks) == 0 {
		return nil, nil
	}

	const (
		k1 = 1.5
		b  = 0.75
	)
	totalDocs := float64(len(s.chunks))
	scores := make(map[int]float64)

	seenQ := make(map[string]bool)
	for _, tok := range qTokens {
		if seenQ[tok] {
			continue
		}
		seenQ[tok] = true

		docMap, exists := s.postings[tok]
		if !exists {
			continue
		}
		df := float64(len(docMap))
		idf := math.Log(1.0 + (totalDocs-df+0.5)/(df+0.5))

		for docID, tf := range docMap {
			if mask != nil && !mask[docID] {
				continue
			}
			dl := float64(s.docLen[docID])
			tfFloat := float64(tf)
			num := tfFloat * (k1 + 1.0)
			den := tfFloat + k1*(1.0-b+b*(dl/s.avgDocLen))
			scores[docID] += idf * (num / den)
		}
	}

	type docScore struct {
		id    int
		score float64
	}
	ranked := make([]docScore, 0, len(scores))
	for id, sc := range scores {
		if sc > 0 {
			ranked = append(ranked, docScore{id: id, score: sc})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	outScores := make(map[int]float64, len(ranked))
	outRanks := make(map[int]int, len(ranked))
	for rankIdx, item := range ranked {
		outScores[item.id] = item.score
		outRanks[item.id] = rankIdx + 1
	}
	return outScores, outRanks
}

func (s *Store) searchVectorsLocked(qNorm []float32, mask []bool, limit int) (map[int]float64, map[int]int) {
	n := len(s.vectors)
	if n == 0 || len(qNorm) == 0 {
		return nil, nil
	}

	rawScores := make([]float32, n)
	workers := runtime.NumCPU()
	if workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}

	var wg sync.WaitGroup
	batchSize := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * batchSize
		end := start + batchSize
		if end > n {
			end = n
		}
		if start >= end {
			break
		}
		wg.Add(1)
		go func(st, en int) {
			defer wg.Done()
			for i := st; i < en; i++ {
				if mask != nil && !mask[i] {
					continue
				}
				vec := s.vectors[i]
				if len(vec) != len(qNorm) {
					continue
				}
				var dot float32
				for j := 0; j < len(qNorm); j++ {
					dot += vec[j] * qNorm[j]
				}
				rawScores[i] = dot
			}
		}(start, end)
	}
	wg.Wait()

	type docScore struct {
		id    int
		score float64
	}
	var ranked []docScore
	for i, sc := range rawScores {
		if sc > 0.05 {
			ranked = append(ranked, docScore{id: i, score: float64(sc)})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	outScores := make(map[int]float64, len(ranked))
	outRanks := make(map[int]int, len(ranked))
	for rankIdx, item := range ranked {
		outScores[item.id] = item.score
		outRanks[item.id] = rankIdx + 1
	}
	return outScores, outRanks
}

func normalizeVector(vec []float32) []float32 {
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if norm == 0 {
		return vec
	}
	out := make([]float32, len(vec))
	inv := float32(1.0 / norm)
	for i, v := range vec {
		out[i] = v * inv
	}
	return out
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true,
	"with": true, "from": true, "are": true, "was": true, "were": true,
	"have": true, "has": true, "had": true, "not": true, "but": true,
	"you": true, "your": true, "can": true, "will": true, "into": true,
}

func tokenizeTechnical(text string) []string {
	lower := strings.ToLower(text)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '+' || r == '#')
	})
	tokens := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, "-_")
		if len(f) < 2 || stopWords[f] {
			continue
		}
		tokens = append(tokens, f)
	}
	return tokens
}

var _ = io.EOF
