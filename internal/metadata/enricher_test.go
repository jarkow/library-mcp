package metadata

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mockRoundTripper func(req *http.Request) (*http.Response, error)

func (m mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m(req)
}

func TestEnrichAndClassifyBook(t *testing.T) {
	enricher := NewEnricher()
	enricher.HTTPClient = &http.Client{
		Transport: mockRoundTripper(func(req *http.Request) (*http.Response, error) {
			var body string
			if strings.Contains(req.URL.Host, "googleapis.com") {
				body = `{
					"items": [{
						"volumeInfo": {
							"title": "AI Engineering",
							"subtitle": "Building Applications with Foundation Models",
							"authors": ["Chip Huyen"],
							"publisher": "O'Reilly Media",
							"publishedDate": "2025-01-07",
							"description": "A comprehensive guide to building production applications with large language models, RAG, agents, and finetuning.",
							"pageCount": 532,
							"categories": ["Artificial Intelligence", "Machine Learning"],
							"infoLink": "https://books.google.com/books?id=aieng",
							"industryIdentifiers": [{"type": "ISBN_13", "identifier": "9781098166304"}]
						}
					}]
				}`
			} else {
				body = `{"docs": []}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	seed := BookMetadata{
		Title:  "AI Engineering (Z-Library)",
		Author: "Chip Huyen",
	}
	chapters := []string{
		"Introduction to Foundation Models",
		"Prompt Engineering",
		"RAG and Agents",
		"Finetuning and Inference Optimization",
	}

	got := enricher.Enrich(context.Background(), seed, chapters)
	if got.Category != "AI & Machine Learning" {
		t.Fatalf("expected category 'AI & Machine Learning', got %q", got.Category)
	}
	if got.Publisher != "O'Reilly Media" {
		t.Errorf("expected publisher 'O'Reilly Media', got %q", got.Publisher)
	}
	if got.PublishedYear != "2025" {
		t.Errorf("expected year '2025', got %q", got.PublishedYear)
	}
	if got.ISBN != "9781098166304" {
		t.Errorf("expected ISBN '9781098166304', got %q", got.ISBN)
	}
	if len(got.Tags) == 0 {
		t.Fatal("expected non-empty tags")
	}
}
