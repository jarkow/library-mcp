# Smart Library (Go + Qdrant Edition)

A 100% local, structure-aware **Parent-Child Hierarchical RAG** system for technical EPUB books written in **Go (`golang`)**, backed by **Qdrant** (Hybrid Dense + Sparse RRF Search) and **Ollama** (`bge-m3` + `qwen2.5-coder:14b`).

Works natively on **Windows (`smart-lib.exe`)**, **Linux**, and **WSL**.

---

## 1. Initial Setup (Windows PowerShell / CMD)

```powershell
# 1. Start Qdrant Vector DB (REST API: 127.0.0.1:6333 | Dashboard: http://127.0.0.1:6333/dashboard)
docker compose up -d

# 2. Pull the embedding model (bge-m3) and chat model in Ollama
ollama pull bge-m3
ollama pull qwen2.5-coder:14b

# 3. Format and build the Windows executable
gofmt -w .
go build -o smart-lib.exe .\cmd\smart-lib
```

---

## 2. How to Feed It With New Books

Smart Library computes a **SHA-256 hash** of every `.epub` file it indexes. Drop new `.epub` files into `H:\Books` anytime and run `ingest`—it **automatically skips already-indexed books** and only processes the new ones.

### Option A: Ingest From Default `H:\Books`
```powershell
.\smart-lib.exe ingest
```

### Option B: Ingest From Any Other Folder on the Fly
```powershell
.\smart-lib.exe ingest --dir "D:\MoreBooks"
```

### Option C: Change the Default Books Folder Permanently
```powershell
.\smart-lib.exe config --books-dir "H:\Books"
```

### Option D: Force Re-Index All Books
If you change chunk sizes or switch embedding models in `config.json`:
```powershell
.\smart-lib.exe ingest --force
```

### Option E: Via the Web UI
Start the Web UI:
```powershell
.\smart-lib.exe serve
```
Open `http://127.0.0.1:8765` and click **Scan Books Dir** in the top bar, or click **+ Upload EPUB** in the left sidebar.

---

## 3. How to Start & Use It as an MCP Server on Windows

The `smart-lib.exe` binary includes a built-in **Model Context Protocol (MCP)** server (`.\smart-lib.exe mcp`) over `stdio` JSON-RPC 2.0. Your AI client spawns it automatically—just ensure **Qdrant** (`docker compose up -d`) and **Ollama** are running.

### A. Connect to Antigravity / Cursor / Claude Desktop on Windows
Add `smart-library` to your MCP configuration file (replace `C:\\path\\to\\smart-library` with where you cloned the repo):

```json
{
  "mcpServers": {
    "smart-library": {
      "command": "C:\\path\\to\\smart-library\\smart-lib.exe",
      "args": ["mcp"],
      "env": {
        "SMART_LIBRARY_CONFIG": "C:\\path\\to\\smart-library\\config.json"
      }
    }
  }
}
```

### B. Connect to Claude Code CLI on Windows
```powershell
claude mcp add smart-library -- C:\path\to\smart-library\smart-lib.exe mcp
```

### C. Sanity-Check the MCP Server in PowerShell
```powershell
'{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | .\smart-lib.exe mcp
```

### Tools Exposed to Your AI Agent via MCP
1. **`search_books`** (`query`, optional `book_filter`, optional `top_k`):
   Runs Qdrant Hybrid Search (Dense `bge-m3` + Sparse BM25 RRF) and returns the **promoted Parent Sections** (~3,200 chars each, with complete code blocks and `[Book > Chapter > Section]` metadata).
2. **`list_books`**:
   Lists all technical EPUB books currently indexed in your library.
3. **`get_chapter`** (`chapter_id`):
   Fetches the full text of a chapter when the AI agent needs to read beyond a single section.
