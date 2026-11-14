package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestOpenAIURLNormalization(t *testing.T) {
	cases := []struct {
		baseURL  string
		protocol string
		wantOpen bool
		wantChat string
	}{
		{
			baseURL:  "https://api.openai.com",
			protocol: "openai",
			wantOpen: true,
			wantChat: "https://api.openai.com/v1/chat/completions",
		},
		{
			baseURL:  "https://api.openai.com/v1/",
			protocol: "openai",
			wantOpen: true,
			wantChat: "https://api.openai.com/v1/chat/completions",
		},
		{
			baseURL:  "https://openrouter.ai/api/v1",
			protocol: "",
			wantOpen: true,
			wantChat: "https://openrouter.ai/api/v1/chat/completions",
		},
		{
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/openai",
			protocol: "openai",
			wantOpen: true,
			wantChat: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		},
		{
			baseURL:  "http://127.0.0.1:11434",
			protocol: "",
			wantOpen: false,
			wantChat: "",
		},
	}

	for _, tc := range cases {
		c := NewClientWithConfig(tc.baseURL, tc.protocol, "sk-test")
		if c.isOpenAI() != tc.wantOpen {
			t.Fatalf("baseURL=%q protocol=%q: isOpenAI()=%v, want %v", tc.baseURL, tc.protocol, c.isOpenAI(), tc.wantOpen)
		}
		if tc.wantOpen {
			if got := c.openAIEndpoint("/chat/completions"); got != tc.wantChat {
				t.Fatalf("openAIEndpoint(%q)=%q, want %q", tc.baseURL, got, tc.wantChat)
			}
		}
	}
}

func TestOpenAIProtocolRequests(t *testing.T) {
	ctx := context.Background()
	const apiKey = "sk-secret-key"

	t.Run("ListModels", func(t *testing.T) {
		c := NewClientWithConfig("https://api.openai.com/v1", "openai", apiKey)
		c.HTTPClient = &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatalf("expected GET, got %s", r.Method)
				}
				if r.URL.String() != "https://api.openai.com/v1/models" {
					t.Fatalf("unexpected URL: %s", r.URL.String())
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
					t.Fatalf("unexpected Authorization header: %q", got)
				}
				return jsonResponse(http.StatusOK, `{"data":[{"id":"gpt-4o-mini"},{"id":"text-embedding-3-small"}]}`), nil
			}),
		}

		models, err := c.ListModels(ctx)
		if err != nil {
			t.Fatalf("ListModels failed: %v", err)
		}
		want := []ModelInfo{{Name: "gpt-4o-mini"}, {Name: "text-embedding-3-small"}}
		if !reflect.DeepEqual(models, want) {
			t.Fatalf("got %+v, want %+v", models, want)
		}
	})

	t.Run("EmbedBatch_ReordersByIndex", func(t *testing.T) {
		c := NewClientWithConfig("https://api.openai.com", "openai", apiKey)
		c.HTTPClient = &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost {
					t.Fatalf("expected POST, got %s", r.Method)
				}
				if r.URL.String() != "https://api.openai.com/v1/embeddings" {
					t.Fatalf("unexpected URL: %s", r.URL.String())
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
					t.Fatalf("unexpected Authorization header: %q", got)
				}
				var req struct {
					Model string   `json:"model"`
					Input []string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if req.Model != "text-embedding-3-small" || !reflect.DeepEqual(req.Input, []string{"first", "second"}) {
					t.Fatalf("unexpected request body: %+v", req)
				}
				// Intentionally out-of-order response indices to verify index mapping
				return jsonResponse(http.StatusOK, `{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}]}`), nil
			}),
		}

		vecs, err := c.EmbedBatch(ctx, "text-embedding-3-small", []string{"first", "second"})
		if err != nil {
			t.Fatalf("EmbedBatch failed: %v", err)
		}
		want := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
		if !reflect.DeepEqual(vecs, want) {
			t.Fatalf("got %+v, want %+v", vecs, want)
		}
	})

	t.Run("Chat", func(t *testing.T) {
		c := NewClientWithConfig("https://openrouter.ai/api/v1", "openai", apiKey)
		c.HTTPClient = &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost {
					t.Fatalf("expected POST, got %s", r.Method)
				}
				if r.URL.String() != "https://openrouter.ai/api/v1/chat/completions" {
					t.Fatalf("unexpected URL: %s", r.URL.String())
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
					t.Fatalf("unexpected Authorization header: %q", got)
				}
				var req struct {
					Model    string        `json:"model"`
					Messages []ChatMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if req.Model != "gpt-4o-mini" || len(req.Messages) != 2 {
					t.Fatalf("unexpected request body: %+v", req)
				}
				return jsonResponse(http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"B-Trees optimize page reads."}}]}`), nil
			}),
		}

		reply, err := c.Chat(ctx, "gpt-4o-mini", []ChatMessage{
			{Role: "system", Content: "Be concise."},
			{Role: "user", Content: "Explain B-Trees."},
		})
		if err != nil {
			t.Fatalf("Chat failed: %v", err)
		}
		if reply != "B-Trees optimize page reads." {
			t.Fatalf("got %q", reply)
		}
	})
}

func TestOllamaProtocolRequests(t *testing.T) {
	ctx := context.Background()

	c := NewClient("http://127.0.0.1:11434")
	c.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("Authorization"); got != "" {
				t.Fatalf("did not expect Authorization header for Ollama without key, got %q", got)
			}
			switch r.URL.Path {
			case "/api/tags":
				return jsonResponse(http.StatusOK, `{"models":[{"name":"bge-m3"}]}`), nil
			case "/api/embed":
				return jsonResponse(http.StatusOK, `{"embeddings":[[0.5,0.6]]}`), nil
			case "/api/chat":
				return jsonResponse(http.StatusOK, `{"message":{"role":"assistant","content":"Local answer."}}`), nil
			default:
				t.Fatalf("unexpected Ollama path: %s", r.URL.Path)
				return nil, nil
			}
		}),
	}

	models, err := c.ListModels(ctx)
	if err != nil || len(models) != 1 || models[0].Name != "bge-m3" {
		t.Fatalf("unexpected Ollama ListModels result: models=%+v err=%v", models, err)
	}

	vecs, err := c.EmbedBatch(ctx, "bge-m3", []string{"hello"})
	if err != nil || !reflect.DeepEqual(vecs, [][]float32{{0.5, 0.6}}) {
		t.Fatalf("unexpected Ollama EmbedBatch result: vecs=%+v err=%v", vecs, err)
	}

	reply, err := c.Chat(ctx, "qwen2.5-coder:14b", []ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil || reply != "Local answer." {
		t.Fatalf("unexpected Ollama Chat result: reply=%q err=%v", reply, err)
	}
}
