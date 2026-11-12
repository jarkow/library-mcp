package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"library-mcp/internal/domain"
	"library-mcp/internal/epub"
)

// SearchOptions configures advanced filtered and diversity-aware Parent-Child search.
type SearchOptions = domain.SearchOptions

// NoteRecord stores a persistent user or AI-agent annotation, bookmark, or architectural note linked to a book/chapter/section.
type NoteRecord = domain.NoteRecord

// TOCSection represents a ParentSection entry inside a book's Table of Contents.
type TOCSection = domain.TOCSection

// TOCChapter represents a chapter and its nested ParentSections in a book's Table of Contents.
type TOCChapter = domain.TOCChapter

// BookTOC holds a book's bibliographic record and full hierarchical Table of Contents.
type BookTOC = domain.BookTOC

// ChapterSearchHit represents a matched chapter or parent section heading from SearchChapters.
type ChapterSearchHit = domain.ChapterSearchHit

// DuplicateGroup represents a group of duplicate or near-duplicate editions of the same book.
type DuplicateGroup = domain.DuplicateGroup

// DedupReport summarizes duplicate book detection and optional pruning.
type DedupReport = domain.DedupReport

type catalogMetaFile struct {
	Version       int                   `json:"version"`
	Books         map[string]BookRecord `json:"books"`
	Notes         map[string]NoteRecord `json:"notes"`
	ChaptersCount int                   `json:"chapters_count"`
	ParentsCount  int                   `json:"parents_count"`
	ChunksCount   int                   `json:"chunks_count"`
	VectorsCount  int                   `json:"vectors_count"`
	UpdatedAt     string                `json:"updated_at"`
}

type binaryCorpusData struct {
	Chapters map[string]epub.ParsedChapter
	Parents  map[string]epub.ParentSection
	Chunks   []epub.ParsedChunk
}

func (s *Store) metaPath() string {
	return filepath.Join(s.dataDir, "catalog_meta.json")
}

func (s *Store) corpusPath() string {
	return filepath.Join(s.dataDir, "corpus.gob")
}

func (s *Store) loadCatalogFastLocked() error {
	metaBytes, err := os.ReadFile(s.metaPath())
	if err == nil {
		var meta catalogMetaFile
		if jsonErr := json.Unmarshal(metaBytes, &meta); jsonErr == nil && len(meta.Books) > 0 {
			s.books = meta.Books
			if meta.Notes != nil {
				s.notes = meta.Notes
			}
			s.cachedChaptersCount = meta.ChaptersCount
			s.cachedParentsCount = meta.ParentsCount
			s.cachedChunksCount = meta.ChunksCount
			s.cachedVectorsCount = meta.VectorsCount
			if _, statErr := os.Stat(s.corpusPath()); statErr == nil {
				s.corpusLoaded = false
				return nil
			}
		}
	}

	// Fallback / automatic migration from legacy library_index.json or corpus.gob
	if err := s.loadFullCorpusLocked(); err != nil {
		return err
	}
	if len(s.books) > 0 {
		_ = s.saveLocked()
	}
	return nil
}

func (s *Store) ensureCorpusLoaded() {
	s.mu.RLock()
	loaded := s.corpusLoaded
	s.mu.RUnlock()
	if loaded {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.corpusLoaded {
		return
	}
	_ = s.loadCorpusFromSQLiteLocked()
}

func (s *Store) loadFullCorpusLocked() error {
	loadedCorpus := false
	if cf, err := os.Open(s.corpusPath()); err == nil {
		var bcd binaryCorpusData
		decErr := gob.NewDecoder(cf).Decode(&bcd)
		_ = cf.Close()
		if decErr == nil {
			if bcd.Chapters != nil {
				s.chapters = bcd.Chapters
			}
			if bcd.Parents != nil {
				s.parents = bcd.Parents
			}
			s.chunks = bcd.Chunks
			loadedCorpus = true
		}
	}

	if !loadedCorpus {
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
	s.corpusLoaded = true
	s.updateCachedCountsLocked()
	return nil
}

func (s *Store) updateCachedCountsLocked() {
	if s.corpusLoaded {
		s.cachedChaptersCount = len(s.chapters)
		s.cachedParentsCount = len(s.parents)
		s.cachedChunksCount = len(s.chunks)
		vCount := 0
		for _, v := range s.vectors {
			if len(v) > 0 {
				vCount++
			}
		}
		s.cachedVectorsCount = vCount
		return
	}
	chCount, pCount, ckCount := 0, 0, 0
	for _, b := range s.books {
		chCount += b.ChapterCount
		pCount += b.ParentCount
		ckCount += b.ChunkCount
	}
	s.cachedChaptersCount = chCount
	s.cachedParentsCount = pCount
	s.cachedChunksCount = ckCount
}

func (s *Store) saveCatalogMetaLocked() error {
	s.updateCachedCountsLocked()
	meta := catalogMetaFile{
		Version:       2,
		Books:         s.books,
		Notes:         s.notes,
		ChaptersCount: s.cachedChaptersCount,
		ParentsCount:  s.cachedParentsCount,
		ChunksCount:   s.cachedChunksCount,
		VectorsCount:  s.cachedVectorsCount,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmpMeta := s.metaPath() + ".tmp"
	if err := os.WriteFile(tmpMeta, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpMeta, s.metaPath())
}

// GetBookRecord returns a book's metadata record in O(1) without loading or scanning chapters.
func (s *Store) GetBookRecord(bookID string) (BookRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.books[bookID]
	return b, ok
}

// ResolveBook finds a BookRecord by exact ID, ISBN, exact title/filename/relpath, or unique substring.
func (s *Store) ResolveBook(idOrQuery string) (BookRecord, error) {
	q := strings.TrimSpace(idOrQuery)
	if q == "" {
		return BookRecord{}, errors.New("book ID or title is required")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if rec, ok := s.books[q]; ok {
		return rec, nil
	}

	lowerQ := strings.ToLower(q)
	for _, b := range s.books {
		if strings.EqualFold(b.ID, q) ||
			strings.EqualFold(b.Title, q) ||
			strings.EqualFold(b.FileName, q) ||
			(b.RelPath != "" && strings.EqualFold(b.RelPath, q)) ||
			(b.ISBN != "" && strings.EqualFold(b.ISBN, q)) {
			return b, nil
		}
	}

	var candidates []BookRecord
	for _, b := range s.books {
		if strings.Contains(strings.ToLower(b.Title), lowerQ) ||
			strings.Contains(strings.ToLower(b.FileName), lowerQ) ||
			strings.Contains(strings.ToLower(b.RelPath), lowerQ) {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 1 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].ID < candidates[j].ID
		})
		var names []string
		for _, c := range candidates {
			names = append(names, fmt.Sprintf("%s (%s)", c.Title, c.ID))
		}
		return BookRecord{}, fmt.Errorf("ambiguous book query %q matched %d books: %s", q, len(candidates), strings.Join(names, ", "))
	}
	return BookRecord{}, fmt.Errorf("book %q not found", q)
}

// GetBookTOC returns the full hierarchical Table of Contents (chapters and parent sections) for a book.
func (s *Store) GetBookTOC(idOrQuery string) (*BookTOC, error) {
	rec, err := s.ResolveBook(idOrQuery)
	if err != nil {
		return nil, err
	}
	if toc, ok := s.getBookTOCFromSQLite(rec); ok {
		return toc, nil
	}
	s.ensureCorpusLoaded()

	s.mu.RLock()
	defer s.mu.RUnlock()

	var chaps []epub.ParsedChapter
	for _, ch := range s.chapters {
		if ch.BookID == rec.ID {
			chaps = append(chaps, ch)
		}
	}
	sort.Slice(chaps, func(i, j int) bool {
		return chaps[i].ChapterIndex < chaps[j].ChapterIndex
	})

	parentsByChap := make(map[string][]epub.ParentSection)
	for _, p := range s.parents {
		if p.BookID == rec.ID {
			parentsByChap[p.ChapterID] = append(parentsByChap[p.ChapterID], p)
		}
	}

	tocChaps := make([]TOCChapter, 0, len(chaps))
	for _, ch := range chaps {
		secs := parentsByChap[ch.ID]
		sort.Slice(secs, func(i, j int) bool {
			return secs[i].ParentIndex < secs[j].ParentIndex
		})
		tocSecs := make([]TOCSection, 0, len(secs))
		for _, sec := range secs {
			tocSecs = append(tocSecs, TOCSection{
				ParentID:     sec.ID,
				SectionIndex: sec.ParentIndex,
				Title:        sec.SectionTitle,
				CharLength:   len(sec.Content),
			})
		}
		tocChaps = append(tocChaps, TOCChapter{
			ChapterID:    ch.ID,
			ChapterIndex: ch.ChapterIndex,
			Title:        ch.Title,
			CharLength:   len(ch.Content),
			Sections:     tocSecs,
		})
	}

	return &BookTOC{
		Book:     rec,
		Chapters: tocChaps,
	}, nil
}

// SearchChapters searches across all chapter titles, parent section headings, and opening summaries.
func (s *Store) SearchChapters(query, bookFilter, category, tag string, topK int) []ChapterSearchHit {
	if topK <= 0 {
		topK = 15
	}

	qTokens := tokenizeTechnical(query)
	lowerQ := strings.ToLower(strings.TrimSpace(query))
	if lowerQ == "" {
		return nil
	}

	s.mu.RLock()
	allowedBooks := s.matchingBookIDsLocked(SearchOptions{
		BookFilter: bookFilter,
		Category:   category,
		Tag:        tag,
	})
	booksCopy := make(map[string]BookRecord, len(s.books))
	for k, v := range s.books {
		booksCopy[k] = v
	}
	db := s.db
	s.mu.RUnlock()

	var hits []ChapterSearchHit

	if db != nil {
		rows, err := db.Query(`SELECT p.id, p.book_id, p.chapter_id, p.book_title, p.author, p.chapter_title, p.section_title, substr(p.content, 1, 260), IFNULL(c.chapter_index, 0)
			FROM parents p LEFT JOIN chapters c ON c.id = p.chapter_id`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var pID, bookID, chapID, bookTitle, author, chapTitle, secTitle, snippet string
				var chapIdx int
				if rows.Scan(&pID, &bookID, &chapID, &bookTitle, &author, &chapTitle, &secTitle, &snippet, &chapIdx) != nil {
					continue
				}
				if allowedBooks != nil && !allowedBooks[bookID] {
					continue
				}
				chapTitleLower := strings.ToLower(chapTitle)
				secTitleLower := strings.ToLower(secTitle)
				score := 0.0
				if strings.Contains(secTitleLower, lowerQ) {
					score += 12.0
				}
				if strings.Contains(chapTitleLower, lowerQ) {
					score += 8.0
				}
				for _, tok := range qTokens {
					if strings.Contains(secTitleLower, tok) {
						score += 4.0
					}
					if strings.Contains(chapTitleLower, tok) {
						score += 2.5
					}
				}
				if score == 0 {
					continue
				}
				book := booksCopy[bookID]
				snippet = strings.TrimSpace(snippet)
				if len(snippet) >= 255 {
					snippet += "..."
				}
				hits = append(hits, ChapterSearchHit{
					BookID:       bookID,
					BookTitle:    bookTitle,
					Author:       author,
					Category:     book.Category,
					ChapterID:    chapID,
					ChapterIndex: chapIdx,
					ChapterTitle: chapTitle,
					ParentID:     pID,
					SectionTitle: secTitle,
					Snippet:      snippet,
					Score:        score,
				})
			}
		}
	} else {
		s.ensureCorpusLoaded()
		s.mu.RLock()
		for _, p := range s.parents {
			if allowedBooks != nil && !allowedBooks[p.BookID] {
				continue
			}
			book := s.books[p.BookID]
			chapTitleLower := strings.ToLower(p.ChapterTitle)
			secTitleLower := strings.ToLower(p.SectionTitle)
			score := 0.0
			if strings.Contains(secTitleLower, lowerQ) {
				score += 12.0
			}
			if strings.Contains(chapTitleLower, lowerQ) {
				score += 8.0
			}
			for _, tok := range qTokens {
				if strings.Contains(secTitleLower, tok) {
					score += 4.0
				}
				if strings.Contains(chapTitleLower, tok) {
					score += 2.5
				}
			}
			if score == 0 {
				continue
			}
			snippet := strings.TrimSpace(p.Content)
			if len(snippet) > 260 {
				snippet = snippet[:260] + "..."
			}
			chapIdx := 0
			if ch, ok := s.chapters[p.ChapterID]; ok {
				chapIdx = ch.ChapterIndex
			}
			hits = append(hits, ChapterSearchHit{
				BookID:       p.BookID,
				BookTitle:    p.BookTitle,
				Author:       p.Author,
				Category:     book.Category,
				ChapterID:    p.ChapterID,
				ChapterIndex: chapIdx,
				ChapterTitle: p.ChapterTitle,
				ParentID:     p.ID,
				SectionTitle: p.SectionTitle,
				Snippet:      snippet,
				Score:        score,
			})
		}
		s.mu.RUnlock()
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			if hits[i].BookTitle == hits[j].BookTitle {
				return hits[i].ChapterIndex < hits[j].ChapterIndex
			}
			return hits[i].BookTitle < hits[j].BookTitle
		}
		return hits[i].Score > hits[j].Score
	})

	// Deduplicate so a single chapter doesn't flood all hits
	deduped := make([]ChapterSearchHit, 0, topK)
	perChap := make(map[string]int)
	for _, h := range hits {
		if perChap[h.ChapterID] >= 2 {
			continue
		}
		perChap[h.ChapterID]++
		deduped = append(deduped, h)
		if len(deduped) >= topK {
			break
		}
	}
	return deduped
}

// SearchWithOptions executes hybrid/keyword/semantic retrieval with category, tag, author, year, code-only, and MMR per-book diversity filtering.
func (s *Store) SearchWithOptions(ctx context.Context, queryVec []float32, opts SearchOptions) []SearchResult {
	if opts.TopK <= 0 {
		opts.TopK = 6
	}
	if opts.Mode == "" {
		opts.Mode = "hybrid"
	}

	var normQueryVec []float32
	if len(queryVec) > 0 {
		normQueryVec = normalizeVector(queryVec)
	}

	s.mu.RLock()
	qc := s.qdrant
	hasAdvancedFilters := opts.Category != "" || opts.Tag != "" || opts.Author != "" || opts.YearMin > 0 || opts.CodeOnly
	s.mu.RUnlock()

	candidatePoolSize := opts.TopK * 6
	if opts.MaxPerBook == 1 || opts.CodeOnly {
		candidatePoolSize = opts.TopK * 12
	}
	if candidatePoolSize < 30 {
		candidatePoolSize = 30
	}

	if qc != nil && !hasAdvancedFilters {
		qPoints, err := qc.QueryHybrid(ctx, opts.Query, normQueryVec, opts.Mode, opts.BookFilter, candidatePoolSize)
		if err == nil && len(qPoints) > 0 {
			childResults := make([]SearchResult, 0, len(qPoints))
			for _, pt := range qPoints {
				childResults = append(childResults, SearchResult{
					Chunk:        pt.Payload,
					ParentID:     pt.Payload.ParentID,
					Score:        pt.Score,
					MatchSources: []string{"qdrant:" + opts.Mode},
				})
			}
			return s.promoteToParentsWithOptions(childResults, opts)
		}
	}

	// Fast SQLite FTS5 path when no dense query vector is present
	if len(normQueryVec) == 0 && (opts.Mode == "hybrid" || opts.Mode == "keyword") {
		if ftsHits, ok := s.searchFTS5(opts, candidatePoolSize); ok {
			return s.promoteToParentsWithOptions(ftsHits, opts)
		}
	}

	s.ensureCorpusLoaded()
	childResults := s.searchLocalWithOptions(opts, normQueryVec, candidatePoolSize)
	return s.promoteToParentsWithOptions(childResults, opts)
}

func (s *Store) searchLocalWithOptions(opts SearchOptions, normQueryVec []float32, limit int) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.chunks) == 0 || limit <= 0 {
		return nil
	}

	candidateMask := s.buildAdvancedFilterMaskLocked(opts)

	var bm25Scores map[int]float64
	var bm25Ranks map[int]int
	if opts.Mode == "hybrid" || opts.Mode == "keyword" {
		bm25Scores, bm25Ranks = s.searchBM25Locked(opts.Query, candidateMask, limit*3)
	}

	var vecScores map[int]float64
	var vecRanks map[int]int
	if (opts.Mode == "hybrid" || opts.Mode == "semantic") && len(normQueryVec) > 0 {
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
		if opts.Mode == "keyword" {
			finalScore = bm25Scores[idx]
		} else if opts.Mode == "semantic" {
			finalScore = vecScores[idx]
		}

		ck := s.chunks[idx]
		if opts.CodeOnly {
			codeBoost := scoreCodeDensity(ck.Text)
			if codeBoost <= 0 {
				continue
			}
			finalScore *= (1.0 + 0.15*codeBoost)
		}

		results = append(results, SearchResult{
			Chunk:        ck,
			ParentID:     ck.ParentID,
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

func (s *Store) promoteToParentsWithOptions(childHits []SearchResult, opts SearchOptions) []SearchResult {
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
			} else if parentSecSQL, okSQL := s.getParentFromSQLite(pID); okSQL && parentSecSQL.Content != "" {
				parentText = parentSecSQL.Content
			}
			if opts.CodeOnly && scoreCodeDensity(parentText) <= 0 {
				continue
			}
			copied := hit
			copied.ParentID = pID
			copied.ParentText = parentText
			copied.ChildHits = 1
			byParent[pID] = &copied
			order = append(order, pID)
		} else {
			existing.ChildHits++
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

	maxPerBook := opts.MaxPerBook
	// Apply default Cross-Book Diversity (max 2 sections per book) when searching across the whole library
	if maxPerBook == 0 && strings.TrimSpace(opts.BookFilter) == "" && len(s.books) > 3 {
		maxPerBook = 2
	}

	selected := make([]SearchResult, 0, opts.TopK)
	selectedTokenSets := make([]map[string]bool, 0, opts.TopK)
	bookCounts := make(map[string]int)

	for _, cand := range promoted {
		if maxPerBook > 0 && bookCounts[cand.Chunk.BookID] >= maxPerBook {
			continue
		}

		// MMR near-duplicate suppression across duplicate editions
		candSet := tokenSetForMMR(cand.ParentText)
		nearDup := false
		for _, prevSet := range selectedTokenSets {
			if jaccardSimilarity(candSet, prevSet) > 0.78 {
				nearDup = true
				break
			}
		}
		if nearDup {
			continue
		}

		selected = append(selected, cand)
		selectedTokenSets = append(selectedTokenSets, candSet)
		bookCounts[cand.Chunk.BookID]++
		if len(selected) >= opts.TopK {
			break
		}
	}

	// If strict per-book cap left fewer than TopK results and more exist, backfill remaining slots
	if len(selected) < opts.TopK && opts.MaxPerBook == 0 {
		usedParents := make(map[string]bool, len(selected))
		for _, sel := range selected {
			usedParents[sel.ParentID] = true
		}
		for _, cand := range promoted {
			if usedParents[cand.ParentID] {
				continue
			}
			selected = append(selected, cand)
			if len(selected) >= opts.TopK {
				break
			}
		}
	}

	return selected
}

func tokenSetForMMR(text string) map[string]bool {
	toks := tokenizeTechnical(text)
	if len(toks) > 120 {
		toks = toks[:120]
	}
	set := make(map[string]bool, len(toks))
	for _, t := range toks {
		set[t] = true
	}
	return set
}

func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

var codePatternIndicators = []string{
	"```", "func ", "function ", "def ", "class ", "interface ", "struct ",
	"public ", "private ", "return ", "import ", "package ", "#include",
	"SELECT ", "FROM ", "WHERE ", "INSERT INTO", "CREATE TABLE",
	"docker ", "kubectl ", "git ", "curl ", "npm ", "go ", "cargo ",
	"=>", " != ", " == ", " := ", "::", "->",
}

func scoreCodeDensity(text string) float64 {
	score := 0.0
	for _, ind := range codePatternIndicators {
		if strings.Contains(text, ind) {
			score += 1.0
		}
	}
	braces := strings.Count(text, "{") + strings.Count(text, "}")
	semis := strings.Count(text, ";")
	parens := strings.Count(text, "(") + strings.Count(text, ")")
	if braces >= 2 && parens >= 2 {
		score += 1.5
	}
	if semis >= 3 {
		score += 1.0
	}
	return score
}

func (s *Store) matchingBookIDsLocked(opts SearchOptions) map[string]bool {
	bf := strings.ToLower(strings.TrimSpace(opts.BookFilter))
	cat := strings.ToLower(strings.TrimSpace(opts.Category))
	tag := strings.ToLower(strings.TrimSpace(opts.Tag))
	author := strings.ToLower(strings.TrimSpace(opts.Author))
	yearMin := opts.YearMin

	if bf == "" && cat == "" && tag == "" && author == "" && yearMin <= 0 {
		return nil
	}

	matching := make(map[string]bool)
	for id, b := range s.books {
		if cat != "" && !strings.Contains(strings.ToLower(b.Category), cat) {
			continue
		}
		if author != "" && !strings.Contains(strings.ToLower(b.Author), author) {
			continue
		}
		if yearMin > 0 {
			yr, _ := strconv.Atoi(strings.TrimSpace(b.PublishedYear))
			if yr < yearMin {
				continue
			}
		}
		if tag != "" {
			wantTags := strings.Split(tag, ",")
			matchedAll := true
			for _, wt := range wantTags {
				wt = strings.TrimSpace(wt)
				if wt == "" {
					continue
				}
				foundTag := false
				for _, bt := range b.Tags {
					if strings.EqualFold(bt, wt) || strings.Contains(strings.ToLower(bt), wt) {
						foundTag = true
						break
					}
				}
				if !foundTag {
					matchedAll = false
					break
				}
			}
			if !matchedAll {
				continue
			}
		}
		if bf != "" {
			matchedBF := strings.ToLower(id) == bf ||
				strings.Contains(strings.ToLower(b.Title), bf) ||
				strings.Contains(strings.ToLower(b.Author), bf) ||
				strings.Contains(strings.ToLower(b.Category), bf) ||
				strings.EqualFold(b.ISBN, bf)
			if !matchedBF {
				for _, bt := range b.Tags {
					if strings.EqualFold(bt, bf) || strings.Contains(strings.ToLower(bt), bf) {
						matchedBF = true
						break
					}
				}
			}
			if !matchedBF {
				continue
			}
		}
		matching[id] = true
	}
	return matching
}

func (s *Store) buildAdvancedFilterMaskLocked(opts SearchOptions) []bool {
	matchingBooks := s.matchingBookIDsLocked(opts)
	if matchingBooks == nil {
		return nil
	}
	mask := make([]bool, len(s.chunks))
	for i, ck := range s.chunks {
		if matchingBooks[ck.BookID] {
			mask[i] = true
		}
	}
	return mask
}

var copySuffixNoiseRe = regexp.MustCompile(`(?i)(\(\d+\)|\(z-lib.*?\)|\(for true epub\)|\(garner.*?\)|\[.*?\])`)
var nonAlphaNumTitleRe = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func canonicalBookSignature(b BookRecord) string {
	t := strings.ToLower(strings.TrimSpace(b.Title))
	t = copySuffixNoiseRe.ReplaceAllString(t, "")
	// Strip author prefix if filename-derived title looks like "Jacqui Read - Communication Patterns..."
	if b.Author != "" && !strings.EqualFold(b.Author, "Unknown Author") {
		authorLower := strings.ToLower(strings.TrimSpace(b.Author))
		if strings.HasPrefix(t, authorLower+" - ") {
			t = strings.TrimSpace(strings.TrimPrefix(t, authorLower+" - "))
		}
	}
	// Only strip long descriptive subtitles (> 18 chars after ':'), preserving short volume names like ": Tests" vs ": Tools"
	if idx := strings.Index(t, ":"); idx > 4 && len(strings.TrimSpace(t[idx+1:])) > 18 {
		t = t[:idx]
	} else if idx := strings.Index(t, "_ "); idx > 4 && len(strings.TrimSpace(t[idx+2:])) > 18 {
		t = t[:idx]
	}
	t = strings.TrimSpace(nonAlphaNumTitleRe.ReplaceAllString(t, " "))
	a := strings.ToLower(strings.TrimSpace(b.Author))
	if a == "unknown author" {
		a = ""
	}
	if idx := strings.Index(a, ","); idx > 0 {
		a = a[:idx]
	}
	a = strings.TrimSpace(nonAlphaNumTitleRe.ReplaceAllString(a, " "))
	if t == "" {
		return ""
	}
	return t + "|" + a
}

func bookQualityScore(b BookRecord) int {
	score := b.ChunkCount
	if strings.EqualFold(b.Format, "epub") || strings.HasSuffix(strings.ToLower(b.FileName), ".epub") {
		score += 5000
	}
	if b.ISBN != "" {
		score += 1000
	}
	if b.MetadataSource != "" && b.MetadataSource != "epub" {
		score += 1500
	}
	if !strings.Contains(b.FileName, "(1)") {
		score += 500
	}
	return score
}

// FindDuplicates detects duplicate books by FileHash, normalized ISBN, or canonical Title+Author signature.
func (s *Store) FindDuplicates() []DuplicateGroup {
	books := s.ListBooks()
	grouped := make(map[string][]BookRecord)
	reasons := make(map[string]string)

	seenBook := make(map[string]bool)

	// Pass 1: Exact ISBN match (when ISBN is present and >= 10 chars)
	byISBN := make(map[string][]BookRecord)
	for _, b := range books {
		isbn := strings.TrimSpace(b.ISBN)
		if len(isbn) >= 10 {
			byISBN[isbn] = append(byISBN[isbn], b)
		}
	}
	for isbn, list := range byISBN {
		if len(list) > 1 {
			key := "isbn:" + isbn
			grouped[key] = list
			reasons[key] = "Identical ISBN (" + isbn + ")"
			for _, b := range list {
				seenBook[b.ID] = true
			}
		}
	}

	// Pass 2: Canonical Title + Author match for remaining books
	bySig := make(map[string][]BookRecord)
	for _, b := range books {
		if seenBook[b.ID] {
			continue
		}
		sig := canonicalBookSignature(b)
		if len(sig) < 5 {
			continue
		}
		bySig[sig] = append(bySig[sig], b)
	}
	for sig, list := range bySig {
		if len(list) > 1 {
			key := "title:" + sig
			grouped[key] = list
			reasons[key] = "Same canonical Title & Author"
		}
	}

	var out []DuplicateGroup
	for key, list := range grouped {
		sort.Slice(list, func(i, j int) bool {
			si, sj := bookQualityScore(list[i]), bookQualityScore(list[j])
			if si == sj {
				return list[i].ID < list[j].ID
			}
			return si > sj
		})
		out = append(out, DuplicateGroup{
			Key:        key,
			Reason:     reasons[key],
			Keep:       list[0],
			Duplicates: list[1:],
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Keep.Title) < strings.ToLower(out[j].Keep.Title)
	})
	return out
}

// DeduplicateBooks detects duplicate books and optionally removes the lower-quality duplicates.
func (s *Store) DeduplicateBooks(ctx context.Context, apply bool) (*DedupReport, error) {
	groups := s.FindDuplicates()
	report := &DedupReport{
		Groups:  groups,
		Applied: apply,
	}
	for _, g := range groups {
		for _, dup := range g.Duplicates {
			report.FreedChunks += dup.ChunkCount
		}
	}
	if !apply || len(groups) == 0 {
		return report, nil
	}

	s.SetBatchMode(true)
	for _, g := range groups {
		for _, dup := range g.Duplicates {
			if _, err := s.RemoveBook(ctx, dup.ID); err != nil {
				return report, err
			}
			report.RemovedBooks = append(report.RemovedBooks, dup)
		}
	}
	if err := s.Flush(); err != nil {
		return report, err
	}
	return report, nil
}

// SaveNote creates or updates a bookmark, highlight, or technical note in the catalog.
func (s *Store) SaveNote(note NoteRecord) (NoteRecord, error) {
	if strings.TrimSpace(note.Content) == "" && strings.TrimSpace(note.Title) == "" {
		return NoteRecord{}, errors.New("note title or content is required")
	}

	// Resolve optional book/chapter/parent metadata if IDs are provided
	if strings.TrimSpace(note.BookID) != "" {
		if rec, err := s.ResolveBook(note.BookID); err == nil {
			note.BookID = rec.ID
			note.BookTitle = rec.Title
			note.Author = rec.Author
		}
	}
	if strings.TrimSpace(note.ChapterID) != "" {
		if ch, ok := s.GetChapter(strings.TrimSpace(note.ChapterID)); ok {
			note.ChapterID = ch.ID
			note.ChapterTitle = ch.Title
			if note.BookID == "" {
				note.BookID = ch.BookID
				if b, okB := s.GetBookRecord(ch.BookID); okB {
					note.BookTitle = b.Title
					note.Author = b.Author
				}
			}
		}
	}
	if strings.TrimSpace(note.ParentID) != "" {
		if p, ok := s.GetParentSection(strings.TrimSpace(note.ParentID)); ok {
			note.ParentID = p.ID
			note.SectionTitle = p.SectionTitle
			if note.ChapterID == "" {
				note.ChapterID = p.ChapterID
				note.ChapterTitle = p.ChapterTitle
			}
			if note.BookID == "" {
				note.BookID = p.BookID
				note.BookTitle = p.BookTitle
				note.Author = p.Author
			}
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.notes == nil {
		s.notes = make(map[string]NoteRecord)
	}
	if strings.TrimSpace(note.ID) == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", note.BookID, note.Title, note.Content, time.Now().UnixNano())))
		note.ID = "note_" + hex.EncodeToString(sum[:6])
		note.CreatedAt = now
	} else if existing, ok := s.notes[note.ID]; ok && existing.CreatedAt != "" {
		note.CreatedAt = existing.CreatedAt
	} else if note.CreatedAt == "" {
		note.CreatedAt = now
	}
	note.UpdatedAt = now
	if strings.TrimSpace(note.Title) == "" {
		snippet := strings.TrimSpace(note.Content)
		if len(snippet) > 60 {
			snippet = snippet[:60] + "..."
		}
		note.Title = snippet
	}

	s.notes[note.ID] = note
	if err := s.saveNoteSQLiteLocked(note); err != nil {
		return note, err
	}
	return note, nil
}

// ListNotes returns saved notes filtered by optional query, bookFilter, or tag.
func (s *Store) ListNotes(query, bookFilter, tag string) []NoteRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	bf := strings.ToLower(strings.TrimSpace(bookFilter))
	tf := strings.ToLower(strings.TrimSpace(tag))

	out := make([]NoteRecord, 0, len(s.notes))
	for _, n := range s.notes {
		if bf != "" {
			if !strings.EqualFold(n.BookID, bf) &&
				!strings.Contains(strings.ToLower(n.BookTitle), bf) &&
				!strings.Contains(strings.ToLower(n.Author), bf) {
				continue
			}
		}
		if tf != "" {
			matchedTag := false
			for _, t := range n.Tags {
				if strings.EqualFold(t, tf) || strings.Contains(strings.ToLower(t), tf) {
					matchedTag = true
					break
				}
			}
			if !matchedTag {
				continue
			}
		}
		if q != "" {
			haystack := strings.ToLower(n.Title + " " + n.Content + " " + n.BookTitle + " " + n.ChapterTitle + " " + strings.Join(n.Tags, " "))
			if !strings.Contains(haystack, q) {
				matchedTok := false
				for _, tok := range tokenizeTechnical(q) {
					if strings.Contains(haystack, tok) {
						matchedTok = true
						break
					}
				}
				if !matchedTok {
					continue
				}
			}
		}
		out = append(out, n)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// DeleteNote removes a saved note by ID.
func (s *Store) DeleteNote(noteID string) (NoteRecord, error) {
	id := strings.TrimSpace(noteID)
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.notes[id]
	if !ok {
		return NoteRecord{}, fmt.Errorf("note %q not found", id)
	}
	delete(s.notes, id)
	if err := s.deleteNoteSQLiteLocked(id); err != nil {
		return n, err
	}
	return n, nil
}
