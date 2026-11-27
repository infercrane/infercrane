package proofgate

import (
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationreport"
)

// CommentMarker starts every gate comment, so a CI job updates its own
// comment instead of adding one per run. Key separates several gates on one
// pull request.
func CommentMarker(key string) string {
	key = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '/' {
			return r
		}
		return '-'
	}, strings.TrimSpace(key))
	if key == "" {
		key = "default"
	}
	if len(key) > 100 {
		key = key[:100]
	}
	return "<!-- infercrane-proof-gate:" + key + " -->"
}

const maxComment = 60_000

// Markdown is the pull-request comment: the verdict, the measured change
// against the measured noise, the answer check, and where the evidence is.
// Every value that came from a setup's configuration is shown as code, so it
// cannot mention people or render as markup.
func Markdown(result Result, key string) string {
	var b strings.Builder
	b.WriteString(CommentMarker(key) + "\n")
	fmt.Fprintf(&b, "### %s %s · InferCrane proof gate\n\n", icon(result.Verdict), result.Verdict)
	b.WriteString(text(result.Headline) + "\n\n")
	if result.Banner != "" {
		b.WriteString("> **" + text(result.Banner) + "**\n\n")
	}
	if result.Setup != nil && result.Baseline != nil {
		fmt.Fprintf(&b, "%s → %s\n\n", code(result.Baseline.Label), code(result.Setup.Label))
	}

	b.WriteString("| | Result |\n|---|---|\n")
	fmt.Fprintf(&b, "| %s | %s |\n", text(metricLabel(result.Speed.Metric)), speedCell(result.Speed))
	fmt.Fprintf(&b, "| Fresh servers | %s |\n", confirmationCell(result.Speed))
	fmt.Fprintf(&b, "| Answer check | %s |\n", answerCell(result.Answers))
	if conclusion := conclusionCell(result); conclusion != "" {
		fmt.Fprintf(&b, "| Check | %s |\n", conclusion)
	}
	b.WriteString("\n")

	if len(result.Reasons) > 0 {
		b.WriteString("<details><summary>Why</summary>\n\n")
		for _, reason := range result.Reasons {
			b.WriteString("- " + text(reason) + "\n")
		}
		b.WriteString("\n</details>\n\n")
	}

	evidence := []string{"Run " + code(result.CampaignID)}
	if result.EvidenceURL != "" {
		evidence[0] = "[Run " + code(result.CampaignID) + "](" + linkTarget(result.EvidenceURL) + ")"
	}
	if result.ReportDigest != "" {
		evidence = append(evidence, "report "+code(result.ReportDigest))
	}
	switch signature := result.Signature; {
	case signature.Error != "":
		evidence = append(evidence, "signature **did not verify**")
	case signature.Verified && signature.Pinned:
		evidence = append(evidence, "signed by your key "+code(signature.KeyID))
	case signature.Verified:
		evidence = append(evidence, "signed by key "+code(signature.KeyID))
	default:
		evidence = append(evidence, "not signed: "+text(optimizationreport.UnsignedText(signature.UnsignedReason)))
	}
	b.WriteString(strings.Join(evidence, " · ") + "\n\n")
	if result.Signature.Verified {
		b.WriteString("Check it yourself: save the report with `infercrane optimize report " + plain(result.CampaignID) + " --output json > report.json`, then run `infercrane optimize report verify report.json` and `infercrane optimize gate --file report.json`.\n")
	}
	out := b.String()
	if len(out) > maxComment {
		out = out[:maxComment] + "\n\n_Comment truncated; the job's gate result file has every value._\n"
	}
	return out
}

func icon(verdict string) string {
	switch verdict {
	case Pass:
		return "✅"
	case Watch:
		return "⚠️"
	case Block:
		return "❌"
	}
	return "❔"
}

func speedCell(speed Speed) string {
	if speed.ChangePercent == nil || speed.NoiseFloorPercent == nil {
		return "unknown"
	}
	return fmt.Sprintf("%+.1f%% against a ±%.1f%% noise floor measured in the same window · %s", *speed.ChangePercent, *speed.NoiseFloorPercent, decisionWords(speed.Decision))
}

func confirmationCell(speed Speed) string {
	switch {
	case speed.Confirmation != nil:
		return fmt.Sprintf("%+.1f%% against ±%.1f%% · %s · gain %s", speed.Confirmation.ChangePercent, speed.Confirmation.NoiseFloorPercent, decisionWords(speed.Confirmation.Decision), speed.Claim)
	case speed.Decision == optimizationreport.DecisionImproved:
		return "not measured again · gain not shown"
	}
	return "not needed: no gain to confirm"
}

func answerCell(answers *optimizationreport.AnswerCheck) string {
	if answers == nil {
		return "UNKNOWN · not run"
	}
	cell := answers.Verdict + " · " + text(answers.Reason)
	if answers.WatchThresholdPercent != nil && answers.BlockThresholdPercent != nil {
		cell += fmt.Sprintf(" (watch above %.0f%% different, block above %.0f%%)", *answers.WatchThresholdPercent, *answers.BlockThresholdPercent)
	}
	return cell
}

func conclusionCell(result Result) string {
	if result.Verdict != Watch {
		return ""
	}
	switch result.Conclusion {
	case ConclusionNeutral:
		return "WATCH is neutral here: the check does not fail"
	case ConclusionFailure:
		return "WATCH fails this check (on-watch: fail)"
	}
	return "WATCH passes this check (on-watch: pass)"
}

func decisionWords(decision string) string {
	switch decision {
	case optimizationreport.DecisionImproved:
		return "better"
	case optimizationreport.DecisionRegressed:
		return "**worse**"
	case optimizationreport.DecisionNoDifference:
		return "no measurable difference"
	}
	return "unknown"
}

// text makes generated prose safe in a table cell and inert as markup.
func text(value string) string {
	replacer := strings.NewReplacer("|", "\\|", "\r", " ", "\n", " ", "<", "&lt;", ">", "&gt;", "@", "&#64;", "`", "'", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_")
	return replacer.Replace(value)
}

// code shows a configuration value verbatim and inert.
func code(value string) string {
	value = plain(value)
	if value == "" {
		value = "unknown"
	}
	return "`" + value + "`"
}

func plain(value string) string {
	value = strings.NewReplacer("`", "'", "\r", " ", "\n", " ", "|", "\\|").Replace(value)
	if len(value) > 300 {
		value = value[:300] + "…"
	}
	return value
}

func linkTarget(value string) string {
	return strings.NewReplacer(" ", "%20", ")", "%29", "(", "%28", "\n", "", "\r", "").Replace(value)
}
