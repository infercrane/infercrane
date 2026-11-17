package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/infercrane/infercrane/internal/domain"
)

var styledNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func styledCampaigns() ([]optimizationCampaignView, []campaignTimes) {
	campaigns := []optimizationCampaignView{
		{ID: "83ccd598540e2dd1f9c897040c1ea503", ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", State: "ranked"},
		{ID: "4f1c2d9e0b7a4e63a1d2c3b4a5f60718", ModelIdentity: "meta-llama/Llama-3.1-8B-Instruct", Objective: "cost-efficiency", State: "awaiting_approval"},
		{ID: "9a8b7c6d5e4f40312a1b2c3d4e5f6a7b", ModelIdentity: "Qwen/Qwen3-8B", Objective: "throughput", State: "failed"},
	}
	times := []campaignTimes{{CreatedAt: styledNow.Add(-2 * time.Hour)}, {CreatedAt: styledNow.Add(-4 * time.Minute)}, {}}
	return campaigns, times
}

func TestStyledCampaignListFitsAndBadgesStates(t *testing.T) {
	campaigns, times := styledCampaigns()
	for _, width := range []int{80, 120} {
		out := renderCampaignList(styledTheme(width, "dark"), campaigns, times, styledNow)
		text := ansi.Strip(out)
		for _, want := range []string{"83ccd598540e2dd1f9c897040c1ea503", "● RANKED", "● AWAITING APPROVAL", "● FAILED", "2h ago", "4m ago", "unknown", "Qwen/Qwen3-0.6B"} {
			if !strings.Contains(text, want) {
				t.Fatalf("width %d: list is missing %q:\n%s", width, want, text)
			}
		}
		assertFits(t, text, width)
	}
	if out := renderCampaignList(plainTheme, nil, nil, styledNow); out != "No optimization runs.\n" {
		t.Fatalf("empty plain list = %q", out)
	}
}

func TestStyledDeploymentListShowsStatusAndRelativeTime(t *testing.T) {
	deployments := []deploymentSummary{
		{Name: "qwen-prod", Model: "Qwen/Qwen3-8B", Runtime: "vllm", RoutingStrategy: "round-robin", ObservedState: "serving", MinReplicas: 1, MaxReplicas: 3, UpdatedAt: styledNow.Add(-90 * time.Second)},
		{Name: "llama-canary", Model: "meta-llama/Llama-3.1-8B-Instruct", Runtime: "sglang", ObservedState: "failed", UpdatedAt: styledNow.Add(-26 * time.Hour)},
		{Name: "observe-only", ObservedState: ""},
	}
	for _, width := range []int{80, 120} {
		text := ansi.Strip(renderDeploymentList(styledTheme(width, "dark"), deployments, styledNow))
		for _, want := range []string{"qwen-prod", "● SERVING", "● FAILED", "● UNKNOWN", "2m ago", "26h ago"} {
			if !strings.Contains(text, want) {
				t.Fatalf("width %d: deployments missing %q:\n%s", width, want, text)
			}
		}
		assertFits(t, text, width)
	}
}

func TestStyledCampaignDetailKeepsAnswerCheckUnderItsSetup(t *testing.T) {
	expires := styledNow.Add(43 * time.Minute)
	limit := 25.0
	warnings := []string{"the untuned starting setup is measured alongside every candidate"}
	view := optimizationCampaignView{ID: "83ccd598540e2dd1f9c897040c1ea503", ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Intent: "new_endpoint", Source: "catalog", State: "running", ApprovedMaxCostUSD: &limit, ApprovalExpiresAt: &expires, Warnings: &warnings}
	view.Candidates = append(view.Candidates, optimizationCampaignSetupView{ID: "cand-1", State: "measuring", EvidenceState: "measured", Rank: 1, Quality: &watchQualityDetail{watchQuality: watchQuality{State: "failed", Reason: "answers drifted from the control"}}})
	execution := &domain.Operation{ID: "op-1", Status: "running"}
	text := ansi.Strip(renderCampaign(styledTheme(80, "dark"), view, map[string]string{"cand-1": "vllm, --kv-cache-dtype fp8 · 1 × L40S · runpod"}, execution, nil, nil, styledNow))
	for _, want := range []string{"● RUNNING", "$25.00 · approval expires in 43m", "op-1 · RUNNING", "▲ the untuned starting setup", "● MEASURING", "vllm, --kv-cache-dtype fp8", "cand-1", "answers: answers drifted from the control"} {
		if !strings.Contains(text, want) {
			t.Fatalf("campaign view missing %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "cand-1") > strings.Index(text, "answers:") {
		t.Fatalf("answer check printed before its setup:\n%s", text)
	}
	assertFits(t, text, 80)
}

func assertFits(t *testing.T, text string, width int) {
	t.Helper()
	for number, line := range strings.Split(text, "\n") {
		if ansi.StringWidth(line) > width {
			t.Fatalf("line %d is wider than %d columns: %q", number+1, width, line)
		}
	}
}
