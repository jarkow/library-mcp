package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"library-mcp/internal/domain"
)

// Client communicates with a local Ollama server or an OpenAI-compatible API endpoint.
type Client struct {
	BaseURL    string
	Protocol   string // "ollama" (default) or "openai"
	APIKey     string
	HTTPClient *http.Client
}

// ModelInfo describes an available AI model.
type ModelInfo = domain.ModelInfo

// ChatMessage represents a single message in a conversation.
type ChatMessage = domain.ChatMessage

var _ domain.AIGateway = (*Client)(nil)

// NewClient creates a new Ollama API client with sensible timeouts.
func NewClient(baseURL string) *Client {
	return NewClientWithConfig(baseURL, "ollama", "")
}

// NewClientWithConfig creates an AI gateway client configured for either "ollama" or "openai" protocol.
func NewClientWithConfig(baseURL, protocol, apiKey string) *Client {
	cleanURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	proto := strings.ToLower(strings.TrimSpace(protocol))
	if proto == "" {
		if strings.HasSuffix(cleanURL, "/v1") {
			proto = "openai"
		} else {
			proto = "ollama"
		}
	}
	return &Client{
		BaseURL:  cleanURL,
		Protocol: proto,
		APIKey:   strings.TrimSpace(apiKey),
		HTTPClient: &http.Client{
			Timeout: 180 * time.Second,
		},
	}
}

func (c *Client) isOpenAI() bool {
	return strings.EqualFold(strings.TrimSpace(c.Protocol), "openai")
}

func (c *Client) openAIEndpoint(path string) string {
	base := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if u, err := url.Parse(base); err == nil {
		cleanPath := strings.TrimRight(u.Path, "/")
		if cleanPath == "" {
			return base + "/v1" + path
		}
	}
	return base + path
}

func (c *Client) setHeaders(req *http.Request, hasBody bool) {
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

// ListModels returns the list of models currently available on the configured AI provider.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if c.isOpenAI() {
		return c.listModelsOpenAI(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, false)
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

func (c *Client) listModelsOpenAI(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.openAIEndpoint("/models"), nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, false)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai /models returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10*1024*1024)).Decode(&payload); err != nil {
		return nil, err
	}
	models := make([]ModelInfo, 0, len(payload.Data))
	for _, item := range payload.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		models = append(models, ModelInfo{Name: item.ID})
	}
	return models, nil
}

// EmbedBatch generates float32 vector embeddings for a batch of input texts.
func (c *Client) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if c.isOpenAI() {
		return c.embedBatchOpenAI(ctx, model, texts)
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
	c.setHeaders(req, true)

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

func (c *Client) embedBatchOpenAI(ctx context.Context, model string, texts []string) ([][]float32, error) {
	reqBody := map[string]any{
		"model": model,
		"input": texts,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.openAIEndpoint("/embeddings"), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, true)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai /embeddings returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var result struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 50*1024*1024)).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Data) != len(texts) {
		return nil, fmt.Errorf("openai /embeddings returned %d embeddings for %d inputs", len(result.Data), len(texts))
	}

	out := make([][]float32, len(texts))
	used := make([]bool, len(texts))
	sequentialFallback := false
	for _, item := range result.Data {
		if len(item.Embedding) == 0 {
			return nil, errors.New("empty embedding returned from openai endpoint")
		}
		if item.Index < 0 || item.Index >= len(texts) || used[item.Index] {
			sequentialFallback = true
			break
		}
		out[item.Index] = item.Embedding
		used[item.Index] = true
	}
	if sequentialFallback {
		for i, item := range result.Data {
			if len(item.Embedding) == 0 {
				return nil, errors.New("empty embedding returned from openai endpoint")
			}
			out[i] = item.Embedding
		}
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
	c.setHeaders(req, true)

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

// Chat sends a non-streaming chat completion request to Ollama or an OpenAI-compatible API.
func (c *Client) Chat(ctx context.Context, model string, messages []ChatMessage) (string, error) {
	if c.isOpenAI() {
		return c.chatOpenAI(ctx, model, messages)
	}
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
	c.setHeaders(req, true)

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

func (c *Client) chatOpenAI(ctx context.Context, model string, messages []ChatMessage) (string, error) {
	reqBody := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": 0.15,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.openAIEndpoint("/chat/completions"), bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	c.setHeaders(req, true)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("openai /chat/completions returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var result struct {
		Choices []struct {
			Message ChatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 20*1024*1024)).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", errors.New("openai /chat/completions returned no choices")
	}
	return result.Choices[0].Message.Content, nil
}
