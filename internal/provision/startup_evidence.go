package provision

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxStartupConsoleBytes = 128 << 10

var startupMarker = regexp.MustCompile(`(?m)^infercrane_startup stage=([a-z_]+) at=([^\r\n ]+)[\r]?$`)

var startupStageOrder = map[string]int{
	"identity_start":            0,
	"identity_ready":            1,
	"image_check":               2,
	"image_cache_hit":           3,
	"image_pull_start":          3,
	"image_cache_miss_required": 3,
	"image_pull_complete":       4,
	"runtime_start":             5,
	"runtime_container_started": 6,
}

// startupEvidence is the closed, provider-neutral subset of machine startup
// output that InferCrane is willing to persist. Arbitrary console output is
// deliberately discarded because it can contain model, runtime, or secret
// material.
type startupEvidence struct {
	SchemaVersion int            `json:"schema_version"`
	Source        string         `json:"source"`
	CurrentStage  string         `json:"current_stage"`
	ImageCache    string         `json:"image_cache"`
	Stages        []startupStage `json:"stages"`
}

type startupStage struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func parseStartupEvidence(raw string) (startupEvidence, bool) {
	if len(raw) > maxStartupConsoleBytes {
		raw = raw[len(raw)-maxStartupConsoleBytes:]
	}
	if !strings.Contains(raw, "infercrane_startup stage=") {
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw)); err == nil && len(decoded) <= maxStartupConsoleBytes {
			raw = string(decoded)
		}
	}
	matches := startupMarker.FindAllStringSubmatch(raw, 64)
	if len(matches) == 0 {
		return startupEvidence{}, false
	}

	// EC2 console output can contain more than one boot. Only the most recent
	// coherent InferCrane sequence is relevant to the current runtime.
	var stages map[string]time.Time
	for _, match := range matches {
		name := match[1]
		if _, known := startupStageOrder[name]; !known {
			continue
		}
		at, err := time.Parse(time.RFC3339, match[2])
		if err != nil {
			continue
		}
		if name == "identity_start" {
			stages = map[string]time.Time{name: at.UTC()}
			continue
		}
		if stages == nil {
			continue
		}
		if previous, exists := stages[name]; !exists || at.Before(previous) {
			stages[name] = at.UTC()
		}
	}
	if len(stages) == 0 {
		return startupEvidence{}, false
	}

	ordered := make([]startupStage, 0, len(stages))
	for name, at := range stages {
		ordered = append(ordered, startupStage{Name: name, At: at})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return startupStageOrder[ordered[i].Name] < startupStageOrder[ordered[j].Name]
		}
		return ordered[i].At.Before(ordered[j].At)
	})

	// Reject markers that move backwards through the startup state machine.
	coherent := ordered[:0]
	lastOrder := -1
	for _, stage := range ordered {
		order := startupStageOrder[stage.Name]
		if order < lastOrder {
			continue
		}
		coherent = append(coherent, stage)
		lastOrder = order
	}
	if len(coherent) == 0 || coherent[0].Name != "identity_start" {
		return startupEvidence{}, false
	}
	imageCache := "unknown"
	if _, ok := stages["image_cache_hit"]; ok {
		imageCache = "hit"
	} else if _, ok := stages["image_cache_miss_required"]; ok {
		imageCache = "required_miss"
	} else if _, ok := stages["image_pull_start"]; ok {
		imageCache = "miss"
	}
	return startupEvidence{SchemaVersion: 1, Source: "provider_console", CurrentStage: coherent[len(coherent)-1].Name, ImageCache: imageCache, Stages: coherent}, true
}
