// Package planning builds deterministic, side-effect-free deployment plans.
package planning

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var nonNameCharacter = regexp.MustCompile(`[^a-z0-9]+`)

type Input struct {
	Name, Model, Cloud, GPU, Region, Runtime, Routing string
	Targets, RuntimeArgs                              []string
	MinReplicas, MaxReplicas                          int
}

type Action struct {
	Order   int    `json:"order"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

type Cost struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type Change struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type Current struct {
	Model, Runtime, Routing, ActiveRevision string
	MinReplicas, MaxReplicas                int
	ActiveRevisionNumber                    int
}

type Plan struct {
	Version     int      `json:"version"`
	Name        string   `json:"name"`
	Model       string   `json:"model"`
	Mode        string   `json:"mode"`
	Cloud       string   `json:"cloud,omitempty"`
	GPU         string   `json:"gpu,omitempty"`
	Region      string   `json:"region,omitempty"`
	Runtime     string   `json:"runtime"`
	Routing     string   `json:"routing"`
	Targets     []string `json:"targets,omitempty"`
	MinReplicas int      `json:"min_replicas"`
	MaxReplicas int      `json:"max_replicas"`
	Actions     []Action `json:"actions"`
	Changes     []Change `json:"changes,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Cost        Cost     `json:"cost"`
}

// Compare turns a creation plan into a deterministic revision rollout plan.
func Compare(p Plan, current Current) Plan {
	addChange := func(field, before, after string) {
		if before != after {
			p.Changes = append(p.Changes, Change{Field: field, Before: before, After: after})
		}
	}
	addChange("model", current.Model, p.Model)
	addChange("runtime", current.Runtime, p.Runtime)
	addChange("routing", current.Routing, p.Routing)
	addChange("replicas", fmt.Sprintf("%d..%d", current.MinReplicas, current.MaxReplicas), fmt.Sprintf("%d..%d", p.MinReplicas, p.MaxReplicas))
	if len(p.Changes) == 0 {
		p.Actions = []Action{{Order: 1, Kind: "noop", Summary: "Persisted deployment already matches the requested specification"}}
		return p
	}
	next := current.ActiveRevisionNumber + 1
	p.Changes = append(p.Changes, Change{Field: "revision", Before: current.ActiveRevision, After: fmt.Sprintf("candidate rev-%d", next)})
	summaries := []struct{ kind, text string }{
		{"provision", "Provision candidate capacity"},
		{"health-check", "Wait for candidate readiness"},
		{"validate", "Evaluate candidate with persisted guard policy"},
		{"route", "Route the accepted candidate generation"},
		{"drain", "Drain the old revision safely"},
		{"terminate", "Terminate old capacity after drain"},
	}
	p.Actions = p.Actions[:0]
	for i, action := range summaries {
		p.Actions = append(p.Actions, Action{Order: i + 1, Kind: action.kind, Summary: action.text})
	}
	return p
}

func Build(in Input) (Plan, error) {
	if strings.TrimSpace(in.Model) == "" {
		return Plan{}, errors.New("model is required")
	}
	if len(in.Targets) > 0 && (in.Cloud != "" || in.GPU != "") {
		return Plan{}, errors.New("use either existing targets or cloud provisioning")
	}
	if (in.Cloud == "") != (in.GPU == "") {
		return Plan{}, errors.New("cloud and GPU must be provided together")
	}
	if in.Name == "" {
		in.Name = DefaultName(in.Model)
	}
	if in.Runtime == "" {
		in.Runtime = "vllm"
	}
	if in.Routing == "" {
		in.Routing = "round-robin"
	}
	if in.MinReplicas == 0 {
		in.MinReplicas = 1
	}
	if in.MaxReplicas == 0 {
		in.MaxReplicas = in.MinReplicas
	}
	if in.MinReplicas < 1 || in.MaxReplicas < in.MinReplicas {
		return Plan{}, errors.New("replicas must be positive and max replicas must be >= min replicas")
	}

	p := Plan{Version: 1, Name: in.Name, Model: in.Model, Cloud: in.Cloud, GPU: in.GPU,
		Region: in.Region, Runtime: in.Runtime, Routing: in.Routing, Targets: in.Targets,
		MinReplicas: in.MinReplicas, MaxReplicas: in.MaxReplicas,
		Cost: Cost{Status: "unavailable", Reason: "live provider pricing is not configured; no estimate is fabricated"}}
	add := func(kind, summary string) {
		p.Actions = append(p.Actions, Action{Order: len(p.Actions) + 1, Kind: kind, Summary: summary})
	}
	add("validate", "Validate deployment inputs and runtime compatibility")
	switch {
	case len(in.Targets) > 0:
		p.Mode = "existing-targets"
		add("resolve", fmt.Sprintf("Resolve %d registered target(s)", len(in.Targets)))
	case in.Cloud != "":
		p.Mode = "provisioned"
		add("provision", fmt.Sprintf("Provision %s on %s", in.GPU, in.Cloud))
		add("bootstrap", fmt.Sprintf("Start %s and wait for model readiness", in.Runtime))
		add("register", "Register the provisioned target")
	default:
		p.Mode = "incomplete"
		p.Warnings = append(p.Warnings, "choose --targets or provide both --cloud and --gpu before deployment")
	}
	if p.Mode != "incomplete" {
		add("persist", "Converge the logical deployment transactionally")
		add("route", fmt.Sprintf("Reconcile the %s routing generation", in.Routing))
	}
	if in.MaxReplicas > in.MinReplicas {
		add("autoscale", fmt.Sprintf("Enable bounded vLLM queue scaling from %d to %d replicas", in.MinReplicas, in.MaxReplicas))
	}
	return p, nil
}

func DefaultName(model string) string {
	parts := strings.Split(strings.TrimSpace(model), "/")
	name := strings.Trim(nonNameCharacter.ReplaceAllString(strings.ToLower(parts[len(parts)-1]), "-"), "-")
	if name == "" {
		return "deployment"
	}
	return name
}
