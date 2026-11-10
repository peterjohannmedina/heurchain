package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// EmbedConfig holds embedding provider settings.
type EmbedConfig struct {
	// Provider is an explicit override. Empty preserves the historical
	// Ollama-first selection; it never infers OpenAI merely from a key.
	Provider    string
	OllamaURL   string
	OllamaModel string
	OpenAIKey   string
	OpenAIModel string
	OpenAIURL   string // base URL for the OpenAI-compatible embeddings endpoint
}

func defaultEmbedConfig() EmbedConfig {
	return EmbedConfig{
		OllamaURL:   "http://localhost:11434",
		OllamaModel: "nomic-embed-text",
		OpenAIModel: "text-embedding-3-small",
		OpenAIURL:   "https://api.openai.com/v1/embeddings",
	}
}

var (
	ollamaHTTPClient = &http.Client{Timeout: 30 * time.Second}
	openaiHTTPClient = &http.Client{Timeout: 15 * time.Second}
)

// getEmbedConfig pulls embedding settings from the server's config.
func (s *Server) getEmbedConfig() EmbedConfig {
	ec := defaultEmbedConfig()
	if s.config != nil {
		if s.config.Embed.Provider != "" {
			ec.Provider = s.config.Embed.Provider
		}
		if s.config.Embed.OllamaURL != "" {
			ec.OllamaURL = s.config.Embed.OllamaURL
		}
		if s.config.Embed.OllamaModel != "" {
			ec.OllamaModel = s.config.Embed.OllamaModel
		}
		if s.config.Embed.OpenAIKey != "" {
			ec.OpenAIKey = s.config.Embed.OpenAIKey
		}
		if s.config.Embed.OpenAIModel != "" {
			ec.OpenAIModel = s.config.Embed.OpenAIModel
		}
		if s.config.Embed.OpenAIURL != "" {
			ec.OpenAIURL = s.config.Embed.OpenAIURL
		}
	}
	return ec
}

// activeEmbedModel returns an unambiguous provider/model provenance tag.
func (s *Server) activeEmbedModel() string {
	cfg := s.getEmbedConfig()
	switch embedProvider(cfg) {
	case "ollama":
		return embeddingProvenance("ollama", cfg.OllamaModel)
	case "openai":
		return embeddingProvenance("openai", cfg.OpenAIModel)
	default:
		return ""
	}
}

// embedProvider makes selection deterministic. An explicit provider is the
// only way to override the historical Ollama-first choice. In particular, a
// configured/default Ollama URL remains primary even when OpenAI credentials
// are present; a failed Ollama request is never retried against OpenAI because
// the resulting vectors would not share a compatible space.
func embedProvider(cfg EmbedConfig) string {
	if provider := strings.ToLower(strings.TrimSpace(cfg.Provider)); provider != "" {
		return provider
	}
	if cfg.OllamaURL != "" {
		return "ollama"
	}
	if cfg.OpenAIKey != "" {
		return "openai"
	}
	return ""
}

// embeddingProvenance is stored in the existing embedding_model field. New
// server-generated vectors use provider:model (for example,
// "ollama:nomic-embed-text"). Existing records may contain only the model
// name; that legacy metadata remains readable and is not rewritten merely by
// reading it. New writes are qualified so incompatible providers cannot be
// mistaken for the same vector space.
func embeddingProvenance(provider, model string) string { return provider + ":" + model }

// legacyEmbedModelAliases holds unqualified embedding_model tags that name the
// active model's vector space (config embed.legacy_model_aliases, or inferred
// by loadConfig). It is set once at startup, before the server takes traffic.
var legacyEmbedModelAliases []string

func setLegacyEmbedModelAliases(aliases []string) {
	cleaned := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias = strings.TrimSpace(alias); alias != "" && !slices.Contains(cleaned, alias) {
			cleaned = append(cleaned, alias)
		}
	}
	legacyEmbedModelAliases = cleaned
}

// compatibleEmbedModels lists the stored embedding_model tags that share the
// query model's vector space: the provider:model tag itself, the bare model
// name older brokers wrote, and any legacy aliases.
func compatibleEmbedModels(model string) []string {
	compatible := []string{model}
	if _, short, ok := strings.Cut(model, ":"); ok {
		compatible = append(compatible, short)
	}
	for _, alias := range legacyEmbedModelAliases {
		if !slices.Contains(compatible, alias) {
			compatible = append(compatible, alias)
		}
	}
	return compatible
}

// embedText generates an embedding for text using the configured provider.
func (s *Server) embedText(ctx context.Context, text string) ([]float32, error) {
	emb, _, err := s.embedTextWithModel(ctx, text)
	return emb, err
}

func (s *Server) embedTextWithModel(ctx context.Context, text string) ([]float32, string, error) {
	// /search runs under a 25-second handler budget (handleSearch) and may spend
	// the remaining time on Qdrant/BM25. Keep embedding itself to at most 15 seconds.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	strict := s.config != nil && s.config.Embed.StrictInput
	if strict && len(text) > maxEmbedTextBytes {
		return nil, "", fmt.Errorf("embedding input exceeds %d bytes; split into passages", maxEmbedTextBytes)
	}
	// Bound what reaches the embedding backend. Deliberately a truncation and
	// not a 400: /store persists the full text and callers legitimately store
	// large documents, so rejecting them would be a breaking change. Nothing
	// Model limits vary; this legacy byte cap can discard semantic content.
	// Prefer strict_input and passage ingestion. This also stops one tenant
	// from pushing ~500 KB per request at full RPM into the shared embedder.
	if len(text) > maxEmbedTextBytes {
		cut := maxEmbedTextBytes
		// Back off to a rune boundary so the truncation can't emit a partial
		// multi-byte character into the provider payload.
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	cfg := s.getEmbedConfig()
	provider := embedProvider(cfg)

	var (
		emb   []float32
		err   error
		model string
	)
	switch provider {
	case "ollama":
		if cfg.OllamaURL == "" || cfg.OllamaModel == "" {
			return nil, "", fmt.Errorf("ollama embedding provider is not configured")
		}
		if strict {
			emb, err = embedViaOllamaStrict(ctx, cfg.OllamaURL, cfg.OllamaModel, text)
		} else {
			emb, err = embedViaOllama(ctx, cfg.OllamaURL, cfg.OllamaModel, text)
		}
		model = embeddingProvenance("ollama", cfg.OllamaModel)
	case "openai":
		if cfg.OpenAIKey == "" || cfg.OpenAIModel == "" || cfg.OpenAIURL == "" {
			return nil, "", fmt.Errorf("openai embedding provider is not configured")
		}
		emb, err = embedViaOpenAI(ctx, cfg.OpenAIURL, cfg.OpenAIKey, cfg.OpenAIModel, text)
		model = embeddingProvenance("openai", cfg.OpenAIModel)
	default:
		if provider == "" {
			return nil, "", fmt.Errorf("no embedding provider configured")
		}
		return nil, "", fmt.Errorf("unknown embedding provider %q", provider)
	}
	if err != nil {
		return nil, "", err
	}
	if err := validateGeneratedEmbedding(emb); err != nil {
		return nil, "", fmt.Errorf("%s returned invalid embedding: %w", provider, err)
	}
	return emb, model, nil
}

func validateGeneratedEmbedding(embedding []float32) error {
	if len(embedding) == 0 {
		return fmt.Errorf("empty vector")
	}
	if len(embedding) > 4096 {
		return fmt.Errorf("%d dimensions exceeds 4096", len(embedding))
	}
	for _, value := range embedding {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("non-finite value")
		}
	}
	return nil
}

// embedViaOllama calls the local Ollama HTTP API.
func embedViaOllama(ctx context.Context, ollamaURL, model, text string) ([]float32, error) {
	payload, err := json.Marshal(map[string]string{
		"model":  model,
		"prompt": text,
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}

	url := strings.TrimSuffix(ollamaURL, "/") + "/api/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ollamaHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusNotFound:
			msg = "model not found"
		case http.StatusUnauthorized:
			msg = "unauthorized"
		}
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("ollama: %s", msg)
	}

	var result struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	return float64sToFloat32s(result.Embedding), nil
}

// embedViaOpenAI calls an OpenAI-compatible embeddings endpoint.
// endpointURL is the full URL (e.g., https://api.openai.com/v1/embeddings or a local TEI instance).
func embedViaOpenAI(ctx context.Context, endpointURL, apiKey, model, text string) ([]float32, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"model": model,
		"input": text,
	})
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := openaiHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := extractOpenAIError(body)
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("openai: %s", msg)
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("openai: empty response data")
	}
	return float64sToFloat32s(result.Data[0].Embedding), nil
}

func float64sToFloat32s(in []float64) []float32 {
	out := make([]float32, len(in))
	for i, v := range in {
		out[i] = float32(v)
	}
	return out
}

func extractOpenAIError(body []byte) string {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	msg := strings.TrimSpace(string(body))
	if msg != "" {
		return msg
	}
	return ""
}

// handleEmbed POST /embed   body: {text string}  → {embedding []float32}
func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Same auth as /store and /query: embedding burns compute (and may call a
	// paid upstream), so an unauthenticated surface here is a cost/abuse hole.
	apiKey := r.Header.Get(s.config.Auth.APIKeyHeader)
	tenantID := r.Header.Get(s.config.Auth.TenantHeader)
	if apiKey == "" {
		jsonError(w, "missing api key", http.StatusUnauthorized)
		return
	}
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant id", http.StatusBadRequest)
		return
	}
	if _, err := validateAPIKey(r.Context(), s.rdb, s.db, tenantID, apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Text string `json:"text"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.Text == "" {
		jsonError(w, "text is required", http.StatusBadRequest)
		return
	}

	embedding, err := s.embedText(r.Context(), req.Text)
	if err != nil {
		log.Printf("embed error: %v", err)
		jsonError(w, "embedding failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"embedding": embedding,
	})
}
