package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	elastic "github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
)

// Client wraps the official elasticsearch client.
type Client struct {
	raw *elastic.Client
}

// IndexInfo represents metadata returned from _cat/indices.
type IndexInfo struct {
	Name       string
	Health     string
	Status     string
	DocsCount  int64
	StoreSize  string
	StoreBytes int64
}

// Document holds the minimal fields needed by the TUI.
type Document struct {
	ID     string
	Source map[string]any
}

// SearchResult wraps a set of documents returned from a search.
type SearchResult struct {
	Documents []Document
	Took      time.Duration
}

// ListFields returns flattened field names for a given index.
func (c *Client) ListFields(ctx context.Context, index string) ([]string, error) {
	res, err := c.raw.Indices.GetMapping(
		c.raw.Indices.GetMapping.WithContext(ctx),
		c.raw.Indices.GetMapping.WithIndex([]string{index}...),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("fields %s: %s", index, body)
	}

	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, err
	}

	fieldSet := make(map[string]struct{})
	for _, data := range decoded {
		idxMap, ok := data.(map[string]any)
		if !ok {
			continue
		}
		mappings, ok := idxMap["mappings"].(map[string]any)
		if !ok {
			continue
		}
		collectMappingFields("", mappings, fieldSet)
	}

	fields := make([]string, 0, len(fieldSet))
	for field := range fieldSet {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields, nil
}

// NewClientFromEnv builds a client using ELASTICSEARCH_* env variables.
func NewClientFromEnv() (*Client, error) {
	address := strings.TrimSpace(os.Getenv("ELASTICSEARCH_URL"))
	if address == "" {
		address = "http://localhost:9200"
	}

	cfg := elastic.Config{
		Addresses: []string{address},
		Transport: &http.Transport{
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	if apiKey := strings.TrimSpace(os.Getenv("ELASTICSEARCH_API_KEY")); apiKey != "" {
		cfg.APIKey = apiKey
	} else {
		cfg.Username = os.Getenv("ELASTICSEARCH_USERNAME")
		cfg.Password = os.Getenv("ELASTICSEARCH_PASSWORD")
	}

	client, err := elastic.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{raw: client}, nil
}

// ListIndices returns details for all indices visible to the user.
func (c *Client) ListIndices(ctx context.Context) ([]IndexInfo, error) {
	res, err := c.raw.Cat.Indices(
		c.raw.Cat.Indices.WithContext(ctx),
		c.raw.Cat.Indices.WithFormat("json"),
		c.raw.Cat.Indices.WithBytes("b"),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("list indices: %s", body)
	}

	var payload []struct {
		Health    string `json:"health"`
		Status    string `json:"status"`
		Index     string `json:"index"`
		DocsCount string `json:"docs.count"`
		StoreSize string `json:"store.size"`
	}

	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, err
	}

	out := make([]IndexInfo, 0, len(payload))
	for _, item := range payload {
		count, _ := strconv.ParseInt(item.DocsCount, 10, 64)
		bytes := parseStoreSize(item.StoreSize)
		out = append(out, IndexInfo{
			Name:       item.Index,
			Health:     item.Health,
			Status:     item.Status,
			DocsCount:  count,
			StoreSize:  item.StoreSize,
			StoreBytes: bytes,
		})
	}

	return out, nil
}

func parseStoreSize(value string) int64 {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0
	}
	if bytes, err := strconv.ParseInt(value, 10, 64); err == nil {
		return bytes
	}
	type unit struct {
		suffix string
		factor float64
	}
	units := []unit{
		{"pb", 1 << 50},
		{"tb", 1 << 40},
		{"gb", 1 << 30},
		{"mb", 1 << 20},
		{"kb", 1 << 10},
		{"b", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(value, u.suffix) {
			num := strings.TrimSpace(value[:len(value)-len(u.suffix)])
			if f, err := strconv.ParseFloat(num, 64); err == nil {
				return int64(f * u.factor)
			}
		}
	}
	return 0
}

func collectMappingFields(prefix string, node map[string]any, out map[string]struct{}) {
	if node == nil {
		return
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for key, raw := range props {
			field := key
			if prefix != "" {
				field = prefix + "." + key
			}
			out[field] = struct{}{}
			if child, ok := raw.(map[string]any); ok {
				collectMappingFields(field, child, out)
			}
		}
	}
	if fields, ok := node["fields"].(map[string]any); ok {
		for key, raw := range fields {
			field := key
			if prefix != "" {
				field = prefix + "." + key
			}
			out[field] = struct{}{}
			if child, ok := raw.(map[string]any); ok {
				collectMappingFields(field, child, out)
			}
		}
	}
}

// Search fetches a page of documents for a given index.
func (c *Client) Search(ctx context.Context, index, query string, size int) (*SearchResult, error) {
	if size <= 0 {
		size = 20
	}

	body := map[string]any{
		"size": size,
	}
	if query == "" {
		body["query"] = map[string]any{"match_all": map[string]any{}}
	} else {
		body["query"] = map[string]any{"query_string": map[string]any{"query": query}}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	res, err := c.raw.Search(
		c.raw.Search.WithContext(ctx),
		c.raw.Search.WithIndex(index),
		c.raw.Search.WithBody(bytes.NewReader(payload)),
		c.raw.Search.WithTrackTotalHits(false),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("search %s: %s", index, body)
	}

	var decoded struct {
		Took int64 `json:"took"`
		Hits struct {
			Hits []struct {
				ID     string          `json:"_id"`
				Source json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}

	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, err
	}

	docs := make([]Document, 0, len(decoded.Hits.Hits))
	for _, hit := range decoded.Hits.Hits {
		doc := Document{ID: hit.ID}
		if len(hit.Source) > 0 {
			if err := json.Unmarshal(hit.Source, &doc.Source); err != nil {
				doc.Source = map[string]any{"_source": string(hit.Source)}
			}
		}
		docs = append(docs, doc)
	}

	took := time.Duration(decoded.Took) * time.Millisecond
	if took == 0 {
		took = time.Since(start)
	}

	return &SearchResult{Documents: docs, Took: took}, nil
}

// DeleteDoc removes a document from an index.
func (c *Client) DeleteDoc(ctx context.Context, index, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("document id required")
	}

	res, err := c.raw.Delete(index, id, c.raw.Delete.WithContext(ctx))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("delete doc: %s", body)
	}
	return nil
}

// CreateDoc indexes a document and returns the id.
func (c *Client) CreateDoc(ctx context.Context, index, id string, body []byte) (string, error) {
	if !json.Valid(body) {
		return "", fmt.Errorf("body must be valid JSON")
	}

	opts := []func(*esapi.IndexRequest){c.raw.Index.WithContext(ctx)}
	if strings.TrimSpace(id) != "" {
		opts = append(opts, c.raw.Index.WithDocumentID(id))
	}

	res, err := c.raw.Index(index, bytes.NewReader(body), opts...)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.IsError() {
		raw, _ := io.ReadAll(res.Body)
		return "", fmt.Errorf("create doc: %s", raw)
	}

	var decoded struct {
		ID string `json:"_id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return "", err
	}
	return decoded.ID, nil
}

// Refresh ensures the latest changes are searchable.
func (c *Client) Refresh(ctx context.Context, index string) error {
	res, err := c.raw.Indices.Refresh(
		c.raw.Indices.Refresh.WithContext(ctx),
		c.raw.Indices.Refresh.WithIndex([]string{index}...),
	)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("refresh index: %s", body)
	}
	return nil
}
