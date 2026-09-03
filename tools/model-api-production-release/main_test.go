package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

func TestRunReleaseBuildsImmutableRevisionTwo(t *testing.T) {
	now := time.Date(2026, 9, 3, 16, 30, 0, 0, time.UTC)
	qualificationPath := writeTestQualification(t, now, 2)
	outputDirectory := filepath.Join(t.TempDir(), "release")
	cfg := releaseConfig{
		QualificationPath:   qualificationPath,
		CredentialReference: "secret-reference",
		OperatorWorkspaceID: "operator-workspace",
		ServingPlanID:       "serving-plan",
		CustomerWorkspaceID: "customer-workspace",
		OutputDirectory:     outputDirectory,
		ReleaseVersion:      2,
	}
	if err := runRelease(cfg, now); err != nil {
		t.Fatal(err)
	}

	var rate modelapiproduct.RetailRate
	readTestManifest(t, filepath.Join(outputDirectory, "02-retail-rate.json"), &rate)
	if rate.ID != "deepseek-v4-flash-rate-2" || rate.Version != 2 {
		t.Fatalf("unexpected rate identity: %s v%d", rate.ID, rate.Version)
	}

	var offer modelapisupply.Offer
	readTestManifest(t, filepath.Join(outputDirectory, "03-supplier-offer.json"), &offer)
	if offer.ID != offerID || offer.Version != 2 || offer.Adapter != supplieradapter.DeepSeekAdapterName {
		t.Fatalf("unexpected offer: %#v", offer)
	}

	var plan store.SupplyPlanDraft
	readTestManifest(t, filepath.Join(outputDirectory, "05-supply-plan.json"), &plan)
	if plan.ID != "supply-deepseek-v4-flash-production-r2" || len(plan.Candidates) != 1 || plan.Candidates[0].CandidateID != "candidate-deepseek-direct-v4-flash-production-r2" || plan.Candidates[0].OfferVersion != 2 || plan.Candidates[0].RetailRateVersion != 2 {
		t.Fatalf("unexpected supply plan: %#v", plan)
	}

	var publication modelapiproduct.OperatorPublication
	readTestManifest(t, filepath.Join(outputDirectory, "06-publication.json"), &publication)
	if publication.SupplyPlanID != plan.ID || publication.RetailRate == nil || publication.RetailRate.Version != 2 {
		t.Fatalf("unexpected publication: %#v", publication)
	}

	var entitlement modelapiproduct.ProductEntitlement
	readTestManifest(t, filepath.Join(outputDirectory, "08-canary-entitlement.json"), &entitlement)
	if entitlement.ID != "entitlement-customer-workspace-deepseek-v4-flash" || entitlement.RetailRateID != rate.ID || entitlement.RetailRateVersion != 2 {
		t.Fatalf("unexpected entitlement: %#v", entitlement)
	}

	entries, err := os.ReadDir(outputDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 8 {
		t.Fatalf("expected 8 manifests, got %d", len(entries))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", entry.Name(), info.Mode().Perm())
		}
	}
	if err := runRelease(cfg, now); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("expected exclusive-write failure, got %v", err)
	}
}

func TestValidateQualificationRejectsWrongRevisionOrExpiredEvidence(t *testing.T) {
	now := time.Date(2026, 9, 3, 16, 30, 0, 0, time.UTC)
	manifest := testQualification(now, 2)
	if err := validateQualification(now, 2, manifest); err != nil {
		t.Fatal(err)
	}

	wrongVersion := manifest
	wrongVersion.OfferVersion = 1
	if err := validateQualification(now, 2, wrongVersion); err == nil {
		t.Fatal("expected immutable offer version mismatch")
	}

	wrongRevision := manifest
	wrongRevision.Evidence.Scope = strings.ReplaceAll(wrongRevision.Evidence.Scope, supplieradapter.DeepSeekV4FlashRevision, "different-revision")
	if err := validateQualification(now, 2, wrongRevision); err == nil {
		t.Fatal("expected exact revision mismatch")
	}

	expired := manifest
	expired.Evidence.ValidUntil = now.Add(30 * time.Minute)
	if err := validateQualification(now, 2, expired); err == nil {
		t.Fatal("expected minimum-currentness failure")
	}
}

func writeTestQualification(t *testing.T, now time.Time, version int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qualification.json")
	body, err := json.Marshal(testQualification(now, version))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testQualification(now time.Time, version int64) qualificationManifest {
	origin := sha256.Sum256([]byte(supplieradapter.DeepSeekBaseURL))
	ttft, output := 700.0, 40.0
	return qualificationManifest{
		OfferID: offerID, OfferVersion: version,
		Evidence: modelapisupply.QualificationEvidence{
			ID: "deepseek-qualification-r2", State: modelapisupply.QualificationQualified,
			TupleKey: "deepseek|deepseek-v4-flash|openai|global", Protocol: "openai", Region: "global",
			Capabilities: []string{"chat-completions", "streaming"},
			Scope:        "deepseek-mvp:inventory+buffered-chat-completions+streaming-chat-completions;revision=" + supplieradapter.DeepSeekV4FlashRevision + ";target_origin_sha256=" + stringHex(origin[:]),
			EvidenceRef:  "github://infercrane/qualification-evidence/test.json", EvidenceDigest: "sha256:test",
			ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(24 * time.Hour), SampleCount: 6,
			TTFTP95MS: &ttft, OutputTokensP5: &output,
		},
	}
}

func stringHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	out := make([]byte, len(value)*2)
	for i, current := range value {
		out[2*i], out[2*i+1] = alphabet[current>>4], alphabet[current&0x0f]
	}
	return string(out)
}

func readTestManifest(t *testing.T, path string, destination any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, destination); err != nil {
		t.Fatal(err)
	}
}
