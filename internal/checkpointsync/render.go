package checkpointsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

const inlineArrayWidth = 120

// FormatCatalog renders the catalog as stable, reviewable JSON: objects keep
// field order and short scalar arrays stay on one line.
func FormatCatalog(catalog checkpointcatalog.Catalog) ([]byte, error) {
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(catalog); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeJSON(&out, bytes.TrimSpace(raw.Bytes()), ""); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func writeJSON(out *bytes.Buffer, raw []byte, indent string) error {
	switch raw[0] {
	case '{':
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if _, err := decoder.Token(); err != nil {
			return err
		}
		if !decoder.More() {
			out.WriteString("{}")
			return nil
		}
		out.WriteString("{\n")
		for first := true; decoder.More(); first = false {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			var value json.RawMessage
			if err = decoder.Decode(&value); err != nil {
				return err
			}
			if !first {
				out.WriteString(",\n")
			}
			name, _ := json.Marshal(key)
			out.WriteString(indent + "  " + string(name) + ": ")
			if err = writeJSON(out, value, indent+"  "); err != nil {
				return err
			}
		}
		out.WriteString("\n" + indent + "}")
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		if len(items) == 0 {
			out.WriteString("[]")
			return nil
		}
		if inline, ok := inlineArray(items); ok {
			out.WriteString(inline)
			return nil
		}
		out.WriteString("[\n")
		for index, item := range items {
			if index > 0 {
				out.WriteString(",\n")
			}
			out.WriteString(indent + "  ")
			if err := writeJSON(out, item, indent+"  "); err != nil {
				return err
			}
		}
		out.WriteString("\n" + indent + "]")
	default:
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return err
		}
		out.Write(compact.Bytes())
	}
	return nil
}

func inlineArray(items []json.RawMessage) (string, bool) {
	parts := make([]string, 0, len(items))
	width := 2
	for _, item := range items {
		if item[0] == '{' || item[0] == '[' {
			return "", false
		}
		parts = append(parts, string(item))
		width += len(item) + 2
	}
	if width > inlineArrayWidth {
		return "", false
	}
	return "[" + strings.Join(parts, ", ") + "]", true
}

// RenderDiff writes the reviewable markdown summary of a plan.
func RenderDiff(result Result) []byte {
	diff, catalog := result.Diff, result.Catalog
	var out bytes.Buffer
	pending, candidates := 0, 0
	for _, entry := range catalog.Entries {
		if entry.Review.State == checkpointcatalog.ReviewPending {
			pending++
		}
		if entry.Eligibility.State == checkpointcatalog.EligibilityCandidate {
			candidates++
		}
	}
	out.WriteString("# Checkpoint catalog diff\n\n")
	fmt.Fprintf(&out, "- Snapshot fetched: %s\n- Snapshot digest: `%s`\n- Policy digest: `%s`\n", diff.FetchedAt, diff.SnapshotDigest, diff.PolicyDigest)
	fmt.Fprintf(&out, "- Entries: %d (%d candidates, %d pending review)\n", len(catalog.Entries), candidates, pending)
	fmt.Fprintf(&out, "- Changes: %d added, %d revised, %d changed, %d removed, %d unchanged\n", len(diff.Added), len(diff.Revised), len(diff.Changed), len(diff.Removed), diff.Unchanged)
	fmt.Fprintf(&out, "- Selection: %s\n", selectionLabel(result.Manifest))
	for _, note := range diff.Notes {
		fmt.Fprintf(&out, "- Note: %s\n", note)
	}
	out.WriteString("\nEvery entry is unmeasured. Publisher tables are priors, not InferCrane evidence.\n")

	out.WriteString("\n## Review required\n\nApprove a publisher or format in the policy, then re-run `plan` on the same snapshot.\n\n")
	rows := [][]string{}
	for _, entry := range catalog.Entries {
		if entry.Review.State == checkpointcatalog.ReviewPending {
			rows = append(rows, []string{entryLabel(entry), formatLabel(entry), hardwareLabel(entry), strings.Join(entry.Review.Reasons, "; ")})
		}
	}
	table(&out, []string{"Entry", "Format", "Hardware", "Reasons"}, rows)

	out.WriteString("\n## Candidates\n\nApproved, licensed, runnable quantized checkpoints. Proposals still treat them as unmeasured.\n\n")
	rows = rows[:0]
	for _, entry := range catalog.Entries {
		if entry.Eligibility.State == checkpointcatalog.EligibilityCandidate {
			rows = append(rows, []string{entryLabel(entry), entry.Base.Repository + " (" + entry.Base.Lineage + ")", formatLabel(entry), hardwareLabel(entry), strings.Join(entry.Runtimes, ", "), requirementsLabel(entry)})
		}
	}
	table(&out, []string{"Entry", "Base", "Format", "Hardware", "Runtimes", "Engine requirements"}, rows)

	out.WriteString("\n## Added\n\n")
	rows = rows[:0]
	for _, entry := range diff.Added {
		selectedBy, found := diff.Selected[entry.Repository]
		if !found {
			selectedBy = "pinned"
		}
		rows = append(rows, []string{entryLabel(entry), entry.Base.Repository, formatLabel(entry), hardwareLabel(entry), selectedBy, eligibilityLabel(entry)})
	}
	table(&out, []string{"Entry", "Base", "Format", "Hardware", "Selected by", "Eligibility"}, rows)

	out.WriteString("\n## New revisions\n\nA new revision of a reviewed publisher and format passes an automated re-review unless its kind, base, format, hardware vendor, or license changed.\n\n")
	rows = rows[:0]
	for _, revision := range diff.Revised {
		var from, to []string
		for _, entry := range revision.From {
			from = append(from, short(entry.Revision))
		}
		for _, entry := range revision.To {
			to = append(to, fmt.Sprintf("%s %s: %s", short(entry.Revision), entry.RevisionState, eligibilityLabel(entry)))
		}
		rows = append(rows, []string{revision.Repository, strings.Join(from, ", "), strings.Join(to, "<br>")})
	}
	table(&out, []string{"Repository", "From", "To"}, rows)

	out.WriteString("\n## Changed\n\n")
	rows = rows[:0]
	for _, change := range diff.Changed {
		rows = append(rows, []string{entryLabel(change.Entry), strings.Join(change.Fields, ", ")})
	}
	table(&out, []string{"Entry", "Fields"}, rows)

	out.WriteString("\n## Removed\n\n")
	rows = rows[:0]
	for _, removal := range diff.Removed {
		rows = append(rows, []string{entryLabel(removal.Entry), removal.Reason})
	}
	table(&out, []string{"Entry", "Reason"}, rows)

	out.WriteString("\n## Skipped repositories\n\nRepositories the selection rule picked that did not become entries.\n")
	reasons := make([]string, 0, len(diff.Skipped))
	for reason := range diff.Skipped {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	if len(reasons) == 0 {
		out.WriteString("\nNone.\n")
	}
	for _, reason := range reasons {
		fmt.Fprintf(&out, "\n### %s (%d)\n\n%s\n", reason, len(diff.Skipped[reason]), strings.Join(diff.Skipped[reason], ", "))
	}
	return out.Bytes()
}

// selectionLabel summarizes the selection rule the snapshot was fetched with.
func selectionLabel(manifest Manifest) string {
	var windows []string
	for _, publisher := range manifest.Publishers {
		switch {
		case publisher.CollectionOnly:
			windows = append(windows, publisher.Organization+" collections only")
		case publisher.MinDownloads > 0:
			windows = append(windows, fmt.Sprintf("%s since %s or %d+ downloads", publisher.Organization, publisher.CreatedAfter, publisher.MinDownloads))
		default:
			windows = append(windows, publisher.Organization+" since "+publisher.CreatedAfter)
		}
	}
	label := strings.Join(windows, "; ")
	for _, collection := range manifest.Collections {
		label += fmt.Sprintf("; collection `%s` (%d models)", collection.Slug, collection.Members)
	}
	return label
}

func table(out *bytes.Buffer, header []string, rows [][]string) {
	if len(rows) == 0 {
		out.WriteString("None.\n")
		return
	}
	out.WriteString("| " + strings.Join(header, " | ") + " |\n|" + strings.Repeat(" --- |", len(header)) + "\n")
	for _, row := range rows {
		cells := make([]string, len(row))
		for index, cell := range row {
			cells[index] = strings.ReplaceAll(cell, "|", `\|`)
		}
		out.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
}

func entryLabel(entry checkpointcatalog.Entry) string {
	return fmt.Sprintf("[%s](%s) @ `%s`", entry.Repository, entry.SourceURL, short(entry.Revision))
}

func formatLabel(entry checkpointcatalog.Entry) string {
	if entry.Speculator != nil {
		return "speculator " + entry.Speculator.Method
	}
	if entry.Quantization != nil {
		return entry.Quantization.Tool + " " + entry.Quantization.Scheme
	}
	return "unknown"
}

func hardwareLabel(entry checkpointcatalog.Entry) string {
	label := entry.Hardware.Vendor
	if len(entry.Hardware.Architectures) > 0 {
		label += " " + strings.Join(entry.Hardware.Architectures, ", ")
	}
	return label + " (" + entry.Hardware.ProductSupport + ")"
}

func eligibilityLabel(entry checkpointcatalog.Entry) string {
	if entry.Eligibility.State == checkpointcatalog.EligibilityCandidate {
		return "candidate"
	}
	return "catalog only: " + strings.Join(entry.Eligibility.Blockers, "; ")
}

func requirementsLabel(entry checkpointcatalog.Entry) string {
	var parts []string
	for _, requirement := range entry.EngineRequirements {
		switch requirement.State {
		case checkpointcatalog.EngineMinimumVersion:
			parts = append(parts, requirement.Engine+" >= "+requirement.MinimumVersion)
		case checkpointcatalog.EngineUnreleasedChange:
			parts = append(parts, requirement.Engine+" needs "+requirement.PullRequest)
		default:
			parts = append(parts, requirement.Engine+" blocked")
		}
	}
	if len(parts) == 0 {
		return "none declared"
	}
	return strings.Join(parts, "; ")
}
