package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

func TestInboxRanksPersistedFleetAttentionDeterministically(t *testing.T) {
	now := time.Now().UTC()
	at := func(offset time.Duration) string { return now.Add(offset).Format(time.RFC3339Nano) }
	campaigns := fmt.Sprintf(`{"data":[
      {"id":"run-approval","model_identity":"Qwen/Qwen3-8B","state":"awaiting_approval","created_at":%[1]q,"updated_at":%[1]q,"candidates":[]},
      {"id":"run-winner","model_identity":"Qwen/Qwen3-8B","state":"qualified","approved_at":%[2]q,"approval_expires_at":%[3]q,"updated_at":%[1]q,"candidates":[{"id":"cand-1","state":"qualified","updated_at":%[1]q}]},
      {"id":"run-failed","model_identity":"Qwen/Qwen3-8B","state":"cleaned","failure_code":"approval_expired","updated_at":%[1]q,"candidates":[{"id":"cand-2","state":"cleaned","failure_code":"approval_expired","updated_at":%[1]q}]},
      {"id":"run-quality","model_identity":"Qwen/Qwen3-8B","state":"running","approved_at":%[4]q,"approval_expires_at":%[5]q,"updated_at":%[4]q,"candidates":[{"id":"cand-3","state":"validating","deployment_name":"qwen-opt-cand-3","updated_at":%[6]q,"quality":{"state":"waiting_for_evidence"}}]},
      {"id":"run-checking","model_identity":"Qwen/Qwen3-8B","state":"running","approved_at":%[4]q,"approval_expires_at":%[5]q,"updated_at":%[4]q,"candidates":[{"id":"cand-5","state":"validating","updated_at":%[6]q,"quality":{"state":"checking"}}]},
      {"id":"run-cancelled","model_identity":"Qwen/Qwen3-8B","state":"awaiting_approval","cancel_requested":true,"updated_at":%[1]q,"candidates":[]},
      {"id":"run-done","model_identity":"Qwen/Qwen3-8B","state":"cleaned","updated_at":%[1]q,"candidates":[{"id":"cand-4","state":"cleaned","updated_at":%[1]q}]}
    ]}`, at(-time.Minute), at(-3*time.Hour), at(10*time.Minute), at(-time.Hour), at(5*time.Hour), at(-10*time.Minute))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/endpoints":
			_, _ = io.WriteString(w, `{"data":[
              {"name":"coder-production","observed_state":"serving","candidate_serving_plan_id":"plan-2","updated_at":"2026-08-13T20:00:00Z"},
              {"name":"broken-api","observed_state":"degraded","prompt":"never-expose-this-content","updated_at":"2026-08-13T20:01:00Z"},
              {"name":"healthy-api","observed_state":"serving","updated_at":"2026-08-13T20:02:00Z"}
            ]}`)
		case "/api/v1/deployments":
			_, _ = io.WriteString(w, `{"data":[
              {"name":"healthy-worker","observed_state":"healthy","updated_at":"2026-08-13T20:03:00Z"},
              {"name":"bad-worker","observed_state":"degraded","candidate_revision_id":"rev-2","updated_at":"2026-08-13T20:04:00Z"},
              {"name":"starting-worker","observed_state":"pending","updated_at":"2026-08-13T20:05:00Z"}
            ]}`)
		case "/api/v1/optimization/campaigns":
			_, _ = io.WriteString(w, campaigns)
		default:
			t.Fatalf("unexpected route %s", r.URL.Path)
		}
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return inboxCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report inboxReport
	if err = json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "never-expose-this-content") {
		t.Fatal("inbox copied unrecognized request content from a fleet response")
	}
	if report.TotalAttention != 9 || report.Critical != 4 || report.Warnings != 5 || report.TotalEndpoints != 3 || report.TotalDeploys != 3 || report.TotalRuns != 7 || report.Truncated || report.Returned != 9 || report.Evidence != "persisted_control_plane_state" {
		t.Fatalf("report=%#v", report)
	}
	want := []string{
		"deployment/bad-worker/deployment_degraded", "endpoint/broken-api/endpoint_not_serving", "optimization/run-failed/optimization_failed", "optimization/run-winner/optimization_decision_waiting",
		"deployment/starting-worker/deployment_not_converged", "optimization/run-approval/optimization_approval_needed", "optimization/run-quality/optimization_quality_evidence_needed",
		"deployment/bad-worker/deployment_candidate_requires_review", "endpoint/coder-production/endpoint_candidate_requires_review",
	}
	if len(report.Items) != len(want) {
		t.Fatalf("items=%#v", report.Items)
	}
	for i, item := range report.Items {
		actual := item.ResourceKind + "/" + item.ResourceName + "/" + item.Code
		if actual != want[i] {
			t.Fatalf("item[%d]=%s want %s; all=%#v", i, actual, want[i], report.Items)
		}
		if item.Evidence == "" || item.Next == "" || item.UpdatedAt.IsZero() {
			t.Fatalf("item lacks actionable evidence: %#v", item)
		}
	}
	if next := report.Items[6].Next; next != "infercrane optimize inspect run-quality" {
		t.Fatalf("quality evidence next=%q", next)
	}
	if summary := report.Items[2].Summary; !strings.Contains(summary, "spend approval ended") {
		t.Fatalf("failure summary=%q", summary)
	}
}

func TestInboxFailsClosedOnPartialFleetRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/endpoints" {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		http.Error(w, `{"error":{"code":"internal","message":"database unavailable"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := readInbox(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, 50)
	if err == nil || !strings.Contains(err.Error(), "read deployment fleet") {
		t.Fatalf("err=%v", err)
	}
}

func TestInboxLimitDoesNotHideTotalAndHumanOutputStatesEvidenceBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/endpoints" {
			_, _ = io.WriteString(w, `{"data":[{"name":"one","observed_state":"pending"},{"name":"two","observed_state":"suspended"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	report, err := readInbox(context.Background(), cfg, 1)
	if err != nil || report.TotalAttention != 2 || report.Returned != 1 || !report.Truncated || report.Items[0].ResourceName != "two" {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	human, err := captureStdout(t, func() error { printInbox(report); return nil })
	if err != nil || !strings.Contains(human, "persisted fleet evidence") || !strings.Contains(human, "does not run Doctor implicitly") || !strings.Contains(human, "rerun with a larger --limit") {
		t.Fatalf("human=%q err=%v", human, err)
	}
}

func TestInboxRejectsInvalidArgumentsBeforeNetworkAccess(t *testing.T) {
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	for _, args := range [][]string{{"unexpected"}, {"--limit", "0"}, {"--limit", "501"}, {"--output", "yaml"}} {
		if err := inboxCommand(context.Background(), cfg, args); err == nil {
			t.Fatalf("args %v unexpectedly accepted", args)
		}
	}
}

func TestInboxTreatsUnknownStateAsAttentionRatherThanHealthy(t *testing.T) {
	endpoint := endpointInboxItems(inboxEndpoint{Name: "unknown-endpoint"})
	deployment := deploymentInboxItems(deploymentSummary{Name: "unknown-deployment"})
	if len(endpoint) != 1 || endpoint[0].Code != "endpoint_state_unknown" || endpoint[0].Severity != "warning" {
		t.Fatalf("endpoint=%#v", endpoint)
	}
	if len(deployment) != 1 || deployment[0].Code != "deployment_state_unknown" || deployment[0].Severity != "warning" {
		t.Fatalf("deployment=%#v", deployment)
	}
}

func TestInboxDoesNotFlagObserveOnlyEndpoint(t *testing.T) {
	if items := endpointInboxItems(inboxEndpoint{Name: "connected", ObservedState: "observing"}); len(items) != 0 {
		t.Fatalf("observing endpoint needs attention: %#v", items)
	}
	if items := endpointInboxItems(inboxEndpoint{Name: "routed", ObservedState: "degraded"}); len(items) != 1 || items[0].Code != "endpoint_not_serving" || items[0].Severity != "critical" {
		t.Fatalf("degraded endpoint=%#v", items)
	}
}

func TestInboxBoundsLargeFleetOutputWithoutLosingTotals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/endpoints" {
			data := make([]inboxEndpoint, 600)
			for i := range data {
				data[i] = inboxEndpoint{Name: fmt.Sprintf("endpoint-%04d", i), ObservedState: "pending"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	report, err := readInbox(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, 500)
	if err != nil || report.TotalAttention != 600 || report.Returned != 500 || !report.Truncated || len(report.Items) != 500 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}
