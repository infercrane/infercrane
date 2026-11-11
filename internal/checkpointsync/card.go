package checkpointsync

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

const (
	maxPriorTables  = 3
	maxPriorRows    = 20
	maxPriorColumns = 8
	maxPriorCell    = 80
)

var (
	sourceRevision    = regexp.MustCompile(`(?i)(?:source|base(?:[ -]model)?)\s+(?:model\s+)?revision[^0-9a-f\n]{0,40}([0-9a-f]{40})\b`)
	enginePullRequest = regexp.MustCompile(`https?://github\.com/(vllm-project/vllm|sgl-project/sglang)/pull/(\d+)`)
	engineMinimum     = regexp.MustCompile(`(?i)\b(vllm|sglang)\s*(?:>=|≥|>|==)\s*v?(\d+\.\d+(?:\.\d+)?(?:\.?post\d+)?)`)
	engineOrLater     = regexp.MustCompile(`(?i)\b(vllm|sglang)\s+v?(\d+\.\d+(?:\.\d+)?)\s*(?:\+|or (?:later|newer|above)|and (?:later|newer|above))`)
	markdownLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTag           = regexp.MustCompile(`<[^>]+>`)
	tableSeparator    = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
	htmlTable         = regexp.MustCompile(`(?is)<table[^>]*>(.*?)</table>`)
	htmlRow           = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	htmlCell          = regexp.MustCompile(`(?is)<t[hd][^>]*>(.*?)</t[hd]>`)
	priorKeywords     = []string{"accuracy", "benchmark", "gsm8k", "mmlu", "gpqa", "aime", "humaneval", "ifeval", "ifbench", "math", "recovery", "bf16", "baseline", "acceptance", "throughput", "latency", "tokens/s", "tok/s", "score", "livecodebench", "eval"}
)

// declaredBaseRevision returns the exact base commit when the card states it.
func declaredBaseRevision(readme []byte, base string) string {
	text := string(readme)
	link := regexp.MustCompile(`(?i)https?://huggingface\.co/` + regexp.QuoteMeta(base) + `/(?:commit|tree|blob|resolve)/([0-9a-f]{40})\b`)
	if match := link.FindStringSubmatch(text); match != nil {
		return strings.ToLower(match[1])
	}
	if match := sourceRevision.FindStringSubmatch(text); match != nil {
		return strings.ToLower(match[1])
	}
	return ""
}

// engineRequirements parses engine requirements from the card. A pull request
// stays an unreleased change unless the policy records the release that
// contains it.
func engineRequirements(readme []byte, policy checkpointcatalog.Policy) []checkpointcatalog.EngineRequirement {
	text := string(readme)
	minimums := map[string]string{}
	for _, pattern := range []*regexp.Regexp{engineMinimum, engineOrLater} {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			engine := strings.ToLower(match[1])
			if current, found := minimums[engine]; !found || checkpointcatalog.CompareVersions(match[2], current) > 0 {
				minimums[engine] = match[2]
			}
		}
	}
	pulls := map[string]string{}
	for _, match := range enginePullRequest.FindAllStringSubmatch(text, -1) {
		engine := "vllm"
		if strings.HasPrefix(match[1], "sgl-project") {
			engine = "sglang"
		}
		pulls[match[1]+"#"+match[2]] = engine
	}
	var out []checkpointcatalog.EngineRequirement
	for pull, engine := range pulls {
		if release, resolved := policy.EngineRelease(pull); resolved {
			if current, found := minimums[engine]; !found || checkpointcatalog.CompareVersions(release, current) > 0 {
				minimums[engine] = release
			}
			continue
		}
		out = append(out, checkpointcatalog.EngineRequirement{Engine: engine, State: checkpointcatalog.EngineUnreleasedChange, PullRequest: pull})
	}
	for engine, version := range minimums {
		out = append(out, checkpointcatalog.EngineRequirement{Engine: engine, State: checkpointcatalog.EngineMinimumVersion, MinimumVersion: version})
	}
	sortRequirements(out)
	return out
}

func sortRequirements(requirements []checkpointcatalog.EngineRequirement) {
	sort.Slice(requirements, func(i, j int) bool {
		a, b := requirements[i], requirements[j]
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		if a.State != b.State {
			return a.State < b.State
		}
		return a.PullRequest+a.MinimumVersion+a.Note < b.PullRequest+b.MinimumVersion+b.Note
	})
}

type cardTable struct {
	offset int
	header []string
	rows   [][]string
}

// priorTables copies benchmark-looking markdown and HTML tables from the card.
// They are stored verbatim as unmeasured publisher priors.
func priorTables(readme []byte, revision string) []checkpointcatalog.PriorTable {
	text := strings.ReplaceAll(string(readme), "\r\n", "\n")
	tables := append(markdownTables(text), htmlTables(text)...)
	sort.SliceStable(tables, func(i, j int) bool { return tables[i].offset < tables[j].offset })
	var out []checkpointcatalog.PriorTable
	for index, table := range tables {
		if len(out) == maxPriorTables {
			break
		}
		if len(table.header) == 0 || len(table.rows) == 0 || !priorRelevant(table.header, table.rows) {
			continue
		}
		out = append(out, checkpointcatalog.PriorTable{
			Source:        fmt.Sprintf("model card table %d at revision %s", index+1, revision),
			EvidenceState: checkpointcatalog.EvidenceUnmeasured,
			Columns:       table.header,
			Rows:          table.rows,
		})
	}
	return out
}

func markdownTables(text string) []cardTable {
	lines := strings.Split(text, "\n")
	offsets := make([]int, len(lines))
	for index, position := 1, len(lines[0])+1; index < len(lines); index++ {
		offsets[index] = position
		position += len(lines[index]) + 1
	}
	var out []cardTable
	for index := 0; index+1 < len(lines); index++ {
		if !strings.Contains(lines[index], "|") || !tableSeparator.MatchString(lines[index+1]) {
			continue
		}
		table := cardTable{offset: offsets[index], header: tableCells(lines[index])}
		next := index + 2
		for ; next < len(lines) && strings.Contains(lines[next], "|") && strings.TrimSpace(lines[next]) != ""; next++ {
			if len(table.rows) < maxPriorRows {
				table.rows = append(table.rows, tableCells(lines[next]))
			}
		}
		out = append(out, table)
		index = next - 1
	}
	return out
}

func htmlTables(text string) []cardTable {
	var out []cardTable
	for _, match := range htmlTable.FindAllStringSubmatchIndex(text, -1) {
		table := cardTable{offset: match[0]}
		for _, row := range htmlRow.FindAllStringSubmatch(text[match[2]:match[3]], -1) {
			var cells []string
			for _, cell := range htmlCell.FindAllStringSubmatch(row[1], -1) {
				if len(cells) < maxPriorColumns {
					cells = append(cells, cleanCell(cell[1]))
				}
			}
			switch {
			case len(cells) == 0:
			case table.header == nil:
				table.header = cells
			case len(table.rows) < maxPriorRows:
				table.rows = append(table.rows, cells)
			}
		}
		out = append(out, table)
	}
	return out
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(strings.TrimSuffix(line, "|"), "|")
	var cells []string
	for _, cell := range strings.Split(line, "|") {
		if len(cells) == maxPriorColumns {
			break
		}
		cells = append(cells, cleanCell(cell))
	}
	return cells
}

func cleanCell(cell string) string {
	cell = markdownLink.ReplaceAllString(cell, "$1")
	cell = htmlTag.ReplaceAllString(cell, " ")
	cell = strings.NewReplacer("**", "", "__", "", "`", "", "&nbsp;", " ").Replace(cell)
	cell = strings.Join(strings.Fields(cell), " ")
	if utf8.RuneCountInString(cell) > maxPriorCell {
		cell = string([]rune(cell)[:maxPriorCell-1]) + "…"
	}
	return cell
}

func priorRelevant(header []string, rows [][]string) bool {
	text := strings.ToLower(strings.Join(header, " "))
	hasDigit := false
	for _, row := range rows {
		if len(row) > 0 {
			text += " " + strings.ToLower(row[0])
		}
		for _, cell := range row {
			if strings.ContainsAny(cell, "0123456789") {
				hasDigit = true
			}
		}
	}
	if !hasDigit {
		return false
	}
	for _, keyword := range priorKeywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}
