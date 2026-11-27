package proofreport

import (
	"fmt"
	"strings"
)

// Text renders the report for a terminal: the summary, then each workload and
// load with its saving and why, and where the full document is. It prints the
// View's strings and formats nothing itself.
func Text(d Document) string {
	v := NewView(d)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n%s\n\n", v.Title, v.Subtitle, v.Headline)
	width := 0
	for _, fact := range v.Facts {
		width = max(width, len(fact.Name))
	}
	for _, fact := range v.Facts {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, fact.Name, fact.Value)
	}
	for _, workload := range v.Workloads {
		fmt.Fprintf(&b, "\n%s\n  %s\n  Traffic: %s\n", workload.Heading, workload.Summary, workload.Traffic)
		for _, pair := range workload.Saving {
			fmt.Fprintf(&b, "  %s: %s\n", pair.Name, pair.Value)
		}
		if workload.LoadNote != "" {
			fmt.Fprintf(&b, "  Per load: %s\n", workload.LoadNote)
		}
		for _, load := range workload.Loads {
			fmt.Fprintf(&b, "    %-12s %-13s %s\n", load.Load, load.Decision, load.Setup)
		}
		for _, run := range workload.Runs {
			fmt.Fprintf(&b, "\n  %s\n    %s\n", run.Heading, run.Headline)
			for _, row := range run.Numbers {
				fmt.Fprintf(&b, "    %-28s %s → %s  %s (noise %s, %s)\n", row.Label, row.Before, row.After, row.Change, row.Noise, row.Decision)
			}
			for _, row := range run.Targets {
				fmt.Fprintf(&b, "    %-28s before %s · after %s\n", row.Limit, row.Before, row.After)
			}
			fmt.Fprintf(&b, "    Answer quality: %s\n    %s\n    %s\n", run.Quality, run.Claim, run.Saving)
			for _, reason := range run.Reasons {
				fmt.Fprintf(&b, "      - %s\n", reason)
			}
			for _, warning := range run.Warnings {
				fmt.Fprintf(&b, "    Check: %s\n", warning)
			}
		}
	}
	if len(v.Unknown) > 0 {
		b.WriteString("\nUnknown, never estimated\n")
		for _, item := range v.Unknown {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
	}
	fmt.Fprintf(&b, "\nReport digest %s\n%s\n", v.Digest, v.Signature)
	b.WriteString("Every number's setup, evidence and rerun command: --output markdown, or --html FILE.\n")
	return b.String()
}
