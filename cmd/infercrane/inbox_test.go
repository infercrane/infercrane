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

	"github.com/infercrane/infercrane/internal/config"
)

func TestInboxRanksPersistedFleetAttentionDeterministically(t *testing.T) {
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
	if report.TotalAttention != 5 || report.Critical != 2 || report.Warnings != 3 || report.TotalEndpoints != 3 || report.TotalDeploys != 3 || report.Truncated || report.Returned != 5 || report.Evidence != "persisted_control_plane_state" {
		t.Fatalf("report=%#v", report)
	}
	want := []string{"deployment/bad-worker/deployment_degraded", "endpoint/broken-api/endpoint_not_serving", "deployment/starting-worker/deployment_not_converged", "deployment/bad-worker/deployment_candidate_requires_review", "endpoint/coder-production/endpoint_candidate_requires_review"}
	for i, item := range report.Items {
		actual := item.ResourceKind + "/" + item.ResourceName + "/" + item.Code
		if actual != want[i] {
			t.Fatalf("item[%d]=%s want %s; all=%#v", i, actual, want[i], report.Items)
		}
		if item.Evidence == "" || item.Next == "" || item.UpdatedAt.IsZero() {
			t.Fatalf("item lacks actionable evidence: %#v", item)
		}
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
