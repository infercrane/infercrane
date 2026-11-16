package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/notification"
)

type inboxItem struct {
	Severity     string    `json:"severity"`
	Code         string    `json:"code"`
	ResourceKind string    `json:"resource_kind"`
	ResourceName string    `json:"resource_name"`
	Summary      string    `json:"summary"`
	Evidence     string    `json:"evidence"`
	Next         string    `json:"next"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

type inboxReport struct {
	Items          []inboxItem `json:"items"`
	Critical       int         `json:"critical"`
	Warnings       int         `json:"warnings"`
	TotalAttention int         `json:"total_attention"`
	TotalEndpoints int         `json:"total_endpoints"`
	TotalDeploys   int         `json:"total_deployments"`
	TotalRuns      int         `json:"total_optimization_runs"`
	Returned       int         `json:"returned"`
	Truncated      bool        `json:"truncated"`
	Evidence       string      `json:"evidence"`
	ObservedAt     time.Time   `json:"observed_at"`
}

// inboxCampaign is the part of an optimization campaign the inbox reads.
type inboxCampaign struct {
	ID                string     `json:"id"`
	ModelIdentity     string     `json:"model_identity"`
	Intent            string     `json:"intent"`
	State             string     `json:"state"`
	FailureCode       string     `json:"failure_code"`
	CancelRequested   bool       `json:"cancel_requested"`
	ApprovalExpiresAt *time.Time `json:"approval_expires_at"`
	ApprovedAt        *time.Time `json:"approved_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Candidates        []struct {
		ID                string    `json:"id"`
		State             string    `json:"state"`
		DeploymentName    string    `json:"deployment_name"`
		QualityEvidenceID string    `json:"quality_evidence_id"`
		FailureCode       string    `json:"failure_code"`
		UpdatedAt         time.Time `json:"updated_at"`
	} `json:"candidates"`
}

type inboxEndpoint struct {
	Name                   string    `json:"name"`
	ObservedState          string    `json:"observed_state"`
	ActiveServingPlanID    string    `json:"active_serving_plan_id"`
	CandidateServingPlanID string    `json:"candidate_serving_plan_id"`
	UpdatedAt              time.Time `json:"updated_at"`
}

func inboxCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := newFlagSet("inbox")
	limit := fs.Int("limit", 50, "maximum attention items")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane inbox [--limit N] [--output human|json]")
	}
	if *limit < 1 || *limit > 500 {
		return errors.New("--limit must be between 1 and 500")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	report, err := readInbox(ctx, cfg, *limit)
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(report)
	}
	printInbox(report)
	return nil
}

func readInbox(ctx context.Context, cfg config.Config, limit int) (inboxReport, error) {
	var endpoints struct {
		Data []inboxEndpoint `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints", "", nil, &endpoints); err != nil {
		return inboxReport{}, fmt.Errorf("read endpoint fleet: %w", err)
	}
	var deployments struct {
		Data []deploymentSummary `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments", "", nil, &deployments); err != nil {
		return inboxReport{}, fmt.Errorf("read deployment fleet: %w", err)
	}

	var campaigns struct {
		Data []inboxCampaign `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns?limit=100", "", nil, &campaigns); err != nil {
		return inboxReport{}, fmt.Errorf("read optimization runs: %w", err)
	}

	items := make([]inboxItem, 0)
	for _, endpoint := range endpoints.Data {
		items = append(items, endpointInboxItems(endpoint)...)
	}
	for _, deployment := range deployments.Data {
		items = append(items, deploymentInboxItems(deployment)...)
	}
	observedAt := time.Now().UTC()
	for _, campaign := range campaigns.Data {
		items = append(items, campaignInboxItems(campaign, observedAt)...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, right := inboxSeverityOrder(items[i].Severity), inboxSeverityOrder(items[j].Severity)
		if left != right {
			return left < right
		}
		left, right = inboxCodeOrder(items[i].Code), inboxCodeOrder(items[j].Code)
		if left != right {
			return left < right
		}
		if items[i].ResourceKind != items[j].ResourceKind {
			return items[i].ResourceKind < items[j].ResourceKind
		}
		if items[i].ResourceName != items[j].ResourceName {
			return items[i].ResourceName < items[j].ResourceName
		}
		return items[i].Code < items[j].Code
	})
	report := inboxReport{Items: items, TotalAttention: len(items), TotalEndpoints: len(endpoints.Data), TotalDeploys: len(deployments.Data), TotalRuns: len(campaigns.Data), Evidence: "persisted_control_plane_state", ObservedAt: observedAt}
	for _, item := range items {
		if item.Severity == "critical" {
			report.Critical++
		} else if item.Severity == "warning" {
			report.Warnings++
		}
	}
	if len(report.Items) > limit {
		report.Items = report.Items[:limit]
		report.Truncated = true
	}
	report.Returned = len(report.Items)
	return report, nil
}

func endpointInboxItems(endpoint inboxEndpoint) []inboxItem {
	items := make([]inboxItem, 0, 2)
	state := strings.ToLower(strings.TrimSpace(endpoint.ObservedState))
	// "observing" is an observe-only endpoint that publishes no route by design.
	if state != "serving" && state != "observing" {
		severity := "warning"
		code := "endpoint_not_converged"
		if state == "degraded" || state == "suspended" {
			severity, code = "critical", "endpoint_not_serving"
		} else if state == "" {
			code = "endpoint_state_unknown"
		}
		items = append(items, inboxItem{Severity: severity, Code: code, ResourceKind: "endpoint", ResourceName: endpoint.Name, Summary: fmt.Sprintf("Persisted endpoint state is %s.", emptyAs(state, "unknown")), Evidence: "endpoint.observed_state", Next: "infercrane observe " + endpoint.Name + " --diagnose", UpdatedAt: endpoint.UpdatedAt})
	}
	if endpoint.CandidateServingPlanID != "" {
		items = append(items, inboxItem{Severity: "warning", Code: "endpoint_candidate_requires_review", ResourceKind: "endpoint", ResourceName: endpoint.Name, Summary: "A candidate serving plan is staged and requires an explicit Guard decision or operator action.", Evidence: "endpoint.candidate_serving_plan_id", Next: "infercrane endpoint guard " + endpoint.Name, UpdatedAt: endpoint.UpdatedAt})
	}
	return items
}

func deploymentInboxItems(deployment deploymentSummary) []inboxItem {
	items := make([]inboxItem, 0, 2)
	state := strings.ToLower(strings.TrimSpace(deployment.ObservedState))
	if state != "healthy" {
		severity := "warning"
		code := "deployment_not_converged"
		if state == "degraded" || state == "failed" {
			severity, code = "critical", "deployment_degraded"
		} else if state == "" {
			code = "deployment_state_unknown"
		}
		items = append(items, inboxItem{Severity: severity, Code: code, ResourceKind: "deployment", ResourceName: deployment.Name, Summary: fmt.Sprintf("Persisted deployment state is %s.", emptyAs(state, "unknown")), Evidence: "deployment.observed_state", Next: "infercrane observe " + deployment.Name, UpdatedAt: deployment.UpdatedAt})
	}
	if deployment.CandidateRevisionID != "" {
		items = append(items, inboxItem{Severity: "warning", Code: "deployment_candidate_requires_review", ResourceKind: "deployment", ResourceName: deployment.Name, Summary: "An immutable candidate revision requires an explicit Guard decision or operator action.", Evidence: "deployment.candidate_revision_id", Next: "infercrane rollout inspect " + deployment.Name, UpdatedAt: deployment.UpdatedAt})
	}
	return items
}

// campaignInboxItems lists what an optimization run needs from a person,
// with the same rules as the run notifications.
func campaignInboxItems(campaign inboxCampaign, now time.Time) []inboxItem {
	run := domain.OptimizationCampaign{ID: campaign.ID, ModelIdentity: campaign.ModelIdentity, Intent: campaign.Intent, State: campaign.State, FailureCode: campaign.FailureCode, CancelRequested: campaign.CancelRequested, ApprovalExpiresAt: campaign.ApprovalExpiresAt, ApprovedAt: campaign.ApprovedAt, CreatedAt: campaign.CreatedAt, UpdatedAt: campaign.UpdatedAt}
	for _, candidate := range campaign.Candidates {
		run.Candidates = append(run.Candidates, domain.OptimizationCandidateRun{ID: candidate.ID, CampaignID: campaign.ID, State: candidate.State, DeploymentName: candidate.DeploymentName, QualityEvidenceID: candidate.QualityEvidenceID, FailureCode: candidate.FailureCode, UpdatedAt: candidate.UpdatedAt})
	}
	attention := notification.CampaignAttention(run, now)
	items := make([]inboxItem, 0, len(attention))
	for _, item := range attention {
		items = append(items, inboxItem{Severity: item.Severity, Code: item.Code, ResourceKind: "optimization", ResourceName: campaign.ID, Summary: item.Summary, Evidence: "optimization_campaign.state", Next: item.Next, UpdatedAt: item.UpdatedAt})
	}
	return items
}

func inboxSeverityOrder(severity string) int {
	if severity == "critical" {
		return 0
	}
	return 1
}

func inboxCodeOrder(code string) int {
	if strings.HasSuffix(code, "_requires_review") {
		return 1
	}
	return 0
}

func printInbox(report inboxReport) {
	fmt.Printf("InferCrane inbox · persisted fleet evidence\n\n")
	if report.TotalAttention == 0 {
		fmt.Printf("No persisted fleet issue currently requires attention.\n\nFleet     %d endpoints · %d deployments · %d optimization runs\nEvidence  persisted state; no diagnosis was run\n\nRun `infercrane observe NAME --diagnose` for a fresh deterministic endpoint evaluation.\n", report.TotalEndpoints, report.TotalDeploys, report.TotalRuns)
		return
	}
	fmt.Printf("%d attention item(s) · %d critical · %d warning\n", report.TotalAttention, report.Critical, report.Warnings)
	if report.Truncated {
		fmt.Printf("Showing the first %d; rerun with a larger --limit (maximum 500).\n", report.Returned)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, item := range report.Items {
		fmt.Fprintf(w, "%s\t%s/%s\t%s\n", strings.ToUpper(item.Severity), item.ResourceKind, item.ResourceName, item.Summary)
		fmt.Fprintf(w, "\t\tEvidence: %s · updated %s\n", item.Evidence, inboxTime(item.UpdatedAt))
		fmt.Fprintf(w, "\t\tNext: %s\n\n", item.Next)
	}
	_ = w.Flush()
	fmt.Println("Inbox reads persisted state only. It never records prompt or response content and does not run Doctor implicitly.")
}

func inboxTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}
