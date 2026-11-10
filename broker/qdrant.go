package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// maxQdrantCandidates bounds the vector-side candidate pool. Unlike `limit`,
// which caps returned results, this value bounds Qdrant's candidate work.
// Qdrant does not cap a client-supplied limit.
const maxQdrantCandidates = 200

// QdrantClient is a thin REST client for the Qdrant vector database.
type QdrantClient struct {
	baseURL    string
	httpClient *http.Client
	vectorSize int
	// apiKey is sent as the `api-key` header when non-empty. Qdrant itself
	// enforces no authorization by default, and per-tenant separation here is
	// only the collection name the broker chooses to address — so anything that
	// reaches the Qdrant port (a broker bug, an SSRF, a sibling process) has
	// unauthenticated cross-tenant read/write/delete over every collection.
	// This is the layer that makes isolation survive a fault in the broker.
	// Empty = no header, preserving existing behaviour for deployments that
	// have not set QDRANT__SERVICE__API_KEY yet.
	apiKey string
}

// setAuth applies the api-key header when one is configured.
func (q *QdrantClient) setAuth(req *http.Request) {
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}
}

// QdrantPoint represents a single vector point with optional payload.
type QdrantPoint struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector"`
	Payload map[string]interface{} `json:"payload"`
}

// QdrantSearchResult is a single nearest-neighbour returned by Qdrant.
type QdrantSearchResult struct {
	ID      string                 `json:"id"`
	Score   float32                `json:"score"`
	Payload map[string]interface{} `json:"payload"`
}

// NewQdrantClient creates a client. vectorSize is the embedding dimension (e.g. 768).
//
// The API key comes from QDRANT_API_KEY rather than broker.yaml: it is a secret,
// and the YAML loader uses KnownFields(true), so adding a key there would make
// older config files fail to boot. This matches how HEURCHAIN_KEK is already
// handled. Unset = no header, so existing deployments keep working
// until QDRANT__SERVICE__API_KEY is configured on the Qdrant side.
func NewQdrantClient(baseURL string, vectorSize int) *QdrantClient {
	return &QdrantClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		vectorSize: vectorSize,
		apiKey:     strings.TrimSpace(os.Getenv("QDRANT_API_KEY")),
	}
}

// qdrantClientFromEnvironment configures the endpoint independently of the
// strict YAML schema. Credentials remain in QDRANT_API_KEY, not in the URL.
func qdrantClientFromEnvironment() (*QdrantClient, error) {
	endpoint := strings.TrimSpace(os.Getenv("QDRANT_URL"))
	if endpoint == "" {
		endpoint = "http://localhost:6333"
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("QDRANT_URL must be an absolute HTTP(S) origin without credentials, query, fragment, or path")
	}
	return NewQdrantClient(strings.TrimRight(endpoint, "/"), defaultVectorSize), nil
}

// EnsureCollection creates the collection if it does not exist.
// One collection per tenant: collection name = tenantID.
// Distance metric: Cosine.
func (q *QdrantClient) EnsureCollection(ctx context.Context, tenantID string, dimensions ...int) error {
	vectorSize := q.vectorSize
	if len(dimensions) > 0 {
		vectorSize = dimensions[0]
	}
	if vectorSize <= 0 {
		return fmt.Errorf("invalid embedding dimension %d", vectorSize)
	}
	// Never mutate the shared client's default or resize an existing collection.
	// Qdrant rejects incompatible upserts; callers report incomplete indexing.
	exists, err := q.CollectionExists(ctx, tenantID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	url := fmt.Sprintf("%s/collections/%s", q.baseURL, tenantID)
	body := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     vectorSize,
			"distance": "Cosine",
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal collection config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	q.setAuth(req)

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create collection failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

// UpsertPoint inserts or updates a single point.
func (q *QdrantClient) UpsertPoint(ctx context.Context, tenantID string, point QdrantPoint) error {
	// Preserve the public document identity; Qdrant only accepts UUID/numeric IDs.
	// Copy the payload so the caller's map is not mutated.
	payload := make(map[string]interface{}, len(point.Payload)+1)
	for key, value := range point.Payload {
		payload[key] = value
	}
	payload["source_id"] = point.ID
	point.Payload = payload
	ensureUUID(&point.ID)

	url := fmt.Sprintf("%s/collections/%s/points?wait=true", q.baseURL, tenantID)
	body := map[string]interface{}{
		"points": []QdrantPoint{point},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal point: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	q.setAuth(req)

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upsert point: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upsert point failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

// Search returns up to limit nearest neighbours.
func (q *QdrantClient) Search(ctx context.Context, tenantID string, vector []float32, limit int, filters ...map[string]interface{}) ([]QdrantSearchResult, error) {
	url := fmt.Sprintf("%s/collections/%s/points/search", q.baseURL, tenantID)
	body := map[string]interface{}{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
	}
	if len(filters) > 0 && len(filters[0]) > 0 {
		body["filter"] = filters[0]
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	q.setAuth(req)

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search failed: %s: %s", resp.Status, string(b))
	}

	var wrapper struct {
		Result []QdrantSearchResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("decode search response: %w", err)
	}
	return wrapper.Result, nil
}

// DeletePoint removes a point by ID.
func (q *QdrantClient) DeletePoint(ctx context.Context, tenantID string, pointID string) error {
	ensureUUID(&pointID)

	url := fmt.Sprintf("%s/collections/%s/points/delete?wait=true", q.baseURL, tenantID)
	body := map[string]interface{}{
		"points": []string{pointID},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal delete request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	q.setAuth(req)

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete point: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		// Confirm the collection is absent rather than accepting an arbitrary
		// proxy/router 404 as evidence that the point was removed.
		if resp.StatusCode == http.StatusNotFound {
			exists, checkErr := q.CollectionExists(ctx, tenantID)
			if checkErr == nil && !exists {
				return nil
			}
		}
		return fmt.Errorf("delete point failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

// CollectionExists returns true if the collection exists in Qdrant.
func (q *QdrantClient) CollectionExists(ctx context.Context, tenantID string) (bool, error) {
	url := fmt.Sprintf("%s/collections/%s", q.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	q.setAuth(req)

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("check collection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	b, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("check collection failed: %s: %s", resp.Status, string(b))
}

// ensureUUID mutates id in-place. If it is not already a valid UUID it is
// replaced with a deterministic UUIDv5 derived from the original string.
func ensureUUID(id *string) {
	if parsed, err := uuid.Parse(*id); err == nil {
		*id = parsed.String()
		return
	}
	*id = uuid.NewSHA1(uuid.NameSpaceURL, []byte(*id)).String()
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	start := time.Now()
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

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

	budget := 25 * time.Second
	if s.config != nil && s.config.Server.Timeout > 1 && time.Duration(s.config.Server.Timeout-1)*time.Second < budget {
		budget = time.Duration(s.config.Server.Timeout-1) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	keyID, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey)
	if err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		RecallOptions
		Filters          *memoryFilters `json:"filters,omitempty"`
		AgentID          string         `json:"agent_id"`
		Tags             []string       `json:"tags"`
		Text             string         `json:"text"`
		Limit            int            `json:"limit"`
		QdrantCandidates int            `json:"qdrant_candidates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := req.Filters.validate(); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	if req.Filters != nil {
		ctx = context.WithValue(ctx, memoryFilterContextKey{}, req.Filters)
	}
	if req.Text == "" {
		jsonError(w, "text is required", http.StatusBadRequest)
		return
	}
	if err := validateTags(req.Tags); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// agent_id is persisted to usage_events and audit_log below and is later
	// rendered by provenance and operator views, so it must satisfy the same
	// charset every other write path enforces (/store handlers.go, /query
	// handlers.go, /vault/import vault.go, /agent-settings). This was the one
	// write path that let arbitrary text through into a rendered value.
	if req.AgentID != "" && !validID.MatchString(req.AgentID) {
		jsonError(w, "invalid agent_id", http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	if req.Limit > 50 {
		req.Limit = 50
	}
	if req.QdrantCandidates <= 0 {
		req.QdrantCandidates = 50
	}
	if req.QdrantCandidates > maxQdrantCandidates {
		req.QdrantCandidates = maxQdrantCandidates
	}
	if req.QdrantCandidates < req.Limit {
		req.QdrantCandidates = req.Limit
	}

	if err := req.RecallOptions.normalize(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	embedStart := time.Now()
	var queryVec []float32
	queryModel := ""
	cacheHit := false
	if req.Mode != "lexical" {
		queryVec, cacheHit, err = s.embedSearchText(ctx, tenantID, req.Text)
		queryModel = s.activeEmbedModel()
	}
	if err != nil {
		log.Printf("embed error tenant=%s: %v", tenantID, err)
		jsonError(w, "embedding failed", http.StatusInternalServerError)
		return
	}

	embedMS := time.Since(embedStart).Milliseconds()
	ctx = context.WithValue(ctx, recallModelContextKey{}, queryModel)
	retrievalStart := time.Now()
	results, diagnostics, err := runRecallSearchWithGraph(ctx, s.rdb, tenantID, req.Text, req.AgentID, req.Tags, queryVec, req.Limit, req.QdrantCandidates, &graphReadContext{db: s.db, enabled: graphTenantEnabled(tenantID)}, req.RecallOptions)
	diagnostics.EmbeddingCacheHit = cacheHit
	if req.Mode != "lexical" {
		diagnostics.EmbeddingModel = s.activeEmbedModel()
	}
	diagnostics.QueryID = uuid.NewString()
	diagnostics.EmbeddingMilliseconds = embedMS
	diagnostics.RetrievalMilliseconds = time.Since(retrievalStart).Milliseconds()
	if err != nil {
		log.Printf("search failed tenant=%s: %v", tenantID, err)
		jsonError(w, "search unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (tenant_id, event_type, vectors_stored, agent_id) VALUES (?, 'search', 0, ?)`,
		tenantID, nullableString(req.AgentID)); err != nil {
		log.Printf("usage_events write failed tenant=%s: %v", tenantID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (tenant_id, actor, action, ip_address, user_agent, success, agent_id) VALUES (?, ?, 'search', ?, ?, 1, ?)`,
		tenantID, fmt.Sprintf("%d", keyID), clientIP(r), auditUserAgent(r), nullableString(req.AgentID)); err != nil {
		log.Printf("audit_log write failed tenant=%s: %v", tenantID, err)
	}

	diagnostics.TotalMilliseconds = time.Since(start).Milliseconds()
	w.Header().Set("X-HeurChain-Query-ID", diagnostics.QueryID)
	respondJSON(w, map[string]interface{}{"results": results, "retrieval": diagnostics})
}

func semanticHitMatches(data map[string]string, payload map[string]interface{}, queryModel string) bool {
	model := data["embedding_model"]
	// Legacy tags contained only model names (or, on a deployment that never
	// reached Ollama, the Ollama model name; see loadConfig). Known different
	// models never compare.
	if queryModel != "" && model != "" && !slices.Contains(compatibleEmbedModels(queryModel), model) {
		return false
	}
	revision, _ := payload["index_revision"].(string)
	if revision != "" {
		return revision == semanticRevision(data)
	}
	return data["index_revision"] == "" && data["indexed_revision"] == ""
}

func validateSemanticHits(ctx context.Context, rdb *redis.Client, tenant, agent string, tags []string, model string, hits []QdrantSearchResult) ([]QdrantSearchResult, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	pipe := rdb.Pipeline()
	data := make([]*redis.MapStringStringCmd, len(hits))
	active := make([]*redis.BoolCmd, len(hits))
	forgotten := make([]*redis.IntCmd, len(hits))
	for i, hit := range hits {
		data[i] = pipe.HGetAll(ctx, vecKey(tenant, hit.ID))
		active[i] = pipe.SIsMember(ctx, vecSetKey(tenant), hit.ID)
		forgotten[i] = pipe.Exists(ctx, memoryTombstoneKey(tenant, hit.ID))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	filtered := hits[:0]
	for i, hit := range hits {
		d, err := data[i].Result()
		if err != nil {
			return nil, err
		}
		if len(d) == 0 || !active[i].Val() || tombstoneHidden(forgotten[i]) || (agent != "" && d["agent_id"] != agent) || !tagsIntersect(parseTags(d["tags"]), tags) || !semanticHitMatches(d, hit.Payload, model) {
			continue
		}
		filtered = append(filtered, hit)
	}
	return filtered, nil
}
