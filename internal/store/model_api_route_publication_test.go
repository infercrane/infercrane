package store

import (
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
)

func TestPublishedRouteCompilerRequiresCurrentQualifiedCallableEvidence(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	current := now.Add(time.Hour)
	expired := now.Add(-time.Second)
	claims := []modelapiproduct.CapabilityClaim{
		{Name: "streaming", State: modelapiproduct.ClaimQualified, EvidenceID: "stream", EvidenceUntil: &current},
		{Name: "chat-completions", State: modelapiproduct.ClaimQualified, EvidenceID: "chat", EvidenceUntil: &current},
		{Name: "embeddings", State: modelapiproduct.ClaimQualified, EvidenceID: "old", EvidenceUntil: &expired},
	}
	operations, until, err := qualifiedOperations(claims, now)
	if err != nil || len(operations) != 2 || operations[0] != "chat" || operations[1] != "streaming" || !until.Equal(current) {
		t.Fatalf("operations=%#v until=%s err=%v", operations, until, err)
	}
	if _, _, err = qualifiedOperations([]modelapiproduct.CapabilityClaim{{Name: "chat-completions", State: modelapiproduct.ClaimCataloged}}, now); err == nil {
		t.Fatal("catalog metadata without current evidence became callable")
	}
	if _, _, err = qualifiedOperations([]modelapiproduct.CapabilityClaim{{Name: "chat-completions", State: modelapiproduct.ClaimQualified, EvidenceID: "chat", EvidenceUntil: &current}}, now); err == nil {
		t.Fatal("stream-capable route launched without qualified streaming evidence")
	}
	operations, _, err = qualifiedOperations([]modelapiproduct.CapabilityClaim{{Name: "embeddings", State: modelapiproduct.ClaimQualified, EvidenceID: "embedding", EvidenceUntil: &current}}, now)
	if err != nil || len(operations) != 1 || operations[0] != "embeddings" {
		t.Fatalf("non-streaming operation unexpectedly required streaming evidence: operations=%v err=%v", operations, err)
	}
	if supportsPublishedOperations([]string{"chat-completions"}, []string{"chat", "streaming"}) {
		t.Fatal("candidate without streaming qualification matched a streaming-qualified snapshot")
	}
	if !supportsPublishedOperations([]string{"chat-completions", "streaming"}, []string{"chat", "streaming"}) {
		t.Fatal("candidate with exact operation and streaming qualification was rejected")
	}
}

func TestPublishedRouteCompilerMatchesExactPersistedPlanPosition(t *testing.T) {
	plan := modelapisupply.Plan{
		Primary:   &modelapisupply.Selection{CandidateID: "primary"},
		Fallbacks: []modelapisupply.Selection{{CandidateID: "fallback-one"}, {CandidateID: "fallback-two"}},
	}
	selection, position, ok := selectionAt(plan, "primary", 0)
	if !ok || position != 0 || selection.CandidateID != "primary" {
		t.Fatalf("primary selection=%#v position=%d ok=%v", selection, position, ok)
	}
	selection, position, ok = selectionAt(plan, "fallback", 2)
	if !ok || position != 2 || selection.CandidateID != "fallback-two" {
		t.Fatalf("fallback selection=%#v position=%d ok=%v", selection, position, ok)
	}
	if _, _, ok = selectionAt(plan, "fallback", 3); ok {
		t.Fatal("candidate outside immutable plan was accepted")
	}
}
