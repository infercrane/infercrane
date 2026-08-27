// Package planning builds deterministic, side-effect-free deployment plans.
package planning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/support"
)

var nonNameCharacter = regexp.MustCompile(`[^a-z0-9]+`)

type Input struct {
	Name, Model, ModelRevision, ComputeMode, Cloud, ProviderAdapter, GPU, Region, Runtime, RuntimeVersion, Routing string
	Targets, RuntimeArgs                                                                                           []string
	GPUCount, MinReplicas, MaxReplicas                                                                             int
	Serving                                                                                                        servingcontract.Topology
}

type StageEstimate struct {
	Name               string   `json:"name"`
	SuccessfulSamples  int      `json:"successful_samples"`
	EstimateP50Seconds *float64 `json:"estimate_p50_seconds,omitempty"`
	EstimateP95Seconds *float64 `json:"estimate_p95_seconds,omitempty"`
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

// Readiness describes only evidence available before a deployment starts. A
// duration is present only when enough tenant-scoped observations match the
// exact serving plan; static specifications never manufacture one.
type Readiness struct {
	EstimateStatus     string          `json:"estimate_status"`
	EstimateP50Seconds *float64        `json:"estimate_p50_seconds,omitempty"`
	EstimateP95Seconds *float64        `json:"estimate_p95_seconds,omitempty"`
	SuccessfulSamples  int             `json:"successful_samples,omitempty"`
	ArtifactCacheState string          `json:"artifact_cache_state"`
	CapacityState      string          `json:"capacity_state"`
	EvidenceBoundary   string          `json:"evidence_boundary,omitempty"`
	Stages             []string        `json:"stages,omitempty"`
	StageEstimates     []StageEstimate `json:"stage_estimates,omitempty"`
	Reason             string          `json:"reason"`
}

// CapacityEvidence is the narrow, tenant-scoped observation boundary used to
// annotate a static plan. It is historical evidence, not a provider promise.
type CapacityEvidence struct {
	Provider, Runtime, ComputeMode, Region, GPU      string
	ModelIdentity, RuntimeVersion, RuntimeArgsDigest string
	GPUCount, Attempts, Succeeded, Pending           int
	SuccessRate                                      float64
	DurationP50Seconds, DurationP95Seconds           *float64
	StartupStages                                    []StageEstimate
}

type Change struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type Current struct {
	Model, Runtime, Routing, ActiveRevision                  string
	ComputeMode, Cloud, ProviderAdapter, GPU, Region         string
	GPUCount, MinReplicas, MaxReplicas, ActiveRevisionNumber int
	Serving                                                  servingcontract.Topology
}

type Plan struct {
	Version           int                      `json:"version"`
	Name              string                   `json:"name"`
	Model             string                   `json:"model"`
	ModelRevision     string                   `json:"model_revision,omitempty"`
	Mode              string                   `json:"mode"`
	Cloud             string                   `json:"cloud,omitempty"`
	ProviderAdapter   string                   `json:"provider_adapter,omitempty"`
	GPU               string                   `json:"gpu,omitempty"`
	GPUCount          int                      `json:"gpu_count,omitempty"`
	Region            string                   `json:"region,omitempty"`
	Runtime           string                   `json:"runtime"`
	RuntimeVersion    string                   `json:"runtime_version,omitempty"`
	RuntimeArgsDigest string                   `json:"runtime_args_digest,omitempty"`
	Routing           string                   `json:"routing"`
	Targets           []string                 `json:"targets,omitempty"`
	MinReplicas       int                      `json:"min_replicas"`
	MaxReplicas       int                      `json:"max_replicas"`
	Actions           []Action                 `json:"actions"`
	Changes           []Change                 `json:"changes,omitempty"`
	Warnings          []string                 `json:"warnings,omitempty"`
	Cost              Cost                     `json:"cost"`
	Readiness         Readiness                `json:"readiness"`
	Serving           servingcontract.Topology `json:"serving,omitzero"`
}

// Compare turns a creation plan into a deterministic revision rollout plan.
func Compare(p Plan, current Current) Plan {
	if current.ComputeMode == "" {
		current.ComputeMode = "existing"
		if current.Cloud != "" || current.GPU != "" {
			current.ComputeMode = "elastic"
		}
	}
	if current.GPU != "" && current.GPUCount == 0 {
		current.GPUCount = 1
	}
	if p.GPU != "" && p.GPUCount == 0 {
		p.GPUCount = 1
	}
	addChange := func(field, before, after string) {
		if before != after {
			p.Changes = append(p.Changes, Change{Field: field, Before: before, After: after})
		}
	}
	addChange("model", current.Model, p.Model)
	addChange("runtime", current.Runtime, p.Runtime)
	addChange("compute", current.ComputeMode, computeModeForPlan(p))
	addChange("cloud", current.Cloud, p.Cloud)
	addChange("provider adapter", current.ProviderAdapter, p.ProviderAdapter)
	addChange("GPU", current.GPU, p.GPU)
	addChange("GPU count", fmt.Sprint(current.GPUCount), fmt.Sprint(p.GPUCount))
	addChange("region", current.Region, p.Region)
	addChange("routing", current.Routing, p.Routing)
	addChange("replicas", fmt.Sprintf("%d..%d", current.MinReplicas, current.MaxReplicas), fmt.Sprintf("%d..%d", p.MinReplicas, p.MaxReplicas))
	currentServing, _ := current.Serving.Digest()
	plannedServing, _ := p.Serving.Digest()
	addChange("serving topology", currentServing, plannedServing)
	if len(p.Changes) == 0 {
		p.Actions = []Action{{Order: 1, Kind: "noop", Summary: "Persisted deployment already matches the requested specification"}}
		return p
	}
	next := current.ActiveRevisionNumber + 1
	active := current.ActiveRevision
	if current.ActiveRevisionNumber > 0 {
		active = fmt.Sprintf("rev-%d", current.ActiveRevisionNumber)
	}
	p.Changes = append(p.Changes, Change{Field: "revision", Before: active, After: fmt.Sprintf("candidate rev-%d", next)})
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

func computeModeForPlan(p Plan) string {
	if p.Mode == "serverless" {
		return "serverless"
	}
	if p.Mode == "provisioned" {
		return "elastic"
	}
	return "existing"
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
		in.Runtime = support.DefaultRuntime
	}
	if in.RuntimeVersion == "" {
		switch in.Runtime {
		case support.DefaultRuntime:
			in.RuntimeVersion = support.DefaultRuntimeVersion
		case "sglang":
			in.RuntimeVersion = support.SGLangRuntimeVersion
		}
	}
	if err := support.V1().ValidateRuntime(in.Runtime); err != nil {
		return Plan{}, fmt.Errorf("support policy: %w", err)
	}
	if in.Routing == "" {
		in.Routing = "round-robin"
	}
	if in.ComputeMode == "" {
		in.ComputeMode = "elastic"
	}
	if in.ComputeMode != "elastic" && in.ComputeMode != "serverless" {
		return Plan{}, errors.New("compute mode must be elastic or serverless")
	}
	if in.GPU == "" {
		in.GPUCount = 0
	} else {
		if in.GPUCount == 0 {
			in.GPUCount = 1
		}
		if in.GPUCount < 1 || in.GPUCount > 1024 {
			return Plan{}, errors.New("GPU count must be between 1 and 1024")
		}
	}
	if in.ComputeMode == "serverless" && in.GPU != "" && in.GPUCount != 1 {
		return Plan{}, errors.New("serverless compute currently requires GPU count 1")
	}
	if in.ComputeMode == "elastic" && in.MinReplicas == 0 {
		in.MinReplicas = 1
	}
	if in.MaxReplicas == 0 {
		in.MaxReplicas = max(1, in.MinReplicas)
	}
	if (in.ComputeMode == "elastic" && in.MinReplicas < 1) || (in.ComputeMode == "serverless" && in.MinReplicas != 0) || in.MaxReplicas < 1 || in.MaxReplicas < in.MinReplicas {
		return Plan{}, errors.New("replicas must be positive and max replicas must be >= min replicas")
	}
	if in.Cloud != "" {
		if err := support.V1().Validate(in.Runtime, in.Cloud, in.ComputeMode); err != nil {
			return Plan{}, fmt.Errorf("support policy: %w", err)
		}
		if in.Cloud == "aws" && in.Region == "" {
			return Plan{}, errors.New("AWS BYOC requires an explicit region")
		}
	}
	in.Serving = in.Serving.Normalize()
	if err := in.Serving.Validate(in.Runtime, in.Cloud, in.ProviderAdapter, in.MinReplicas, in.MaxReplicas); err != nil {
		return Plan{}, fmt.Errorf("serving topology: %w", err)
	}

	args, _ := json.Marshal(in.RuntimeArgs)
	argsSum := sha256.Sum256(args)
	p := Plan{Version: 1, Name: in.Name, Model: in.Model, ModelRevision: in.ModelRevision, Cloud: in.Cloud, ProviderAdapter: in.ProviderAdapter, GPU: in.GPU, GPUCount: in.GPUCount, Serving: in.Serving,
		Region: in.Region, Runtime: in.Runtime, Routing: in.Routing, Targets: in.Targets,
		RuntimeVersion: in.RuntimeVersion, RuntimeArgsDigest: "sha256:" + hex.EncodeToString(argsSum[:]),
		MinReplicas: in.MinReplicas, MaxReplicas: in.MaxReplicas,
		Cost: Cost{Status: "unavailable", Reason: "live provider pricing is not configured; no estimate is fabricated"},
		Readiness: Readiness{
			EstimateStatus:     "unavailable",
			ArtifactCacheState: "unknown",
			CapacityState:      "unknown",
			Stages:             []string{"capacity", "container", "artifact", "runtime", "readiness"},
			Reason:             "fresh provider capacity and artifact-cache observations are not available during this static plan; no startup time is fabricated",
		}}
	add := func(kind, summary string) {
		p.Actions = append(p.Actions, Action{Order: len(p.Actions) + 1, Kind: kind, Summary: summary})
	}
	add("validate", "Validate deployment inputs and runtime compatibility")
	switch {
	case len(in.Targets) > 0:
		p.Mode = "existing-targets"
		p.Readiness = Readiness{EstimateStatus: "externally-managed", ArtifactCacheState: "not-observed", CapacityState: "not-observed", Reason: "startup and cache lifecycle remain owned by the connected target operator"}
		add("resolve", fmt.Sprintf("Resolve %d registered target(s)", len(in.Targets)))
	case in.ComputeMode == "serverless" && in.Cloud != "":
		p.Mode = "serverless"
		add("artifact", "Resolve the model to an immutable Hugging Face artifact")
		add("template", "Validate the configured serverless runtime template against that artifact")
		add("endpoint", fmt.Sprintf("Create a provider-native serverless endpoint on %s with zero minimum workers", in.GPU))
		add("register", "Register the provider-native OpenAI-compatible endpoint")
	case in.Cloud != "":
		p.Mode = "provisioned"
		add("provision", fmt.Sprintf("Provision %d x %s per replica on %s", in.GPUCount, in.GPU, in.Cloud))
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
	if p.Mode == "serverless" {
		add("autoscale", fmt.Sprintf("Delegate zero-to-%d worker scaling to the provider backend", in.MaxReplicas))
	} else if in.MaxReplicas > in.MinReplicas {
		add("autoscale", fmt.Sprintf("Enable bounded runtime-signal scaling from %d to %d replicas", in.MinReplicas, in.MaxReplicas))
	}
	return p, nil
}

// ApplyCapacityEvidence adds a readiness estimate only when at least three
// successful, deduplicated observations match the exact serving plan. P95 is
// accepted only when the store exposes it, which currently requires twenty
// successes. Unknown or weak evidence remains explicit.
func ApplyCapacityEvidence(p Plan, evidence []CapacityEvidence) Plan {
	if p.Mode != "provisioned" {
		return p
	}
	expectedProvider := p.ProviderAdapter
	if expectedProvider == "" {
		expectedProvider = defaultProviderAdapter(p.Cloud)
	}
	expectedIdentity := p.Model
	if p.ModelRevision != "" {
		expectedIdentity += "@" + p.ModelRevision
	}
	for _, row := range evidence {
		gpuCount := row.GPUCount
		if gpuCount == 0 {
			gpuCount = 1
		}
		if row.Provider != expectedProvider || row.Runtime != p.Runtime || row.ComputeMode != "elastic" || row.Region != p.Region || row.GPU != p.GPU || gpuCount != p.GPUCount || expectedIdentity != row.ModelIdentity || p.RuntimeVersion == "" || p.RuntimeVersion != row.RuntimeVersion || p.RuntimeArgsDigest != row.RuntimeArgsDigest {
			continue
		}
		for _, stage := range row.StartupStages {
			if stage.SuccessfulSamples >= 3 && stage.EstimateP50Seconds != nil {
				p.Readiness.StageEstimates = append(p.Readiness.StageEstimates, stage)
			}
		}
		break
	}
	for _, row := range evidence {
		gpuCount := row.GPUCount
		if gpuCount == 0 {
			gpuCount = 1
		}
		if row.Provider != expectedProvider || row.Runtime != p.Runtime || row.ComputeMode != "elastic" || row.Region != p.Region || row.GPU != p.GPU || gpuCount != p.GPUCount {
			continue
		}
		terminal := row.Attempts - row.Pending
		p.Readiness.CapacityState = fmt.Sprintf("observed %.0f%% success across %d terminal placement(s); %d pending", row.SuccessRate*100, terminal, row.Pending)
		if row.Succeeded < 3 || row.DurationP50Seconds == nil {
			p.Readiness.Reason = fmt.Sprintf("only %d successful matching readiness observation(s); at least 3 are required before showing p50", row.Succeeded)
			return p
		}
		p.Readiness.EstimateStatus = "observed"
		p.Readiness.EstimateP50Seconds = row.DurationP50Seconds
		p.Readiness.EstimateP95Seconds = row.DurationP95Seconds
		p.Readiness.SuccessfulSamples = row.Succeeded
		p.Readiness.EvidenceBoundary = "durable replica intent through runtime readiness"
		p.Readiness.Reason = "tenant-scoped historical evidence for this exact provider, runtime, region, GPU type, and GPU count; not a capacity guarantee"
		return p
	}
	return p
}

func defaultProviderAdapter(cloud string) string {
	switch cloud {
	case "aws":
		return "aws-ec2"
	case "gcp":
		return "gcp-compute"
	case "kubernetes":
		return "kubernetes"
	case "runpod":
		return "skypilot"
	default:
		return cloud
	}
}

func DefaultName(model string) string {
	parts := strings.Split(strings.TrimSpace(model), "/")
	name := strings.Trim(nonNameCharacter.ReplaceAllString(strings.ToLower(parts[len(parts)-1]), "-"), "-")
	if name == "" {
		return "deployment"
	}
	return name
}
