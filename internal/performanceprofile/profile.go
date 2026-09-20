// Package performanceprofile defines versioned benchmark workload shapes.
// Profiles are measurement inputs, not performance claims or magic runtime
// flags. Exact results remain tied to the persisted model/runtime/GPU tuple.
package performanceprofile

import (
	"fmt"
	"sort"
	"strings"
)

const Version = "benchmark-profile-v1"

type Profile struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Objective    string `json:"objective"`
	Requests     int    `json:"requests"`
	Concurrency  int    `json:"concurrency"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Streaming    bool   `json:"streaming"`
}

var profiles = map[string]Profile{
	"balanced": {
		Name: "balanced", Objective: "balanced", Requests: 256, Concurrency: 8, InputTokens: 512, OutputTokens: 128, Streaming: true,
		Description: "Representative interactive traffic with moderate concurrency.",
	},
	"interactive": {
		Name: "interactive", Objective: "latency", Requests: 256, Concurrency: 1, InputTokens: 512, OutputTokens: 128, Streaming: true,
		Description: "Single-client latency emphasizing TTFT and TPOT.",
	},
	"throughput": {
		Name: "throughput", Objective: "throughput", Requests: 512, Concurrency: 32, InputTokens: 512, OutputTokens: 256, Streaming: true,
		Description: "Sustained concurrent generation emphasizing output tokens per second.",
	},
	"long-context": {
		Name: "long-context", Objective: "long_context", Requests: 64, Concurrency: 4, InputTokens: 8192, OutputTokens: 256, Streaming: true,
		Description: "Long-prompt behavior with bounded generation.",
	},
	"long-generation": {
		Name: "long-generation", Objective: "long_generation", Requests: 64, Concurrency: 4, InputTokens: 512, OutputTokens: 1024, Streaming: true,
		Description: "Long generation behavior and sustained decode latency.",
	},
	"overload": {
		Name: "overload", Objective: "bounded_overload", Requests: 512, Concurrency: 128, InputTokens: 512, OutputTokens: 256, Streaming: true,
		Description: "Bounded high-concurrency load for admission, error, and goodput evidence.",
	},
	"buffered": {
		Name: "buffered", Objective: "buffered_latency", Requests: 256, Concurrency: 8, InputTokens: 512, OutputTokens: 128, Streaming: false,
		Description: "Non-streaming response latency for buffered clients.",
	},
	"public-interactive": {
		Name: "public-interactive", Objective: "latency", Requests: 32, Concurrency: 8, InputTokens: 3919, OutputTokens: 295, Streaming: true,
		Description: "Median token shape from the sampled public trace.",
	},
	"public-long-prefill": {
		Name: "public-long-prefill", Objective: "long_context", Requests: 32, Concurrency: 4, InputTokens: 7897, OutputTokens: 295, Streaming: true,
		Description: "P95 input with median output to stress prefill.",
	},
	"public-decode-heavy": {
		Name: "public-decode-heavy", Objective: "long_generation", Requests: 32, Concurrency: 4, InputTokens: 3919, OutputTokens: 1377, Streaming: true,
		Description: "Median input with P95 output to stress decode.",
	},
}

var publicPriorNames = []string{"public-interactive", "public-long-prefill", "public-decode-heavy"}

func Get(name string) (Profile, error) {
	profile, ok := profiles[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Profile{}, fmt.Errorf("unknown benchmark profile %q (choose %s)", name, strings.Join(Names(), ", "))
	}
	return profile, nil
}

func Names() []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func PublicPriorNames() []string {
	return append([]string(nil), publicPriorNames...)
}

func IsPublicPrior(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, candidate := range publicPriorNames {
		if name == candidate {
			return true
		}
	}
	return false
}
