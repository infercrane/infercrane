package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapisupply"
)

func TestRunGeneratesCompleteGLMLaunchSequence(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	p, err := launchProfile("glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	qualificationPath := writeTestQualification(t, now, p, p.DefaultEndpoint)
	output := filepath.Join(t.TempDir(), "release")
	err = run(config{
		Profile: "glm-5.3", QualificationPath: qualificationPath, CredentialReference: "zai-secret-ref",
		OperatorWorkspaceID: "operator-workspace", ServingPlanID: "serving-plan", CustomerWorkspaceID: "customer-workspace",
		CommercialTermsRef: "contract://zai-mvp-2026-09", OutputDirectory: output, ReleaseVersion: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 9 {
		t.Fatalf("generated %d manifests, want 9", len(entries))
	}
	var rate struct {
		Input   int64 `json:"input_microusd_per_million"`
		Output  int64 `json:"output_microusd_per_million"`
		Version int   `json:"version"`
	}
	readJSON(t, filepath.Join(output, "02-retail-rate.json"), &rate)
	if rate.Input != 1_400_000 || rate.Output != 4_400_000 || rate.Version != 2 {
		t.Fatalf("unexpected launch parity rate: %+v", rate)
	}
	var offer struct {
		Version int64 `json:"version"`
	}
	readJSON(t, filepath.Join(output, "03-supplier-offer.json"), &offer)
	if offer.Version != 2 {
		t.Fatalf("offer version=%d, want 2", offer.Version)
	}
	var binding struct {
		Kind   string `json:"kind"`
		Digest string `json:"contract_digest"`
	}
	readJSON(t, filepath.Join(output, "05-target-binding.json"), &binding)
	if binding.Kind != "upstream" || !strings.HasPrefix(binding.Digest, "sha256:") {
		t.Fatalf("unexpected target binding: %+v", binding)
	}
}

func TestRunNeverOverwritesReleaseArtifacts(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	p, err := launchProfile("glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "release")
	cfg := config{
		Profile: p.Name, QualificationPath: writeTestQualification(t, now, p, p.DefaultEndpoint), CredentialReference: "zai-secret-ref",
		OperatorWorkspaceID: "operator-workspace", ServingPlanID: "serving-plan", CustomerWorkspaceID: "customer-workspace",
		CommercialTermsRef: "contract://zai-mvp-2026-09", OutputDirectory: output, ReleaseVersion: 2,
	}
	if err = run(cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = run(cfg, now); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("second release error=%v, want append-only refusal", err)
	}
}

func TestRunPodRequiresMeasuredEconomicsAndExactQualifiedOrigin(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	p, err := launchProfile("qwen3.8-27b-runpod")
	if err != nil {
		t.Fatal(err)
	}
	if p.Region != "EU-NL-1" || !strings.HasSuffix(p.ExpectedTuple, "|EU-NL-1") {
		t.Fatalf("unexpected pinned RunPod placement: region=%q tuple=%q", p.Region, p.ExpectedTuple)
	}
	endpoint := "https://qwen38pilot.api.runpod.ai"
	qualificationPath := writeTestQualification(t, now, p, endpoint)
	cfg := config{
		Profile: p.Name, QualificationPath: qualificationPath, CredentialReference: "runpod-secret-ref",
		OperatorWorkspaceID: "operator-workspace", ServingPlanID: "serving-plan", CustomerWorkspaceID: "customer-workspace",
		Endpoint: endpoint, CommercialTermsRef: "contract://runpod-mvp-2026-09", OutputDirectory: filepath.Join(t.TempDir(), "release"), ReleaseVersion: 2,
	}
	if err = run(cfg, now); err == nil || !strings.Contains(err.Error(), "measured COGS") {
		t.Fatalf("missing measured economics error=%v", err)
	}
	cfg.CostInput, cfg.CostOutput = 100_000, 300_000
	cfg.RetailInput, cfg.RetailOutput = 100_000, 300_000
	cfg.Endpoint = "https://different.api.runpod.ai"
	if err = run(cfg, now); err == nil || !strings.Contains(err.Error(), "bind the expected supplier origin") {
		t.Fatalf("target-origin drift error=%v", err)
	}
}

func writeTestQualification(t *testing.T, now time.Time, p profile, endpoint string) string {
	t.Helper()
	ttft, throughput := 100.0, 200.0
	manifest := qualificationManifest{
		OfferID: p.OfferID, OfferVersion: 2,
		Evidence: modelapisupply.QualificationEvidence{
			ID: "qualification-" + p.ProductID, State: modelapisupply.QualificationQualified,
			TupleKey: p.ExpectedTuple, Protocol: "openai", Region: p.Region,
			Capabilities: []string{"chat-completions", "streaming"},
			Scope:        "mvp;revision=" + p.ExpectedRevision + ";target_origin_sha256=" + digestString(endpoint),
			EvidenceRef:  "s3://qualification/evidence.json", EvidenceDigest: "sha256:" + strings.Repeat("a", 64),
			ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(2 * time.Hour), SampleCount: 6,
			TTFTP95MS: &ttft, OutputTokensP5: &throughput,
		},
	}
	path := filepath.Join(t.TempDir(), "qualification.json")
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, target); err != nil {
		t.Fatal(err)
	}
}
