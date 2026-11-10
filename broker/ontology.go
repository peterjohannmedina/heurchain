package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// ontologyContractVersion is shared with the HCI policy module. Assertions
// are optional metadata, not generated facts; the broker validates them only
// when ontology enforcement has been explicitly enabled.
const ontologyContractVersion = "heurchain-ontology/v1"

type ontologyAssertion struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}

type ontologyEnvelope struct {
	Version        string             `json:"version"`
	Assertion      *ontologyAssertion `json:"assertion"`
	SourceExcerpt  string             `json:"source_excerpt,omitempty"`
	SourceMemoryID string             `json:"source_memory_id,omitempty"`
}

type ontologyFinding struct {
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// validateOntology applies the v1 closed-world policy at the broker boundary.
// Cross-memory cardinality and graph closure are intentionally out of scope
// here: they belong to the future offline auditor, not an HTTP request path.
func (s *Server) validateOntology(ctx context.Context, tenantID, agentID, sessionID, text string, envelope *ontologyEnvelope) ([]ontologyFinding, error) {
	if !s.config.Ontology.Enabled || envelope == nil {
		return nil, nil
	}

	findings := []ontologyFinding{}
	if envelope.Version != ontologyContractVersion {
		findings = append(findings, ontologyFinding{
			Rule: "contract:version", Message: "ontology.version must be " + ontologyContractVersion, Severity: "warning",
		})
	}
	if envelope.Assertion == nil {
		findings = append(findings, ontologyFinding{
			Rule: "contract:assertion", Message: "ontology.assertion is required when ontology metadata is supplied", Severity: "warning",
		})
	} else {
		if err := s.validateOntologyEvidence(ctx, tenantID, text, envelope); err != nil {
			findings = append(findings, ontologyFinding{Rule: "evidence:grounding", Message: err.Error(), Severity: "warning"})
		}
		a := envelope.Assertion
		if strings.TrimSpace(a.Subject) == "" || strings.TrimSpace(a.Object) == "" {
			findings = append(findings, ontologyFinding{
				Rule: "required:subject-object", Message: "ontology assertion subject and object are required", Severity: "warning",
			})
		}
		switch a.Predicate {
		case "has_status":
			if _, ok := map[string]struct{}{"active": {}, "consolidated": {}, "superseded": {}, "invalidated": {}}[strings.ToLower(a.Object)]; !ok {
				findings = append(findings, ontologyFinding{
					Rule: "enum:MemoryStatus", Message: "has_status must be active, consolidated, superseded, or invalidated", Severity: "warning",
				})
			}
		case "belongs_to":
			if !validID.MatchString(a.Object) {
				findings = append(findings, ontologyFinding{Rule: "identifier:belongs_to", Message: "belongs_to requires a stable tenant identifier", Severity: "warning"})
			} else if a.Object != tenantID {
				findings = append(findings, ontologyFinding{Rule: "tenant:belongs_to", Message: "belongs_to must match the authenticated tenant", Severity: "warning"})
			}
		case "created_in":
			if !validID.MatchString(a.Object) {
				findings = append(findings, ontologyFinding{Rule: "identifier:created_in", Message: "created_in requires a stable session identifier", Severity: "warning"})
			} else if sessionID != "" && a.Object != sessionID {
				findings = append(findings, ontologyFinding{Rule: "session:created_in", Message: "created_in must match the request session_id", Severity: "warning"})
			}
		case "written_by":
			if !validID.MatchString(a.Object) {
				findings = append(findings, ontologyFinding{Rule: "identifier:written_by", Message: "written_by requires a stable agent identifier", Severity: "warning"})
			} else if agentID != "" && a.Object != agentID {
				findings = append(findings, ontologyFinding{Rule: "agent:written_by", Message: "written_by must match the request agent_id", Severity: "warning"})
			}
		case "references", "supersedes", "consolidated_from":
			if a.Subject == a.Object {
				findings = append(findings, ontologyFinding{Rule: "irreflexive:" + a.Predicate, Message: a.Predicate + " cannot point an entity at itself", Severity: "warning"})
			}
		default:
			findings = append(findings, ontologyFinding{Rule: "predicate:allowed", Message: fmt.Sprintf("unsupported ontology predicate %q", a.Predicate), Severity: "warning"})
		}
	}

	if len(findings) > 0 && s.config.Ontology.Mode == "reject" {
		return findings, fmt.Errorf("ontology validation failed: %s", findings[0].Message)
	}
	if len(findings) > 0 {
		log.Printf("ontology: allowing %d finding(s) in warn mode tenant=%s", len(findings), tenantID)
	}
	return findings, nil
}

func (s *Server) validateOntologyEvidence(ctx context.Context, tenantID, text string, envelope *ontologyEnvelope) error {
	if excerpt := strings.TrimSpace(envelope.SourceExcerpt); excerpt != "" {
		if len(excerpt) > 1000 {
			return fmt.Errorf("source_excerpt exceeds 1000 characters")
		}
		if text == "" || !strings.Contains(normalizeOntologyText(text), normalizeOntologyText(excerpt)) {
			return fmt.Errorf("source_excerpt does not appear in the supplied text")
		}
		return nil
	}
	if sourceID := envelope.SourceMemoryID; sourceID != "" {
		if !validID.MatchString(sourceID) {
			return fmt.Errorf("source_memory_id must be a stable memory identifier")
		}
		exists, err := s.rdb.Exists(ctx, vecKey(tenantID, sourceID)).Result()
		if err != nil {
			return fmt.Errorf("verify source_memory_id: %w", err)
		}
		if exists == 0 {
			return fmt.Errorf("source_memory_id is not a memory in the authenticated tenant")
		}
		// A forgotten memory cannot serve as evidence, residue or not.
		if forgotten, err := memoryForgotten(ctx, s.rdb, tenantID, sourceID); err != nil {
			return fmt.Errorf("verify source_memory_id: %w", err)
		} else if forgotten {
			return fmt.Errorf("source_memory_id is not a memory in the authenticated tenant")
		}
		return nil
	}
	return fmt.Errorf("ontology assertion requires source_excerpt or source_memory_id")
}

func normalizeOntologyText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

// attachOntologyMetadata makes the validated assertion durable and available
// to a later auditor or HeurGraph worker. The top-level request field avoids
// clients accidentally hiding an assertion inside unrelated metadata.
func attachOntologyMetadata(metadata json.RawMessage, envelope *ontologyEnvelope) (json.RawMessage, error) {
	if envelope == nil {
		return metadata, nil
	}
	values := map[string]json.RawMessage{}
	if len(metadata) > 0 && string(metadata) != "null" {
		if err := json.Unmarshal(metadata, &values); err != nil || values == nil {
			return nil, fmt.Errorf("metadata must be a JSON object when ontology is supplied")
		}
	}
	if _, exists := values["ontology"]; exists {
		return nil, fmt.Errorf("metadata.ontology is reserved; use the top-level ontology field")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal ontology metadata: %w", err)
	}
	values["ontology"] = encoded
	merged, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	return json.RawMessage(merged), nil
}

func (s *Server) handleOntologyCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		jsonError(w, "method not allowed", 405)
		return
	}
	respondJSON(w, map[string]interface{}{
		"enabled":              s.config.Ontology.Enabled,
		"mode":                 s.config.Ontology.Mode,
		"assertion_schema":     ontologyContractVersion,
		"evidence":             "source_excerpt (verified against text) or source_memory_id (verified in tenant)",
		"supported_predicates": []string{"has_status", "belongs_to", "created_in", "written_by", "references", "supersedes", "consolidated_from"},
	})
}
