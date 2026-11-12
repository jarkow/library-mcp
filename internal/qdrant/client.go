package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"library-mcp/internal/domain"
	"library-mcp/internal/epub"
)

// SparseVector represents a Qdrant sparse vector of sorted token hash indices and TF weights.
type SparseVector = domain.SparseVector

// ScoredPoint represents a matched Qdrant child chunk point and its payload.
type ScoredPoint = domain.ScoredPoint

var _ domain.VectorIndex = (*Client)(nil)

// Client communicates with Qdrant's HTTP REST & Universal Query API.
type Client struct {
	BaseURL    string
	Collection string
	HTTPClient *http.Client
}

// NewClient initializes a Qdrant client for the given baseURL and collection name.
func NewClient(baseURL, collection string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Collection: collection,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// HealthCheck checks whether the Qdrant server is reachable and healthy.
func (c *Client) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qdrant healthz returned status %d", resp.StatusCode)
	}
	return nil
}

// EnsureCollection creates the hybrid (dense + sparse IDF) collection and payload indexes if they do not exist.
func (c *Client) EnsureCollection(ctx context.Context, denseDim int) error {
	checkURL := fmt.Sprintf("%s/collections/%s", c.BaseURL, c.Collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	if denseDim <= 0 {
		denseDim = 1024 // Default dimension for bge-m3
	}

	createBody := map[string]any{
		"vectors": map[string]any{
			"dense": map[string]any{
				"size":     denseDim,
				"distance": "Cosine",
			},
		},
		"sparse_vectors": map[string]any{
			"sparse": map[string]any{
				"modifier": "idf",
			},
		},
	}
	if err := c.doJSON(ctx, http.MethodPut, checkURL, createBody, nil); err != nil {
		return fmt.Errorf("failed to create qdrant collection: %w", err)
	}

	// Create filterable payload indexes for fast metadata filtering
	indexFields := []struct {
		field  string
		schema string
	}{
		{"book_id", "keyword"},
		{"parent_id", "keyword"},
		{"chapter_id", "keyword"},
		{"book_title", "text"},
	}
	indexURL := fmt.Sprintf("%s/collections/%s/index?wait=true", c.BaseURL, c.Collection)
	for _, idx := range indexFields {
		_ = c.doJSON(ctx, http.MethodPut, indexURL, map[string]any{
			"field_name":   idx.field,
			"field_schema": idx.schema,
		}, nil)
	}

	return nil
}

// UpsertChunks upserts child chunks into Qdrant in batches with both dense and sparse vectors.
func (c *Client) UpsertChunks(ctx context.Context, chunks []epub.ParsedChunk, denseVectors [][]float32) error {
	if len(chunks) == 0 {
		return nil
	}

	denseDim := 0
	for _, v := range denseVectors {
		if len(v) > 0 {
			denseDim = len(v)
			break
		}
	}
	if err := c.EnsureCollection(ctx, denseDim); err != nil {
		return err
	}

	const batchSize = 64
	upsertURL := fmt.Sprintf("%s/collections/%s/points?wait=true", c.BaseURL, c.Collection)

	for start := 0; start < len(chunks); start += batchSize {
		end := start + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}

		points := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			ck := chunks[i]
			sparseVec := BuildSparseVector(ck.Text)

			vectorsMap := map[string]any{
				"sparse": sparseVec,
			}
			if i < len(denseVectors) && len(denseVectors[i]) > 0 {
				vectorsMap["dense"] = denseVectors[i]
			}

			pointID := ck.PointUUID
			if pointID == "" {
				pointID = epub.DeterministicUUID(ck.ID)
			}

			points = append(points, map[string]any{
				"id":      pointID,
				"vector":  vectorsMap,
				"payload": ck,
			})
		}

		body := map[string]any{
			"points": points,
		}
		if err := c.doJSON(ctx, http.MethodPut, upsertURL, body, nil); err != nil {
			return fmt.Errorf("qdrant batch upsert failed: %w", err)
		}
	}

	return nil
}

// DeleteByBookID deletes all child chunk points belonging to bookID from Qdrant.
func (c *Client) DeleteByBookID(ctx context.Context, bookID string) error {
	delURL := fmt.Sprintf("%s/collections/%s/points/delete?wait=true", c.BaseURL, c.Collection)
	body := map[string]any{
		"filter": map[string]any{
			"must": []map[string]any{
				{
					"key": "book_id",
					"match": map[string]any{
						"value": bookID,
					},
				},
			},
		},
	}
	return c.doJSON(ctx, http.MethodPost, delURL, body, nil)
}

// QueryHybrid executes Qdrant's Universal Query API (/points/query) with Dense + Sparse RRF fusion.
func (c *Client) QueryHybrid(
	ctx context.Context,
	queryText string,
	queryDense []float32,
	mode string,
	bookFilter string,
	limit int,
) ([]ScoredPoint, error) {
	if limit <= 0 {
		limit = 10
	}
	querySparse := BuildSparseVector(queryText)
	filterObj := buildBookFilter(bookFilter)

	var reqBody map[string]any
	prefetchLimit := limit * 3
	if prefetchLimit < 24 {
		prefetchLimit = 24
	}

	hasDense := len(queryDense) > 0
	hasSparse := len(querySparse.Indices) > 0

	switch {
	case mode == "semantic" && hasDense:
		reqBody = map[string]any{
			"query":        queryDense,
			"using":        "dense",
			"limit":        limit,
			"with_payload": true,
		}
	case mode == "keyword" || (!hasDense && hasSparse):
		reqBody = map[string]any{
			"query":        querySparse,
			"using":        "sparse",
			"limit":        limit,
			"with_payload": true,
		}
	case hasDense && hasSparse:
		// Server-side Reciprocal Rank Fusion (RRF) across Dense + Sparse vectors in Qdrant
		p1 := map[string]any{
			"query": queryDense,
			"using": "dense",
			"limit": prefetchLimit,
		}
		p2 := map[string]any{
			"query": querySparse,
			"using": "sparse",
			"limit": prefetchLimit,
		}
		if filterObj != nil {
			p1["filter"] = filterObj
			p2["filter"] = filterObj
		}
		reqBody = map[string]any{
			"prefetch": []map[string]any{p1, p2},
			"query": map[string]any{
				"fusion": "rrf",
			},
			"limit":        limit,
			"with_payload": true,
		}
	case hasDense:
		reqBody = map[string]any{
			"query":        queryDense,
			"using":        "dense",
			"limit":        limit,
			"with_payload": true,
		}
	default:
		return nil, nil
	}

	if filterObj != nil {
		reqBody["filter"] = filterObj
	}

	queryURL := fmt.Sprintf("%s/collections/%s/points/query", c.BaseURL, c.Collection)
	var rawResp struct {
		Result struct {
			Points []ScoredPoint `json:"points"`
		} `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodPost, queryURL, reqBody, &rawResp); err != nil {
		return nil, err
	}
	return rawResp.Result.Points, nil
}

func buildBookFilter(bookFilter string) map[string]any {
	f := strings.TrimSpace(bookFilter)
	if f == "" {
		return nil
	}
	return map[string]any{
		"should": []map[string]any{
			{
				"key": "book_id",
				"match": map[string]any{
					"value": f,
				},
			},
			{
				"key": "book_title",
				"match": map[string]any{
					"text": f,
				},
			},
		},
	}
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, reqPayload any, respTarget any) error {
	var bodyReader io.Reader
	if reqPayload != nil {
		b, err := json.Marshal(reqPayload)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return err
	}
	if reqPayload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("qdrant %s %s returned %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(errBytes)))
	}

	if respTarget != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 25*1024*1024)).Decode(respTarget)
	}
	return nil
}

var sparseStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true,
	"with": true, "from": true, "are": true, "was": true, "were": true,
	"have": true, "has": true, "had": true, "not": true, "but": true,
	"you": true, "your": true, "can": true, "will": true, "into": true,
}

// BuildSparseVector tokenizes technical text and builds a sorted Qdrant SparseVector using BM25 TF saturation.
func BuildSparseVector(text string) SparseVector {
	lower := strings.ToLower(text)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '+' || r == '#' || r == '.')
	})

	tfMap := make(map[uint32]int)
	docLen := 0
	for _, tok := range fields {
		tok = strings.Trim(tok, "-_.")
		if len(tok) < 2 || sparseStopWords[tok] {
			continue
		}
		docLen++
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		idx := h.Sum32()
		tfMap[idx]++
	}

	if len(tfMap) == 0 {
		return SparseVector{Indices: []uint32{}, Values: []float32{}}
	}

	indices := make([]uint32, 0, len(tfMap))
	for idx := range tfMap {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool {
		return indices[i] < indices[j]
	})

	const (
		k1        = 1.5
		b         = 0.75
		avgDocLen = 90.0
	)
	dl := float64(docLen)
	values := make([]float32, len(indices))
	for i, idx := range indices {
		tf := float64(tfMap[idx])
		weight := (tf * (k1 + 1.0)) / (tf + k1*(1.0-b+b*(dl/avgDocLen)))
		values[i] = float32(weight)
	}

	return SparseVector{
		Indices: indices,
		Values:  values,
	}
}
