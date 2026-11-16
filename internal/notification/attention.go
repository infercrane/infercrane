package notification

import (
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// Attention is one thing a run needs from a person now. It feeds
// `infercrane inbox`, using the same rules as the notifications.
type Attention struct {
	Severity  string
	Code      string
	Summary   string
	Next      string
	UpdatedAt time.Time
}

// CampaignAttention lists what the campaign needs now. A run waiting for
// approval, a winner waiting for rollout, an approval about to end, setups
// waiting on quality evidence, and a run that failed recently each need a
// person. A finished run with nothing to roll out needs nothing.
func CampaignAttention(campaign domain.OptimizationCampaign, now time.Time) []Attention {
	if campaign.CancelRequested || campaign.ID == "" {
		return nil
	}
	model := emptyAs(campaign.ModelIdentity, "this model")
	items := make([]Attention, 0, 2)
	switch {
	case campaign.State == optimizationcampaign.CampaignAwaitingApproval:
		items = append(items, Attention{Severity: "warning", Code: "optimization_approval_needed", Summary: fmt.Sprintf("The %s run is ready and needs your spend approval to start.", model), Next: "infercrane optimize approve " + campaign.ID + " --max-cost-usd AMOUNT --expires-in 1h", UpdatedAt: campaign.UpdatedAt})
	case AwaitingRollout(campaign.State):
		severity, summary := "warning", fmt.Sprintf("The %s run has a result waiting for your decision.", model)
		if campaign.ApprovalExpiresAt != nil {
			summary = fmt.Sprintf("The %s run has a result waiting for your decision before its approval ends at %s.", model, clock(*campaign.ApprovalExpiresAt))
			if _, expiring := expiryWarningAt(campaign, now); expiring {
				severity = "critical"
			}
		}
		items = append(items, Attention{Severity: severity, Code: "optimization_decision_waiting", Summary: summary, Next: "infercrane optimize report " + campaign.ID, UpdatedAt: campaign.UpdatedAt})
	case holdsAuthority(campaign.State):
		if _, expiring := expiryWarningAt(campaign, now); expiring && campaign.ApprovalExpiresAt != nil {
			items = append(items, Attention{Severity: "warning", Code: "optimization_approval_expiring", Summary: fmt.Sprintf("The %s run's approval ends at %s. Work still running then stops and is cleaned up.", model, clock(*campaign.ApprovalExpiresAt)), Next: "infercrane optimize watch " + campaign.ID, UpdatedAt: campaign.UpdatedAt})
		}
	}
	if _, waiting := waitingOnQualityEvidence(campaign, now); len(waiting) > 0 {
		deployment := "DEPLOYMENT"
		for _, candidate := range campaign.Candidates {
			if candidate.ID == waiting[0] && candidate.DeploymentName != "" {
				deployment = candidate.DeploymentName
			}
		}
		items = append(items, Attention{Severity: "warning", Code: "optimization_quality_evidence_needed", Summary: fmt.Sprintf("The %s run is waiting for signed answer-quality results for %s.", model, plural(len(waiting), "setup")), Next: "infercrane evaluation attach " + deployment + " --file evidence.json", UpdatedAt: campaign.UpdatedAt})
	}
	failed := campaign.State == optimizationcampaign.CampaignFailed || campaign.FailureCode != "" && now.Sub(campaign.UpdatedAt) < RecentFailure
	if failed && !AwaitingRollout(campaign.State) {
		reason := FailureReason(campaign.FailureCode)
		items = append(items, Attention{Severity: "critical", Code: "optimization_failed", Summary: fmt.Sprintf("The %s run failed: %s.", model, reason), Next: "infercrane optimize inspect " + campaign.ID, UpdatedAt: campaign.UpdatedAt})
	}
	return items
}

// FailureReason turns a failure code into words. An unrecognized code is
// shown as is; an empty one stays unknown.
func FailureReason(code string) string {
	switch code {
	case "":
		return "reason unknown"
	case "approval_expired":
		return "the spend approval ended before the run finished"
	case "approval_expired_before_activation":
		return "the approval ended before the result was rolled out"
	}
	return strings.ReplaceAll(code, "_", " ")
}

func clock(at time.Time) string {
	return at.UTC().Format("2006-01-02 15:04 UTC")
}

func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
