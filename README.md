# Library MCP (Go + Qdrant Edition)

A 100% local, structure-aware **Parent-Child Hierarchical RAG** system and **Model Context Protocol (MCP)** server for technical EPUB books written in **Go (`golang`)**, backed by **Qdrant** (Hybrid Dense + Sparse RRF Search), **Ollama** (`bge-m3` + `qwen2.5-coder:14b`), and **automatic Web Metadata Enrichment, Categorization & Tagging** (via Google Books & Open Library).

Works natively on **Linux (`library-mcp`)**, **Windows (`library-mcp.exe`)**, and **WSL**.

---

## 1. Initial Setup (Linux, WSL & Windows)

### A. Linux / WSL (Bash / Zsh)
```bash
# 1. Start Qdrant Vector DB (REST API: 127.0.0.1:6333 | Dashboard: http://127.0.0.1:6333/dashboard)
docker compose up -d

# 2. Pull the embedding model (bge-m3) and chat model in Ollama
ollama pull bge-m3
ollama pull qwen2.5-coder:14b

# 3. Format, test, and build the Linux binary
gofmt -w .
go test ./...
go build -o library-mcp ./cmd/library-mcp
```

### B. Windows (PowerShell / CMD)
```powershell
# 1. Start Qdrant Vector DB
docker compose up -d

# 2. Pull the embedding model (bge-m3) and chat model in Ollama
ollama pull bge-m3
ollama pull qwen2.5-coder:14b

# 3. Format, test, and build the Windows executable
gofmt -w .
go test ./...
go build -o library-mcp.exe .\cmd\library-mcp
```

---

## 2. Ingesting Books & Automatic Web Metadata / Categorization / Tagging

Whenever new `.epub` files are ingested, **Library MCP**:
1. Computes a **SHA-256 hash** to automatically skip unchanged books.
2. Extracts embedded OPF metadata, chapters, and ISBNs.
3. Queries **Google Books** and **Open Library** over HTTPS to fetch canonical bibliographic metadata (**Title, Subtitle, Author, Publisher, Published Year, ISBN-13, Page Count, Description, Cover URL, and Web URL**).
4. Automatically assigns a primary **Category** (e.g., `AI & Machine Learning`, `Security & Cryptography`, `Software Architecture & Design`, `Software Engineering & DevOps`, `Data Engineering & Databases`, `Systems & Networking`) and up to 8 normalized technical **Tags** (e.g., `llm`, `rag`, `xss`, `domain-driven-design`, `bounded-context`, `ci-cd`).

### Ingest Commands (Linux & Windows)
```bash
# Ingest from configured books_dir (supports ~/Books, /mnt/h/Books, or H:\Books)
./library-mcp ingest          # Linux / WSL
.\library-mcp.exe ingest      # Windows

# Ingest from a custom folder
./library-mcp ingest --dir ~/Books

# Fetch/refresh web metadata, categories & tags for already-indexed books
./library-mcp enrich --force

# List indexed books (optionally filter by --category or --tag)
./library-mcp books
./library-mcp books --category "Security"
./library-mcp books --tag "rag"
```

---

## 3. Running as an MCP Server (Linux & Windows)

The binary includes a built-in **Model Context Protocol (MCP)** server (`library-mcp mcp`) over `stdio` JSON-RPC 2.0.

### A. Linux / WSL MCP Configuration (`mcp_config.json`)
```json
{
  "mcpServers": {
    "library-mcp": {
      "command": "/path/to/library-mcp/library-mcp",
      "args": ["mcp"],
      "env": {
        "LIBRARY_MCP_CONFIG": "/path/to/library-mcp/config.json"
      }
    }
  }
}
```

### B. Windows MCP Configuration (`mcp_config.json`)
```json
{
  "mcpServers": {
    "library-mcp": {
      "command": "C:\\path\\to\\library-mcp\\library-mcp.exe",
      "args": ["mcp"],
      "env": {
        "LIBRARY_MCP_CONFIG": "C:\\path\\to\\library-mcp\\config.json"
      }
    }
  }
}
```

### Tools Exposed to Your AI Agent via MCP
1. **`search_books`** (`query`, optional `book_filter`, optional `category`, optional `tag`, optional `top_k`):
   Runs Qdrant Hybrid Search (Dense `bge-m3` + Sparse BM25 RRF) and returns the **promoted Parent Sections** (~3,200 chars each, with complete code blocks and `[Book > Chapter > Section]` metadata).
2. **`list_books`** (optional `category`, optional `tag`):
   Lists all technical EPUB books currently indexed in your library along with their **Category**, **Tags**, **Publisher**, **Year**, **ISBN**, and **Metadata Source**.
3. **`get_chapter`** (`chapter_id`):
   Fetches the full text of a chapter when the AI agent needs to read beyond a single section.
4. **`enrich_books`** (optional `book_id`, optional `force`):
   Fetches/refreshes live web metadata from Google Books & Open Library and updates categories and tags.
