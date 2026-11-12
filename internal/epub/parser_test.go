package epub

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFileHierarchicalAndXXEProtection(t *testing.T) {
	tmpDir := t.TempDir()
	epubPath := filepath.Join(tmpDir, "test_systems.epub")

	if err := createSyntheticEPUB(epubPath); err != nil {
		t.Fatalf("failed to create synthetic epub: %v", err)
	}

	book, err := ParseFile(epubPath, 2000, 500, 80)
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}

	if book.Title != "Systems Programming in Go" {
		t.Errorf("unexpected title: %q", book.Title)
	}
	if len(book.Chapters) != 1 {
		t.Fatalf("expected 1 chapter, got %d", len(book.Chapters))
	}
	if len(book.Parents) == 0 {
		t.Fatal("expected at least 1 ParentSection")
	}
	if len(book.Chunks) == 0 {
		t.Fatal("expected at least 1 ChildChunk")
	}

	foundCode := false
	for _, ck := range book.Chunks {
		if ck.ParentID == "" {
			t.Errorf("chunk %s missing ParentID", ck.ID)
		}
		if strings.Contains(ck.Text, "io.LimitReader") {
			foundCode = true
		}
	}
	if !foundCode {
		t.Error("expected code block content 'io.LimitReader' to be preserved in chunks")
	}

	// Verify XXE entity rejection
	xxePayload := []byte(`<?xml version="1.0"?><!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><container>&xxe;</container>`)
	var c containerXML
	if err := safeXMLUnmarshal(xxePayload, &c); err == nil {
		t.Error("expected safeXMLUnmarshal to reject XML with <!ENTITY declaration")
	}
}

func createSyntheticEPUB(filePath string) error {
	f, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	files := map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`,
		"OEBPS/content.opf": `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Systems Programming in Go</dc:title>
    <dc:creator>Jane Doe</dc:creator>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
  </spine>
</package>`,
		"OEBPS/ch1.xhtml": `<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml">
<body>
  <h1>Memory and I/O Safety</h1>
  <p>Bounded readers prevent denial-of-service when reading untrusted streams.</p>
  <pre><code>rc := io.LimitReader(conn, 1048576)</code></pre>
</body>
</html>`,
	}

	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(body)); err != nil {
			return err
		}
	}
	return nil
}
