package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
)

type observeSnapshot struct {
	Kind                  string                             `json:"kind"`
	Name                  string                             `json:"name"`
	Endpoint              map[string]any                     `json:"endpoint,omitempty"`
	Deployment            *deploymentView                    `json:"deployment,omitempty"`
	Guard                 map[string]any                     `json:"release_guard,omitempty"`
	Admission             map[string]any                     `json:"admission,omitempty"`
	Alerts                []map[string]any                   `json:"alerts"`
	Findings              []map[string]any                   `json:"findings"`
	RecentEvents          []cliEvent                         `json:"recent_events"`
	Monitoring            *domain.EndpointMonitoringSnapshot `json:"monitoring,omitempty"`
	MonitoringUnavailable string                             `json:"monitoring_unavailable,omitempty"`
	ObservedAt            time.Time                          `json:"observed_at"`
}

func observeCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane observe ENDPOINT_OR_DEPLOYMENT [--watch] [--diagnose] [--events N] [--output human|json]")
	}
	name := args[0]
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	watch := fs.Bool("watch", false, "refresh until interrupted")
	diagnose := fs.Bool("diagnose", false, "persist a fresh deterministic Doctor evaluation for an endpoint")
	eventLimit := fs.Int("events", 5, "recent deployment events to include")
	window := fs.Duration("window", time.Hour, "Doctor and Release Guard evidence window")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() != 0 || *eventLimit < 0 || *eventLimit > 100 || *window <= 0 || *window > 30*24*time.Hour {
		return errors.New("--events must be between 0 and 100 and --window between 1ns and 720h")
	}
	if *watch && *output == "json" {
		return errors.New("--watch cannot be combined with --output json; use repeated JSON snapshots explicitly")
	}
	for {
		snapshot, err := readObserveSnapshot(ctx, cfg, name, *diagnose, *eventLimit, *window)
		if err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(snapshot)
		}
		if *watch {
			fmt.Print("\033[H\033[2J")
		}
		printObserveSnapshot(snapshot)
		if !*watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func readObserveSnapshot(ctx context.Context, cfg config.Config, name string, diagnose bool, eventLimit int, window time.Duration) (observeSnapshot, error) {
	snapshot := observeSnapshot{Name: name, Alerts: []map[string]any{}, Findings: []map[string]any{}, RecentEvents: []cliEvent{}, ObservedAt: time.Now().UTC()}
	var endpoint map[string]any
	err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(name), "", nil, &endpoint)
	if err == nil {
		snapshot.Kind, snapshot.Endpoint = "endpoint", endpoint
		monitoringPath := fmt.Sprintf("/api/v1/endpoints/%s/monitoring?window_seconds=%d", url.PathEscape(name), int(window.Seconds()))
		var monitoring domain.EndpointMonitoringSnapshot
		if monitoringErr := controlJSON(ctx, cfg, http.MethodGet, monitoringPath, "", nil, &monitoring); monitoringErr == nil {
			snapshot.Monitoring = &monitoring
		} else {
			snapshot.MonitoringUnavailable = monitoringErr.Error()
		}
		var guardPolicy struct {
			Policy map[string]any `json:"policy"`
		}
		if guardErr := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(name)+"/release-guard/policy", "", nil, &guardPolicy); guardErr == nil {
			var evaluations struct {
				Data []map[string]any `json:"data"`
			}
			if historyErr := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(name)+"/release-guard/evaluations", "", nil, &evaluations); historyErr == nil {
				snapshot.Guard = map[string]any{"policy": guardPolicy.Policy, "evaluations": evaluations.Data}
			}
		}
		var admissionResponse struct {
			Policy map[string]any `json:"policy"`
		}
		if admissionErr := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(name)+"/admission", "", nil, &admissionResponse); admissionErr == nil {
			snapshot.Admission = admissionResponse.Policy
		}
		var alertResponse struct {
			Data []map[string]any `json:"data"`
		}
		if alertErr := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(name)+"/alerts", "", nil, &alertResponse); alertErr == nil && alertResponse.Data != nil {
			snapshot.Alerts = alertResponse.Data
		}
		if diagnose {
			var findings struct {
				Data []map[string]any `json:"data"`
			}
			path := fmt.Sprintf("/api/v1/endpoints/%s/doctor?window_seconds=%d", url.PathEscape(name), int(window.Seconds()))
			if err = controlJSON(ctx, cfg, http.MethodPost, path, "", map[string]any{}, &findings); err != nil {
				return snapshot, err
			}
			if findings.Data != nil {
				snapshot.Findings = findings.Data
			}
		}
		return snapshot, nil
	}
	var controlErr *ControlError
	if !errors.As(err, &controlErr) || controlErr.Code != "not_found" {
		return snapshot, err
	}
	view, deploymentErr := fetchDeployment(ctx, cfg, name)
	if deploymentErr != nil {
		return snapshot, fmt.Errorf("%q is neither an endpoint nor a deployment: %w", name, deploymentErr)
	}
	snapshot.Kind, snapshot.Deployment = "deployment", &view
	if eventLimit > 0 {
		events, eventErr := deploymentEvents(ctx, cfg, name)
		if eventErr != nil {
			return snapshot, eventErr
		}
		if len(events) > eventLimit {
			events = events[len(events)-eventLimit:]
		}
		snapshot.RecentEvents = events
	}
	return snapshot, nil
}

func printObserveSnapshot(snapshot observeSnapshot) {
	fmt.Printf("InferCrane observe · %s · %s\n\n", snapshot.Kind, snapshot.Name)
	if snapshot.Kind == "deployment" && snapshot.Deployment != nil {
		view := snapshot.Deployment
		d := view.Deployment
		lifecycle := view.LifecycleStatus
		fmt.Printf("STATE\n  Serving       %s\n  Convergence   %s\n  Ready         %d/%d\n  Provisioning  %d\n  Draining      %d\n\nIDENTITY\n  Model         %s\n  Runtime       %s\n  Revision      %s\n  Candidate     %s\n  Routing       %s\n\nTRAFFIC\n  Requests/s    %.2f\n  Error rate    %.2f%%\n", lifecycle.ServingState, lifecycle.ConvergenceState, lifecycle.ReadyReplicas, lifecycle.DesiredReplicas, lifecycle.ProvisioningReplicas, lifecycle.DrainingReplicas, d.Model, d.Runtime, shortValue(d.ActiveRevisionID), emptyAs(shortValue(d.CandidateRevisionID), "none"), d.RoutingStrategy, view.RequestStats.RequestsPerSecond, view.RequestStats.ErrorRate*100)
		if view.ActiveOperation != nil {
			fmt.Printf("\nOPERATION\n  %s  %s  %s\n  Resume: infercrane operation watch %s\n", view.ActiveOperation.Kind, view.ActiveOperation.Status, view.ActiveOperation.ID, view.ActiveOperation.ID)
		}
		if len(view.ReleaseGuardEvaluations) > 0 {
			latest := view.ReleaseGuardEvaluations[0]
			fmt.Printf("\nRELEASE GUARD\n  Decision      %s\n  Candidate     %s\n  Evaluated     %s\n", latest.Decision, shortValue(latest.CandidateRevisionID), latest.CreatedAt.Format(time.RFC3339))
		}
		if len(snapshot.RecentEvents) > 0 {
			fmt.Println("\nRECENT EVENTS")
			for _, event := range snapshot.RecentEvents {
				fmt.Printf("  %s  %-22s %s\n", event.CreatedAt.Format("15:04:05"), event.Type, event.Summary)
			}
		}
		return
	}
	endpoint, _ := snapshot.Endpoint["endpoint"].(map[string]any)
	logical, _ := snapshot.Endpoint["logical_model"].(map[string]any)
	environment, _ := snapshot.Endpoint["environment"].(map[string]any)
	active, _ := snapshot.Endpoint["active_plan"].(map[string]any)
	candidate, _ := snapshot.Endpoint["candidate_plan"].(map[string]any)
	bindings, _ := snapshot.Endpoint["bindings"].([]any)
	fmt.Printf("IDENTITY\n  Logical model  %v\n  Environment    %v\n  State          %v\n  Stable model   %s\n\nSERVING PLAN\n  Active         %s\n  Policy         %v\n  Candidate      %s\n  Bindings       %d\n", logical["name"], environment["name"], endpoint["observed_state"], snapshot.Name, shortValue(fmt.Sprint(active["id"])), active["routing_policy"], emptyAs(shortValue(fmt.Sprint(candidate["id"])), "none"), len(bindings))
	if snapshot.Monitoring != nil {
		stats := snapshot.Monitoring.Summary
		fmt.Printf("\nTRAFFIC\n  Requests      %d (%.2f/s)\n  Error rate    %s\n  Fallback      %s\n  TTFT p95      %s\n  Latency p95   %s\n  Output        %s\n  Evidence      %s · %d samples\n", stats.Requests, stats.RequestsPerSecond, observePercent(stats.ErrorRate), observePercent(stats.FallbackRate), observeMillis(stats.P95TTFTMS), observeMillis(stats.P95LatencyMS), observeTokenRate(stats.OutputTokensPerSecond), observeFreshness(snapshot.Monitoring.Evidence), snapshot.Monitoring.Evidence.SampleCount)
	} else {
		fmt.Printf("\nTRAFFIC\n  Unavailable   %s\n", emptyAs(snapshot.MonitoringUnavailable, "monitoring capability unavailable"))
	}
	if evaluations, ok := snapshot.Guard["evaluations"].([]map[string]any); ok && len(evaluations) > 0 {
		fmt.Printf("\nRELEASE GUARD\n  Decision       %v\n  Evaluated      %v\n", evaluations[0]["decision"], evaluations[0]["created_at"])
	} else {
		fmt.Println("\nRELEASE GUARD\n  No persisted evaluation")
	}
	if len(snapshot.Findings) > 0 {
		fmt.Println("\nDOCTOR")
		for _, finding := range snapshot.Findings {
			fmt.Printf("  %-8s %v — %v\n", strings.ToUpper(fmt.Sprint(finding["severity"])), finding["code"], finding["summary"])
		}
	} else {
		fmt.Println("\nDOCTOR\n  Not evaluated in this snapshot; add --diagnose to persist deterministic findings.")
	}
	encoded, _ := json.Marshal(snapshot.Admission)
	if len(snapshot.Admission) > 0 {
		fmt.Printf("\nADMISSION\n  %s\n", encoded)
	}
	fmt.Printf("\nNEXT\n  Send request    infercrane request %s\n  Diagnose        infercrane observe %s --diagnose\n  Open console    infercrane ui\n", snapshot.Name, snapshot.Name)
}

func observePercent(value *float64) string {
	if value == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.2f%%", *value*100)
}

func observeMillis(value *float64) string {
	if value == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.1fms", *value)
}

func observeTokenRate(value *float64) string {
	if value == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.2f tok/s", *value)
}

func observeFreshness(evidence domain.MonitoringEvidence) string {
	if evidence.SampleCount == 0 {
		return "empty"
	}
	if evidence.Fresh {
		return "fresh"
	}
	return "stale"
}
