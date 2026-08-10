package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
)

func TestLoadUISnapshotUsesOnlyReadControlPlaneAPIs(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("TUI performed mutation: %s %s", r.Method, r.URL.Path)
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deployments":
			_, _ = w.Write([]byte(`{"data":[{"name":"qwen-prod","model":"Qwen/Qwen3-8B","observed_state":"healthy"}]}`))
		case "/api/v1/deployments/qwen-prod":
			_, _ = w.Write([]byte(`{"deployment":{"name":"qwen-prod","model":"Qwen/Qwen3-8B","runtime":"vllm"},"lifecycle_status":{"serving_state":"healthy","ready_replicas":1,"desired_replicas":1}}`))
		case "/api/v1/deployments/qwen-prod/events":
			_, _ = w.Write([]byte(`{"data":[{"type":"replica.ready","summary":"replica is serving"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot := loadUISnapshot(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "")
	if snapshot.err != nil || snapshot.selected != "qwen-prod" || snapshot.view.Deployment.Name != "qwen-prod" || len(snapshot.events) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	want := strings.Join([]string{"/api/v1/deployments", "/api/v1/deployments/qwen-prod", "/api/v1/deployments/qwen-prod/events"}, ",")
	if strings.Join(paths, ",") != want {
		t.Fatalf("paths=%v want=%s", paths, want)
	}
}

func TestUIRendersDurableOperationAndExplanation(t *testing.T) {
	next := time.Date(2026, 8, 11, 12, 5, 0, 0, time.UTC)
	model := uiModel{
		cfg:         config.Config{ControlURL: "https://control.example.com"},
		width:       120,
		height:      40,
		dark:        true,
		explain:     true,
		deployments: []deploymentSummary{{Name: "qwen-prod", Model: "Qwen/Qwen3-8B", ObservedState: "unhealthy"}},
		view: deploymentView{
			Deployment:      deploymentSummary{Name: "qwen-prod", Model: "Qwen/Qwen3-8B", Runtime: "vllm", ActiveRevisionID: "qwen-prod-rev-18"},
			LifecycleStatus: lifecycleStatusView{ServingState: "unhealthy", ConvergenceState: "converging", DesiredReplicas: 1, ProvisioningReplicas: 1},
			ActiveOperation: &domain.Operation{ID: "op-capacity", Status: "waiting", Progress: 55, Attempt: 6, MaxAttempts: 120, Message: "Provider capacity: INIT", NextAttemptAt: &next},
		},
		events: []cliEvent{{Type: "step.waiting", Summary: "capacity constrained", CreatedAt: next}},
	}
	output := model.render()
	for _, expected := range []string{"operations console · read-only", "qwen-prod", "https://control.example.com/v1", "WAITING FOR CAPACITY", "check 6/120", "EXPLANATION", "blocking convergence", "capacity constrained"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("rendered UI does not contain %q:\n%s", expected, output)
		}
	}
	if got := model.copyValue(); got != "infercrane operation watch op-capacity" {
		t.Fatalf("copy value=%q", got)
	}
}

func TestUIEmptyStateIsActionable(t *testing.T) {
	output := (uiModel{width: 80, height: 24, dark: false}).render()
	if !strings.Contains(output, "No deployments") || !strings.Contains(output, "infercrane deploy MODEL") {
		t.Fatalf("empty UI is not actionable:\n%s", output)
	}
}

func TestUIIgnoresStaleRefreshResponses(t *testing.T) {
	model := uiModel{requestSeq: 2, deployments: []deploymentSummary{{Name: "current"}}}
	updated, _ := model.Update(uiSnapshotMsg{sequence: 1, deployments: []deploymentSummary{{Name: "stale"}}})
	got := updated.(uiModel)
	if len(got.deployments) != 1 || got.deployments[0].Name != "current" {
		t.Fatalf("stale response replaced current state: %#v", got.deployments)
	}
}

func TestDeploymentInfrastructureUsesImmutableRevisionSpec(t *testing.T) {
	provider, compute := deploymentInfrastructure(deploymentView{
		Deployment: deploymentSummary{ActiveRevisionID: "rev-2"},
		Revisions: []revisionView{
			{ID: "rev-1", Spec: []byte(`{"cloud":"wrong"}`)},
			{ID: "rev-2", Spec: []byte(`{"cloud":"runpod","compute_mode":"serverless"}`)},
		},
	})
	if provider != "runpod" || compute != "serverless" {
		t.Fatalf("provider=%q compute=%q", provider, compute)
	}
}

func TestUIEventTimeDistinguishesHistoricalEvents(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.FixedZone("local", 2*60*60))
	if got := uiEventTime(now.Add(-time.Minute), now); got != "11:59:00" {
		t.Fatalf("same-day event=%q", got)
	}
	if got := uiEventTime(now.Add(-48*time.Hour), now); got != "08-09 12:00" {
		t.Fatalf("historical event=%q", got)
	}
}
