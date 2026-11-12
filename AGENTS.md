# AGENTS.md — AI Coding Agent Guide for `library-mcp`

This document provides architectural context, domain rules, and step-by-step instructions for AI coding agents working on **`library-mcp`**. Read this file before modifying or extending the codebase.

---

## 1. Project Purpose & Core Concepts

**`library-mcp`** is a local, structure-aware **Parent-Child Hierarchical RAG** engine and **Model Context Protocol (MCP)** server for technical books (`.epub`, `.pdf`, `.mobi`, `.azw`, `.azw3`), written in pure Go (`CGO_ENABLED=0`).

### The Three-Tier Book Hierarchy
Every ingested book is parsed into three levels of granularity defined in `internal/domain`:
1. **`ParsedChapter` (Full Chapters)**: Complete chapters from the book spine (`10k–50k+` characters) used when a human or AI agent wants to read an entire chapter (`get_chapter`).
2. **`ParentSection` (Medium Sections, `~3,200` chars)**: Coherent subsections that preserve complete code blocks (`<pre><code>`), Markdown tables, and `[Book > Chapter > Section]` breadcrumbs. **Search results always return promoted `ParentSection`s**, never fragmented snippets.
3. **`ParsedChunk` (Small Paragraphs, `~650` chars, `120`-char overlap)**: Fine-grained search units linked to their parent via `ParentID`. These are embedded into vectors and indexed for search.

---

## 2. Hexagonal Architecture (Ports & Adapters) & DDD Layout

The codebase is organized using **Domain-Driven Design (DDD)** and **Hexagonal Architecture (Ports & Adapters)**:

```text
library-mcp/
├── cmd/library-mcp/        # Composition Root & CLI Driving Adapter (main.go)
├── internal/
│   ├── domain/             # DOMAIN CORE: Entities, Value Objects & Port Interfaces (ZERO internal imports)
│   │   ├── entities.go     # ParsedBook, BookRecord, ParentSection, ParsedChunk, NoteRecord, BookTOC, etc.
│   │   └── ports.go        # Driving Port (LibraryService) & Driven Ports (CatalogRepository, VectorIndex, AIGateway, MetadataGateway)
│   ├── rag/                # APPLICATION LAYER: Implements domain.LibraryService use cases
│   │   ├── engine.go       # Ingestion, metadata enrichment, hybrid search, Q&A orchestration
│   │   └── features.go     # Multi-book comparison, code extraction, study dossiers, live folder watcher
│   ├── mcp/                # DRIVING ADAPTER (Inbound): MCP JSON-RPC 2.0 server over stdio
│   ├── web/                # DRIVING ADAPTER (Inbound): Local HTTP REST API & Web UI (127.0.0.1:8765)
│   ├── store/              # DRIVEN ADAPTER (Outbound): SQLite + FTS5 + Hybrid catalog (implements domain.CatalogRepository)
│   │   ├── sqlite.go       # Pure-Go SQLite schema, FTS5 full-text search, WAL checkpointing, auto-migration
│   │   ├── store.go        # Hybrid Qdrant + SQLite coordination, Parent-Child promotion
│   │   └── features.go     # TOC, chapter search, MMR diversity, deduplication, notes CRUD
│   ├── qdrant/             # DRIVEN ADAPTER (Outbound): Qdrant REST + Hybrid RRF client (implements domain.VectorIndex)
│   ├── ollama/             # DRIVEN ADAPTER (Outbound): Ollama embeddings & chat client (implements domain.AIGateway)
│   ├── metadata/           # DRIVEN ADAPTER (Outbound): Google Books + Open Library + Taxonomy classifier (implements domain.MetadataGateway)
│   ├── epub/               # DRIVEN ADAPTER (Outbound): Multi-format parsers (.epub, .pdf, .mobi, .azw3)
│   └── config/             # Cross-platform configuration & WSL/Windows/Linux path translation
```

### Strict Dependency Rules
1. **`internal/domain` is the inner core**: It must **never** import any other package in `internal/`. It may only import the Go standard library.
2. **Compile-time Port Assertions**: Every adapter must maintain its compile-time interface check:
   - `var _ domain.LibraryService = (*rag.Engine)(nil)` in `internal/rag/engine.go`
   - `var _ domain.CatalogRepository = (*store.Store)(nil)` in `internal/store/store.go`
   - `var _ domain.VectorIndex = (*qdrant.Client)(nil)` in `internal/qdrant/client.go`
   - `var _ domain.AIGateway = (*ollama.Client)(nil)` in `internal/ollama/client.go`
   - `var _ domain.MetadataGateway = (*metadata.Enricher)(nil)` in `internal/metadata/enricher.go`
3. **Adapter Isolation**:
   - Driving adapters (`internal/mcp`, `internal/web`, `cmd/library-mcp`) must never write raw SQL or call Qdrant/Ollama HTTP endpoints directly; they must go through `internal/rag` (`domain.LibraryService`) or `domain.CatalogRepository`.

---

## 3. Storage & Retrieval Architecture (SQLite vs. Qdrant)

### Why Both Databases Exist
- **SQLite (`data/library.db` via `internal/store/sqlite.go`)**:
  - Stores **all relational data and full text**: `books`, `chapters`, `parents` (~3,200 chars), `chunks` (~650 chars), `notes`, and the `chunks_fts` **FTS5** full-text search virtual table.
  - Handles all direct ID lookups (`GetBook`, `GetBookTOC`, `GetChapter`, `GetParentSection`), metadata filtering (`category`, `tag`, `author`, `year_min`), notes CRUD, and **offline keyword search** via `chunks_fts` (`BM25`).
- **Qdrant (`data/qdrant_storage/` via `internal/qdrant/client.go`)**:
  - Stores **only `ParsedChunk` points** with their dense embeddings (`bge-m3`) and sparse keyword vectors (`IDF`).
  - Performs semantic + hybrid Reciprocal Rank Fusion (RRF) search to discover the best matching child chunks, which `internal/store` then promotes to full `ParentSection`s from SQLite.

### Critical Performance & Resilience Invariants
1. **Never Load the Full Corpus Into RAM for Standard Queries**:
   - `GetBook`, `GetBookTOC`, `GetChapter`, `GetParentSection`, `SearchChapters`, `ListBooks`, `Notes`, and `SearchWithOptions` (when using SQLite `chunks_fts`) must query `s.db` (`data/library.db`) directly via indexed SQL queries.
   - `s.ensureCorpusLoaded()` is reserved strictly for full-corpus maintenance operations like `SyncAllToQdrant`.
2. **Offline Fast-Fail Cooldowns**:
   - The system must remain 100% functional when Docker (Qdrant), Ollama, or the internet (Google Books / Open Library) is unreachable.
   - Both `rag.Engine` (`ollamaOfflineUntil`) and `metadata.Enricher` (`offlineUntil`) enforce a **60-second fast-fail cooldown** after a network connection error so batch ingestion never stalls waiting on per-chunk or per-book timeouts.
3. **Clean SQLite Handle Closure**:
   - Always close `*store.Store` (`defer st.Close()`) in CLI commands and unit tests (`t.TempDir()`). On Windows, open SQLite file handles prevent `t.TempDir()` cleanup if `st.Close()` is omitted.

---

## 4. Security & Cross-Platform Guidelines

1. **Pure Go (`CGO_ENABLED=0`)**:
   - Do not introduce CGO dependencies. We use `modernc.org/sqlite` specifically so both Linux (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`) and Windows binaries cross-compile cleanly from any host OS.
2. **Path Traversal & Archive Safety**:
   - Always validate paths using `config.IsSafeSubpath(targetPath, allowedBaseDir)` before ingesting files from disk.
   - Keep `MaxEPUBFileSize` (250 MB) and `MaxEntryFileSize` (25 MB) limits and `Entity: map[string]string{}` (XXE protection) intact in `internal/epub`.
3. **Cross-Platform Path Translation**:
   - Use `config.TranslatePath` when resolving configured paths so Windows paths (`C:\...`, `H:\...`) and Linux/WSL paths (`/mnt/c/...`, `/mnt/h/...`) work interchangeably.

---

## 5. Playbooks for Common Changes

### A. Adding a New MCP Tool
1. Define any new domain request/response structs in `internal/domain/entities.go` and add the method to `domain.LibraryService` in `internal/domain/ports.go`.
2. Implement the method on `*rag.Engine` (in `internal/rag/features.go`) and/or `*store.Store` (in `internal/store/features.go`).
3. Register the tool schema in `handleToolsList` in `internal/mcp/server.go` and handle its execution in `handleToolsCall`.
4. Expose a corresponding CLI subcommand in `cmd/library-mcp/main.go` and document it in `README.md`.

### B. Adding Support for a New Book Format
1. Add the file extension to `IsSupportedBookExtension` and magic-byte header validation in `ValidateBookHeader` (`internal/epub/formats.go`).
2. Implement the format parser returning `*domain.ParsedBook` and call `buildHierarchyFromChapters` so chapters are split into `ParentSection`s (~3,200 chars) and `ParsedChunk`s (~650 chars) consistently.
3. Add a synthetic unit test in `internal/epub/formats_test.go`.

### C. Formatting, Testing & Building
Always run formatting and unit tests before finishing any task:

```bash
# Format all Go files
gofmt -w .

# Run all unit tests across all packages
go test -v ./...

# Build Linux binary (pure Go, static)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o library-mcp ./cmd/library-mcp

# Build Windows binary
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o library-mcp.exe ./cmd/library-mcp
```
