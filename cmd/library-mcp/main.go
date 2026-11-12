package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

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
		topKFlag := fs.Int("top-k", cfg.TopK, "Number of Parent Sections to retrieve")
		_ = fs.Parse(args)

		filter := *bookFlag
		if filter == "" && *catFlag != "" {
			filter = *catFlag
		}
		if filter == "" && *tagFlag != "" {
			filter = *tagFlag
		}

		query := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if query == "" {
			fmt.Fprintln(os.Stderr, "Usage: library-mcp search [--mode hybrid|semantic|keyword] [--book <filter>] [--category <cat>] [--tag <tag>] <query>")
			os.Exit(1)
		}

		results := engine.Search(ctx, query, *modeFlag, filter, *topKFlag)
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
  ingest [--dir <path>] [--force]             Scan, fetch web metadata, tag/categorize, and index EPUB books
  enrich [--book <id>] [--force]              Fetch/refresh web metadata (Google Books + Open Library) & tags/categories
  search [--category <c>] [--tag <t>] <query> Hybrid RRF search promoting Parent Sections
  ask [--book <filter>] <question>            Ask a technical question answered by local Ollama with citations
  books [--category <c>] [--tag <t>]          List indexed EPUB books with categories, tags, and web metadata
  status                                      Show Qdrant, Ollama, and library catalog status
  config [--books-dir <path>]                 View or update configuration (Windows, Linux, and WSL paths)
  serve [--port 8765]                         Start the local Web UI on http://127.0.0.1:8765
  mcp                                         Start the Model Context Protocol (MCP) server over stdio`)
}
