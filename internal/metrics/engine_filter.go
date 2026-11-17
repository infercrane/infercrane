package metrics

import (
	"bufio"
	"fmt"
	"strings"
)

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
	for _, field := range spec.fields {
		for _, name := range field.names {
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
		name, _, _, ok := splitSample(line)
		if !ok {
			continue
		}
		if _, relevant := wanted[name]; !relevant {
			continue
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
