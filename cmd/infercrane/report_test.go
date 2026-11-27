package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/proofreport"
)

// signedProof is a finished report of one run whose current setup was kept,
// signed with a fresh key, as the control plane serves it.
func signedProof(t *testing.T) ([]byte, proofreport.Document, ed25519.PublicKey) {
	t.Helper()
	public, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	report := proofreport.Assemble(proofreport.Input{Title: "Proof Report", Runs: []proofreport.RunEvidence{{Claim: "not shown",
		Report: optimizationreport.Report{CampaignID: "run-a", ModelIdentity: "Qwen/Qwen3-8B", Finished: true, Outcome: optimizationreport.Outcome{Kind: optimizationreport.OutcomeBaselineKept, Headline: "Keep your current setup."}}}}})
	document, err := proofreport.Sign(report, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(document)
	return raw, document, public
}

func TestReportProofWritesEveryFormat(t *testing.T) {
	raw, document, _ := signedProof(t)
	dir := t.TempDir()
	htmlPath, markdownPath := filepath.Join(dir, "proof.html"), filepath.Join(dir, "proof.md")
	var stdout, stderr bytes.Buffer
	if err := writeProofReport(&stdout, &stderr, raw, document, "text", htmlPath, markdownPath); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "Proof Report\n") || !strings.Contains(stdout.String(), "Signed by key") || !strings.Contains(stderr.String(), "HTML report written to") {
		t.Fatalf("text = %s\nstderr = %s", stdout.String(), stderr.String())
	}
	page, _ := os.ReadFile(htmlPath)
	markdown, _ := os.ReadFile(markdownPath)
	if !strings.HasPrefix(string(page), "<!doctype html>") || !strings.HasPrefix(string(markdown), "# Proof Report") {
		t.Fatalf("files: %.40s / %.40s", page, markdown)
	}
	for output, prefix := range map[string]string{"json": "{\n  \"report\"", "markdown": "# Proof Report", "html": "<!doctype html>"} {
		stdout.Reset()
		if err := writeProofReport(&stdout, &stderr, raw, document, output, "", ""); err != nil || !strings.HasPrefix(stdout.String(), prefix) {
			t.Fatalf("%s: %v %.40s", output, err, stdout.String())
		}
	}
}

// A report saved with --output json verifies offline; one changed byte of
// content, or another trusted key, fails.
func TestReportProofVerify(t *testing.T) {
	raw, _, public := signedProof(t)
	var indented bytes.Buffer
	_ = json.Indent(&indented, raw, "", "  ")
	path := filepath.Join(t.TempDir(), "proof.json")
	if err := os.WriteFile(path, indented.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := reportProofVerifyCommand(&stdout, []string{path, "--public-key", base64.StdEncoding.EncodeToString(public)}); err != nil || !strings.Contains(stdout.String(), "VERIFIED") || !strings.Contains(stdout.String(), "Runs      run-a") || !strings.Contains(stdout.String(), "matches --public-key") {
		t.Fatalf("verify: %v %s", err, stdout.String())
	}
	changed := strings.Replace(indented.String(), `"Keep your current setup."`, `"Roll out the new setup."`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reportProofVerifyCommand(&stdout, []string{path}); err == nil || !strings.Contains(err.Error(), "the report was changed") {
		t.Fatalf("changed report: %v", err)
	}
}

func TestReportProofUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"campaign"}, {"proof"}, {"proof", "--title", "x"}, {"proof", "run-a", "--output", "pdf"}, {"proof", "verify"}} {
		if err := reportCommand(t.Context(), args); err == nil || !strings.Contains(err.Error(), "usage: infercrane report proof") {
			t.Errorf("%q: %v", args, err)
		}
	}
	if err := reportCommand(t.Context(), []string{"proof", "run-a", "run-a"}); err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Fatalf("repeated run: %v", err)
	}
}
