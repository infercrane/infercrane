package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
)

func TestLoadUISnapshotUsesOnlyReadControlPlaneAPIs(t *testing.T) {
	var paths []string
	var pathsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("TUI performed mutation: %s %s", r.Method, r.URL.Path)
		}
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deployments":
			_, _ = w.Write([]byte(`{"data":[{"name":"qwen-prod","model":"Qwen/Qwen3-8B","observed_state":"healthy"}]}`))
		case "/api/v1/deployments/qwen-prod":
			_, _ = w.Write([]byte(`{"deployment":{"name":"qwen-prod","model":"Qwen/Qwen3-8B","runtime":"vllm"},"lifecycle_status":{"serving_state":"healthy","ready_replicas":1,"desired_replicas":1}}`))
		case "/api/v1/deployments/qwen-prod/events":
			_, _ = w.Write([]byte(`{"data":[{"type":"replica.ready","summary":"replica is serving"}]}`))
		case "/api/v1/deployments/qwen-prod/benchmarks":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case "/api/v1/deployments/qwen-prod/scaling-decisions":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	snapshot := loadUISnapshot(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "")
	if snapshot.err != nil || snapshot.selected != "qwen-prod" || snapshot.view.Deployment.Name != "qwen-prod" || len(snapshot.events) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	wantPaths := []string{"/api/v1/deployments", "/api/v1/deployments/qwen-prod", "/api/v1/deployments/qwen-prod/events", "/api/v1/deployments/qwen-prod/benchmarks", "/api/v1/deployments/qwen-prod/scaling-decisions"}
	sort.Strings(paths)
	sort.Strings(wantPaths)
	want := strings.Join(wantPaths, ",")
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
	for _, expected := range []string{"operations workspace", "Overview", "qwen-prod", "https://control.example.com/v1", "WAITING FOR CAPACITY", "check 6/120", "EXPLANATION", "blocking convergence"} {
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

func TestUIActionsAreStateAwareAndReadOnlyModeNeverMutates(t *testing.T) {
	view := deploymentView{Deployment: deploymentSummary{Name: "prod", Model: "model", CandidateRevisionID: "rev-2", MinReplicas: 1, MaxReplicas: 4}, ActiveOperation: &domain.Operation{ID: "op-1"}, ReleaseGuardEvaluations: []releaseGuardView{{CandidateRevisionID: "rev-2", Decision: "pass"}}}
	mutable := uiModel{view: view, deployments: []deploymentSummary{{Name: "prod"}}}.availableActions()
	joined := ""
	for _, action := range mutable {
		joined += action.name + ","
	}
	for _, expected := range []string{"cancel", "evaluate", "promote"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing applicable action %q in %q", expected, joined)
		}
	}
	for _, action := range (uiModel{readOnly: true, view: view, deployments: []deploymentSummary{{Name: "prod"}}}).availableActions() {
		if action.method != "COPY" {
			t.Fatalf("read-only action can mutate: %#v", action)
		}
	}
}

func TestUIPromoteRequiresCurrentPersistedAcceptance(t *testing.T) {
	model := uiModel{deployments: []deploymentSummary{{Name: "prod"}}, view: deploymentView{Deployment: deploymentSummary{Name: "prod", CandidateRevisionID: "rev-2"}, ReleaseGuardEvaluations: []releaseGuardView{{CandidateRevisionID: "old", Decision: "pass"}}}}
	for _, action := range model.availableActions() {
		if action.name == "promote" {
			t.Fatal("historical acceptance exposed promote action")
		}
	}
}

func TestUIV01CapabilityContractIsComplete(t *testing.T) {
	want := []string{"deployment", "operation", "Release Guard", "benchmark", "cold starts", "infrastructure", "autoscaling", "event", "deletion", "administration"}
	all := ""
	for _, capability := range uiCapabilities {
		all += capability.Capability + " " + capability.Surface + "\n"
	}
	all = strings.ToLower(all)
	for _, term := range want {
		if !strings.Contains(all, strings.ToLower(term)) {
			t.Fatalf("v0.1 capability contract does not cover %q", term)
		}
	}
}

func TestUITabsRemainCompleteAcrossTerminalWidths(t *testing.T) {
	styles := newUIStyles(true)
	for _, width := range []int{40, 80, 120, 204} {
		tabs := (uiModel{}).renderTabs(styles, width)
		if lipgloss.Width(tabs) > width {
			t.Fatalf("tabs overflow at width %d: visual width %d", width, lipgloss.Width(tabs))
		}
		for _, index := range []string{"1", "2", "3", "4", "5", "6", "7"} {
			if !strings.Contains(tabs, index) {
				t.Fatalf("tabs at width %d omit %s: %q", width, index, tabs)
			}
		}
		if strings.Contains(tabs, "…") {
			t.Fatalf("tabs were ANSI-truncated at width %d: %q", width, tabs)
		}
	}
}

func TestUIDirectTabNavigation(t *testing.T) {
	updated, _ := (uiModel{}).Update(tea.KeyPressMsg{Text: "7", Code: '7'})
	if updated.(uiModel).tab != 6 {
		t.Fatalf("7 selected tab %d", updated.(uiModel).tab)
	}
}

func TestUIFooterStaysAtBottomOfTallTerminal(t *testing.T) {
	model := uiModel{width: 160, height: 40, dark: true, deployments: []deploymentSummary{{Name: "prod", ObservedState: "healthy"}}, view: deploymentView{Deployment: deploymentSummary{Name: "prod"}, LifecycleStatus: lifecycleStatusView{ServingState: "serving"}}}
	output := model.render()
	if got := lipgloss.Height(output); got < 39 || got > 40 {
		t.Fatalf("rendered height=%d, want 39–40", got)
	}
}
