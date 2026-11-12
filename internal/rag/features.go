package rag

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"library-mcp/internal/config"
	"library-mcp/internal/store"
)

// WatcherEvent records a file-watcher auto-ingestion event.
type WatcherEvent struct {
	Timestamp string `json:"timestamp"`
	File      string `json:"file"`
	BookTitle string `json:"book_title,omitempty"`
	Chunks    int    `json:"chunks,omitempty"`
	Status    string `json:"status"`
}

// SearchWithOptions executes hybrid search with category, tag, author, year, code-only, and per-book diversity options.
func (e *Engine) SearchWithOptions(ctx context.Context, opts store.SearchOptions) []store.SearchResult {
	cfg := e.GetConfig()
	if opts.TopK <= 0 {
		opts.TopK = cfg.TopK
	}
	if opts.Mode == "" {
		opts.Mode = "hybrid"
	}

	var queryVec []float32
	if opts.Mode == "hybrid" || opts.Mode == "semantic" {
		e.mu.RLock()
		client := e.ollama
		offlineUntil := e.ollamaOfflineUntil
		e.mu.RUnlock()

		if time.Now().After(offlineUntil) {
			if vecs, err := client.EmbedBatch(ctx, cfg.EmbeddingModel, []string{opts.Query}); err == nil && len(vecs) == 1 {
				queryVec = vecs[0]
			} else if err != nil {
				e.mu.Lock()
				e.ollamaOfflineUntil = time.Now().Add(60 * time.Second)
				e.mu.Unlock()
			}
		}
	}

	return e.store.SearchWithOptions(ctx, queryVec, opts)
}

// CompareBooksOnTopic retrieves the best matching ParentSection from up to maxBooks distinct books (`MaxPerBook: 1`)
// and formats a comparative multi-author synthesis.
func (e *Engine) CompareBooksOnTopic(ctx context.Context, topic, category, tag string, maxBooks int) ([]store.SearchResult, string) {
	if maxBooks <= 0 {
		maxBooks = 5
	}
	results := e.SearchWithOptions(ctx, store.SearchOptions{
		Query:      topic,
		Mode:       "hybrid",
		Category:   category,
		Tag:        tag,
		MaxPerBook: 1,
		TopK:       maxBooks,
	})
	if len(results) == 0 {
		return nil, fmt.Sprintf("No books matched topic %q.", topic)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Cross-Book Comparison: %s\n", topic))
	sb.WriteString(fmt.Sprintf("*Synthesized across %d distinct books in your library (MMR & 1-per-book diversity enabled)*\n\n", len(results)))

	sb.WriteString("## Summary Matrix\n\n")
	sb.WriteString("| # | Book Title | Author | Year | Category | Chapter & Section | Relevance |\n")
	sb.WriteString("|---|---|---|---|---|---|---|\n")
	for i, r := range results {
		ck := r.Chunk
		bookRec, _ := e.store.GetBookRecord(ck.BookID)
		year := bookRec.PublishedYear
		if year == "" {
			year = "n/a"
		}
		cat := bookRec.Category
		if cat == "" {
			cat = "General"
		}
		sb.WriteString(fmt.Sprintf("| %d | **%s** | %s | %s | %s | %s > %s | `%.4f` (%d hits) |\n",
			i+1, ck.BookTitle, ck.Author, year, cat, ck.ChapterTitle, ck.SectionTitle, r.Score, r.ChildHits))
	}
	sb.WriteString("\n---\n\n")

	for i, r := range results {
		ck := r.Chunk
		bookRec, _ := e.store.GetBookRecord(ck.BookID)
		sb.WriteString(fmt.Sprintf("## Perspective %d: %s — *%s*\n", i+1, ck.BookTitle, ck.Author))
		metaLine := fmt.Sprintf("- **Category:** %s | **Chapter:** %s (`%s`) | **Section:** %s (`%s`)\n",
			bookRec.Category, ck.ChapterTitle, ck.ChapterID, ck.SectionTitle, r.ParentID)
		if bookRec.PublishedYear != "" || bookRec.Publisher != "" {
			metaLine += fmt.Sprintf("- **Publication:** %s (%s)\n", bookRec.Publisher, bookRec.PublishedYear)
		}
		sb.WriteString(metaLine + "\n")

		text := strings.TrimSpace(r.ParentText)
		if text == "" {
			text = strings.TrimSpace(ck.Text)
		}
		sb.WriteString(text + "\n\n---\n\n")
	}

	return results, sb.String()
}

// ExtractCodeExamples retrieves ParentSections that contain technical code snippets, CLI commands, SQL queries, or config blocks.
func (e *Engine) ExtractCodeExamples(ctx context.Context, query, language, bookFilter, category string, topK int) []store.SearchResult {
	if topK <= 0 {
		topK = 5
	}
	effectiveQuery := strings.TrimSpace(query)
	if strings.TrimSpace(language) != "" {
		effectiveQuery = effectiveQuery + " " + strings.TrimSpace(language)
	}
	return e.SearchWithOptions(ctx, store.SearchOptions{
		Query:      effectiveQuery,
		Mode:       "hybrid",
		BookFilter: bookFilter,
		Category:   category,
		Tag:        language,
		CodeOnly:   true,
		MaxPerBook: 2,
		TopK:       topK,
	})
}

// ExportTopicDossier compiles a comprehensive Markdown Study Guide / Technical Dossier on a topic across the library,
// combining multi-book perspectives, code examples, related chapters, and saved notes.
func (e *Engine) ExportTopicDossier(ctx context.Context, topic, category, tag, outputPath string, topK int) (string, string, error) {
	if strings.TrimSpace(topic) == "" {
		return "", "", fmt.Errorf("topic is required")
	}
	if topK <= 0 {
		topK = 6
	}

	sections := e.SearchWithOptions(ctx, store.SearchOptions{
		Query:      topic,
		Mode:       "hybrid",
		Category:   category,
		Tag:        tag,
		MaxPerBook: 2,
		TopK:       topK,
	})
	codeExamples := e.ExtractCodeExamples(ctx, topic, tag, "", category, 3)
	chapterHits := e.store.SearchChapters(topic, "", category, tag, 8)
	notes := e.store.ListNotes(topic, "", tag)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Technical Study Dossier: %s\n\n", topic))
	sb.WriteString(fmt.Sprintf("- **Generated At:** `%s`\n", time.Now().Format(time.RFC3339)))
	if category != "" {
		sb.WriteString(fmt.Sprintf("- **Category Filter:** `%s`\n", category))
	}
	if tag != "" {
		sb.WriteString(fmt.Sprintf("- **Tag Filter:** `%s`\n", tag))
	}
	sb.WriteString(fmt.Sprintf("- **Promoted Sections:** `%d` | **Code Examples:** `%d` | **Related Chapters:** `%d` | **Saved Notes:** `%d`\n\n",
		len(sections), len(codeExamples), len(chapterHits), len(notes)))

	// 1. Key Books Consulted
	seenBooks := make(map[string]bool)
	sb.WriteString("## 1. Primary Reference Books\n\n")
	for _, s := range sections {
		if seenBooks[s.Chunk.BookID] {
			continue
		}
		seenBooks[s.Chunk.BookID] = true
		b, ok := e.store.GetBookRecord(s.Chunk.BookID)
		if !ok {
			continue
		}
		sb.WriteString(fmt.Sprintf("- **%s** by %s (`id`: `%s`)", b.Title, b.Author, b.ID))
		if b.PublishedYear != "" {
			sb.WriteString(fmt.Sprintf(" — *%s*", b.PublishedYear))
		}
		if b.Category != "" {
			sb.WriteString(fmt.Sprintf(" [%s]", b.Category))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// 2. Saved Personal / Agent Notes
	if len(notes) > 0 {
		sb.WriteString("## 2. Saved Library Notes & Takeaways\n\n")
		for _, n := range notes {
			sb.WriteString(fmt.Sprintf("### 📝 %s (`%s`)\n", n.Title, n.ID))
			if n.BookTitle != "" {
				sb.WriteString(fmt.Sprintf("- **Book:** %s", n.BookTitle))
				if n.ChapterTitle != "" {
					sb.WriteString(fmt.Sprintf(" > %s (`%s`)", n.ChapterTitle, n.ChapterID))
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n" + n.Content + "\n\n")
		}
	}

	// 3. Core Concepts & Multi-Book Synthesis
	sb.WriteString("## 3. Core Excerpts & Author Perspectives\n\n")
	for i, s := range sections {
		ck := s.Chunk
		sb.WriteString(fmt.Sprintf("### [%d] %s — *%s*\n", i+1, ck.BookTitle, ck.Author))
		sb.WriteString(fmt.Sprintf("- **Chapter:** %s (`%s`) > **Section:** %s (`%s`) | **Score:** `%.4f`\n\n",
			ck.ChapterTitle, ck.ChapterID, ck.SectionTitle, s.ParentID, s.Score))
		text := strings.TrimSpace(s.ParentText)
		if text == "" {
			text = strings.TrimSpace(ck.Text)
		}
		sb.WriteString(text + "\n\n---\n\n")
	}

	// 4. Extracted Code & Technical Examples
	if len(codeExamples) > 0 {
		sb.WriteString("## 4. Code & Implementation Examples\n\n")
		for i, c := range codeExamples {
			ck := c.Chunk
			sb.WriteString(fmt.Sprintf("### Example %d: %s (%s > %s)\n", i+1, ck.BookTitle, ck.ChapterTitle, ck.SectionTitle))
			text := strings.TrimSpace(c.ParentText)
			if text == "" {
				text = strings.TrimSpace(ck.Text)
			}
			sb.WriteString(text + "\n\n")
		}
	}

	// 5. Further Reading Chapters
	if len(chapterHits) > 0 {
		sb.WriteString("## 5. Recommended Chapters for Deep-Dive Reading\n\n")
		for _, ch := range chapterHits {
			sb.WriteString(fmt.Sprintf("- **%s** (%s) — *%s > %s* (`chapter_id`: `%s`, `parent_id`: `%s`)\n",
				ch.BookTitle, ch.Author, ch.ChapterTitle, ch.SectionTitle, ch.ChapterID, ch.ParentID))
		}
		sb.WriteString("\n")
	}

	markdown := sb.String()
	savedPath := ""
	if strings.TrimSpace(outputPath) != "" {
		cleanOut := filepath.Clean(outputPath)
		if err := os.MkdirAll(filepath.Dir(cleanOut), 0o755); err != nil {
			return markdown, "", err
		}
		if err := os.WriteFile(cleanOut, []byte(markdown), 0o644); err != nil {
			return markdown, "", err
		}
		savedPath = cleanOut
	}

	return markdown, savedPath, nil
}

// StartWatcher monitors the configured books directory for newly added or modified book files and ingests them automatically.
func (e *Engine) StartWatcher(ctx context.Context, pollInterval time.Duration, onEvent func(ev WatcherEvent)) {
	if pollInterval <= 0 {
		pollInterval = 10 * time.Second
	}

	type fileState struct {
		size    int64
		modTime time.Time
	}
	known := make(map[string]fileState)

	scanOnce := func(initial bool) {
		cfg := e.GetConfig()
		targetDir := cfg.ResolvedBooksDir()
		info, err := os.Stat(targetDir)
		if err != nil || !info.IsDir() {
			return
		}

		_ = filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if p != targetDir && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !isSupportedBookFile(d.Name()) || !config.IsSafeSubpath(p, targetDir) {
				return nil
			}
			fi, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			prev, exists := known[p]
			if initial {
				known[p] = fileState{size: fi.Size(), modTime: fi.ModTime()}
				return nil
			}
			// Wait until file stopped changing for at least 1.5 seconds before ingesting
			if (!exists || prev.size != fi.Size() || !prev.modTime.Equal(fi.ModTime())) && time.Since(fi.ModTime()) >= 1500*time.Millisecond {
				known[p] = fileState{size: fi.Size(), modTime: fi.ModTime()}
				rel, _ := filepath.Rel(targetDir, p)
				relSlash := filepath.ToSlash(rel)
				indexed, _, chunksCount, _, ingErr := e.IngestFile(ctx, p, targetDir, false)
				if ingErr != nil {
					ev := WatcherEvent{
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						File:      relSlash,
						Status:    "error: " + ingErr.Error(),
					}
					e.recordWatcherEvent(ev)
					if onEvent != nil {
						onEvent(ev)
					}
				} else if indexed {
					log.Printf("[Watcher] Auto-indexed new book: %s (%d chunks)", relSlash, chunksCount)
					ev := WatcherEvent{
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						File:      relSlash,
						Chunks:    chunksCount,
						Status:    "indexed",
					}
					e.recordWatcherEvent(ev)
					if onEvent != nil {
						onEvent(ev)
					}
				}
			}
			return nil
		})
	}

	scanOnce(true)
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				scanOnce(false)
			}
		}
	}()
}
