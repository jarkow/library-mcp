# Library MCP (Go + Qdrant Edition)

A 100% local, structure-aware **Parent-Child Hierarchical RAG** system and **Model Context Protocol (MCP)** server for technical EPUB books written in **Go (`golang`)**, backed by **Qdrant** (Hybrid Dense + Sparse RRF Search) and **Ollama** (`bge-m3` + `qwen2.5-coder:14b`).

Works natively on **Windows (`library-mcp.exe`)**, **Linux**, and **WSL**.

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
go build -o library-mcp.exe .\cmd\library-mcp
```

---

## 2. How to Feed It With New Books

Library MCP computes a **SHA-256 hash** of every `.epub` file it indexes. Drop new `.epub` files into `H:\Books` anytime and run `ingest`—it **automatically skips already-indexed books** and only processes the new ones.

### Option A: Ingest From Default `H:\Books`
```powershell
.\library-mcp.exe ingest
```

### Option B: Ingest From Any Other Folder on the Fly
```powershell
.\library-mcp.exe ingest --dir "D:\MoreBooks"
```

### Option C: Change the Default Books Folder Permanently
```powershell
.\library-mcp.exe config --books-dir "H:\Books"
```

### Option D: Force Re-Index All Books
If you change chunk sizes or switch embedding models in `config.json`:
```powershell
.\library-mcp.exe ingest --force
```

### Option E: Via the Web UI
Start the Web UI:
```powershell
.\library-mcp.exe serve
```
Open `http://127.0.0.1:8765` and click **Scan Books Dir** in the top bar, or click **+ Upload EPUB** in the left sidebar.

---

## 3. How to Start & Use It as an MCP Server on Windows

The `library-mcp.exe` binary includes a built-in **Model Context Protocol (MCP)** server (`.\library-mcp.exe mcp`) over `stdio` JSON-RPC 2.0. Your AI client spawns it automatically—just ensure **Qdrant** (`docker compose up -d`) and **Ollama** are running.

### A. Connect to Antigravity / Cursor / Claude Desktop on Windows
Add `library-mcp` to your MCP configuration file (replace `C:\\path\\to\\library-mcp` with where you cloned the repo):

```json
{
  "mcpServers": {
    "library-mcp": {
      "command": "C:\\path\\to\\library-mcp\\library-mcp.exe",
      "args": ["mcp"],
      "env": {
        "SMART_LIBRARY_CONFIG": "C:\\path\\to\\library-mcp\\config.json"
      }
    }
  }
}
```

### B. Connect to Claude Code CLI on Windows
```powershell
claude mcp add library-mcp -- C:\path\to\library-mcp\library-mcp.exe mcp
```

### C. Sanity-Check the MCP Server in PowerShell
```powershell
'{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | .\library-mcp.exe mcp
```

### Tools Exposed to Your AI Agent via MCP
1. **`search_books`** (`query`, optional `book_filter`, optional `top_k`):
   Runs Qdrant Hybrid Search (Dense `bge-m3` + Sparse BM25 RRF) and returns the **promoted Parent Sections** (~3,200 chars each, with complete code blocks and `[Book > Chapter > Section]` metadata).
2. **`list_books`**:
   Lists all technical EPUB books currently indexed in your library.
3. **`get_chapter`** (`chapter_id`):
   Fetches the full text of a chapter when the AI agent needs to read beyond a single section.
