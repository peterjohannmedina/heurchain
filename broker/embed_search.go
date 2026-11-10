package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

type cachedEmbedding struct {
	vector  []float32
	expires time.Time
}
type embeddingCache struct {
	mu     sync.Mutex
	values map[[32]byte]cachedEmbedding
}

func (s *Server) embedSearchText(ctx context.Context, tenant, text string) ([]float32, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.config == nil || s.config.Embed.QueryCacheEntries <= 0 {
		v, e := s.embedText(ctx, text)
		return v, false, e
	}
	c := s.config.Embed
	if !c.StrictInput || c.ModelRevision == "" || c.QueryCacheTTLSeconds <= 0 {
		return nil, false, fmt.Errorf("unsafe embedding cache configuration")
	}
	cfg := s.getEmbedConfig()
	identity, _ := json.Marshal([]string{tenant, s.activeEmbedModel(), cfg.OllamaURL, cfg.OllamaModel, cfg.OpenAIURL, cfg.OpenAIModel, c.ModelRevision, text})
	key := sha256.Sum256(identity)
	cache := &s.queryEmbeddings
	cache.mu.Lock()
	hit, ok := cache.values[key]
	if ok && time.Now().Before(hit.expires) {
		v := append([]float32(nil), hit.vector...)
		cache.mu.Unlock()
		return v, true, nil
	}
	cache.mu.Unlock()
	v, err := s.embedText(ctx, text)
	if err != nil {
		return nil, false, err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.values == nil {
		cache.values = map[[32]byte]cachedEmbedding{}
	}
	now := time.Now()
	for k, entry := range cache.values {
		if !now.Before(entry.expires) {
			delete(cache.values, k)
		}
	}
	if len(cache.values) >= c.QueryCacheEntries {
		var oldest [32]byte
		var expiry time.Time
		for k, entry := range cache.values {
			if expiry.IsZero() || entry.expires.Before(expiry) {
				oldest = k
				expiry = entry.expires
			}
		}
		delete(cache.values, oldest)
	}
	cache.values[key] = cachedEmbedding{append([]float32(nil), v...), now.Add(time.Duration(c.QueryCacheTTLSeconds) * time.Second)}
	return v, false, nil
}

// /api/embed rejects overlength input when truncate=false. Never retry with
// truncation or another model: dimension equality does not imply compatibility.
func embedViaOllamaStrict(ctx context.Context, base, model, text string) ([]float32, error) {
	payload, _ := json.Marshal(map[string]interface{}{"model": model, "input": text, "truncate": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ollamaHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding provider rejected input (HTTP %d); check model/context limits", resp.StatusCode)
	}
	var result struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&result); err != nil {
		return nil, err
	}
	if result.Model != "" && result.Model != model {
		return nil, fmt.Errorf("embedding provider returned unexpected model")
	}
	if len(result.Embeddings) != 1 || len(result.Embeddings[0]) == 0 || len(result.Embeddings[0]) > 4096 {
		return nil, fmt.Errorf("invalid embedding response shape")
	}
	for _, v := range result.Embeddings[0] {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("nonfinite embedding")
		}
	}
	return result.Embeddings[0], nil
}
