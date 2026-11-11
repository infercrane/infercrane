package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const campaignWarningsHeading = "Proposal warnings"

// serveCampaign answers every campaign action with one campaign whose
// warnings field is the given raw JSON, or absent when warnings is empty.
func serveCampaign(t *testing.T, warnings string) {
	t.Helper()
	field := ""
	if warnings != "" {
		field = `,"warnings":` + warnings
	}
	body := `{"campaign":{"id":"campaign-1","model_identity":"acme/model","objective":"interactive","source":"recipe-search-v1","intent":"new_endpoint","state":"awaiting_approval"` + field + `,"candidates":[{"id":"candidate-1","rank":1,"state":"proposed","evidence_state":"unmeasured"}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/optimization/campaigns/campaign-1") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
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

var campaignActions = [][]string{
	{"inspect", "campaign-1"},
	{"results", "campaign-1"},
	{"approve", "campaign-1", "--max-cost-usd", "1", "--expires-in", "10m"},
	{"activate", "campaign-1", "--candidate", "candidate-1"},
	{"cancel", "campaign-1"},
}

func TestOptimizeCampaignActionsPrintProposalWarnings(t *testing.T) {
	serveCampaign(t, `["speculative decoding skipped: no draft model is reviewed for acme/model","every recipe arm is unmeasured until benchmarked"]`)
	for _, args := range campaignActions {
		output, err := captureStdout(t, func() error { return optimizeCampaignCommand(context.Background(), args) })
		if err != nil {
			t.Fatalf("args=%v err=%v", args, err)
		}
		block := campaignWarningsHeading + "\n  - speculative decoding skipped: no draft model is reviewed for acme/model\n  - every recipe arm is unmeasured until benchmarked\n"
		header := strings.Index(output, "Optimization campaign campaign-1 · awaiting_approval")
		warnings := strings.Index(output, block)
		candidates := strings.Index(output, "CANDIDATES")
		if header < 0 || warnings < header || candidates < warnings {
			t.Fatalf("args=%v: warnings block missing or out of order:\n%s", args, output)
		}
	}
}

func TestOptimizeCampaignJSONKeepsProposalWarnings(t *testing.T) {
	serveCampaign(t, `["every recipe arm is unmeasured until benchmarked"]`)
	output, err := captureStdout(t, func() error {
		return optimizeCampaignCommand(context.Background(), []string{"inspect", "campaign-1", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Campaign struct {
			Warnings []string `json:"warnings"`
		} `json:"campaign"`
	}
	if err = json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("output=%q err=%v", output, err)
	}
	if want := []string{"every recipe arm is unmeasured until benchmarked"}; !reflect.DeepEqual(decoded.Campaign.Warnings, want) {
		t.Fatalf("warnings=%q want %q", decoded.Campaign.Warnings, want)
	}
}

func TestOptimizeCampaignWithoutWarningsPrintsNoBlock(t *testing.T) {
	for _, test := range []struct {
		name     string
		warnings string
		// jsonField is the warnings value --output json must keep, or empty
		// when the field must stay absent.
		jsonField string
	}{
		{name: "proposal recorded none", warnings: `[]`, jsonField: `[]`},
		{name: "stored proposal unreadable", warnings: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			serveCampaign(t, test.warnings)
			output, err := captureStdout(t, func() error {
				return optimizeCampaignCommand(context.Background(), []string{"inspect", "campaign-1"})
			})
			if err != nil || !strings.Contains(output, "CANDIDATES") || strings.Contains(output, campaignWarningsHeading) {
				t.Fatalf("output=%q err=%v", output, err)
			}

			output, err = captureStdout(t, func() error {
				return optimizeCampaignCommand(context.Background(), []string{"inspect", "campaign-1", "--output", "json"})
			})
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Campaign map[string]json.RawMessage `json:"campaign"`
			}
			if err = json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("output=%q err=%v", output, err)
			}
			field, present := decoded.Campaign["warnings"]
			if test.jsonField == "" {
				if present {
					t.Fatalf("unknown warnings became %s", field)
				}
				return
			}
			if !present || string(field) != test.jsonField {
				t.Fatalf("warnings=%s present=%t want %s", field, present, test.jsonField)
			}
		})
	}
}
