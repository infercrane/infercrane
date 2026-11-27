package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
)

// StartingSetup is what the diagnosed endpoint runs now: the active revision
// of the one deployment it routes to, which the control API reads with the
// diagnosis. For an endpoint an InferCrane agent serves there is no revision
// to read, so its operator declares the setup (DeclaredSetup) and the
// control API records it with Endpoint set; a client cannot otherwise supply
// it. With it the plan starts from the customer's own engine flags instead
// of an untuned server, so a change such as fewer draft tokens can be
// derived from them.
type StartingSetup struct {
	Deployment string `json:"deployment,omitempty"`
	Revision   int    `json:"revision,omitempty"`
	// Endpoint names the agent's endpoint whose server runs a setup its
	// operator declared; Deployment and Revision are then empty.
	Endpoint string            `json:"endpoint,omitempty"`
	Engine   string            `json:"engine"`
	Version  string            `json:"version"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	GPU      string            `json:"gpu,omitempty"`
	GPUCount int               `json:"gpu_count,omitempty"`
}

func validateStartingSetup(setup StartingSetup) error {
	revisioned := setup.Deployment != "" && len(setup.Deployment) <= 255 && setup.Revision >= 1 && setup.Endpoint == ""
	declared := setup.Endpoint != "" && len(setup.Endpoint) <= 255 && setup.Deployment == "" && setup.Revision == 0
	if !revisioned && !declared || setup.Engine == "" || len(setup.Engine) > 64 || setup.Version == "" || len(setup.Version) > 64 || len(setup.Args) > 256 || len(setup.Env) > 64 {
		return errors.New("starting setup requires its deployment and revision, or the agent endpoint it was declared for, and its engine and version, within bounds")
	}
	for _, arg := range setup.Args {
		if len(arg) > 8192 {
			return errors.New("starting setup argument is too long")
		}
	}
	return nil
}

// applyStartingSetup makes the endpoint's own setup the plan's control: the
// first candidate keeps its provider, resources, and model, and takes the
// endpoint's engine version, flags, and environment. It returns why it did
// not, when it could not: another engine, another GPU, or a control the
// planner may not change by flags. The control's identity then covers the
// endpoint's revision.
func applyStartingSetup(proposal *Proposal) string {
	setup := proposal.Input.StartingSetup
	if setup == nil || len(proposal.Candidates) == 0 {
		return ""
	}
	control := &proposal.Candidates[0]
	runtime := &control.Deployment.Runtime
	switch {
	case runtime.Engine != setup.Engine:
		return fmt.Sprintf("The endpoint runs %s and the plan's starting setup %s, so the plan starts from its own setup, not the endpoint's.", engineName(setup.Engine), engineName(runtime.Engine))
	case setup.GPU != "" && !strings.EqualFold(setup.GPU, control.Deployment.Resources.GPU):
		return fmt.Sprintf("The endpoint runs on %s and the plan is for %s, so the plan starts from its own setup, not the endpoint's.", setup.GPU, control.Deployment.Resources.GPU)
	case !control.Deployment.Serving.Empty() || !runtime.Workload.Empty():
		return "The plan's starting setup is a serving graph or a pinned image, so it cannot take the endpoint's engine flags."
	}
	runtime.Version, runtime.Args = setup.Version, append([]string(nil), setup.Args...)
	runtime.Env = maps.Clone(setup.Env)
	source := fmt.Sprintf("%s revision %d", setup.Deployment, setup.Revision)
	if setup.Endpoint != "" {
		source = "declared for endpoint " + setup.Endpoint
	}
	control.Features = append(control.Features, Feature{Name: "endpoint_setup", State: "current", Source: source})
	encoded, _ := json.Marshal(struct {
		Control string        `json:"control"`
		Setup   StartingSetup `json:"setup"`
	}{control.ID, *setup})
	sum := sha256.Sum256(encoded)
	control.ID = hex.EncodeToString(sum[:])
	return ""
}

// withoutFlagValues removes each named flag and the value that follows it.
func withoutFlagValues(args []string, flags ...string) ([]string, bool) {
	out := make([]string, 0, len(args))
	removed := false
	for index := 0; index < len(args); index++ {
		name, _, inline := strings.Cut(args[index], "=")
		if !contains(flags, name) {
			out = append(out, args[index])
			continue
		}
		removed = true
		if !inline && index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			index++
		}
	}
	return out, removed
}

// DeclaredSetup is what an agent endpoint's operator says its server runs,
// for the control API to record as the starting setup. Nothing reads it
// from the server: the agent cannot see an engine's flags.
type DeclaredSetup struct {
	Engine   string            `json:"engine"`
	Version  string            `json:"version"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	GPU      string            `json:"gpu,omitempty"`
	GPUCount int               `json:"gpu_count,omitempty"`
}

// StartingSetup records the declaration for endpoint, checked like a setup
// read from a revision.
func (d DeclaredSetup) StartingSetup(endpoint string) (StartingSetup, error) {
	setup := StartingSetup{Endpoint: strings.TrimSpace(endpoint), Engine: strings.ToLower(strings.TrimSpace(d.Engine)), Version: strings.TrimSpace(d.Version),
		Args: append([]string(nil), d.Args...), Env: maps.Clone(d.Env), GPU: strings.TrimSpace(d.GPU), GPUCount: d.GPUCount}
	return setup, validateStartingSetup(setup)
}
