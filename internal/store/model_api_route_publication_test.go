package store

import (
	"errors"
	"fmt"
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

func TestPublishedRouteCompilerWithholdsOnlyLapsedProducts(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	current := productReadiness{
		publicationState: "qualified", availability: "available", planStatus: modelapisupply.StatusReady,
		publicationUntil: later, planUntil: later, entitlementFrom: now.Add(-time.Hour), rateFrom: now.Add(-time.Hour), rateUntil: later,
	}
	if reason := productLapse(now, current); reason != "" {
		t.Fatalf("current product was withheld: %s", reason)
	}
	degraded := current
	degraded.availability = "degraded"
	if reason := productLapse(now, degraded); reason != "" {
		t.Fatalf("degraded product was withheld: %s", reason)
	}
	ended := now
	for name, lapsed := range map[string]func(*productReadiness){
		"publication expired":   func(p *productReadiness) { p.publicationUntil = now },
		"plan expired":          func(p *productReadiness) { p.planUntil = now.Add(-time.Second) },
		"rate expired":          func(p *productReadiness) { p.rateUntil = now },
		"entitlement ended":     func(p *productReadiness) { p.entitlementUntil = &ended },
		"entitlement not begun": func(p *productReadiness) { p.entitlementFrom = later },
		"not qualified":         func(p *productReadiness) { p.publicationState = "suspended" },
		"unavailable":           func(p *productReadiness) { p.availability = "unavailable" },
		"plan not ready":        func(p *productReadiness) { p.planStatus = "draft" },
	} {
		readiness := current
		lapsed(&readiness)
		if productLapse(now, readiness) == "" {
			t.Errorf("%s: lapsed product stayed callable", name)
		}
	}

	candidate := candidateReadiness{
		offerState: "active", access: "ready", availability: "available", health: "healthy", commercialState: "ready", qualificationState: "qualified",
		costUntil: later, commercialUntil: later, qualificationUntil: later, selectionUntil: later,
	}
	if reason := candidateLapse(now, candidate); reason != "" {
		t.Fatalf("current candidate was withheld: %s", reason)
	}
	for name, lapsed := range map[string]func(*candidateReadiness){
		"qualification expired": func(c *candidateReadiness) { c.qualificationUntil = now },
		"cost expired":          func(c *candidateReadiness) { c.costUntil = now },
		"commercial expired":    func(c *candidateReadiness) { c.commercialUntil = now },
		"selection expired":     func(c *candidateReadiness) { c.selectionUntil = now },
		"offer unhealthy":       func(c *candidateReadiness) { c.health = "unhealthy" },
		"not qualified":         func(c *candidateReadiness) { c.qualificationState = "failed" },
	} {
		readiness := candidate
		lapsed(&readiness)
		if candidateLapse(now, readiness) == "" {
			t.Errorf("%s: lapsed candidate stayed callable", name)
		}
	}

	var lapse routeLapse
	if !errors.As(fmt.Errorf("compile: %w", routeLapse{reason: "qualification evidence expired"}), &lapse) || lapse.reason != "qualification evidence expired" {
		t.Fatal("a candidate lapse is not recognised as a withheld product")
	}
}
