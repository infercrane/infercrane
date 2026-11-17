package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/spf13/cobra"
)

// The CLI uses the dashboard's words: a run tests setups, a winner is rolled
// out, and a rollout can be rolled back. "Campaign" and "candidate" are the
// control API's names. They stay in command names, flag names, JSON, API
// paths, and codes, and never appear as prose a customer reads.
var (
	apiRunNoun   = regexp.MustCompile(`(?i)campaigns?`)
	apiSetupNoun = regexp.MustCompile(`(?i)candidates?`)
	// Flag names are stable for scripts: --candidate, --max-candidates.
	candidateFlagName = regexp.MustCompile(`--(max-)?candidates?\b`)
	// Codes and enum values print as the API's identifiers.
	apiIdentifier = regexp.MustCompile(`\b[a-z0-9]+(_[a-z0-9]+)+\b`)
)

// revisionCandidateHelp lists the commands whose help says "candidate" for a
// deployment's staged revision or an endpoint's staged serving plan. That is
// the control API's slot name and a --revision value, not a setup of a run.
var revisionCandidateHelp = map[string]bool{"rollout": true, "environment": true, "benchmark": true, "replay": true}

func TestCLIHelpSaysRunAndSetup(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(command *cobra.Command) {
		text := command.Short + "\n" + command.Long + "\n" + command.UsageString()
		if match := apiRunNoun.FindString(text); match != "" {
			t.Errorf("%s help says %q; say run", command.CommandPath(), match)
		}
		if !revisionCandidateHelp[command.Name()] {
			if match := apiSetupNoun.FindString(candidateFlagName.ReplaceAllString(text, "")); match != "" {
				t.Errorf("%s help says %q; say setup", command.CommandPath(), match)
			}
		}
		for _, child := range command.Commands() {
			walk(child)
		}
	}
	root := newRootCommand(context.Background())
	walk(root)

	help := func(name string) string {
		command, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		return command.Long + "\n" + command.UsageString()
	}
	for _, want := range []string{"optimization run", "setups", "activate  roll out the winning setup", "--candidate SETUP_ID", "optimize report RUN_ID"} {
		if !strings.Contains(help("optimize"), want) {
			t.Errorf("optimize help lacks %q:\n%s", want, help("optimize"))
		}
	}
	for _, want := range []string{"# Roll out the candidate revision", "rollout promote production", "# Roll back to an earlier revision", "rollout rollback production"} {
		if !strings.Contains(help("rollout"), want) {
			t.Errorf("rollout help lacks %q:\n%s", want, help("rollout"))
		}
	}
}

// customerProse removes what is not prose: flag names and API identifiers.
func customerProse(text string) string {
	return apiIdentifier.ReplaceAllString(candidateFlagName.ReplaceAllString(ansi.Strip(text), ""), "")
}

func assertCustomerVocabulary(t *testing.T, name, text string) {
	t.Helper()
	prose := customerProse(text)
	for _, noun := range []*regexp.Regexp{apiRunNoun, apiSetupNoun} {
		if match := noun.FindString(prose); match != "" {
			t.Errorf("%s says %q:\n%s", name, match, text)
		}
	}
}

func TestOptimizeUsageSaysRunAndSetup(t *testing.T) {
	for _, action := range []string{"inspect", "results", "approve", "activate", "cancel", "watch", "report"} {
		err := optimizeCommand(context.Background(), []string{action})
		if err == nil || !strings.Contains(err.Error(), "usage: infercrane optimize "+action+" RUN") {
			t.Errorf("optimize %s usage = %v", action, err)
		}
	}
	serveCampaign(t, "")
	err := optimizeCommand(context.Background(), []string{"activate", "campaign-1"})
	if err == nil || !strings.Contains(err.Error(), "--candidate SETUP_ID") || !strings.Contains(err.Error(), "roll out") {
		t.Fatalf("activate without a setup = %v", err)
	}
	assertCustomerVocabulary(t, "activate error", err.Error())
}

// vocabularyRun is a decided run as the control API returns it: the winner is
// "promoted" and the current setup it replaced is "rejected".
const vocabularyRun = `{"id":"run-1","model_identity":"Qwen/Qwen3-0.6B","objective":"interactive","source":"fixture","intent":"evolve_endpoint","target_deployment":"qwen-chat","state":"promoted","approved_max_cost_usd":3.3,"created_at":"2026-11-17T01:00:00Z",
"progress":{"stage":"decided","elapsed_seconds":5400,"setups_total":2},
"candidates":[
{"id":"setup-a","rank":1,"state":"rejected","evidence_state":"rejected","failure_code":"measured_candidate_superseded","label":"vLLM defaults","control":true},
{"id":"setup-b","rank":2,"state":"promoted","evidence_state":"qualified","label":"FP8 KV cache","control":false}]}`

func serveVocabularyRun(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/optimization/campaigns":
			_, _ = w.Write([]byte(`{"data":[` + vocabularyRun + `]}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/optimization/campaigns/run-1"):
			_, _ = w.Write([]byte(`{"campaign":` + vocabularyRun + `,"activation_operation":{"id":"op-2","status":"succeeded"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
}

func TestOptimizeOutputSaysRunSetupAndRollOut(t *testing.T) {
	serveVocabularyRun(t)
	plain := map[string]string{}
	for _, args := range [][]string{{"list"}, {"inspect", "run-1"}, {"results", "run-1"}, {"activate", "run-1", "--candidate", "setup-b"}, {"cancel", "run-1"}} {
		output, err := captureStdout(t, func() error { return optimizeCommand(context.Background(), args) })
		if err != nil {
			t.Fatalf("optimize %v: %v", args, err)
		}
		plain[args[0]] = output
		assertCustomerVocabulary(t, "optimize "+args[0], output)
	}
	for _, want := range []string{"Optimization run run-1 · promoted", "Rollout    op-2 · succeeded", "\nSETUPS\n", "vLLM defaults (current setup)"} {
		if !strings.Contains(plain["inspect"], want) {
			t.Errorf("plain inspect lacks %q:\n%s", want, plain["inspect"])
		}
	}
	// The watch view names the replaced current setup the baseline.
	for _, want := range []string{"vLLM defaults · current setup", "baseline · another setup measured better", "FP8 KV cache                                  rolled out", "“FP8 KV cache” is live"} {
		if !strings.Contains(plain["results"], want) {
			t.Errorf("results lacks %q:\n%s", want, plain["results"])
		}
	}
	for _, wrong := range []string{"rejected", "promoted"} {
		if strings.Contains(plain["results"], wrong) {
			t.Errorf("results says %q:\n%s", wrong, plain["results"])
		}
	}

	// A person at a terminal reads the styled views.
	marked, other := true, false
	view := optimizationCampaignView{ID: "run-1", ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Intent: "evolve_endpoint", Source: "fixture", State: "promoted", Candidates: []optimizationCampaignSetupView{
		{ID: "setup-a", State: "rejected", EvidenceState: "rejected", FailureCode: "measured_candidate_superseded", Rank: 1, Label: "vLLM defaults", Control: &marked},
		{ID: "setup-b", State: "promoted", EvidenceState: "qualified", Rank: 2, Label: "FP8 KV cache", Control: &other},
	}}
	now := time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	for _, width := range []int{80, 120} {
		list := ansi.Strip(renderCampaignList(styledTheme(width, "dark"), []optimizationCampaignView{view}, []campaignTimes{{CreatedAt: now.Add(-2 * time.Hour)}}, now))
		assertCustomerVocabulary(t, "styled list", list)
		if !strings.HasPrefix(list, "RUN ") || !strings.Contains(list, "● ROLLED OUT") || strings.Contains(list, "PROMOTED") {
			t.Errorf("width %d: styled list does not say the run was rolled out:\n%s", width, list)
		}
		detail := ansi.Strip(renderCampaign(styledTheme(width, "dark"), view, map[string]string{"setup-a": "vLLM defaults", "setup-b": "FP8 KV cache"}, nil, &domain.Operation{ID: "op-2", Status: "succeeded"}, nil, now))
		assertCustomerVocabulary(t, "styled inspect", detail)
		for _, want := range []string{"Optimization run", "● ROLLED OUT", "Rollout", "● BASELINE", "current setup · vLLM defaults"} {
			if !strings.Contains(detail, want) {
				t.Errorf("width %d: styled inspect lacks %q:\n%s", width, want, detail)
			}
		}
		for _, wrong := range []string{"PROMOTED", "REJECTED", "rejected", "Activation"} {
			if strings.Contains(detail, wrong) {
				t.Errorf("width %d: styled inspect says %q:\n%s", width, wrong, detail)
			}
		}
	}
	// A setup that is not the current one keeps the API's verdict.
	view.Candidates[1].State = "rejected"
	if detail := ansi.Strip(renderCampaign(styledTheme(100, "dark"), view, nil, nil, nil, nil, now)); !strings.Contains(detail, "● REJECTED") || !strings.Contains(detail, "● BASELINE") {
		t.Errorf("a rejected setup lost its verdict:\n%s", detail)
	}
}
