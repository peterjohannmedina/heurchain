package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const maxBodyBytes = 512 * 1024 // 512 KB — generous for a 4096-dim embedding (~65 KB)

const (
	maxTagsPerVector = 16
	maxTagLength     = 32

	// maxTopK matches the limit the discovery document already advertises.
	maxTopK = 100

	// maxEmbedTextBytes bounds text handed to the embedding backend. The 512 KB
	// body cap alone let one tenant push ~500 KB per request at its full RPM
	// into the single shared embedder, degrading latency for every tenant.
	// Note the "chunking" the discovery doc describes does not exist in code.
	maxEmbedTextBytes = 32 * 1024
)

// maxIdentifierLength bounds every caller-supplied identifier (tenant, doc,
// agent, session, silo). The length bound is part of validID itself so that all
// ~50 call sites inherit it — an identifier that passed the character class but
// had no length limit was an availability bug, not a cosmetic one: docIDs are
// embedded in Redis KEY NAMES once per unique term by updateBM25Index, so a
// 50 KB id in a 512 KB /store request expanded into gigabytes of key-name data
// built in Go heap before Exec, killing the process via RLIMIT_AS with an
// unrecoverable OOM. Keep the bound on the regex; do not loosen it per-site.
const maxIdentifierLength = 128

var validID = regexp.MustCompile(`^[a-zA-Z0-9\-_.]{1,128}$`)

// storeSessionID resolves the session a /store belongs to.
//
// The top-level field wins when both are present: it is the one the silo path
// already authenticates against, so honouring it keeps the session that
// authorises the write and the session the item is filed under the same one.
// The metadata form is the documented convention and is checked second.
//
// A metadata value is validated here rather than trusted: it arrives inside an
// opaque JSON blob, so unlike the top-level field it has never been checked,
// and it becomes part of a Redis key. Anything that is not a well-formed id is
// ignored — the store proceeds without a context entry rather than failing, and
// rather than composing a key out of caller-controlled text.
func storeSessionID(topLevel string, metadata json.RawMessage) string {
	if topLevel != "" {
		return topLevel
	}
	if len(metadata) == 0 {
		return ""
	}
	var meta struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return ""
	}
	if !validID.MatchString(meta.SessionID) {
		return ""
	}
	return meta.SessionID
}

var validTag = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// resolveNamespace validates the requested ACT-R decay tier and applies the
// default. An unrecognised namespace is rejected rather than accepted, because
// decayRate() would silently fall back to the default rate and quietly retier
// the caller's memories — the failure mode that made the entire legacy corpus
// vault-eligible when decay_default went unread.
func resolveNamespace(ns string) (string, error) {
	if ns == "" {
		return "notes", nil
	}
	if _, ok := consolidationCfg.DecayRates[ns]; !ok {
		return "", fmt.Errorf("unknown namespace %q (expected one of the configured decay tiers)", ns)
	}
	return ns, nil
}

// validateTags enforces the tag taxonomy rules. Returns the first violation.
// Tags are lowercase, alphanumeric with dash/underscore, max 32 chars, max 16
// per vector. An empty slice is valid (no tags).
func validateTags(tags []string) error {
	if len(tags) > maxTagsPerVector {
		return fmt.Errorf("too many tags (max %d)", maxTagsPerVector)
	}
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		if len(t) == 0 || len(t) > maxTagLength {
			return fmt.Errorf("invalid tag length: %q", t)
		}
		if !validTag.MatchString(t) {
			return fmt.Errorf("invalid tag %q (lowercase alphanumeric, dash, underscore; must start alphanumeric)", t)
		}
		if _, dup := seen[t]; dup {
			return fmt.Errorf("duplicate tag: %q", t)
		}
		seen[t] = struct{}{}
	}
	return nil
}

// Server holds dependencies for all HTTP handlers.
type Server struct {
	queryEmbeddings embeddingCache
	config          *Config
	rdb             *redis.Client
	db              *sql.DB
	crypto          *siloCrypto // nil = silo features dark (HEURCHAIN_KEK unset)
	attest          *attestor   // nil = attestation dark (HEURCHAIN_ATTEST_SEED unset)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Set before any WriteHeader: the degraded path writes 503 below, and a
	// header set afterwards would be ignored.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	degraded := false
	status := map[string]interface{}{
		"status":      "ok",
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"version":     brokerVersion,
		"commit":      brokerCommit,
		"sha256":      brokerArtifactSHA256(),
		"instance_id": brokerInstanceID(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		status["redis"] = "unreachable: " + err.Error()
		degraded = true
	} else {
		status["redis"] = "ok"
	}

	if err := s.db.Ping(); err != nil {
		status["sqlite"] = "unreachable: " + err.Error()
		degraded = true
	} else {
		status["sqlite"] = "ok"
	}

	if degraded {
		status["status"] = "degraded"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	respondJSON(w, status)
}

// ---------------------------------------------------------------------------
// Store — POST /store
// ---------------------------------------------------------------------------

func (s *Server) handleStore(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
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

	ctx := r.Context()
	keyID, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey)
	if err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		ID               string            `json:"id"`
		ExpectedRevision *int64            `json:"expected_revision,omitempty"`
		AgentID          string            `json:"agent_id"`
		Tags             []string          `json:"tags"`
		Embedding        []float32         `json:"embedding"`
		Metadata         json.RawMessage   `json:"metadata"`
		Text             string            `json:"text"`
		Namespace        string            `json:"namespace"`  // decay tier: ops | notes | self (default notes)
		SessionID        string            `json:"session_id"` // silo path: inherit silo/engagement from session
		Silo             string            `json:"silo"`       // silo path: explicit silo (direct, non-billable)
		Ontology         *ontologyEnvelope `json:"ontology"`   // optional heurchain-ontology/v1 assertion
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.SessionID != "" && !validID.MatchString(req.SessionID) {
		jsonError(w, "invalid session_id", http.StatusBadRequest)
		return
	}
	if req.Silo != "" && !validID.MatchString(req.Silo) {
		jsonError(w, "invalid silo", http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		req.ID = uuid.New().String()
	} else if !validID.MatchString(req.ID) {
		jsonError(w, "invalid id (alphanumeric, dash, dot, underscore only)", http.StatusBadRequest)
		return
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision < 0 {
		jsonError(w, "expected_revision must be nonnegative", 400)
		return
	}
	if req.AgentID != "" && !validID.MatchString(req.AgentID) {
		jsonError(w, "invalid agent_id (alphanumeric, dash, dot, underscore only)", http.StatusBadRequest)
		return
	}
	if err := validateTags(req.Tags); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	ns, err := resolveNamespace(req.Namespace)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Namespace = ns
	if req.Metadata == nil {
		req.Metadata = json.RawMessage(`{}`)
	}
	if err := recallMetadataValid(req.Metadata); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Validation runs before silo resolution, embeddings, indexing, or writes.
	// Reject mode must not spend model compute for a memory it will discard.
	ontologyFindings, err := s.validateOntology(
		ctx, tenantID, req.AgentID, req.SessionID, req.Text, req.Ontology,
	)
	if err != nil {
		jsonError(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if req.Metadata, err = attachOntologyMetadata(req.Metadata, req.Ontology); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve + enforce the silo write path before any storage work.
	var wc *writeContext
	if req.SessionID != "" || req.Silo != "" {
		if req.ExpectedRevision != nil {
			jsonError(w, "expected_revision is supported only for ordinary memory", http.StatusBadRequest)
			return
		}
		if s.crypto == nil {
			jsonError(w, "silo features not configured on this deployment", http.StatusServiceUnavailable)
			return
		}
		var err error
		wc, err = s.resolveWriteContext(ctx, tenantID, keyID, req.SessionID, req.Silo)
		if err != nil {
			if ee, ok := asEnforceError(err); ok {
				jsonError(w, "forbidden: "+ee.Code, http.StatusForbidden)
				return
			}
			log.Printf("silo resolve error tenant=%s: %v", tenantID, err)
			jsonError(w, "silo resolution failed", http.StatusInternalServerError)
			return
		}
	}
	embedModel := ""
	if len(req.Embedding) == 0 {
		if req.Text == "" {
			jsonError(w, "either embedding or text is required", http.StatusBadRequest)
			return
		}
		emb, model, err := s.embedTextWithModel(ctx, req.Text)
		if err != nil {
			log.Printf("auto-embed failed tenant=%s: %v", tenantID, err)
			jsonError(w, "embedding service unavailable", http.StatusServiceUnavailable)
			return
		}
		req.Embedding = emb
		// Record which model produced this server-side embedding so retrieval
		// can detect cross-model mixing later.
		embedModel = model
	}
	if len(req.Embedding) > 4096 {
		jsonError(w, "embedding too large (max 4096 dimensions)", http.StatusBadRequest)
		return
	}
	if err := validateGeneratedEmbedding(req.Embedding); err != nil {
		jsonError(w, "invalid embedding: "+err.Error(), http.StatusBadRequest)
		return
	}
	sv := storedVector{
		ExpectedRevision: req.ExpectedRevision,
		Receipt:          &memoryWriteReceipt{},
		ID:               req.ID,
		AgentID:          req.AgentID,
		Tags:             req.Tags,
		Embedding:        req.Embedding,
		Metadata:         req.Metadata,
		Text:             req.Text,
		Namespace:        req.Namespace,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		Model:            embedModel,
	}
	if checkpoint, _ := ctx.Value(checkpointWriteContextKey{}).(bool); checkpoint {
		sv.RecordKind = "checkpoint"
	}

	var siloID, engagementID interface{}
	var indexErr error
	if wc != nil {
		// Silo path: content (text + metadata) is sealed under the silo DEK
		// and lands in the silo's dedicated keyspace. No plaintext BM25 index
		// is built for encrypted content (vector-only retrieval in this
		// release — a blind index would otherwise leak terms in key names).
		if err := storeSiloVec(ctx, s.rdb, wc.DEK, tenantID, wc.SiloName, sv, req.Text); err != nil {
			log.Printf("silo store error tenant=%s silo=%s: %v", tenantID, wc.SiloName, err)
			jsonError(w, "storage failed", http.StatusInternalServerError)
			return
		}
		siloID = wc.SiloID
		engagementID = nullableString(wc.EngagementID)
	} else {
		var storeErr error
		storeErr, indexErr = s.storeWithIndexes(ctx, tenantID, sv)
		if storeErr != nil {
			if errors.Is(storeErr, errMemoryForgotten) || errors.Is(storeErr, errMemoryConflict) || errors.Is(storeErr, errCheckpointImmutable) {
				memoryError(w, storeErr)
				return
			}
			log.Printf("store error tenant=%s: %v", tenantID, storeErr)
			jsonError(w, "storage failed", http.StatusInternalServerError)
			return
		}
		if indexErr != nil {
			log.Printf("store indexing incomplete tenant=%s doc=%s: %v", tenantID, req.ID, indexErr)
		}

		// Record the item in its session's context list, which is what
		// GET /session/{id}/context reads back to resume a conversation.
		//
		// The session id is taken from the top-level field or from
		// metadata.session_id. The broker's agent guide documents the metadata
		// form ("pass session_id in metadata.session_id on every /store call
		// this conversation"), while the top-level field was, until now, read
		// only for the silo path — so a caller following the guide produced a
		// store that carried its session id nowhere.
		if req.Text != "" {
			if sid := storeSessionID(req.SessionID, req.Metadata); sid != "" {
				if err := appendMemoryContext(ctx, s.rdb, tenantID, sid, req.ID); err != nil {
					// Non-fatal: the memory is stored and indexed. Losing the
					// context-list entry degrades session resume, it does not
					// corrupt or lose the memory itself.
					log.Printf("session context append error tenant=%s session=%s: %v", tenantID, sid, err)
				}
			}
		}

		// Queue heurgraph extraction. Only reached on the non-silo branch,
		// where content is plaintext; silo content is sealed under the silo
		// DEK and must never be handed to an external extraction model.
		// Text-free stores (embedding supplied directly) have nothing to extract.
		if req.Text != "" {
			queueHeurgraphExtraction(ctx, s.rdb, tenantID, req.ID, req.AgentID, req.SessionID, req.Namespace)
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (tenant_id, event_type, vectors_stored, agent_id, silo_id, engagement_id) VALUES (?, 'store', 1, ?, ?, ?)`,
		tenantID, nullableString(req.AgentID), siloID, engagementID); err != nil {
		log.Printf("usage_events write failed tenant=%s: %v", tenantID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (tenant_id, actor, action, resource, ip_address, user_agent, success, agent_id, silo_id, engagement_id) VALUES (?, ?, 'store', ?, ?, ?, 1, ?, ?, ?)`,
		tenantID, fmt.Sprintf("%d", keyID), req.ID, clientIP(r), auditUserAgent(r), nullableString(req.AgentID), siloID, engagementID); err != nil {
		log.Printf("audit_log write failed tenant=%s: %v", tenantID, err)
	}

	resp := map[string]interface{}{
		"id":     req.ID,
		"stored": true,
	}
	if wc == nil {
		resp["revision"] = sv.Receipt.Revision
		resp["changed"] = sv.Receipt.Changed
	}
	if wc != nil {
		resp["silo"] = wc.SiloName
		if wc.EngagementID != "" {
			resp["engagement_id"] = wc.EngagementID
		}
	}
	if len(ontologyFindings) > 0 {
		resp["ontology_findings"] = ontologyFindings
	}
	if wc == nil {
		resp["indexed"] = indexErr == nil
		if indexErr != nil {
			resp["warning"] = "memory stored; indexing incomplete; retry the same document ID"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
		}
	}
	respondJSON(w, resp)
}

// ---------------------------------------------------------------------------
// Query — POST /query
// ---------------------------------------------------------------------------

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
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

	ctx := r.Context()
	keyID, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey)
	if err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		AgentID   string         `json:"agent_id"`
		Tags      []string       `json:"tags"`
		Embedding []float32      `json:"embedding"`
		TopK      int            `json:"top_k"`
		Filters   *memoryFilters `json:"filters,omitempty"`
		Threshold float32        `json:"threshold"`
		Text      string         `json:"text"`
		Silo      string         `json:"silo"` // single-silo scoped, read-grant enforced
	}
	req.TopK = 10

	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if err := req.Filters.validate(); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	if req.Filters != nil {
		if req.Silo != "" {
			jsonError(w, "structured filters are not supported for silo queries", 400)
			return
		}
		ctx = context.WithValue(ctx, memoryFilterContextKey{}, req.Filters)
	}
	if len(req.Embedding) > 4096 {
		jsonError(w, "embedding too large (max 4096 dimensions)", http.StatusBadRequest)
		return
	}
	if req.AgentID != "" && !validID.MatchString(req.AgentID) {
		jsonError(w, "invalid agent_id (alphanumeric, dash, dot, underscore only)", http.StatusBadRequest)
		return
	}
	if err := validateTags(req.Tags); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.TopK <= 0 {
		req.TopK = 10
	}
	// The API's own discovery document advertises "max 100"; without this the
	// only thing bounding the scan was the operator-set query.scan_cap, which
	// defaults to unbounded.
	if req.TopK > maxTopK {
		req.TopK = maxTopK
	}
	// MCP advertises text queries and forwards to this same handler. Generate
	// the vector after authentication and input validation, while retaining the
	// existing client-vector path (including its optional lexical text).
	if len(req.Embedding) == 0 {
		if strings.TrimSpace(req.Text) == "" {
			jsonError(w, "embedding or text is required", http.StatusBadRequest)
			return
		}
		embedding, err := s.embedText(ctx, req.Text)
		if err != nil {
			log.Printf("query embedding failed tenant=%s: %v", tenantID, err)
			jsonError(w, "embedding service unavailable", http.StatusServiceUnavailable)
			return
		}
		if len(embedding) == 0 || len(embedding) > 4096 {
			jsonError(w, "embedding service returned an invalid vector", http.StatusServiceUnavailable)
			return
		}
		req.Embedding = embedding
	}

	var results []searchResult
	var siloID interface{}
	if req.Silo != "" {
		// Silo reads fail closed to deny; there is no multi-silo or wildcard
		// query on this surface (cross-silo fusion is admin/audit-plane work).
		if !validID.MatchString(req.Silo) {
			jsonError(w, "invalid silo", http.StatusBadRequest)
			return
		}
		if s.crypto == nil {
			jsonError(w, "silo features not configured on this deployment", http.StatusServiceUnavailable)
			return
		}
		wc, err := s.checkReadGrant(ctx, tenantID, keyID, req.Silo)
		if err != nil {
			if ee, ok := asEnforceError(err); ok {
				jsonError(w, "forbidden: "+ee.Code, http.StatusForbidden)
				return
			}
			log.Printf("silo read resolve error tenant=%s: %v", tenantID, err)
			jsonError(w, "search failed", http.StatusInternalServerError)
			return
		}
		results, err = searchSiloVecs(ctx, s.rdb, wc.DEK, tenantID, wc.SiloName, req.AgentID, req.Embedding, req.TopK, req.Threshold)
		if err != nil {
			log.Printf("silo query error tenant=%s silo=%s: %v", tenantID, req.Silo, err)
			jsonError(w, "search failed", http.StatusInternalServerError)
			return
		}
		siloID = wc.SiloID
	} else {
		var err error
		results, err = searchVecs(ctx, s.rdb, tenantID, req.AgentID, req.Tags, req.Embedding, req.TopK, req.Threshold, crossAgentEnabled(s.config))
		if err != nil {
			log.Printf("query error tenant=%s: %v", tenantID, err)
			jsonError(w, "search failed", http.StatusInternalServerError)
			return
		}
	}
	if results == nil {
		results = []searchResult{}
	}

	if req.Text != "" && req.Silo == "" { // silo'd content has no plaintext BM25 index
		queryTerms := tokenize(req.Text)
		if len(queryTerms) > 0 {
			docIDs := make([]string, len(results))
			for i, r := range results {
				docIDs[i] = r.ID
			}
			bm25Scores, err := scoreBM25(ctx, s.rdb, tenantID, queryTerms, docIDs)
			if err != nil {
				log.Printf("bm25 score error tenant=%s: %v", tenantID, err)
			} else {
				for i := range results {
					if score, ok := bm25Scores[results[i].ID]; ok {
						score := score
						results[i].BM25Score = &score
					}
				}
			}
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (tenant_id, event_type, vectors_stored, agent_id, silo_id) VALUES (?, 'query', 0, ?, ?)`,
		tenantID, nullableString(req.AgentID), siloID); err != nil {
		log.Printf("usage_events write failed tenant=%s: %v", tenantID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (tenant_id, actor, action, ip_address, user_agent, success, agent_id, silo_id) VALUES (?, ?, 'query', ?, ?, 1, ?, ?)`,
		tenantID, fmt.Sprintf("%d", keyID), clientIP(r), auditUserAgent(r), nullableString(req.AgentID), siloID); err != nil {
		log.Printf("audit_log write failed tenant=%s: %v", tenantID, err)
	}

	respondJSON(w, map[string]interface{}{
		"results": results,
		"count":   len(results),
	})
}

// ---------------------------------------------------------------------------
// Provenance — read-only audit_log surfacing
//
// Auth: same as /store, /query — X-HeurChain-API-Key + X-HeurChain-Tenant.
// Both handlers are strictly tenant-scoped in the SQL WHERE clause and perform
// no writes.
// ---------------------------------------------------------------------------

// auditLogRow is the read-only shape returned by the provenance endpoints.
type auditLogRow struct {
	ID        int64  `json:"id"`
	Actor     string `json:"actor,omitempty"`
	Action    string `json:"action"`
	Resource  string `json:"resource,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
	Success   int    `json:"success"`
	Details   string `json:"details,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	CreatedAt string `json:"created_at"`
}

// scanAuditRows reads audit_log rows from a query result into auditLogRow
// values, tolerating NULL columns. Caller owns rows.Close().
func scanAuditRows(rows *sql.Rows) ([]auditLogRow, error) {
	out := []auditLogRow{}
	for rows.Next() {
		var (
			r         auditLogRow
			actor     sql.NullString
			resource  sql.NullString
			ipAddress sql.NullString
			userAgent sql.NullString
			details   sql.NullString
			agentID   sql.NullString
		)
		if err := rows.Scan(&r.ID, &actor, &r.Action, &resource, &ipAddress, &userAgent, &r.Success, &details, &agentID, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Actor = actor.String
		r.Resource = resource.String
		r.IPAddress = ipAddress.String
		r.UserAgent = userAgent.String
		r.Details = details.String
		r.AgentID = agentID.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// GET /vectors/{id}/history
//
// Returns the audit_log trail for a single document/vector id, scoped to the
// caller's tenant. Newest first, bounded to the last 100 events.
func (s *Server) handleVectorHistory(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	if r.Method != http.MethodGet {
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

	ctx := r.Context()
	if _, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/vectors/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "history" {
		jsonError(w, "invalid path", http.StatusBadRequest)
		return
	}
	id := parts[0]
	if id == "" || !validID.MatchString(id) {
		jsonError(w, "missing or invalid id", http.StatusBadRequest)
		return
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, action, resource, ip_address, user_agent, success, details, agent_id, created_at
		   FROM audit_log
		  WHERE tenant_id = ? AND resource = ?
		  ORDER BY id DESC
		  LIMIT 100`,
		tenantID, id)
	if err != nil {
		log.Printf("vector history error tenant=%s id=%s: %v", tenantID, id, err)
		jsonError(w, "history failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	events, err := scanAuditRows(rows)
	if err != nil {
		log.Printf("vector history scan error tenant=%s id=%s: %v", tenantID, id, err)
		jsonError(w, "history failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"id":     id,
		"events": events,
		"count":  len(events),
	})
}

// GET /memory/provenance?agent_id=&action=&limit=
//
// Returns recent audit_log events for the caller's tenant, newest first, with
// optional filters on agent_id and action. Default limit 50, max 200.
func (s *Server) handleMemoryProvenance(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	if r.Method != http.MethodGet {
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

	ctx := r.Context()
	if _, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 200 {
		limit = 200
	}

	agentID := r.URL.Query().Get("agent_id")
	if agentID != "" && !validID.MatchString(agentID) {
		jsonError(w, "invalid agent_id (alphanumeric, dash, dot, underscore only)", http.StatusBadRequest)
		return
	}
	action := r.URL.Query().Get("action")
	if action != "" && !validTag.MatchString(action) {
		jsonError(w, "invalid action", http.StatusBadRequest)
		return
	}

	query := `SELECT id, actor, action, resource, ip_address, user_agent, success, details, agent_id, created_at
		   FROM audit_log
		  WHERE tenant_id = ?`
	args := []interface{}{tenantID}
	if agentID != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	if action != "" {
		query += ` AND action = ?`
		args = append(args, action)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("provenance error tenant=%s: %v", tenantID, err)
		jsonError(w, "provenance failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	events, err := scanAuditRows(rows)
	if err != nil {
		log.Printf("provenance scan error tenant=%s: %v", tenantID, err)
		jsonError(w, "provenance failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"events": events,
		"count":  len(events),
	})
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	stats := map[string]interface{}{
		"service":   "heurchain-broker",
		"version":   brokerVersion,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"endpoints": []string{"/health", "/store", "/query", "/admin", "/admin/db-version"},
	}

	if s.db != nil {
		var tenantCount, keyCount int
		_ = s.db.QueryRow("SELECT COUNT(*) FROM tenants").Scan(&tenantCount)
		_ = s.db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL").Scan(&keyCount)
		stats["tenants"] = tenantCount
		stats["active_keys"] = keyCount
	}

	respondJSON(w, stats)
}

func (s *Server) handleDBVersion(w http.ResponseWriter, r *http.Request) {
	if !s.checkAdminAuth(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var version int
	var appliedAt string
	var description string

	if s.db != nil {
		err := s.db.QueryRow(
			"SELECT version, applied_at, description FROM schema_version ORDER BY version DESC LIMIT 1",
		).Scan(&version, &appliedAt, &description)
		if err != nil {
			respondJSON(w, map[string]interface{}{
				"version":     0,
				"error":       err.Error(),
				"description": "schema_version table may be missing — run schema.sql",
			})
			return
		}
	}

	respondJSON(w, map[string]interface{}{
		"version":     version,
		"applied_at":  appliedAt,
		"description": description,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// nullableString returns nil for empty strings so SQLite stores NULL instead of "".
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func respondJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode error: %v", err)
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// Unwrap preserves optional ResponseWriter capabilities, including streaming
// through http.ResponseController, when request logging wraps the writer.
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.bytes += n
	return n, err
}

func (s *Server) logRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", reqID)
		tenantID := r.Header.Get(s.config.Auth.TenantHeader)
		start := time.Now()

		// Log at request START so hangs are visible in journald before Caddy times out.
		log.Printf(`{"event":"req.start","req_id":%q,"method":%q,"path":%q,"tenant":%q,"remote":%q}`,
			reqID, r.Method, r.URL.Path, tenantID, r.RemoteAddr)

		sr := &statusRecorder{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf(`{"event":"req.panic","req_id":%q,"path":%q,"panic":"%v","stack":%q}`,
					reqID, r.URL.Path, rec, string(debug.Stack()))
				if sr.status == 0 {
					http.Error(sr.ResponseWriter, "internal server error", http.StatusInternalServerError)
				}
			}
			log.Printf(`{"event":"req.end","req_id":%q,"method":%q,"path":%q,"tenant":%q,"status":%d,"bytes":%d,"dur_ms":%d}`,
				reqID, r.Method, r.URL.Path, tenantID, sr.status, sr.bytes, time.Since(start).Milliseconds())
		}()
		next(sr, r)
	}
}

// ---------------------------------------------------------------------------
// Graph Ask -- POST /graph/ask
// ---------------------------------------------------------------------------

// handleGraphAsk runs a multi-hop knowledge-graph query and returns
// serialized subgraph results with per-edge provenance citations.
//
// Disabled tenants receive an explicit 503; successful empty traversal returns
// 200 with retrieval diagnostics, distinguishing no evidence from unavailable.
func (s *Server) handleGraphAsk(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// tenantStatusGuard has already validated the tenant header and confirmed
	// the tenant is active, but re-check here so the handler is safe on its
	// own — matching handleStore/handleQuery.
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
	ctx := r.Context()
	keyID, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey)
	if err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Query   string   `json:"query"`
		Hops    int      `json:"max_hops,omitempty"`
		SeedIDs []string `json:"seed_ids,omitempty"`
	}
	if !decodeJSONBody(w, r, &req, maxBodyBytes) {
		return
	}
	if req.Query == "" && len(req.SeedIDs) == 0 {
		jsonError(w, "query or seed_ids required", http.StatusBadRequest)
		return
	}
	if len(req.SeedIDs) > 32 {
		jsonError(w, "too many seed_ids (max 32)", http.StatusBadRequest)
		return
	}
	for _, id := range req.SeedIDs {
		if !validID.MatchString(id) {
			jsonError(w, "invalid seed_id", http.StatusBadRequest)
			return
		}
	}
	// Hops bounds traversal fan-out; clamp rather than reject so callers
	// asking for more depth get a usable answer instead of an error.
	if req.Hops <= 0 || req.Hops > 5 {
		req.Hops = 2
	}

	if !graphTenantEnabled(tenantID) {
		jsonError(w, "graph retrieval disabled for tenant", http.StatusServiceUnavailable)
		return
	}
	req.Hops = min(req.Hops, 2)
	result, err := traverseGraph(ctx, s.rdb, s.db, tenantID, req.Query, "", nil, RecallOptions{GraphHops: req.Hops}, req.SeedIDs, graphMaxCandidates)
	if err != nil {
		jsonError(w, "graph retrieval unavailable", http.StatusServiceUnavailable)
		return
	}
	citations := []graphCitation{}
	seen := map[string]bool{}
	ids := []string{}
	for id := range result.Ranks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return result.Ranks[ids[i]] < result.Ranks[ids[j]] })
	for _, id := range ids {
		for _, edge := range result.Paths[id] {
			if !seen[edge.RelationID] {
				seen[edge.RelationID] = true
				citations = append(citations, edge)
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO usage_events (tenant_id,event_type,vectors_stored) VALUES (?,'graph_ask',0)`, tenantID); err != nil {
		log.Printf("graph usage event: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO audit_log (tenant_id,actor,action,ip_address,user_agent,success) VALUES (?,?,'graph_ask',?,?,1)`, tenantID, fmt.Sprint(keyID), clientIP(r), auditUserAgent(r)); err != nil {
		log.Printf("graph audit event: %v", err)
	}
	respondJSON(w, map[string]interface{}{"query": req.Query, "max_hops": req.Hops, "citations": citations, "source_memory_ids": ids, "retrieval": map[string]interface{}{"graph_status": "ok", "seeds": result.Seeds, "edges_examined": result.Examined, "truncated": result.Truncated}})
}
