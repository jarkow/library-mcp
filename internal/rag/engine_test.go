package rag

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"library-mcp/internal/config"
	"library-mcp/internal/store"
)

func TestRecursiveDirectoryIngestion(t *testing.T) {
	rootDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")

	nestedAI := filepath.Join(rootDir, "AI", "Agents")
	nestedSec := filepath.Join(rootDir, "Security", "Web", "OWASP")
	if err := os.MkdirAll(nestedAI, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nestedSec, 0o755); err != nil {
		t.Fatal(err)
	}

	book1Path := filepath.Join(nestedAI, "llm_agents.epub")
	book2Path := filepath.Join(nestedSec, "xss_defense.epub")
	if err := writeTestEPUB(book1Path, "Building LLM Agents", "Alice Smith", "Retrieval-Augmented Generation and prompt engineering."); err != nil {
		t.Fatal(err)
	}
	if err := writeTestEPUB(book2Path, "Modern Web Application Security", "Bob Jones", "Defending against Cross-Site Scripting (XSS) and CSRF."); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.BooksDir = rootDir
	cfg.DataDir = dataDir

	st, err := store.Open(dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(cfg, st)
	// Avoid external network calls in unit tests
	eng.enricher.GoogleBaseURL = ""
	eng.enricher.OpenLibURL = ""

	rep, err := eng.IngestDir(context.Background(), rootDir, false, nil)
	if err != nil {
		t.Fatalf("IngestDir failed: %v", err)
	}
	if rep.ScannedFiles != 2 || rep.IndexedBooks != 2 {
		t.Fatalf("expected 2 scanned and indexed books, got scanned=%d indexed=%d", rep.ScannedFiles, rep.IndexedBooks)
	}
	if rep.ScannedDirs < 5 {
		t.Errorf("expected at least 5 scanned directories, got %d", rep.ScannedDirs)
	}

	books := st.ListBooks()
	if len(books) != 2 {
		t.Fatalf("expected 2 books in store, got %d", len(books))
	}
	for _, b := range books {
		if b.RelPath == "" {
			t.Errorf("expected non-empty RelPath for nested book %s", b.Title)
		}
	}
}

func writeTestEPUB(filePath, title, author, bodyText string) error {
	f, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	entries := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"content.opf": `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>` + title + `</dc:title>
    <dc:creator>` + author + `</dc:creator>
    <dc:language>en</dc:language>
  </metadata>
  <manifest><item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="c1"/></spine>
</package>`,
		"c1.xhtml": `<!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml"><body><h1>Chapter 1</h1><p>` + bodyText + `</p></body></html>`,
	}

	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return err
		}
	}
	return nil
}
