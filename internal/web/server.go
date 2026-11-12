package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"library-mcp/internal/config"
	"library-mcp/internal/epub"
	"library-mcp/internal/rag"
)

//go:embed static/*
var staticFS embed.FS

type sessionData struct {
	ID        string
	CSRFToken string
	ExpiresAt time.Time
}

// Server serves the local Smart Library Web UI and REST API over 127.0.0.1.
type Server struct {
	engine     *rag.Engine
	configPath string

	sessMu   sync.Mutex
	sessions map[string]sessionData

	rateMu     sync.Mutex
	rateCounts map[string]int
	rateWindow time.Time
}

// NewServer creates a new Web UI & API server.
func NewServer(engine *rag.Engine, configPath string) *Server {
	// Ensure session secret initialization check runs
	_ = config.GetSessionSecret(engine.GetConfig().ResolvedDataDir())
	return &Server{
		engine:     engine,
		configPath: configPath,
		sessions:   make(map[string]sessionData),
		rateCounts: make(map[string]int),
		rateWindow: time.Now(),
	}
}

// Handler returns the wrapped http.Handler with security headers, rate limiting, session, and CSRF protection.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	subStatic, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(subStatic))

	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))

	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/books", s.handleBooks)
	mux.HandleFunc("/api/book", s.handleBookDetail)
	mux.HandleFunc("/api/chapter", s.handleChapter)
	mux.HandleFunc("/api/parent", s.handleParentSection)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/ask", s.handleAsk)
	mux.HandleFunc("/api/ingest", s.handleIngest)
	mux.HandleFunc("/api/enrich", s.handleEnrich)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/books/delete", s.handleDeleteBook)
	mux.HandleFunc("/api/qdrant/sync", s.handleQdrantSync)
	mux.HandleFunc("/api/session/logout", s.handleLogout)

	return s.securityMiddleware(mux)
}

// ListenAndServe starts the HTTP server strictly bound to 127.0.0.1:<port>.
func (s *Server) ListenAndServe() error {
	cfg := s.engine.GetConfig()
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("Smart Library Web UI listening on http://%s", addr)
	return srv.ListenAndServe()
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strict HTTP method allow-list: only GET and POST are permitted
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Rate limiting (120 requests per minute)
		if !s.allowRequest(r.RemoteAddr) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}

		nonce := randomHex(16)
		csp := fmt.Sprintf(
			"default-src 'self'; script-src 'self' 'nonce-%s'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'",
			nonce,
		)
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Referrer-Policy", "same-origin")

		sess := s.getOrCreateSession(w, r)

		// Validate CSRF token on all state-changing POST requests
		if r.Method == http.MethodPost {
			clientToken := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
			if len(clientToken) == 0 || subtle.ConstantTimeCompare([]byte(clientToken), []byte(sess.CSRFToken)) != 1 {
				writeJSONError(w, http.StatusForbidden, "Invalid or missing CSRF token.")
				return
			}
		}

		ctx := context.WithValue(r.Context(), ctxKeyNonce{}, nonce)
		ctx = context.WithValue(ctx, ctxKeySession{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxKeyNonce struct{}
type ctxKeySession struct{}

func (s *Server) allowRequest(remoteAddr string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	now := time.Now()
	if now.Sub(s.rateWindow) > time.Minute {
		s.rateCounts = make(map[string]int)
		s.rateWindow = now
	}
	s.rateCounts[remoteAddr]++
	return s.rateCounts[remoteAddr] <= 180
}

// TODO(security): Consider OAuth2/MFA integration and leaked-password checks if deployed as a multi-user cloud service.
func (s *Server) getOrCreateSession(w http.ResponseWriter, r *http.Request) sessionData {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()

	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.ExpiresAt) {
			delete(s.sessions, k)
		}
	}

	if c, err := r.Cookie("__Secure-smartlib_sid"); err == nil {
		if sess, ok := s.sessions[c.Value]; ok && now.Before(sess.ExpiresAt) {
			return sess
		}
	}

	sess := sessionData{
		ID:        randomHex(32),
		CSRFToken: randomHex(32),
		ExpiresAt: now.Add(12 * time.Hour),
	}
	s.sessions[sess.ID] = sess

	http.SetCookie(w, &http.Cookie{
		Name:     "__Secure-smartlib_sid",
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // Localhost 127.0.0.1 HTTP transport
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 3600,
	})
	return sess
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	nonce, _ := r.Context().Value(ctxKeyNonce{}).(string)
	sess, _ := r.Context().Value(ctxKeySession{}).(sessionData)

	page := string(b)
	page = strings.ReplaceAll(page, "__CSP_NONCE__", nonce)
	page = strings.ReplaceAll(page, "__CSRF_TOKEN__", sess.CSRFToken)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cfg := s.engine.GetConfig()
	booksCount, chaptersCount, parentsCount, chunksCount, vectorsCount := s.engine.Store().Stats()
	qdrantOK := s.engine.CheckQdrant(ctx)
	models, ollamaErr := s.engine.ListOllamaModels(ctx)

	var modelNames []string
	for _, m := range models {
		modelNames = append(modelNames, m.Name)
	}

	resolvedBooksPath := cfg.ResolvedBooksDir()
	_, dirErr := os.Stat(resolvedBooksPath)

	writeJSON(w, http.StatusOK, map[string]any{
		"config":             cfg,
		"resolved_books_dir": resolvedBooksPath,
		"books_dir_exists":   dirErr == nil,
		"books_count":        booksCount,
		"chapters_count":     chaptersCount,
		"parents_count":      parentsCount,
		"chunks_count":       chunksCount,
		"vectors_count":      vectorsCount,
		"qdrant_online":      qdrantOK,
		"qdrant_dashboard":   strings.TrimRight(cfg.QdrantURL, "/") + "/dashboard",
		"ollama_online":      ollamaErr == nil,
		"ollama_models":      modelNames,
		"retrieval_strategy": "Parent-Child Hierarchical + Qdrant Hybrid RRF (Dense + Sparse)",
	})
}

func (s *Server) handleBooks(w http.ResponseWriter, r *http.Request) {
	cat := strings.TrimSpace(r.URL.Query().Get("category"))
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	books := s.engine.Store().ListBooksFiltered(cat, tag)
	categories, tags := s.engine.Store().ListCategoriesAndTags()
	writeJSON(w, http.StatusOK, map[string]any{
		"books":      books,
		"categories": categories,
		"tags":       tags,
	})
}

func (s *Server) handleEnrich(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	var req struct {
		BookID string `json:"book_id"`
		Force  bool   `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req)

	if strings.TrimSpace(req.BookID) != "" {
		rec, err := s.engine.EnrichBook(r.Context(), strings.TrimSpace(req.BookID))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"book": rec})
		return
	}

	books, err := s.engine.EnrichAllBooks(r.Context(), req.Force)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"books": books,
		"count": len(books),
	})
}

func (s *Server) handleBookDetail(w http.ResponseWriter, r *http.Request) {
	bookID := strings.TrimSpace(r.URL.Query().Get("id"))
	if bookID == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing book id parameter.")
		return
	}
	book, chapters, ok := s.engine.Store().GetBook(bookID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "Book not found.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"book":     book,
		"chapters": chapters,
	})
}

func (s *Server) handleChapter(w http.ResponseWriter, r *http.Request) {
	chapID := strings.TrimSpace(r.URL.Query().Get("id"))
	if chapID == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing chapter id parameter.")
		return
	}
	chap, ok := s.engine.Store().GetChapter(chapID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "Chapter not found.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chapter": chap,
	})
}

func (s *Server) handleParentSection(w http.ResponseWriter, r *http.Request) {
	parentID := strings.TrimSpace(r.URL.Query().Get("id"))
	if parentID == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing parent section id parameter.")
		return
	}
	parent, ok := s.engine.Store().GetParentSection(parentID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "Parent section not found.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"parent": parent,
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	var req struct {
		Query      string `json:"query"`
		Mode       string `json:"mode"`
		BookFilter string `json:"book_filter"`
		TopK       int    `json:"top_k"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON request body.")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSONError(w, http.StatusBadRequest, "Search query cannot be empty.")
		return
	}
	if req.Mode != "hybrid" && req.Mode != "semantic" && req.Mode != "keyword" {
		req.Mode = "hybrid"
	}

	results := s.engine.Search(r.Context(), req.Query, req.Mode, req.BookFilter, req.TopK)
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
	})
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	var req struct {
		Question   string `json:"question"`
		BookFilter string `json:"book_filter"`
		TopK       int    `json:"top_k"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON request body.")
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeJSONError(w, http.StatusBadRequest, "Question cannot be empty.")
		return
	}

	resp, err := s.engine.Ask(r.Context(), req.Question, req.BookFilter, req.TopK)
	if err != nil {
		log.Printf("Ask error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to process question.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	var req struct {
		BooksDir string `json:"books_dir"`
		Force    bool   `json:"force"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req)

	report, err := s.engine.IngestDir(r.Context(), req.BooksDir, req.Force, nil)
	if err != nil {
		log.Printf("IngestDir error: %v", err)
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Could not scan directory: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// TODO(security): Integrate antivirus/CDR scanning if untrusted public users are permitted to upload EPUBs.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}

	// Enforce maximum upload size (100 MB)
	const maxUploadSize = 100 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if err := r.ParseMultipartForm(32 * 1024 * 1024); err != nil {
		writeJSONError(w, http.StatusBadRequest, "File exceeds maximum allowed upload size (100 MB).")
		return
	}

	file, header, err := r.FormFile("epub")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Missing 'epub' file field.")
		return
	}
	defer file.Close()

	origBase := filepath.Base(header.Filename)
	if !strings.EqualFold(filepath.Ext(origBase), ".epub") {
		writeJSONError(w, http.StatusBadRequest, "Only .epub files are allowed.")
		return
	}

	cfg := s.engine.GetConfig()
	uploadsDir := cfg.UploadsDir()
	if err := os.MkdirAll(uploadsDir, 0o700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to prepare upload storage.")
		return
	}

	// Rename uploaded file to a random UUID string outside the web root
	safeFileName := randomHex(16) + ".epub"
	destPath := filepath.Join(uploadsDir, safeFileName)
	if !config.IsSafeSubpath(destPath, uploadsDir) {
		writeJSONError(w, http.StatusBadRequest, "Invalid destination path.")
		return
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Could not save uploaded file.")
		return
	}
	_, copyErr := io.Copy(out, io.LimitReader(file, maxUploadSize))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destPath)
		writeJSONError(w, http.StatusInternalServerError, "Failed to write uploaded file.")
		return
	}

	// Validate magic bytes and structure before indexing
	if _, _, err := epub.ValidateEPUBHeader(destPath); err != nil {
		_ = os.Remove(destPath)
		writeJSONError(w, http.StatusBadRequest, "Uploaded file is not a valid EPUB archive.")
		return
	}

	indexed, parentsCount, chunksCount, embedded, err := s.engine.IngestFile(r.Context(), destPath, uploadsDir, true)
	if err != nil {
		_ = os.Remove(destPath)
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Failed to parse EPUB: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"indexed":       indexed,
		"original_name": origBase,
		"parents_count": parentsCount,
		"chunks_count":  chunksCount,
		"embedded":      embedded,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	var incoming config.Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&incoming); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid configuration JSON.")
		return
	}

	curr := s.engine.GetConfig()
	if strings.TrimSpace(incoming.BooksDir) != "" {
		curr.BooksDir = strings.TrimSpace(incoming.BooksDir)
	}
	if strings.TrimSpace(incoming.QdrantURL) != "" {
		if u, err := config.ValidateEndpointURL(incoming.QdrantURL); err == nil {
			curr.QdrantURL = u
		}
	}
	if strings.TrimSpace(incoming.OllamaURL) != "" {
		if u, err := config.ValidateEndpointURL(incoming.OllamaURL); err == nil {
			curr.OllamaURL = u
		}
	}
	if strings.TrimSpace(incoming.EmbeddingModel) != "" {
		curr.EmbeddingModel = strings.TrimSpace(incoming.EmbeddingModel)
	}
	if strings.TrimSpace(incoming.ChatModel) != "" {
		curr.ChatModel = strings.TrimSpace(incoming.ChatModel)
	}
	if incoming.ParentChunkSize > 0 {
		curr.ParentChunkSize = incoming.ParentChunkSize
	}
	if incoming.ChildChunkSize > 0 {
		curr.ChildChunkSize = incoming.ChildChunkSize
	}
	if incoming.TopK > 0 {
		curr.TopK = incoming.TopK
	}

	curr.Sanitize()
	if err := curr.Save(s.configPath); err != nil {
		log.Printf("Failed to save config: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to save configuration file.")
		return
	}
	s.engine.UpdateConfig(&curr)

	writeJSON(w, http.StatusOK, map[string]any{
		"config":             curr,
		"resolved_books_dir": curr.ResolvedBooksDir(),
	})
}

func (s *Server) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	var req struct {
		BookID string `json:"book_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8*1024)).Decode(&req); err != nil || strings.TrimSpace(req.BookID) == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing book_id.")
		return
	}
	removed, err := s.engine.Store().RemoveBook(r.Context(), strings.TrimSpace(req.BookID))
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": true,
		"book":    removed,
	})
}

func (s *Server) handleQdrantSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	if err := s.engine.Store().SyncAllToQdrant(r.Context()); err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("Qdrant sync failed: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"synced": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required.")
		return
	}
	sess, _ := r.Context().Value(ctxKeySession{}).(sessionData)
	s.sessMu.Lock()
	delete(s.sessions, sess.ID)
	s.sessMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "__Secure-smartlib_sid",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"logged_out": true})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": msg,
	})
}

func randomHex(numBytes int) string {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
