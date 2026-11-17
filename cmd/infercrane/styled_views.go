package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// Styled renderings of list and detail views for a person at a terminal. The
// plain output of each command stays where it was, unchanged, for scripts.

// campaignTimes reads the timestamps the campaign list returns. They are
// decoded apart from optimizationCampaignView so --output json stays the
// same document it always was.
type campaignTimes struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func renderCampaignList(t theme, campaigns []optimizationCampaignView, times []campaignTimes, now time.Time) string {
	if len(campaigns) == 0 {
		return t.muted("No optimization campaigns.") + "\n"
	}
	// Wide terminals get one row per campaign; narrow ones put the model and
	// goal on a second line, so the full campaign ID never wraps.
	wide := t.width >= 110
	columns := []column{{header: "Campaign"}, {header: "State"}}
	if wide {
		columns = append(columns, column{header: "Model", wrap: true}, column{header: "Goal"})
	}
	columns = append(columns, column{header: "Created", right: true})
	rows := make([][]string, 0, len(campaigns))
	for index, campaign := range campaigns {
		created := "unknown"
		if index < len(times) {
			created = relativeTime(now, times[index].CreatedAt)
		}
		row := []string{campaign.ID, stateWord(campaign.State)}
		if wide {
			row = append(row, emptyAs(campaign.ModelIdentity, "unknown"), emptyAs(campaign.Objective, "unknown"))
		}
		rows = append(rows, append(row, created))
	}
	var detail func(int) string
	if !wide {
		detail = func(row int) string {
			campaign := campaigns[row]
			line := emptyAs(campaign.ModelIdentity, "unknown") + " · " + emptyAs(campaign.Objective, "unknown")
			var out strings.Builder
			for _, part := range wrapWords(line, t.width-2) {
				out.WriteString("  " + t.muted(part) + "\n")
			}
			return out.String()
		}
	}
	return t.tableWithDetail(0, columns, rows, func(row, col int, text string) string {
		switch columns[col].header {
		case "State":
			return t.tone(statusTone(campaigns[row].State), text)
		case "Campaign":
			return t.title(text)
		case "Created", "Goal":
			return t.muted(text)
		}
		return text
	}, detail)
}

// stateWord is a state as a badge word with its dot, for table cells that are
// colored as a whole.
func stateWord(state string) string {
	return "● " + strings.ToUpper(strings.ReplaceAll(emptyAs(state, "unknown"), "_", " "))
}

// renderCampaign is one optimization run: its facts, warnings, and setups.
// described maps a setup ID to what it runs, when the spec was returned.
func renderCampaign(t theme, view optimizationCampaignView, described map[string]string, execution, activation, cleanup *domain.Operation, now time.Time) string {
	var b strings.Builder
	b.WriteString(t.title("Optimization campaign") + "  " + t.badge(view.State) + "\n")
	b.WriteString(t.muted(view.ID) + "\n\n")
	names := []string{"Model", "Objective", "Intent", "Source"}
	values := []string{emptyAs(view.ModelIdentity, "unknown"), emptyAs(view.Objective, "unknown"), emptyAs(view.Intent, "unknown"), emptyAs(view.Source, "unknown")}
	if view.TargetDeployment != "" {
		names, values = append(names, "Target"), append(values, view.TargetDeployment)
	}
	if view.ApprovedMaxCostUSD != nil {
		cap := fmt.Sprintf("$%.2f", *view.ApprovedMaxCostUSD)
		if view.ApprovalExpiresAt != nil {
			cap += " · approval expires " + relativeTime(now, *view.ApprovalExpiresAt)
		}
		names, values = append(names, "Cost cap"), append(values, cap)
	}
	operationNames := []string{}
	for _, item := range []struct {
		name      string
		operation *domain.Operation
	}{{"Execution", execution}, {"Activation", activation}, {"Cleanup", cleanup}} {
		if item.operation != nil {
			names, values = append(names, item.name), append(values, item.operation.ID+" · "+strings.ToUpper(item.operation.Status))
			operationNames = append(operationNames, item.name)
		}
	}
	b.WriteString(t.definitions(0, 10, names, values, func(index int, text string) string {
		for _, name := range operationNames {
			if names[index] == name {
				if id, status, ok := strings.Cut(text, " · "); ok {
					return t.muted(id+" · ") + t.tone(statusTone(status), status)
				}
			}
		}
		return text
	}))
	if view.Warnings != nil && len(*view.Warnings) > 0 {
		b.WriteString("\n" + t.section("Proposal warnings") + "\n")
		for _, warning := range *view.Warnings {
			for index, line := range wrapWords(warning, t.width-4) {
				prefix := "    "
				if index == 0 {
					prefix = "  " + t.tone(toneWatch, "▲") + " "
				}
				b.WriteString(prefix + line + "\n")
			}
		}
	}
	if len(view.Candidates) > 0 {
		b.WriteString("\n" + t.section("Setups") + "\n")
		rows := make([][]string, 0, len(view.Candidates))
		for _, candidate := range view.Candidates {
			detail := candidate.ID
			if setup := described[candidate.ID]; setup != "" {
				if candidate.Control != nil && *candidate.Control {
					setup = "current setup · " + setup
				}
				detail = setup + " · " + candidate.ID
			}
			if candidate.TargetEndpoint != "" {
				detail += " · endpoint " + candidate.TargetEndpoint
			}
			if candidate.FailureCode != "" {
				detail += " · " + candidate.FailureCode
			}
			rows = append(rows, []string{fmt.Sprintf("%d", candidate.Rank), stateWord(candidate.State), emptyAs(candidate.EvidenceState, "unknown"), detail})
		}
		b.WriteString(t.tableWithDetail(2, []column{{header: "#", right: true}, {header: "State"}, {header: "Evidence"}, {header: "Setup", wrap: true}}, rows, func(row, col int, text string) string {
			switch col {
			case 1:
				return t.tone(statusTone(view.Candidates[row].State), text)
			case 0, 2:
				return t.muted(text)
			}
			return text
		}, func(row int) string {
			quality := view.Candidates[row].Quality
			if quality == nil || quality.State == "pending" || quality.State == "not_run" {
				return ""
			}
			var detail strings.Builder
			for _, line := range wrapWords("answers: "+quality.Reason, t.width-8) {
				detail.WriteString("      " + t.muted(line) + "\n")
			}
			if quality.Command != "" {
				detail.WriteString("      " + t.muted("run: ") + quality.Command + "\n")
			}
			return detail.String()
		}))
	}
	return b.String()
}

func renderDeploymentList(t theme, deployments []deploymentSummary, now time.Time) string {
	if len(deployments) == 0 {
		return t.muted("No deployments.") + "\n"
	}
	wide := t.width >= 100
	columns := []column{{header: "Name"}, {header: "Status"}, {header: "Model", wrap: true}, {header: "Runtime"}}
	if wide {
		columns = append(columns, column{header: "Routing"}, column{header: "Replicas", right: true})
	}
	columns = append(columns, column{header: "Updated", right: true})
	rows := make([][]string, 0, len(deployments))
	for _, deployment := range deployments {
		row := []string{deployment.Name, stateWord(deployment.ObservedState), emptyAs(deployment.Model, "unknown"), emptyAs(deployment.Runtime, "unknown")}
		if wide {
			row = append(row, emptyAs(deployment.RoutingStrategy, "unknown"), fmt.Sprintf("%d–%d", deployment.MinReplicas, deployment.MaxReplicas))
		}
		rows = append(rows, append(row, relativeTime(now, deployment.UpdatedAt)))
	}
	return t.table(0, columns, rows, func(row, col int, text string) string {
		switch columns[col].header {
		case "Name":
			return t.title(text)
		case "Status":
			return t.tone(statusTone(deployments[row].ObservedState), text)
		case "Model":
			return text
		}
		if text == "unknown" {
			return t.tone(toneUnknown, text)
		}
		return t.muted(text)
	})
}
