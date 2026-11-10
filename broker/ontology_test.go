package main

import (
	"context"
	"encoding/json"
	"testing"
)

func ontologyServer(enabled bool, mode string) *Server {
	return &Server{config: &Config{Ontology: struct {
		Enabled bool   `yaml:"enabled"`
		Mode    string `yaml:"mode"`
	}{Enabled: enabled, Mode: mode}}}
}

func TestOntologyDisabledIsNoop(t *testing.T) {
	findings, err := ontologyServer(false, "reject").validateOntology(context.Background(), "tenant-a", "", "", "", &ontologyEnvelope{
		Version:   ontologyContractVersion,
		Assertion: &ontologyAssertion{Subject: "broker", Predicate: "has_status", Object: "unknown"},
	})
	if err != nil || len(findings) != 0 {
		t.Fatalf("disabled ontology got findings=%v err=%v", findings, err)
	}
}

func TestOntologyRejectsInvalidStatus(t *testing.T) {
	findings, err := ontologyServer(true, "reject").validateOntology(context.Background(), "tenant-a", "", "", "Broker status is unknown.", &ontologyEnvelope{
		Version:       ontologyContractVersion,
		Assertion:     &ontologyAssertion{Subject: "broker", Predicate: "has_status", Object: "unknown"},
		SourceExcerpt: "Broker status is unknown.",
	})
	if err == nil || len(findings) == 0 || findings[0].Rule != "enum:MemoryStatus" {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}

func TestOntologyWarnReportsButAllows(t *testing.T) {
	findings, err := ontologyServer(true, "warn").validateOntology(context.Background(), "tenant-a", "", "", "doc-1 supersedes doc-1", &ontologyEnvelope{
		Version:       ontologyContractVersion,
		Assertion:     &ontologyAssertion{Subject: "doc-1", Predicate: "supersedes", Object: "doc-1"},
		SourceExcerpt: "doc-1 supersedes doc-1",
	})
	if err != nil || len(findings) != 1 || findings[0].Rule != "irreflexive:supersedes" {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}

func TestOntologyBindsBelongsToToAuthenticatedTenant(t *testing.T) {
	_, err := ontologyServer(true, "reject").validateOntology(context.Background(), "tenant-a", "", "", "doc-1 belongs to tenant-b", &ontologyEnvelope{
		Version:       ontologyContractVersion,
		Assertion:     &ontologyAssertion{Subject: "doc-1", Predicate: "belongs_to", Object: "tenant-b"},
		SourceExcerpt: "doc-1 belongs to tenant-b",
	})
	if err == nil {
		t.Fatal("tenant mismatch was accepted")
	}
}

func TestOntologyRejectsUngroundedAssertion(t *testing.T) {
	_, err := ontologyServer(true, "reject").validateOntology(context.Background(), "tenant-a", "", "", "unrelated text", &ontologyEnvelope{
		Version:       ontologyContractVersion,
		Assertion:     &ontologyAssertion{Subject: "doc-1", Predicate: "references", Object: "doc-2"},
		SourceExcerpt: "doc-1 references doc-2",
	})
	if err == nil {
		t.Fatal("ungrounded assertion was accepted")
	}
}

func TestAttachOntologyMetadataPreservesAssertion(t *testing.T) {
	metadata, err := attachOntologyMetadata(json.RawMessage(`{"project":"heurchain"}`), &ontologyEnvelope{
		Version:       ontologyContractVersion,
		Assertion:     &ontologyAssertion{Subject: "doc-1", Predicate: "references", Object: "doc-2"},
		SourceExcerpt: "doc-1 references doc-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["project"]) != `"heurchain"` || len(decoded["ontology"]) == 0 {
		t.Fatalf("metadata did not preserve both values: %s", metadata)
	}
}
