package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"library-mcp/internal/config"
	"library-mcp/internal/mcp"
	"library-mcp/internal/qdrant"
	"library-mcp/internal/rag"
	"library-mcp/internal/store"
	"library-mcp/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	subcmd := os.Args[1]
	args := os.Args[2:]

	cfgPath := os.Getenv("LIBRARY_MCP_CONFIG")
	if cfgPath == "" {
		cfgPath = os.Getenv("SMART_LIBRARY_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "config.json"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	qc := qdrant.NewClient(cfg.QdrantURL, cfg.QdrantCollection)
	st, err := store.Open(cfg.ResolvedDataDir(), qc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening library store: %v\n", err)
		os.Exit(1)
	}
	engine := rag.NewEngine(cfg, st)
	ctx := context.Background()

	switch subcmd {
	case "ingest":
		fs := flag.NewFlagSet("ingest", flag.ExitOnError)
		dirFlag := fs.String("dir", "", "Books directory to scan (overrides config.json books_dir)")
		forceFlag := fs.Bool("force", false, "Re-index books even if SHA-256 hash is unchanged")
		_ = fs.Parse(args)

		target := *dirFlag
		if target == "" && fs.NArg() > 0 {
			target = fs.Arg(0)
		}
		resolved := cfg.ResolvedBooksDir()
		if target != "" {
			resolved = config.TranslatePath(target)
		}

		fmt.Printf("Scanning EPUB library in: %s\n", resolved)
		report, err := engine.IngestDir(ctx, target, *forceFlag, func(file string, idx, total int) {
			fmt.Printf("  [%d/%d] Processing & fetching web metadata for %s...\n", idx, total, file)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ingest failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nIngestion Complete (%d ms):\n", report.DurationMs)
		fmt.Printf("  Scanned Dirs   : %d\n", report.ScannedDirs)
		fmt.Printf("  Scanned Files  : %d\n", report.ScannedFiles)
		fmt.Printf("  Indexed Books  : %d\n", report.IndexedBooks)
		fmt.Printf("  Skipped Books  : %d\n", report.SkippedBooks)
		fmt.Printf("  Parent Sections: %d\n", report.TotalParents)
		fmt.Printf("  Child Chunks   : %d\n", report.TotalChunks)
		fmt.Printf("  Dense Embedded : %v\n", report.Embedded)
		if len(report.Books) > 0 {
			fmt.Println("\nCataloged Books (Web Metadata, Categories & Tags):")
			printBookRecords(report.Books)
		}
		if len(report.Errors) > 0 {
			fmt.Println("  Warnings/Errors:")
			for _, e := range report.Errors {
				fmt.Printf("    - %s\n", e)
			}
		}

	case "enrich":
		fs := flag.NewFlagSet("enrich", flag.ExitOnError)
		bookIDFlag := fs.String("book", "", "Specific book ID to enrich (default: all books)")
		forceFlag := fs.Bool("force", true, "Refresh web metadata even if already enriched")
		_ = fs.Parse(args)

		if strings.TrimSpace(*bookIDFlag) != "" {
			rec, err := engine.EnrichBook(ctx, strings.TrimSpace(*bookIDFlag))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Enrich failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Enriched Book Metadata:")
			printBookRecords([]store.BookRecord{rec})
			return
		}

		fmt.Println("Fetching web metadata (Google Books + Open Library) and categorizing/tagging books...")
		books, err := engine.EnrichAllBooks(ctx, *forceFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Enrich failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Enriched %d books:\n", len(books))
		printBookRecords(books)

	case "search":
		fs := flag.NewFlagSet("search", flag.ExitOnError)
		modeFlag := fs.String("mode", "hybrid", "Search mode: hybrid, semantic, or keyword")
		bookFlag := fs.String("book", "", "Filter by book title, author, book ID, category, or tag")
		catFlag := fs.String("category", "", "Filter by category")
		tagFlag := fs.String("tag", "", "Filter by tag")
		authorFlag := fs.String("author", "", "Filter by author substring")
		yearMinFlag := fs.Int("year-min", 0, "Filter by minimum published year")
		maxPerBookFlag := fs.Int("max-per-book", 0, "Maximum sections per book (0 = auto diversity)")
		codeOnlyFlag := fs.Bool("code-only", false, "Only return sections containing code or CLI examples")
		topKFlag := fs.Int("top-k", cfg.TopK, "Number of Parent Sections to retrieve")
		_ = fs.Parse(args)

		query := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if query == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp search [--mode hybrid|semantic|keyword] [--book <filter>] [--category <cat>] [--tag <tag>] [--author <name>] [--year-min <yr>] [--max-per-book <n>] [--code-only] <query>")
			os.Exit(1)
		}

		results := engine.SearchWithOptions(ctx, store.SearchOptions{
			Query:      query,
			Mode:       *modeFlag,
			BookFilter: *bookFlag,
			Category:   *catFlag,
			Tag:        *tagFlag,
			Author:     *authorFlag,
			YearMin:    *yearMinFlag,
			MaxPerBook: *maxPerBookFlag,
			CodeOnly:   *codeOnlyFlag,
			TopK:       *topKFlag,
		})
		if len(results) == 0 {
			fmt.Println("No matching sections found.")
			return
		}
		for i, res := range results {
			ck := res.Chunk
			fmt.Printf("=== [%d] %s — %s > %s (Score: %.4f | Child Hits: %d) ===\n",
				i+1, ck.BookTitle, ck.ChapterTitle, ck.SectionTitle, res.Score, res.ChildHits)
			fmt.Printf("Author: %s | Chapter ID: %s | Parent ID: %s\n\n", ck.Author, ck.ChapterID, res.ParentID)
			text := res.ParentText
			if text == "" {
				text = ck.Text
			}
			fmt.Println(text)
			fmt.Println()
		}

	case "toc":
		target := strings.TrimSpace(strings.Join(args, " "))
		if target == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp toc <book-id | title | isbn>")
			os.Exit(1)
		}
		toc, err := st.GetBookTOC(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Table of Contents: %s by %s [%s]\n", toc.Book.Title, toc.Book.Author, toc.Book.ID)
		fmt.Printf("Category: %s | Chapters: %d | Parent Sections: %d\n\n", toc.Book.Category, toc.Book.ChapterCount, toc.Book.ParentCount)
		for _, ch := range toc.Chapters {
			fmt.Printf("  [%02d] %s (chapter_id: %s, %d chars)\n", ch.ChapterIndex, ch.Title, ch.ChapterID, ch.CharLength)
			for _, sec := range ch.Sections {
				if sec.Title != "" && !strings.EqualFold(sec.Title, ch.Title) {
					fmt.Printf("       - %s (parent_id: %s, %d chars)\n", sec.Title, sec.ParentID, sec.CharLength)
				}
			}
		}

	case "chapters":
		fs := flag.NewFlagSet("chapters", flag.ExitOnError)
		bookFlag := fs.String("book", "", "Filter by book ID or title")
		catFlag := fs.String("category", "", "Filter by category")
		tagFlag := fs.String("tag", "", "Filter by tag")
		topKFlag := fs.Int("top-k", 15, "Maximum chapters to return")
		_ = fs.Parse(args)

		query := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if query == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp chapters [--book <filter>] [--category <cat>] [--tag <tag>] <query>")
			os.Exit(1)
		}
		hits := st.SearchChapters(query, *bookFlag, *catFlag, *tagFlag, *topKFlag)
		if len(hits) == 0 {
			fmt.Println("No matching chapters found.")
			return
		}
		for i, h := range hits {
			fmt.Printf("[%d] %s (%s) — %s > %s\n", i+1, h.BookTitle, h.Author, h.ChapterTitle, h.SectionTitle)
			fmt.Printf("    chapter_id: %s | parent_id: %s | Category: %s | Score: %.1f\n", h.ChapterID, h.ParentID, h.Category, h.Score)
		}

	case "compare":
		fs := flag.NewFlagSet("compare", flag.ExitOnError)
		catFlag := fs.String("category", "", "Filter by category")
		tagFlag := fs.String("tag", "", "Filter by tag")
		maxBooksFlag := fs.Int("max-books", 5, "Number of distinct books to compare")
		_ = fs.Parse(args)

		topic := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if topic == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp compare [--category <cat>] [--tag <tag>] [--max-books 5] <topic>")
			os.Exit(1)
		}
		_, md := engine.CompareBooksOnTopic(ctx, topic, *catFlag, *tagFlag, *maxBooksFlag)
		fmt.Println(md)

	case "code":
		fs := flag.NewFlagSet("code", flag.ExitOnError)
		langFlag := fs.String("lang", "", "Filter by programming language or tool")
		bookFlag := fs.String("book", "", "Filter by book")
		catFlag := fs.String("category", "", "Filter by category")
		topKFlag := fs.Int("top-k", 5, "Number of code sections to retrieve")
		_ = fs.Parse(args)

		query := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if query == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp code [--lang <lang>] [--book <book>] [--category <cat>] <query>")
			os.Exit(1)
		}
		results := engine.ExtractCodeExamples(ctx, query, *langFlag, *bookFlag, *catFlag, *topKFlag)
		if len(results) == 0 {
			fmt.Println("No matching code sections found.")
			return
		}
		for i, res := range results {
			ck := res.Chunk
			fmt.Printf("=== [Code Example %d] %s — %s > %s (Score: %.4f) ===\n", i+1, ck.BookTitle, ck.ChapterTitle, ck.SectionTitle, res.Score)
			fmt.Println(res.ParentText)
			fmt.Println()
		}

	case "dedup":
		fs := flag.NewFlagSet("dedup", flag.ExitOnError)
		applyFlag := fs.Bool("apply", false, "Remove duplicate editions and keep the highest-quality copy of each book")
		_ = fs.Parse(args)

		rep, err := st.DeduplicateBooks(ctx, *applyFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Deduplication failed: %v\n", err)
			os.Exit(1)
		}
		if len(rep.Groups) == 0 {
			fmt.Println("No duplicate books found in the catalog.")
			return
		}
		if *applyFlag {
			fmt.Printf("Deduplication Complete: Removed %d duplicate books (%d chunks freed):\n", len(rep.RemovedBooks), rep.FreedChunks)
		} else {
			fmt.Printf("Duplicate Books Analysis (Dry Run): %d duplicate groups found (%d chunks can be freed).\nRun 'library-mcp dedup --apply' to remove duplicates.\n\n", len(rep.Groups), rep.FreedChunks)
		}
		for i, g := range rep.Groups {
			fmt.Printf("  [%d] %s — %s (%s)\n", i+1, g.Keep.Title, g.Keep.Author, g.Reason)
			fmt.Printf("      KEEP : [%s] %s (%d chunks)\n", g.Keep.ID, g.Keep.FileName, g.Keep.ChunkCount)
			for _, d := range g.Duplicates {
				fmt.Printf("      DUP  : [%s] %s (%d chunks)\n", d.ID, d.FileName, d.ChunkCount)
			}
		}

	case "notes":
		subAction := "list"
		subArgs := args
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			subAction = args[0]
			subArgs = args[1:]
		}
		switch subAction {
		case "add", "save":
			fs := flag.NewFlagSet("notes add", flag.ExitOnError)
			titleFlag := fs.String("title", "", "Note title")
			bookFlag := fs.String("book", "", "Book ID or title")
			chapFlag := fs.String("chapter", "", "Chapter ID")
			parentFlag := fs.String("parent", "", "Parent section ID")
			tagFlag := fs.String("tag", "", "Comma-separated tags")
			_ = fs.Parse(subArgs)
			content := strings.TrimSpace(strings.Join(fs.Args(), " "))
			if content == "" {
				fmt.Fprintln(os.Stderr, "Usage: library-mcp notes add [--title <t>] [--book <b>] [--chapter <c>] [--tag <t1,t2>] <note content>")
				os.Exit(1)
			}
			var tags []string
			for _, t := range strings.Split(*tagFlag, ",") {
				if strings.TrimSpace(t) != "" {
					tags = append(tags, strings.TrimSpace(t))
				}
			}
			saved, err := st.SaveNote(store.NoteRecord{
				Title:     *titleFlag,
				Content:   content,
				BookID:    *bookFlag,
				ChapterID: *chapFlag,
				ParentID:  *parentFlag,
				Tags:      tags,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error saving note: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Saved note [%s] %s (Book: %s)\n", saved.ID, saved.Title, saved.BookTitle)

		case "remove", "delete", "rm":
			if len(subArgs) == 0 {
				fmt.Fprintln(os.Stderr, "Usage: library-mcp notes remove <note-id>")
				os.Exit(1)
			}
			deleted, err := st.DeleteNote(subArgs[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error deleting note: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Deleted note [%s] %s\n", deleted.ID, deleted.Title)

		default:
			fs := flag.NewFlagSet("notes list", flag.ExitOnError)
			bookFlag := fs.String("book", "", "Filter by book")
			tagFlag := fs.String("tag", "", "Filter by tag")
			_ = fs.Parse(subArgs)
			query := strings.TrimSpace(strings.Join(fs.Args(), " "))
			notes := st.ListNotes(query, *bookFlag, *tagFlag)
			if len(notes) == 0 {
				fmt.Println("No saved notes found.")
				return
			}
			fmt.Printf("Saved Notes (%d):\n", len(notes))
			for _, n := range notes {
				fmt.Printf("  - [%s] %s (Book: %s | Updated: %s)\n    %s\n", n.ID, n.Title, n.BookTitle, n.UpdatedAt, n.Content)
			}
		}

	case "dossier":
		fs := flag.NewFlagSet("dossier", flag.ExitOnError)
		catFlag := fs.String("category", "", "Filter by category")
		tagFlag := fs.String("tag", "", "Filter by tag")
		outFlag := fs.String("out", "", "Output Markdown file path")
		topKFlag := fs.Int("top-k", 6, "Number of core sections to include")
		_ = fs.Parse(args)

		topic := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if topic == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp dossier [--category <cat>] [--tag <tag>] [--out <file.md>] <topic>")
			os.Exit(1)
		}
		md, savedPath, err := engine.ExportTopicDossier(ctx, topic, *catFlag, *tagFlag, *outFlag, *topKFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Dossier export failed: %v\n", err)
			os.Exit(1)
		}
		if savedPath != "" {
			fmt.Printf("Saved dossier to %s\n\n", savedPath)
		}
		fmt.Println(md)

	case "watch":
		fs := flag.NewFlagSet("watch", flag.ExitOnError)
		intervalSec := fs.Int("interval", 10, "Poll interval in seconds")
		_ = fs.Parse(args)
		fmt.Printf("Watching %s for new or modified books (poll every %ds)...\n", cfg.ResolvedBooksDir(), *intervalSec)
		engine.StartWatcher(ctx, time.Duration(*intervalSec)*time.Second, func(ev rag.WatcherEvent) {
			fmt.Printf("[%s] %s -> %s (%d chunks)\n", ev.Timestamp, ev.File, ev.Status, ev.Chunks)
		})
		select {}

	case "ask":
		fs := flag.NewFlagSet("ask", flag.ExitOnError)
		bookFlag := fs.String("book", "", "Filter by book title, author, category, tag, or book ID")
		topKFlag := fs.Int("top-k", cfg.TopK, "Number of Parent Sections to feed into LLM")
		_ = fs.Parse(args)

		question := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if question == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp ask [--book <filter>] <question>")
			os.Exit(1)
		}

		resp, err := engine.Ask(ctx, question, *bookFlag, *topKFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n%s\n\n", resp.Answer)
		fmt.Printf("--- Promoted Parent Sections (%d sources, %d ms) ---\n", len(resp.Sources), resp.DurationMs)
		for i, s := range resp.Sources {
			fmt.Printf("  [Source %d] %s (%s) — %s > %s [Parent: %s, Children matched: %d]\n",
				i+1, s.Chunk.BookTitle, s.Chunk.Author, s.Chunk.ChapterTitle, s.Chunk.SectionTitle, s.ParentID, s.ChildHits)
		}

	case "books":
		fs := flag.NewFlagSet("books", flag.ExitOnError)
		catFlag := fs.String("category", "", "Filter books by category")
		tagFlag := fs.String("tag", "", "Filter books by tag")
		_ = fs.Parse(args)

		books := st.ListBooksFiltered(*catFlag, *tagFlag)
		if len(books) == 0 {
			fmt.Println("No matching books indexed yet. Run: library-mcp ingest")
			return
		}
		fmt.Printf("Indexed Books (%d):\n", len(books))
		printBookRecords(books)

	case "remove", "delete", "rm":
		fs := flag.NewFlagSet("remove", flag.ExitOnError)
		allFlag := fs.Bool("all", false, "Remove all indexed books from the catalog")
		_ = fs.Parse(args)

		if *allFlag {
			books := st.ListBooks()
			for _, b := range books {
				_, _ = st.RemoveBook(ctx, b.ID)
			}
			fmt.Printf("Removed all %d books from the catalog.\n", len(books))
			return
		}

		target := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if target == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp remove <book-id | title | isbn | filename> [--all]")
			os.Exit(1)
		}
		removed, err := st.RemoveBook(ctx, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error removing book: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed book: [%s] %s by %s (%d chapters, %d parent sections, %d child chunks)\n",
			removed.ID, removed.Title, removed.Author, removed.ChapterCount, removed.ParentCount, removed.ChunkCount)

	case "status":
		bCount, chCount, pCount, ckCount, vCount := st.Stats()
		cats, tags := st.ListCategoriesAndTags()
		qdrantOK := engine.CheckQdrant(ctx)
		models, ollamaErr := engine.ListOllamaModels(ctx)
		fmt.Println("Library MCP Status:")
		fmt.Printf("  Configured Books Dir : %s\n", cfg.BooksDir)
		fmt.Printf("  Resolved OS Path     : %s\n", cfg.ResolvedBooksDir())
		fmt.Printf("  Qdrant Server        : %s (Online: %v)\n", cfg.QdrantURL, qdrantOK)
		fmt.Printf("  Qdrant Dashboard     : %s/dashboard\n", cfg.QdrantURL)
		fmt.Printf("  Ollama Server        : %s (Online: %v, Models: %d)\n", cfg.OllamaURL, ollamaErr == nil, len(models))
		fmt.Printf("  Embedding / Chat LLM : %s / %s\n", cfg.EmbeddingModel, cfg.ChatModel)
		fmt.Printf("  Catalog Totals       : %d books, %d chapters, %d parent sections, %d child chunks (%d vectors)\n",
			bCount, chCount, pCount, ckCount, vCount)
		fmt.Printf("  Taxonomy             : %d categories, %d unique tags\n", len(cats), len(tags))
		fmt.Printf("  Saved Notes          : %d notes | Duplicate Groups: %d\n", len(st.ListNotes("", "", "")), len(st.FindDuplicates()))

	case "sync":
		fmt.Printf("Syncing local catalog chunks to Qdrant (%s, collection: %s)...\n", cfg.QdrantURL, cfg.QdrantCollection)
		if err := st.SyncAllToQdrant(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Qdrant sync failed: %v\n", err)
			os.Exit(1)
		}
		_, _, _, ckCount, _ := st.Stats()
		fmt.Printf("Successfully synced %d child chunks to Qdrant.\n", ckCount)
		fmt.Printf("Inspect collection in Qdrant Dashboard: %s/dashboard#/collections/%s\n", cfg.QdrantURL, cfg.QdrantCollection)

	case "config":
		fs := flag.NewFlagSet("config", flag.ExitOnError)
		booksDir := fs.String("books-dir", "", "Set default books directory (e.g. H:\\Books or ~/Books)")
		qdrantURL := fs.String("qdrant-url", "", "Set Qdrant server URL")
		ollamaURL := fs.String("ollama-url", "", "Set Ollama server URL")
		embedModel := fs.String("embed-model", "", "Set Ollama embedding model")
		chatModel := fs.String("chat-model", "", "Set Ollama chat model")
		_ = fs.Parse(args)

		updated := false
		if *booksDir != "" {
			cfg.BooksDir = *booksDir
			updated = true
		}
		if *qdrantURL != "" {
			cfg.QdrantURL = *qdrantURL
			updated = true
		}
		if *ollamaURL != "" {
			cfg.OllamaURL = *ollamaURL
			updated = true
		}
		if *embedModel != "" {
			cfg.EmbeddingModel = *embedModel
			updated = true
		}
		if *chatModel != "" {
			cfg.ChatModel = *chatModel
			updated = true
		}
		if updated {
			if err := cfg.Save(cfgPath); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Configuration updated and saved.")
		}
		fmt.Printf("Current Config (%s):\n", cfgPath)
		fmt.Printf("  books_dir        : %s (resolves to: %s)\n", cfg.BooksDir, cfg.ResolvedBooksDir())
		fmt.Printf("  qdrant_url       : %s (collection: %s)\n", cfg.QdrantURL, cfg.QdrantCollection)
		fmt.Printf("  ollama_url       : %s\n", cfg.OllamaURL)
		fmt.Printf("  embedding_model  : %s\n", cfg.EmbeddingModel)
		fmt.Printf("  chat_model       : %s\n", cfg.ChatModel)
		fmt.Printf("  parent_chunk_size: %d\n", cfg.ParentChunkSize)
		fmt.Printf("  child_chunk_size : %d\n", cfg.ChildChunkSize)

	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		portFlag := fs.Int("port", cfg.Port, "Localhost port for Web UI")
		_ = fs.Parse(args)
		cfg.Port = *portFlag
		cfg.Sanitize()
		engine.UpdateConfig(cfg)

		srv := web.NewServer(engine, cfgPath)
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "Server stopped: %v\n", err)
			os.Exit(1)
		}

	case "mcp":
		mcpSrv := mcp.NewServer(engine)
		if err := mcpSrv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
			os.Exit(1)
		}

	default:
		printUsage()
		os.Exit(1)
	}
}

func printBookRecords(books []store.BookRecord) {
	for _, b := range books {
		title := b.Title
		if b.Subtitle != "" {
			title += ": " + b.Subtitle
		}
		fmt.Printf("  - [%s] %s — %s\n", b.ID, title, b.Author)
		cat := b.Category
		if cat == "" {
			cat = "Uncategorized"
		}
		fmt.Printf("      Category : %s\n", cat)
		if len(b.Tags) > 0 {
			fmt.Printf("      Tags     : %s\n", strings.Join(b.Tags, ", "))
		}
		var metaParts []string
		if b.Publisher != "" {
			metaParts = append(metaParts, "Publisher: "+b.Publisher)
		}
		if b.PublishedYear != "" {
			metaParts = append(metaParts, "Year: "+b.PublishedYear)
		}
		if b.ISBN != "" {
			metaParts = append(metaParts, "ISBN: "+b.ISBN)
		}
		if b.MetadataSource != "" {
			metaParts = append(metaParts, "Source: "+b.MetadataSource)
		}
		if len(metaParts) > 0 {
			fmt.Printf("      Metadata : %s\n", strings.Join(metaParts, " | "))
		}
		fmt.Printf("      Stats    : %d chapters, %d parent sections, %d child chunks\n",
			b.ChapterCount, b.ParentCount, b.ChunkCount)
	}
}

func printUsage() {
	fmt.Println(`Library MCP (Go + Qdrant Edition) — Parent-Child Hierarchical RAG for Technical EPUBs

Usage:
  library-mcp <command> [options]

Commands:
  ingest [--dir <path>] [--force]             Scan, fetch web metadata, tag/categorize, and index books
  enrich [--book <id>] [--force]              Fetch/refresh web metadata (Google Books + Open Library) & tags/categories
  search [--category <c>] [--tag <t>] <query> Hybrid RRF search with MMR diversity promoting Parent Sections
  toc <book-id-or-title>                      Inspect a book's full hierarchical Table of Contents
  chapters [--category <c>] <query>           Search across all chapter titles and section headings
  compare [--category <c>] <topic>            Compare perspectives on a topic across distinct books/authors
  code [--lang <lang>] <query>                Extract code examples, CLI commands, and technical patterns
  dedup [--apply]                             Detect and optionally remove duplicate book editions
  notes [list|add|remove] [...]               Save and search personal/agent notes, bookmarks, and takeaways
  dossier [--out <file.md>] <topic>           Compile a comprehensive Markdown Study Guide / Technical Dossier
  watch [--interval 10]                       Watch books directory for new/modified files and auto-index
  ask [--book <filter>] <question>            Ask a technical question answered by local Ollama with citations
  books [--category <c>] [--tag <t>]          List indexed books with categories, tags, and web metadata
  remove <id-or-title> [--all]                Remove an indexed book by ID, title, ISBN, or filename
  sync                                        Push all locally cataloged chunks & vectors to Qdrant
  status                                      Show Qdrant, Ollama, and library catalog status
  config [--books-dir <path>]                 View or update configuration (Windows, Linux, and WSL paths)
  serve [--port 8765]                         Start the local Web UI on http://127.0.0.1:8765 (with live watcher)
  mcp                                         Start the Model Context Protocol (MCP) server over stdio`)
}
