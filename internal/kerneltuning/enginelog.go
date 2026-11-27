package kerneltuning

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// EngineReading is what an engine's start-up log says about its fused-MoE
// kernel: the backend it chose, the tuned files it read, and the files it
// looked for and did not find, for which it used its built-in default.
type EngineReading struct {
	// Backend is the MoE backend the engine chose ("TRITON"), when it said.
	Backend string `json:"backend,omitempty"`
	// ConfigFiles are the paths of the tuned files the engine read.
	ConfigFiles []string `json:"config_files,omitempty"`
	// DefaultFor are the file names the engine looked for and did not find.
	DefaultFor []string `json:"default_for,omitempty"`
}

// vLLM 0.34.0 log lines (fused_moe.py get_moe_configs; oracle/base.py
// make_log_backend). Each is logged once per process.
var (
	ansiEscape     = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	usedConfigLine = regexp.MustCompile(`Using configuration from (\S+) for MoE layer\.`)
	defaultLine    = regexp.MustCompile(`Using default MoE config\. Performance might be sub-optimal! Config file not found at (.+)$`)
	backendLine    = regexp.MustCompile(`Using ([A-Z0-9_]+) [A-Za-z0-9]+ MoE (?:LoRA )?backend`)
)

// ReadEngineLog reads the fused-MoE lines of an engine log.
func ReadEngineLog(text string) EngineReading {
	var reading EngineReading
	for _, line := range strings.Split(ansiEscape.ReplaceAllString(text, ""), "\n") {
		line = strings.TrimRight(line, "\r ")
		if match := usedConfigLine.FindStringSubmatch(line); match != nil && !slices.Contains(reading.ConfigFiles, match[1]) {
			reading.ConfigFiles = append(reading.ConfigFiles, match[1])
		}
		if match := defaultLine.FindStringSubmatch(line); match != nil {
			for _, missing := range strings.Split(match[1], ", ") {
				if name := path.Base(strings.TrimSpace(missing)); name != "." && name != "/" && !slices.Contains(reading.DefaultFor, name) {
					reading.DefaultFor = append(reading.DefaultFor, name)
				}
			}
		}
		if match := backendLine.FindStringSubmatch(line); match != nil && reading.Backend == "" {
			reading.Backend = match[1]
		}
	}
	return reading
}

// ReadFrom lists the files the engine read from folder: the proof that a
// tuned set was invoked, not only mounted.
func (reading EngineReading) ReadFrom(folder string) []string {
	folder = path.Clean(folder)
	var names []string
	for _, file := range reading.ConfigFiles {
		if path.Dir(path.Clean(file)) == folder {
			names = append(names, path.Base(file))
		}
	}
	return names
}

// Gaps lists the shapes the engine served with its default tiles: the files
// it looked for and did not find. A tuned set for one of them can change the
// kernel; a set for any other shape cannot.
func (reading EngineReading) Gaps() []FusedMoEShape {
	var shapes []FusedMoEShape
	for _, name := range reading.DefaultFor {
		if shape, _, err := ParseFileName(name); err == nil {
			shapes = append(shapes, shape)
		}
	}
	return shapes
}
