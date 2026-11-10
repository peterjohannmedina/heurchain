package main

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// RecallOptions is opt-in. Omitted fields preserve the existing search contract.
type RecallOptions struct {
	Mode          string  `json:"retrieval_mode"`
	RRFK          float64 `json:"rrf_k"`
	LexicalWeight float64 `json:"lexical_weight"`
	DenseWeight   float64 `json:"dense_weight"`
	AsOf          string  `json:"as_of"`
	MemoryKind    string  `json:"memory_kind"`
	MaxPerSource  int     `json:"max_per_source"`
	RerankMode    string  `json:"rerank_mode,omitempty"`
}

func (o *RecallOptions) normalize() error {
	if o.Mode == "" {
		o.Mode = "hybrid"
	}
	if o.Mode != "hybrid" && o.Mode != "lexical" && o.Mode != "dense" {
		return fmt.Errorf("invalid retrieval_mode")
	}
	if o.RerankMode != "" && o.RerankMode != "none" && o.RerankMode != "exact_identifiers" {
		return fmt.Errorf("invalid rerank_mode")
	}
	if o.RRFK == 0 {
		o.RRFK = 60
	}
	if o.LexicalWeight == 0 {
		o.LexicalWeight = 1
	}
	if o.DenseWeight == 0 {
		o.DenseWeight = 1
	}
	for _, v := range []float64{o.RRFK, o.LexicalWeight, o.DenseWeight} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return fmt.Errorf("fusion settings must be finite and positive")
		}
	}
	if o.RRFK > 1000 || o.LexicalWeight > 10 || o.DenseWeight > 10 {
		return fmt.Errorf("fusion setting exceeds bound")
	}
	if o.MaxPerSource < 0 || o.MaxPerSource > 50 {
		return fmt.Errorf("max_per_source must be 0..50")
	}
	if o.AsOf != "" {
		t, err := time.Parse(time.RFC3339, o.AsOf)
		if err != nil {
			return fmt.Errorf("as_of must be RFC3339")
		}
		o.AsOf = t.UTC().Format(time.RFC3339)
	}
	if o.MemoryKind != "" && o.MemoryKind != "factual" && o.MemoryKind != "creative" {
		return fmt.Errorf("memory_kind must be factual or creative")
	}
	return nil
}

func recallMetadataValid(raw json.RawMessage) error {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return fmt.Errorf("metadata must be an object")
	}
	times := map[string]time.Time{}
	for _, key := range []string{"valid_from", "valid_until"} {
		if value, ok := m[key]; ok {
			str, ok := value.(string)
			if !ok {
				return fmt.Errorf("%s must be RFC3339", key)
			}
			t, err := time.Parse(time.RFC3339, str)
			if err != nil {
				return fmt.Errorf("%s must be RFC3339", key)
			}
			times[key] = t
		}
	}
	if from, ok := times["valid_from"]; ok {
		if until, ok := times["valid_until"]; ok && !from.Before(until) {
			return fmt.Errorf("valid_until must follow valid_from")
		}
	}
	if kind, ok := m["memory_kind"]; ok && kind != "factual" && kind != "creative" {
		return fmt.Errorf("invalid memory_kind")
	}
	return nil
}

func recallMetadataEligible(raw string, o RecallOptions) bool {
	if o.AsOf == "" && o.MemoryKind == "" {
		return true
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return false
	}
	if o.MemoryKind != "" && m["memory_kind"] != o.MemoryKind {
		return false
	}
	if o.AsOf != "" {
		at, _ := time.Parse(time.RFC3339, o.AsOf)
		for _, key := range []string{"valid_from", "valid_until"} {
			if value, ok := m[key]; ok {
				str, ok := value.(string)
				if !ok {
					return false
				}
				t, err := time.Parse(time.RFC3339, str)
				if err != nil {
					return false
				}
				if key == "valid_from" && at.Before(t) || key == "valid_until" && !at.Before(t) {
					return false
				}
			}
		}
	}
	return true
}

func recallPayload(data map[string]string) map[string]interface{} {
	return recallPayloadFor(data["agent_id"], parseTags(data["tags"]), data["metadata"])
}

func recallPayloadFor(agent string, tags []string, raw string) map[string]interface{} {
	payload := map[string]interface{}{"agent_id": agent, "tags": tags}
	var m map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &m)
	for _, key := range []string{"memory_kind", "valid_from", "valid_until"} {
		if value, ok := m[key]; ok {
			payload[key] = value
		}
	}
	return payload
}
