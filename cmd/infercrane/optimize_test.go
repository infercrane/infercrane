package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/spec"
)

func TestOptimizeProposeRunsOfflineAndEmitsUnmeasuredJSON(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	output, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"optimize", "propose", "mistral-7b-instruct", "--provider", "aws", "--region", "eu-central-1", "--gpu", "L40S", "--objective", "interactive", "--max-error-rate", "0.01", "--min-goodput", "4", "--source", "catalog", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var proposal optimizer.Proposal
	if err = json.Unmarshal([]byte(output), &proposal); err != nil {
		t.Fatalf("decode proposal: %v\n%s", err, output)
	}
	if proposal.Mutation != "none" || len(proposal.Candidates) != 3 || proposal.Candidates[0].Status != "proposed-unmeasured" || proposal.Candidates[0].ConfigurationProfile != "vllm-interactive" {
		t.Fatalf("proposal=%#v", proposal)
	}
	if proposal.Input.MaxErrorRate == nil || *proposal.Input.MaxErrorRate != 0.01 || proposal.Input.MinGoodput == nil || *proposal.Input.MinGoodput != 4 {
		t.Fatalf("full-path SLO evidence requirements were lost: %+v", proposal.Input)
	}
}

func TestOptimizeProposeRejectsInvalidErrorRate(t *testing.T) {
	err := optimizeCommand(context.Background(), []string{"propose", "qwen3-8b", "--provider", "aws", "--region", "eu-central-1", "--gpu", "L40S", "--max-error-rate", "1.1", "--source", "catalog"})
	if err == nil || !strings.Contains(err.Error(), "between zero and one") {
		t.Fatalf("invalid error-rate policy was accepted: %v", err)
	}
}

func TestOptimizeProposeWritesLoadableImmutableDeploymentSpecs(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "candidates")
	if err := optimizeCommand(context.Background(), []string{"propose", "mistral-7b-instruct", "--provider", "aws", "--region", "eu-central-1", "--gpu", "L40S", "--source", "catalog", "--write-dir", directory, "--output", "json"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		loaded, loadErr := spec.Load(filepath.Join(directory, entry.Name()))
		if loadErr != nil {
			t.Fatalf("load %s: %v", entry.Name(), loadErr)
		}
		if loaded.Model.Revision == "" || loaded.Runtime.Version == "" || loaded.Provider.Adapter != "aws-ec2" {
			t.Fatalf("candidate lost immutable identity: %#v", loaded)
		}
	}
	if err = optimizeCommand(context.Background(), []string{"propose", "mistral-7b-instruct", "--provider", "aws", "--region", "eu-central-1", "--gpu", "L40S", "--source", "catalog", "--write-dir", directory}); err == nil || !strings.Contains(err.Error(), "must be empty") {
		t.Fatalf("existing evidence directory was overwritten: %v", err)
	}
}

func TestOptimizeProposeFailsClosedWithoutReviewedModel(t *testing.T) {
	output, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"propose", "unreviewed/model", "--provider", "gcp", "--region", "europe-west4", "--gpu", "nvidia-l4", "--source", "catalog"})
	})
	if err != nil || !strings.Contains(output, "reviewed_model_recipe") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestOptimizeAutoFallbackIsExplicitAndDeterministic(t *testing.T) {
	missingPython := filepath.Join(t.TempDir(), "missing-python")
	output, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"propose", "qwen3-8b", "--provider", "aws", "--region", "eu-central-1", "--gpu", "L40S", "--source", "auto", "--aiconfigurator-python", missingPython, "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var proposal optimizer.Proposal
	if err = json.Unmarshal([]byte(output), &proposal); err != nil {
		t.Fatal(err)
	}
	if len(proposal.Warnings) != 1 || !strings.Contains(proposal.Warnings[0], "unavailable") || proposal.Candidates[0].EvidenceState != "unmeasured" {
		t.Fatalf("fallback boundary is not explicit: %+v", proposal)
	}
}

func TestOptimizeCreatePersistsProposalWithoutProviderMutation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/optimization/campaigns" || r.Header.Get("Idempotency-Key") == "" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatalf("unexpected request %s %s key=%q auth=%q", r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"))
		}
		var body struct {
			Proposal optimizer.Proposal `json:"proposal"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || optimizer.ValidateProposal(body.Proposal) != nil {
			t.Fatalf("invalid proposal body=%+v err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"campaign":{"id":"campaign-1","model_identity":"` + body.Proposal.Input.ModelIdentity + `","objective":"interactive","source":"catalog-candidate-planner-v1","state":"awaiting_approval","candidates":[{"id":"candidate-1","rank":1,"state":"proposed","evidence_state":"unmeasured"}]},"created":true,"provider_mutation":false}`))
	}))
	defer server.Close()
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	output, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"create", "mistral-7b-instruct", "--provider", "aws", "--region", "eu-central-1", "--gpu", "L40S", "--source", "catalog"})
	})
	if err != nil || !strings.Contains(output, "Optimization campaign campaign-1 · awaiting_approval") || !strings.Contains(output, "Mutation    none") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestOptimizeCampaignActionsAcceptSubjectBeforeFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/optimization/campaigns/campaign-1":
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/optimization/campaigns/campaign-1/approve":
			var body struct {
				MaxCostUSD      float64 `json:"max_cost_usd"`
				ExpiresInSecond int     `json:"expires_in_seconds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MaxCostUSD != 1 || body.ExpiresInSecond != 600 {
				t.Fatalf("approve body=%+v err=%v", body, err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/optimization/campaigns/campaign-1/cancel":
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"campaign":{"id":"campaign-1","model_identity":"llama-3.1-8b-instruct","objective":"interactive","source":"catalog-candidate-planner-v1","state":"awaiting_approval","candidates":[]}}`))
	}))
	defer server.Close()
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))

	for _, args := range [][]string{
		{"inspect", "campaign-1", "--output", "json"},
		{"approve", "campaign-1", "--max-cost-usd", "1", "--expires-in", "10m", "--output", "json"},
		{"cancel", "campaign-1", "--output", "json"},
	} {
		output, err := captureStdout(t, func() error { return optimizeCampaignCommand(context.Background(), args) })
		if err != nil || !strings.Contains(output, `"id": "campaign-1"`) {
			t.Fatalf("args=%v output=%q err=%v", args, output, err)
		}
	}
}
