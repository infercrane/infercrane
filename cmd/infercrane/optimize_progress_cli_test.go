package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// serveCampaignBody answers every campaign read with body.
func serveCampaignBody(t *testing.T, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/optimization/campaigns/campaign-1") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
}

const liveCampaignBody = `{"campaign":{"id":"campaign-1","model_identity":"Qwen/Qwen3-0.6B","objective":"interactive","source":"fixture","intent":"new_endpoint","state":"ranked","approved_max_cost_usd":3.3,
"progress":{"stage":"running","started_at":"2026-11-17T01:15:00Z","ended_at":null,"elapsed_seconds":2460,"setups_total":2,"setups_measured":1,"turns_done":4,"turns_total":10,"eta":{"state":"estimated","basis":"observed_durations","remaining_seconds":3120,"completes_at":"2026-11-17T02:48:00Z","observed":{"turns":4}}},
"spend":{"state":"estimated","basis":"quoted_rate_x_replica_seconds","label":"estimated from quoted rate","estimated_usd":0.38,"replica_seconds":4140,"cap_usd":3.3},
"candidates":[
{"id":"control","rank":1,"state":"ranked","evidence_state":"measured","label":"Your current setup (vLLM defaults)","control":true,"state_entered_at":{"proposed":"2026-11-17T01:10:00Z"},"measurement":null},
{"id":"ngram","rank":2,"state":"measuring","evidence_state":"unmeasured","label":"Speculative decoding (n-gram)","control":false,"state_entered_at":{"measuring":"2026-11-17T01:40:00Z"},"measurement":{"method":"campaign-interleaved-control-v1","turns_planned":7,"turns_done":1,"current":{"kind":"turn","arm":"candidate","turn":2,"started_at":"2026-11-17T01:44:00Z","requests":200},"runs":[{"kind":"turn","arm":"control","turn":1,"started_at":"2026-11-17T01:42:00Z","ended_at":"2026-11-17T01:44:00Z","requests":200,"output_token_throughput":246}]}}]}}`

func TestOptimizeCampaignJSONPassesRunProgressThroughUnchanged(t *testing.T) {
	serveCampaignBody(t, liveCampaignBody)
	output, err := captureStdout(t, func() error {
		return optimizeCampaignCommand(context.Background(), []string{"inspect", "campaign-1", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var printed, served struct {
		Campaign struct {
			Progress   json.RawMessage `json:"progress"`
			Spend      json.RawMessage `json:"spend"`
			Candidates []map[string]json.RawMessage
		} `json:"campaign"`
	}
	if err = json.Unmarshal([]byte(output), &printed); err != nil {
		t.Fatalf("output=%q err=%v", output, err)
	}
	_ = json.Unmarshal([]byte(liveCampaignBody), &served)
	if !jsonEqual(printed.Campaign.Progress, served.Campaign.Progress) || !jsonEqual(printed.Campaign.Spend, served.Campaign.Spend) {
		t.Fatalf("progress or spend changed in --output json:\n%s", output)
	}
	for index, candidate := range printed.Campaign.Candidates {
		for _, key := range []string{"label", "control", "state_entered_at", "measurement"} {
			if !jsonEqual(candidate[key], served.Campaign.Candidates[index][key]) {
				t.Fatalf("candidate %d %s changed: %s", index, key, candidate[key])
			}
		}
	}

	// A control plane without the fields gets none invented.
	serveCampaignBody(t, `{"campaign":{"id":"campaign-1","state":"running","candidates":[{"id":"a","rank":1,"state":"measuring","evidence_state":"unmeasured"}]}}`)
	output, err = captureStdout(t, func() error {
		return optimizeCampaignCommand(context.Background(), []string{"inspect", "campaign-1", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"label"`, `"control"`, `"progress"`, `"spend"`, `"measurement"`} {
		if strings.Contains(output, key) {
			t.Fatalf("--output json invented %s for an older control plane:\n%s", key, output)
		}
	}
}

func jsonEqual(left, right json.RawMessage) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && mustJSON(a) == mustJSON(b)
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestOptimizeResultsIsPlainWhenNotATerminal(t *testing.T) {
	serveCampaignBody(t, liveCampaignBody)
	output, err := captureStdout(t, func() error {
		return optimizeCampaignCommand(context.Background(), []string{"results", "campaign-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "\x1b[") {
		t.Fatalf("results wrote ANSI to a pipe:\n%q", output)
	}
	for _, want := range []string{"Setup 2 of 2 · turn 2 of 7", "Speculative decoding (n-gram)", "estimated from quoted rate", "Still running. Follow it live"} {
		if !strings.Contains(output, want) {
			t.Fatalf("results lacks %q:\n%s", want, output)
		}
	}
}

func TestStyledCampaignDetailNamesSetups(t *testing.T) {
	marked := true
	view := optimizationCampaignView{ID: "campaign-1", ModelIdentity: "Qwen/Qwen3-0.6B", State: "running"}
	view.Candidates = append(view.Candidates,
		optimizationCampaignSetupView{ID: "control", State: "ranked", Rank: 1, Label: "vLLM defaults", Control: &marked},
		optimizationCampaignSetupView{ID: "ngram", State: "measuring", Rank: 2, Label: "Speculative decoding (n-gram)"})
	described := map[string]string{}
	for _, candidate := range view.Candidates {
		described[candidate.ID] = approvalSetup{ID: candidate.ID, Label: candidate.Label}.describe()
	}
	text := ansi.Strip(renderCampaign(styledTheme(100, "dark"), view, described, nil, nil, nil, styledNow))
	for _, want := range []string{"current setup · vLLM defaults", "Speculative decoding (n-gram)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("styled campaign lacks %q:\n%s", want, text)
		}
	}
}
