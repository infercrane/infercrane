package notification

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

// Destinations.
const (
	DestinationWebhook = "webhook"
	DestinationSlack   = "slack"
)

// SchemaVersion is the version of the webhook body.
const SchemaVersion = 1

// Body is the JSON a webhook destination receives. Slack receives Summary as
// its message text instead.
type Body struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Event         string    `json:"event"`
	OccurredAt    time.Time `json:"occurred_at"`
	WorkspaceID   string    `json:"workspace_id"`
	Summary       string    `json:"summary"`
	Campaign      *Campaign `json:"campaign,omitempty"`
	Outcome       *Outcome  `json:"outcome,omitempty"`
	Failure       *Failure  `json:"failure,omitempty"`
	Links         Links     `json:"links"`
}

type Campaign struct {
	ID                string     `json:"id"`
	Model             string     `json:"model"`
	Objective         string     `json:"objective"`
	Intent            string     `json:"intent"`
	State             string     `json:"state"`
	TargetDeployment  string     `json:"target_deployment,omitempty"`
	ApprovalExpiresAt *time.Time `json:"approval_expires_at,omitempty"`
	// CandidateIDs names the setups the event is about: the ones waiting on
	// quality evidence, or the ones waiting for rollout.
	CandidateIDs []string `json:"candidate_ids,omitempty"`
}

// Outcome is the decision, copied from the outcome report.
type Outcome struct {
	Kind              string `json:"kind"`
	Headline          string `json:"headline,omitempty"`
	WinnerCandidateID string `json:"winner_candidate_id,omitempty"`
	WinnerLabel       string `json:"winner_label,omitempty"`
	Banner            string `json:"banner,omitempty"`
}

type Failure struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// Links point at the run. Dashboard is present only when the control plane
// knows the dashboard address. ReportAPI is relative to the control API.
type Links struct {
	Dashboard string `json:"dashboard,omitempty"`
	ReportAPI string `json:"report_api,omitempty"`
}

// EventName is the value of Body.Event.
func EventName(kind string) string {
	if kind == EventTest {
		return "infercrane.notification.test"
	}
	return "infercrane.optimization." + kind
}

// OutcomeFromReport maps the outcome report to the decision outcome. Any
// report that is not a finished decision stays unknown.
func OutcomeFromReport(report optimizationreport.Report) Outcome {
	result := Outcome{Kind: OutcomeUnknown, Headline: report.Outcome.Headline, Banner: report.Banner}
	switch report.Outcome.Kind {
	case optimizationreport.OutcomeWinnerSelected:
		result.Kind, result.WinnerCandidateID, result.WinnerLabel = OutcomeWinner, report.Outcome.WinnerCandidateID, report.Outcome.WinnerLabel
	case optimizationreport.OutcomeBaselineKept:
		result.Kind = OutcomeNoClearWinner
	case optimizationreport.OutcomeNoWinner:
		result.Kind = OutcomeNothingPassed
	default:
		result.Headline = ""
	}
	return result
}

// CampaignBody builds the body of one campaign event. outcome is used only by
// decided and rolled-out events and may be nil when the report is unreadable.
func CampaignBody(workspace string, notice Notice, campaign domain.OptimizationCampaign, outcome *Outcome, links Links) Body {
	body := Body{SchemaVersion: SchemaVersion, ID: notice.ID, Event: EventName(notice.Type), OccurredAt: notice.OccurredAt.UTC(), WorkspaceID: workspace, Links: links}
	body.Campaign = &Campaign{ID: campaign.ID, Model: campaign.ModelIdentity, Objective: campaign.Objective, Intent: campaign.Intent, State: campaign.State, TargetDeployment: campaign.TargetDeployment, CandidateIDs: notice.CandidateIDs}
	if campaign.ApprovalExpiresAt != nil {
		expires := campaign.ApprovalExpiresAt.UTC()
		body.Campaign.ApprovalExpiresAt = &expires
	}
	model := emptyAs(campaign.ModelIdentity, "your model")
	switch notice.Type {
	case EventApprovalNeeded:
		body.Summary = fmt.Sprintf("The %s optimization run is ready and needs your spend approval to start.", model)
	case EventApprovalExpiring:
		ends := "soon"
		if campaign.ApprovalExpiresAt != nil {
			ends = "at " + clock(*campaign.ApprovalExpiresAt)
		}
		if AwaitingRollout(campaign.State) {
			body.Summary = fmt.Sprintf("The %s run's approval ends %s. Its result is waiting for your decision; after that it is cleaned up.", model, ends)
		} else {
			body.Summary = fmt.Sprintf("The %s run's approval ends %s. Work still running then stops and is cleaned up.", model, ends)
		}
	case EventQualityEvidenceNeeded:
		body.Summary = fmt.Sprintf("The %s run is waiting for signed answer-quality results for %s before it can decide.", model, plural(len(notice.CandidateIDs), "setup"))
	case EventDecided:
		decided := Outcome{Kind: OutcomeUnknown}
		if outcome != nil {
			decided = *outcome
		}
		body.Outcome = &decided
		body.Summary = decisionSummary(model, decided)
	case EventFailed:
		body.Failure = &Failure{Code: campaign.FailureCode, Reason: FailureReason(campaign.FailureCode)}
		if body.Failure.Code == "" {
			body.Failure.Code = OutcomeUnknown
		}
		body.Summary = fmt.Sprintf("The %s run failed: %s.", model, body.Failure.Reason)
	case EventRolledOut:
		target := campaign.TargetDeployment
		if outcome != nil && outcome.WinnerLabel != "" {
			body.Outcome = outcome
			body.Summary = fmt.Sprintf("The %s run's winner, “%s”, is rolled out.", model, outcome.WinnerLabel)
		} else {
			body.Summary = fmt.Sprintf("The %s run's winner is rolled out.", model)
		}
		if target != "" {
			body.Summary = strings.TrimSuffix(body.Summary, ".") + " on " + target + "."
		}
	}
	return body
}

func decisionSummary(model string, outcome Outcome) string {
	prefix := fmt.Sprintf("The %s run finished. ", model)
	summary := ""
	switch {
	case outcome.Headline != "" && outcome.Kind != OutcomeUnknown:
		summary = prefix + outcome.Headline
	case outcome.Kind == OutcomeNoClearWinner:
		summary = prefix + "Keep your current setup: nothing beat it by more than the measurement noise."
	case outcome.Kind == OutcomeNothingPassed:
		summary = prefix + "No setup qualified. Nothing changes in production."
	default:
		summary = prefix + "Open the outcome report for the result."
	}
	if outcome.Banner != "" {
		summary += " (" + outcome.Banner + ")"
	}
	return summary
}

// TestBody is the body of a test event.
func TestBody(workspace, id string, at time.Time, links Links) Body {
	return Body{SchemaVersion: SchemaVersion, ID: id, Event: EventName(EventTest), OccurredAt: at.UTC(), WorkspaceID: workspace, Summary: "Test notification from InferCrane. This destination receives optimization run events.", Links: links}
}

// Encode returns the exact bytes sent to a destination.
func Encode(destination string, body Body) ([]byte, error) {
	if destination == DestinationSlack {
		text := slackEscape(body.Summary)
		switch {
		case body.Links.Dashboard != "":
			text += " <" + body.Links.Dashboard + "|Open the run>"
		case body.Campaign != nil:
			text += " Run `infercrane optimize inspect " + slackEscape(body.Campaign.ID) + "`"
		}
		return json.Marshal(map[string]any{"text": text})
	}
	return json.Marshal(body)
}

// slackEscape escapes the three characters Slack reserves for markup.
func slackEscape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// RunLinks returns the links for one campaign. consoleURL is the dashboard
// origin and may be empty.
func RunLinks(consoleURL, campaignID string) Links {
	links := Links{ReportAPI: "/api/v1/optimization/campaigns/" + url.PathEscape(campaignID) + "/report"}
	if base := strings.TrimRight(strings.TrimSpace(consoleURL), "/"); base != "" {
		links.Dashboard = base + "/optimization/" + url.PathEscape(campaignID)
	}
	return links
}

// needsReport reports the events whose body reads the outcome report.
func needsReport(kind string) bool {
	return kind == EventDecided || kind == EventRolledOut
}
