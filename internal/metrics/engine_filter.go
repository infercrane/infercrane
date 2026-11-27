package metrics

import (
	"bufio"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// infoGroupLabels are kept on an info gauge's line besides the labels a field
// reads, so data-parallel engine cores still count once each.
var infoGroupLabels = []string{"engine"}

// FilterEngineExposition keeps only the sample lines ParseEngine reads for
// the detected engine and drops every comment, help text, and unrelated
// family. The InferCrane agent runs it on the customer's machine before a
// scrape is relayed, so arbitrary metric names and labels never leave that
// machine, and the control plane still parses the result with ParseEngine
// exactly as it parses a scrape it made itself. It returns the detected
// engine with the filtered text.
func FilterEngineExposition(text string) (string, string, error) {
	if len(text) > MaxEnginePayloadBytes {
		return "", "", fmt.Errorf("engine metrics payload exceeds %d bytes", MaxEnginePayloadBytes)
	}
	spec, err := detectEngine(text)
	if err != nil {
		return "", "", err
	}
	wanted := map[string]struct{}{}
	// An info gauge carries its values as labels, most of which ParseEngine
	// never reads; only the labels a field reads leave the machine.
	infoLabels := map[string][]string{}
	for _, field := range spec.fields {
		for _, name := range field.names {
			if field.label != "" {
				if len(infoLabels[name]) == 0 {
					infoLabels[name] = append([]string{}, infoGroupLabels...)
				}
				infoLabels[name] = append(infoLabels[name], field.label)
			}
			if field.kind == histogram {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					wanted[name+suffix] = struct{}{}
				}
				continue
			}
			wanted[name] = struct{}{}
		}
	}
	var out strings.Builder
	kept := 0
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), MaxEnginePayloadBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawLabels, value, ok := splitSample(line)
		if !ok {
			continue
		}
		if _, relevant := wanted[name]; !relevant {
			continue
		}
		if keep := infoLabels[name]; len(keep) > 0 {
			labels, labelErr := parsePrometheusLabels(rawLabels)
			if labelErr != nil {
				return "", "", fmt.Errorf("engine metric %s: %w", name, labelErr)
			}
			line = infoLine(name, labels, keep, value)
		}
		kept++
		if kept > maxEngineSamples {
			return "", "", fmt.Errorf("engine metrics payload contains more than %d relevant samples", maxEngineSamples)
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err = scanner.Err(); err != nil {
		return "", "", fmt.Errorf("scan engine metrics payload: %w", err)
	}
	if kept == 0 {
		return "", "", ErrNoEngineMetrics
	}
	return out.String(), spec.engine, nil
}

// infoLine writes an info gauge's sample with only the kept labels, in a
// stable order.
func infoLine(name string, labels map[string]string, keep []string, value string) string {
	kept := make([]string, 0, len(keep))
	for _, key := range keep {
		if _, ok := labels[key]; ok && !slices.Contains(kept, key) {
			kept = append(kept, key)
		}
	}
	sort.Strings(kept)
	escaper := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	parts := make([]string, 0, len(kept))
	for _, key := range kept {
		parts = append(parts, key+`="`+escaper.Replace(labels[key])+`"`)
	}
	return name + "{" + strings.Join(parts, ",") + "} " + value
}
