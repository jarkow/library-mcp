package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goFiles returns non-test Go files under dir (relative to internal/).
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(filepath.Join("..", dir), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

func imports(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, imp := range f.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out
}

// The domain core may only import the standard library.
func TestDomainHasNoInternalImports(t *testing.T) {
	for _, file := range goFiles(t, "domain") {
		for _, imp := range imports(t, file) {
			if strings.HasPrefix(imp, "library-mcp/") {
				t.Errorf("%s imports %s; domain must only use the standard library", file, imp)
			}
		}
	}
}

// Driving adapters must go through rag.Engine / domain ports, never driven adapters.
func TestDrivingAdaptersDoNotImportDrivenAdapters(t *testing.T) {
	forbidden := []string{
		"library-mcp/internal/store",
		"library-mcp/internal/qdrant",
		"library-mcp/internal/ollama",
		"library-mcp/internal/metadata",
	}
	for _, dir := range []string{"mcp", "web"} {
		for _, file := range goFiles(t, dir) {
			for _, imp := range imports(t, file) {
				for _, bad := range forbidden {
					if imp == bad {
						t.Errorf("%s imports %s; driving adapters must use rag.Engine / domain types", file, imp)
					}
				}
			}
		}
	}
}

// Nobody outside the composition root may bypass the use-case layer via Engine.Store().
func TestNoStoreBypassInAdapters(t *testing.T) {
	files := append(goFiles(t, "mcp"), goFiles(t, "web")...)
	files = append(files, goFiles(t, filepath.Join("..", "cmd"))...)
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), ".Store()") {
			t.Errorf("%s calls .Store(); use the rag.Engine use-case methods instead", file)
		}
	}
}
