package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/proofgate"
	"github.com/infercrane/infercrane/internal/testtools/proofgatefixture"
)

// savedScenario writes a scenario's signed report as optimize report --output
// json saves it.
func savedScenario(t *testing.T, scenario string) string {
	t.Helper()
	document, err := proofgatefixture.Document(scenario, fixtureSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), scenario+".json")
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type gateOutput struct {
	proofgate.Result
	Markdown string `json:"markdown"`
}

func TestOptimizeGateFromASavedReportExitsOnTheConclusion(t *testing.T) {
	cases := []struct {
		scenario, onWatch, verdict string
		fails                      bool
	}{
		{proofgatefixture.Pass, "", proofgate.Pass, false},
		{proofgatefixture.Gain, "", proofgate.Pass, false},
		{proofgatefixture.Watch, "", proofgate.Watch, false},
		{proofgatefixture.Watch, "fail", proofgate.Watch, true},
		{proofgatefixture.Block, "pass", proofgate.Block, true},
		{proofgatefixture.AnswersBlocked, "", proofgate.Block, true},
		{proofgatefixture.Unknown, "", proofgate.Unknown, true},
		{proofgatefixture.Running, "", proofgate.Unknown, true},
	}
	for _, tc := range cases {
		args := []string{"optimize", "gate", "--file", savedScenario(t, tc.scenario), "--output", "json", "--comment-key", "deploy/qwen.yaml"}
		if tc.onWatch != "" {
			args = append(args, "--on-watch", tc.onWatch)
		}
		stdout, stderr, err := runCLI(t, args...)
		if failed := err != nil; failed != tc.fails {
			t.Fatalf("%s on-watch %q: error %v, want failure %v\n%s", tc.scenario, tc.onWatch, err, tc.fails, stderr)
		}
		var gateFailed errGateFailed
		if tc.fails && !errors.As(err, &gateFailed) {
			t.Fatalf("%s: a failed check is not a usage error: %v", tc.scenario, err)
		}
		var printed gateOutput
		if decodeErr := json.Unmarshal([]byte(stdout), &printed); decodeErr != nil {
			t.Fatalf("%s: stdout is not the gate result even when the check fails: %v\n%s", tc.scenario, decodeErr, stdout)
		}
		if printed.Verdict != tc.verdict || printed.SchemaVersion != proofgate.SchemaVersion || !strings.HasPrefix(printed.Markdown, "<!-- infercrane-proof-gate:deploy/qwen.yaml -->") {
			t.Fatalf("%s: printed %s, markdown %q", tc.scenario, printed.Verdict, printed.Markdown)
		}
		if tc.scenario != proofgatefixture.Running && !printed.Signature.Verified {
			t.Fatalf("%s: a finished fixture report is signed and verifies: %+v", tc.scenario, printed.Signature)
		}
	}
}

func TestOptimizeGateNeverTrustsAChangedOrForeignReport(t *testing.T) {
	path := savedScenario(t, proofgatefixture.Block)
	body, _ := os.ReadFile(path)
	// Turning the regression into "no difference" breaks the digest.
	tampered := bytes.Replace(body, []byte(`"decision":"regressed"`), []byte(`"decision":"no_measurable_difference"`), 1)
	if bytes.Equal(tampered, body) {
		t.Fatal("fixture has no regression decision to tamper with")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, "optimize", "gate", "--file", path, "--output", "json")
	var printed gateOutput
	_ = json.Unmarshal([]byte(stdout), &printed)
	if err == nil || printed.Verdict != proofgate.Block && printed.Verdict != proofgate.Unknown || printed.Signature.Error == "" {
		t.Fatalf("a changed report must never pass: %v %s %+v", err, printed.Verdict, printed.Signature)
	}

	other, _, keyErr := passport.GenerateKey()
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	stdout, _, err = runCLI(t, "optimize", "gate", "--file", savedScenario(t, proofgatefixture.Pass), "--public-key", base64.StdEncoding.EncodeToString(other), "--output", "json")
	printed = gateOutput{}
	_ = json.Unmarshal([]byte(stdout), &printed)
	if err == nil || printed.Verdict != proofgate.Unknown || !strings.Contains(printed.Signature.Error, "not the key you trust") {
		t.Fatalf("a report signed by another key is unknown: %v %s %+v", err, printed.Verdict, printed.Signature)
	}
	own := base64.StdEncoding.EncodeToString(fixtureSigningKey.Public().(ed25519.PublicKey))
	stdout, _, err = runCLI(t, "optimize", "gate", "--file", savedScenario(t, proofgatefixture.Pass), "--public-key", own)
	if err != nil || !strings.Contains(stdout, "Proof gate · PASS · check success") {
		t.Fatalf("pinned to the signing key: %v\n%s", err, stdout)
	}
}

func TestOptimizeGateRefusesBadUsage(t *testing.T) {
	for _, args := range [][]string{
		{"optimize", "gate"},
		{"optimize", "gate", "run-pass", "--file", "report.json"},
		{"optimize", "gate", "--file", "report.json", "--output", "yaml"},
		{"optimize", "gate", "--file", "report.json", "--on-watch", "ignore"},
	} {
		if _, _, err := runCLI(t, args...); err == nil || errors.As(err, new(errGateFailed)) {
			t.Fatalf("%v: want a usage error, got %v", args, err)
		}
	}
}

const gateSpec = `apiVersion: infercrane.dev/v1
kind: Deployment
name: qwen-chat
model:
  id: Qwen/Qwen3-8B
  revision: b968826d9c46dd6066d109eabc6255188de91218
runtime:
  engine: vllm
  version: 0.22.1
  args: [%s]
resources:
  gpu: L40S
provider:
  cloud: runpod
`

func TestCompareApproveAndGateAgainstAFakeControlPlane(t *testing.T) {
	fake := &proofgatefixture.Server{Key: fixtureSigningKey, APIKey: "test-secret", OnCreate: proofgatefixture.Block, RunningReads: 1}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	directory := t.TempDir()
	current, proposed := filepath.Join(directory, "current.yaml"), filepath.Join(directory, "proposed.yaml")
	_ = os.WriteFile(current, []byte(strings.Replace(gateSpec, "%s", "", 1)), 0o600)
	_ = os.WriteFile(proposed, []byte(strings.Replace(gateSpec, "%s", "--max-num-batched-tokens, '8192'", 1)), 0o600)

	stdout, stderr, err := runCLI(t, "optimize", "compare", current, proposed, "--objective", "latency", "--output", "json")
	if err != nil {
		t.Fatalf("compare: %v\n%s", err, stderr)
	}
	var created struct {
		Campaign struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"campaign"`
	}
	if err = json.Unmarshal([]byte(stdout), &created); err != nil || created.Campaign.ID != "run-block" || created.Campaign.State != "awaiting_approval" {
		t.Fatalf("compare output %v:\n%s", err, stdout)
	}
	proposals := fake.Proposals()
	if len(proposals) != 1 || proposals[0].AlgorithmVersion != optimizer.ComparisonAlgorithmVersion || len(proposals[0].Candidates) != 2 || strings.Join(proposals[0].Candidates[1].Deployment.Runtime.Args, " ") != "--max-num-batched-tokens 8192" {
		t.Fatalf("the control plane received %+v", proposals)
	}

	if _, stderr, err = runCLI(t, "optimize", "approve", "run-block", "--max-cost-usd", "12", "--expires-in", "3h", "--yes", "--output", "json"); err != nil {
		t.Fatalf("approve: %v\n%s", err, stderr)
	}
	if fake.Approved()["run-block"] != 12 {
		t.Fatalf("approved caps %v", fake.Approved())
	}

	// The first read finds the run measuring; the gate waits for it.
	stdout, _, err = runCLI(t, "optimize", "gate", "run-block", "--wait", "30s", "--poll", "1s", "--output", "markdown")
	if !errors.As(err, new(errGateFailed)) || !strings.Contains(stdout, "### ❌ BLOCK · InferCrane proof gate") || !strings.Contains(stdout, server.URL+"/api/v1/optimization/campaigns/run-block/report") {
		t.Fatalf("gate after waiting: %v\n%s", err, stdout)
	}

	if _, stderr, err = runCLI(t, "optimize", "cancel", "run-block", "--output", "json"); err != nil {
		t.Fatalf("cancel: %v\n%s", err, stderr)
	}
	if cancelled := fake.Cancelled(); len(cancelled) != 1 || cancelled[0] != "run-block" {
		t.Fatalf("cancelled %v", cancelled)
	}

	// A wait that ends before the run finishes is decided as it stands.
	waiting := &proofgatefixture.Server{Key: fixtureSigningKey}
	slow := httptest.NewServer(waiting)
	t.Cleanup(slow.Close)
	t.Setenv("INFERCRANE_URL", slow.URL)
	stdout, _, err = runCLI(t, "optimize", "gate", "run-running", "--output", "json")
	var printed gateOutput
	_ = json.Unmarshal([]byte(stdout), &printed)
	if err == nil || printed.Verdict != proofgate.Unknown || printed.Finished {
		t.Fatalf("an unfinished run is unknown and fails: %v %+v", err, printed.Result)
	}
}
