package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type memoryFilterContextKey struct{}

// memoryFilterFields are the vec-hash fields recall eligibility reads: what
// (*memoryFilters).matches consults through memoryRecordFromFields, plus the
// metadata that the as_of and memory_kind scope checks parse. The recall paths
// fetch exactly these with HMGET. HGETALL would also carry the embedding JSON,
// by far the largest field, for every active document on each filtered recall.
// TestMemoryFilterFieldsCoverMatches fails when memoryFilters gains a field.
var memoryFilterFields = []string{"agent_id", "tags", "namespace", "metadata", "created_at", "updated_at"}

// hashFieldMap converts an HMGET reply for fields into the map shape the
// filter helpers read. Absent fields are left out, so a missing hash yields an
// empty map exactly as HGETALL does.
func hashFieldMap(fields []string, values []interface{}) map[string]string {
	out := make(map[string]string, len(fields))
	for i, field := range fields {
		if i >= len(values) {
			break
		}
		if value, ok := values[i].(string); ok {
			out[field] = value
		}
	}
	return out
}

// Filters are exact and conjunctive. Metadata is caller-provided data, never
// an authorization boundary; tenant identity always comes from authentication.
type memoryFilters struct {
	AgentID       string                 `json:"agent_id,omitempty"`
	Tags          []string               `json:"tags,omitempty"`
	Namespace     string                 `json:"namespace,omitempty"`
	Status        string                 `json:"status,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	CreatedAfter  string                 `json:"created_after,omitempty"`
	CreatedBefore string                 `json:"created_before,omitempty"`
	UpdatedAfter  string                 `json:"updated_after,omitempty"`
	UpdatedBefore string                 `json:"updated_before,omitempty"`
}

func (f *memoryFilters) UnmarshalJSON(raw []byte) error {
	type plain memoryFilters
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(f))
}

func (f *memoryFilters) validate() error {
	if f == nil {
		return nil
	}
	if f.AgentID != "" && !validID.MatchString(f.AgentID) {
		return fmt.Errorf("invalid filter agent_id")
	}
	if err := validateTags(f.Tags); err != nil {
		return err
	}
	if f.Namespace != "" {
		if _, err := resolveNamespace(f.Namespace); err != nil {
			return err
		}
	}
	if f.Status != "" && f.Status != "active" && f.Status != "archived" {
		return fmt.Errorf("filter status must be active or archived")
	}
	if len(f.Metadata) > 8 {
		return fmt.Errorf("at most 8 exact metadata filters are allowed")
	}
	for k, v := range f.Metadata {
		if len(k) == 0 || len(k) > 128 {
			return fmt.Errorf("invalid metadata filter key")
		}
		switch value := v.(type) {
		case string:
			if len(value) > 1024 {
				return fmt.Errorf("metadata filter value too long")
			}
		case bool, float64:
		default:
			return fmt.Errorf("metadata filters accept only string, number or boolean values")
		}
	}
	for _, pair := range [][2]string{{f.CreatedAfter, f.CreatedBefore}, {f.UpdatedAfter, f.UpdatedBefore}} {
		var start, end time.Time
		var err error
		if pair[0] != "" {
			start, err = time.Parse(time.RFC3339Nano, pair[0])
			if err != nil {
				return fmt.Errorf("filter dates must be RFC3339")
			}
		}
		if pair[1] != "" {
			end, err = time.Parse(time.RFC3339Nano, pair[1])
			if err != nil {
				return fmt.Errorf("filter dates must be RFC3339")
			}
		}
		if !start.IsZero() && !end.IsZero() && start.After(end) {
			return fmt.Errorf("filter start must precede end")
		}
	}
	return nil
}

func (f *memoryFilters) matches(record memoryRecord) bool {
	if f == nil {
		return true
	}
	if f.AgentID != "" && f.AgentID != record.AgentID {
		return false
	}
	if f.Namespace != "" && f.Namespace != record.Namespace {
		return false
	}
	if f.Status != "" && f.Status != record.Status {
		return false
	}
	if !tagsIntersect(record.Tags, f.Tags) {
		return false
	}
	var meta map[string]interface{}
	_ = json.Unmarshal(record.Metadata, &meta)
	for k, v := range f.Metadata {
		if meta[k] != v {
			return false
		}
	}
	for _, window := range [][3]string{{record.CreatedAt, f.CreatedAfter, f.CreatedBefore}, {record.UpdatedAt, f.UpdatedAfter, f.UpdatedBefore}} {
		if window[1] == "" && window[2] == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, window[0])
		if err != nil {
			return false
		}
		if window[1] != "" {
			from, _ := time.Parse(time.RFC3339Nano, window[1])
			if at.Before(from) {
				return false
			}
		}
		if window[2] != "" {
			until, _ := time.Parse(time.RFC3339Nano, window[2])
			if at.After(until) {
				return false
			}
		}
	}
	return true
}

func matchesMemoryContext(ctx context.Context, id string, fields map[string]string) bool {
	f, _ := ctx.Value(memoryFilterContextKey{}).(*memoryFilters)
	return f.matches(memoryRecordFromFields(id, "active", fields))
}
