package proofreport

import (
	"strings"
)

// Markdown renders the report as one Markdown document, ready to print or to
// convert to PDF. Text from records (setup labels, model names, flags) and the
// caller's title are escaped, so none of it is read as Markdown or HTML.
func Markdown(d Document) []byte {
	v := NewView(d)
	var b strings.Builder
	line := func(parts ...string) {
		b.WriteString(strings.Join(parts, ""))
		b.WriteByte('\n')
	}
	line("# ", md(v.Title))
	line()
	line(md(v.Subtitle))
	line()
	line("> **", md(v.Headline), "**")
	line()
	line("| | |")
	line("|---|---|")
	for _, fact := range v.Facts {
		line("| ", md(fact.Name), " | ", md(fact.Value), " |")
	}
	line()
	line("**How to read this report.** ", md(v.ClaimRule), " A number marked *unknown* was not measured or not comparable; nothing is estimated.")
	for _, workload := range v.Workloads {
		line()
		line("## ", md(workload.Heading))
		line()
		line(md(workload.Summary))
		line()
		line("Traffic: ", md(workload.Traffic), ".")
		if len(workload.Saving) > 0 {
			line()
			for _, pair := range workload.Saving {
				line("- **", md(pair.Name), ":** ", md(pair.Value))
			}
		}
		if len(workload.Loads) > 0 || workload.LoadNote != "" {
			line()
			line("### Verdict per load")
			line()
			if workload.LoadNote != "" {
				line(md(workload.LoadNote))
				line()
			}
			if len(workload.Loads) > 0 {
				line("| Load | Decision | Setup | Why |")
				line("|---|---|---|---|")
				for _, load := range workload.Loads {
					line("| ", md(load.Load), " | ", md(load.Decision), " | ", md(load.Setup), " | ", md(load.Reason), " |")
				}
			}
		}
		for _, run := range workload.Runs {
			writeRun(&b, run)
		}
	}
	line()
	line("## Limits")
	line()
	for _, limit := range v.Limits {
		line("- ", md(limit))
	}
	if len(v.Unknown) > 0 {
		line()
		line("## Unknown, never estimated")
		line()
		for _, item := range v.Unknown {
			line("- ", md(item))
		}
	}
	line()
	line("---")
	line()
	line("Report digest `", code(v.Digest), "`. Every number comes from the runs' persisted measurements.")
	line()
	line(md(v.Signature))
	return []byte(b.String())
}

func writeRun(b *strings.Builder, run RunView) {
	line := func(parts ...string) {
		b.WriteString(strings.Join(parts, ""))
		b.WriteByte('\n')
	}
	line()
	line("### ", md(run.Heading))
	line()
	line("**", md(run.Headline), "**")
	for _, warning := range run.Warnings {
		line()
		line("> Check: ", md(warning))
	}
	if len(run.Setups) > 0 {
		line()
		line("| Measured on | Before (your current setup) | After |")
		line("|---|---|---|")
		for _, row := range run.Setups {
			line("| ", md(row.Name), " | ", md(row.Before), " | ", md(row.After), " |")
		}
	}
	line()
	for _, pair := range run.Context {
		line("- **", md(pair.Name), ":** ", md(pair.Value))
	}
	if len(run.Numbers) > 0 {
		line()
		line("| Outcome | Before | After | Change | Noise | Decision |")
		line("|---|---|---|---|---|---|")
		for _, row := range run.Numbers {
			decision := md(row.Decision)
			if row.Reason != "" {
				decision += ": " + md(row.Reason)
			}
			line("| ", md(row.Label), " | ", md(row.Before), " | ", md(row.After), " | ", md(row.Change), " | ", md(row.Noise), " | ", decision, " |")
		}
	}
	if len(run.Targets) > 0 {
		line()
		line("| Your limit | Before | After |")
		line("|---|---|---|")
		for _, row := range run.Targets {
			line("| ", md(row.Limit), " | ", md(row.Before), " | ", md(row.After), " |")
		}
	}
	line()
	line("- **Answer quality:** ", md(run.Quality))
	line("- **", md(run.Claim), "**")
	line("- **", md(run.Saving), "**")
	for _, reason := range run.Reasons {
		line("  - ", md(upperFirst(reason)), ".")
	}
	line()
	line("Evidence:")
	line()
	for _, pair := range run.Evidence {
		line("- ", md(upperFirst(pair.Name)), ": `", code(pair.Value), "`")
	}
	if len(run.Rerun) > 0 {
		line()
		line("Measure it again:")
		line()
		line("```text")
		for _, command := range run.Rerun {
			line(strings.ReplaceAll(command, "```", "'''"))
		}
		line("```")
	}
}

// md escapes text so Markdown reads it as plain text, on one line: every
// character that can open emphasis, a link, code, HTML or a table cell, and a
// first character that could start a heading, a list or a quote.
func md(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	var b strings.Builder
	for index, r := range value {
		if strings.ContainsRune("\\`*_[]<>|~", r) || index == 0 && strings.ContainsRune("#+-=>", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// code keeps a value inside an inline code span.
func code(value string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(value), " "), "`", "'")
}
