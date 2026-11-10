package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) memoryRequest(w http.ResponseWriter, r *http.Request, body interface{}) (string, int64, bool) {
	if handleCORSPreflight(w, r) {
		return "", 0, false
	}
	setCORSHeaders(w)
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", 405)
		return "", 0, false
	}
	tenant := r.Header.Get(s.config.Auth.TenantHeader)
	if !validID.MatchString(tenant) {
		jsonError(w, "invalid tenant", 400)
		return "", 0, false
	}
	key, err := validateAPIKey(r.Context(), s.rdb, s.db, tenant, r.Header.Get(s.config.Auth.APIKeyHeader))
	if err != nil {
		jsonError(w, "unauthorized", 401)
		return "", 0, false
	}
	if !decodeJSONBody(w, r, body, maxBodyBytes) {
		return "", 0, false
	}
	return tenant, key, true
}

func memoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errMemoryForgotten):
		jsonError(w, errMemoryForgotten.Error(), 410)
	case errors.Is(err, errMemoryNotFound):
		jsonError(w, errMemoryNotFound.Error(), 404)
	case errors.Is(err, errMemoryConflict):
		jsonError(w, errMemoryConflict.Error(), 409)
	case errors.Is(err, errCheckpointImmutable):
		jsonError(w, errCheckpointImmutable.Error(), 409)
	default:
		jsonError(w, "memory operation unavailable", 503)
	}
}

func (s *Server) handleMemoryGet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string `json:"id"`
		Revision       int64  `json:"revision"`
		IncludeHistory bool   `json:"include_history"`
	}
	tenant, _, ok := s.memoryRequest(w, r, &req)
	if !ok {
		return
	}
	if !validID.MatchString(req.ID) || req.Revision < 0 {
		jsonError(w, "valid id and nonnegative revision required", 400)
		return
	}
	fields, status, err := readMemoryFields(r.Context(), s.rdb, tenant, req.ID)
	if err != nil {
		memoryError(w, err)
		return
	}
	record := memoryRecordFromFields(req.ID, status, fields)
	response := map[string]interface{}{"memory": record}
	if req.IncludeHistory || (req.Revision > 0 && req.Revision != record.Revision) {
		history, err := s.rdb.LRange(r.Context(), memoryVersionsKey(tenant, req.ID), 0, memoryRevisionRetention-1).Result()
		if err != nil {
			memoryError(w, err)
			return
		}
		versions := []memoryRecord{}
		found := req.Revision == 0 || req.Revision == record.Revision
		for _, raw := range history {
			var old map[string]string
			if json.Unmarshal([]byte(raw), &old) != nil {
				continue
			}
			version := memoryRecordFromFields(req.ID, "superseded", old)
			if version.Revision == req.Revision {
				response["memory"] = version
				found = true
			}
			versions = append(versions, version)
		}
		if !found {
			jsonError(w, "revision not retained", 404)
			return
		}
		if req.IncludeHistory {
			response["history"] = versions
			response["history_retention"] = memoryRevisionRetention
			response["history_byte_limit"] = memoryRevisionByteLimit
		}
	}
	respondJSON(w, response)
}

// Cursor pagination is deliberately bounded and not a snapshot. Concurrent
// writes may be encountered twice; clients should deduplicate by ID/revision.
type memoryListCursor struct {
	Phase  int    `json:"p"`
	Scan   uint64 `json:"s"`
	Offset int    `json:"o"`
}

func (s *Server) handleMemoryList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Limit   int            `json:"limit"`
		Cursor  string         `json:"cursor"`
		Filters *memoryFilters `json:"filters"`
	}
	tenant, _, ok := s.memoryRequest(w, r, &req)
	if !ok {
		return
	}
	if req.Limit == 0 {
		req.Limit = 25
	}
	if req.Limit < 1 || req.Limit > 100 {
		jsonError(w, "limit must be 1 to 100", 400)
		return
	}
	if err := req.Filters.validate(); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	cursor := memoryListCursor{}
	if req.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if len(req.Cursor) > 256 || err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Phase < 0 || cursor.Phase > 1 || cursor.Offset < 0 || cursor.Offset > 10000 {
			jsonError(w, "invalid cursor", 400)
			return
		}
	}
	items := []memoryRecord{}
	for pages := 0; pages < 10 && cursor.Phase < 2 && len(items) < req.Limit; pages++ {
		prefix := "tenant:" + tenant + ":vec:"
		if cursor.Phase == 1 {
			prefix = "tenant:" + tenant + ":vault:"
		}
		keys, next, err := s.rdb.Scan(r.Context(), cursor.Scan, prefix+"*", 100).Result()
		if err != nil {
			memoryError(w, err)
			return
		}
		for cursor.Offset < len(keys) && len(items) < req.Limit {
			id := strings.TrimPrefix(keys[cursor.Offset], prefix)
			cursor.Offset++
			fields, status, err := readMemoryFields(r.Context(), s.rdb, tenant, id)
			if errors.Is(err, errMemoryNotFound) || errors.Is(err, errMemoryForgotten) {
				continue
			}
			if err != nil {
				memoryError(w, err)
				return
			}
			if cursor.Phase == 1 && status != "archived" {
				continue
			}
			item := memoryRecordFromFields(id, status, fields)
			if req.Filters.matches(item) {
				items = append(items, item)
			}
		}
		if cursor.Offset >= len(keys) {
			cursor.Offset = 0
			cursor.Scan = next
			if next == 0 {
				cursor.Phase++
			}
		}
	}
	nextCursor := ""
	if cursor.Phase < 2 {
		raw, _ := json.Marshal(cursor)
		nextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	respondJSON(w, map[string]interface{}{"memories": items, "next_cursor": nextCursor, "snapshot": false})
}

func (s *Server) handleMemoryUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID               string           `json:"id"`
		ExpectedRevision int64            `json:"expected_revision"`
		Text             *string          `json:"text"`
		Metadata         *json.RawMessage `json:"metadata"`
		Tags             *[]string        `json:"tags"`
		Namespace        *string          `json:"namespace"`
		Reason           string           `json:"reason"`
	}
	tenant, key, ok := s.memoryRequest(w, r, &req)
	if !ok {
		return
	}
	if !validID.MatchString(req.ID) || req.ExpectedRevision < 1 || len(req.Reason) > 1000 || strings.TrimSpace(req.Reason) == "" {
		jsonError(w, "id, expected_revision and a short reason are required", 400)
		return
	}
	if req.Text == nil && req.Metadata == nil && req.Tags == nil && req.Namespace == nil {
		jsonError(w, "provide a field to correct", 400)
		return
	}
	fields, _, err := readMemoryFields(r.Context(), s.rdb, tenant, req.ID)
	if err != nil {
		memoryError(w, err)
		return
	}
	old := memoryRecordFromFields(req.ID, "active", fields)
	if old.Revision != req.ExpectedRevision {
		memoryError(w, errMemoryConflict)
		return
	}
	text := old.Text
	if req.Text != nil {
		text = *req.Text
	}
	if strings.TrimSpace(text) == "" || len(text) > maxEmbedTextBytes {
		jsonError(w, "memory text is empty or too long", 400)
		return
	}
	meta := old.Metadata
	if req.Metadata != nil {
		meta = *req.Metadata
	}
	var object map[string]interface{}
	if json.Unmarshal(meta, &object) != nil || object == nil || recallMetadataValid(meta) != nil {
		jsonError(w, "metadata must be an object", 400)
		return
	}
	tags := old.Tags
	if req.Tags != nil {
		tags = *req.Tags
	}
	if err := validateTags(tags); err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	namespace := old.Namespace
	if req.Namespace != nil {
		namespace = *req.Namespace
	}
	namespace, err = resolveNamespace(namespace)
	if err != nil {
		jsonError(w, err.Error(), 400)
		return
	}
	var embedding []float32
	model := fields["embedding_model"]
	if text == old.Text {
		_ = json.Unmarshal([]byte(fields["embedding"]), &embedding)
	}
	if len(embedding) == 0 {
		embedding, err = s.embedText(r.Context(), text)
		if err != nil {
			jsonError(w, "embedding unavailable", 503)
			return
		}
		model = s.activeEmbedModel()
	}
	receipt := memoryWriteReceipt{}
	sv := storedVector{ID: req.ID, Text: text, AgentID: old.AgentID, Tags: tags, Metadata: meta, Namespace: namespace, Embedding: embedding, Model: model, CreatedAt: old.CreatedAt, ExpectedRevision: &req.ExpectedRevision, ChangeReason: req.Reason, Receipt: &receipt}
	storeErr, indexErr := s.storeWithIndexes(r.Context(), tenant, sv)
	if storeErr != nil {
		memoryError(w, storeErr)
		return
	}
	s.auditMemoryChange(r, tenant, key, "memory.update", req.ID, receipt.Revision)
	if indexErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	}
	respondJSON(w, map[string]interface{}{"id": req.ID, "stored": true, "revision": receipt.Revision, "changed": receipt.Changed, "indexed": indexErr == nil, "retry_required": indexErr != nil})
}

func (s *Server) auditMemoryChange(r *http.Request, tenant string, key int64, action, id string, revision int64) {
	// Only identities/revisions enter audit telemetry, never memory content.
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO audit_log (tenant_id,actor,action,resource,success,details,created_at) VALUES (?,?,?,?,1,?,?)`, tenant, strconv.FormatInt(key, 10), action, id, fmt.Sprintf("revision=%d", revision), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		log.Printf("memory audit unavailable action=%s tenant=%s id=%s", action, tenant, id)
	}
}
