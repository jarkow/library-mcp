package qdrant

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnsureContainerRunningHealthyFastPath(t *testing.T) {
	var hits atomic.Int32
	c := NewClient("http://127.0.0.1:6333", "smart_library_chunks")
	c.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/healthz" {
				hits.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("ok")),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
			}, nil
		}),
	}
	c.EnableAutoStart(t.TempDir())

	if err := c.EnsureContainerRunning(context.Background()); err != nil {
		t.Fatalf("expected nil error on healthy endpoint, got %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected exactly 1 /healthz call, got %d", hits.Load())
	}
}

func TestCopyAutoStartFrom(t *testing.T) {
	src := NewClient("http://127.0.0.1:6333", "col1")
	dir := t.TempDir()
	src.EnableAutoStart(dir, dir)

	dst := NewClient("http://127.0.0.1:6333", "col2")
	dst.CopyAutoStartFrom(src)

	if !dst.autoStart {
		t.Fatal("expected autoStart to be copied")
	}
	if len(dst.composeDirs) != 1 || dst.composeDirs[0] != src.composeDirs[0] {
		t.Fatalf("unexpected copied composeDirs: %#v (src=%#v)", dst.composeDirs, src.composeDirs)
	}
}

func TestBuildBookFilter(t *testing.T) {
	if got := buildBookFilter("   "); got != nil {
		t.Fatalf("expected nil for blank filter, got %#v", got)
	}

	plain := buildBookFilter("Clean Architecture")
	should, ok := plain["should"].([]map[string]any)
	if !ok || len(should) != 2 {
		t.Fatalf("expected 2 'should' clauses for plain filter, got %#v", plain)
	}

	multi := buildBookFilter("book_ids: b1, b2 , ")
	must, ok := multi["must"].([]map[string]any)
	if !ok || len(must) != 1 {
		t.Fatalf("expected 1 'must' clause for book_ids filter, got %#v", multi)
	}
	match, _ := must[0]["match"].(map[string]any)
	wantIDs := []string{"b1", "b2"}
	if gotIDs, _ := match["any"].([]string); !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("expected match.any=%v, got %#v", wantIDs, match["any"])
	}
}
