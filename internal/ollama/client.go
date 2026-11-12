package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"library-mcp/internal/domain"
)

// Client communicates with a local Ollama server.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// ModelInfo describes an installed Ollama model.
type ModelInfo = domain.ModelInfo

// ChatMessage represents a single message in an Ollama conversation.
type ChatMessage = domain.ChatMessage

var _ domain.AIGateway = (*Client)(nil)

// NewClient creates a new Ollama API client with sensible timeouts.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 180 * time.Second,
		},
	}
}

// ListModels returns the list of models currently installed in Ollama.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags returned status %d", resp.StatusCode)
	}

	var payload struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Models, nil
}

// EmbedBatch generates float32 vector embeddings for a batch of input texts.
func (c *Client) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	reqBody := map[string]any{
		"model": model,
		"input": texts,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/embed", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var result struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 50*1024*1024)).Decode(&result); err != nil {
			return nil, err
		}
		if len(result.Embeddings) == len(texts) {
			return result.Embeddings, nil
		}
	}

	// Fallback to single-prompt /api/embeddings for older Ollama versions
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vec, err := c.embedSingleFallback(ctx, model, text)
		if err != nil {
			return nil, err
		}
		out = append(out, vec)
	}
	return out, nil
}

func (c *Client) embedSingleFallback(ctx context.Context, model, prompt string) ([]float32, error) {
	reqBody := map[string]any{
		"model":  model,
		"prompt": prompt,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/embeddings", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama embeddings failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Embedding) == 0 {
		return nil, errors.New("empty embedding returned from ollama")
	}
	return result.Embedding, nil
}

// Chat sends a non-streaming chat completion request to Ollama.
func (c *Client) Chat(ctx context.Context, model string, messages []ChatMessage) (string, error) {
	reqBody := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   false,
		"options": map[string]any{
			"temperature": 0.15,
			"num_ctx":     16384,
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("ollama /api/chat returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var result struct {
		Message ChatMessage `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 20*1024*1024)).Decode(&result); err != nil {
		return "", err
	}
	return result.Message.Content, nil
}
