// Package technique is the contract every optimization technique plugs into:
// a trained draft, speculation that changes with load, a tuned kernel
// config, or a plain engine option. A technique declares what it needs, what
// limit it relieves and what it costs, how it predicts, whether it builds an
// artifact and where, whether it can change answers, how it behaves at run
// time, and what serves when it cannot. It then proposes candidate setups;
// it never measures them. The screen and the judge measure every candidate
// the same way, so a technique earns nothing by being one.
//
// The contract is public and versioned (schemas/technique-v1.schema.json),
// so a technique can be added without InferCrane's hosted control plane: in
// Go through Technique, or as a JSON manifest with fixed changes
// (LoadStatic). It reuses the core's packages rather than building beside
// them: an artifact is an optimizedartifact kind built through
// acceleratorlab's isolated builders, launch flags are compiled through
// optimizationcapability, and rollout goes through releaseguard.
package technique

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// SchemaV1 is the manifest schema this package reads and writes.
const SchemaV1 = "infercrane.dev/technique/v1"

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	tokenPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

// Limit names what a technique relieves or costs. The names match the
// diagnosis's bottlenecks, so a plan can tell which techniques address the
// limit it found.
type Limit string

const (
	// LimitDecodeSteps is memory-bound decode: fewer target-model steps per
	// output token (speculation) or fewer bytes per step (precision).
	LimitDecodeSteps Limit = "decode-steps"
	// LimitKVCacheRoom is how many requests fit at once in the KV cache.
	LimitKVCacheRoom Limit = "kv-cache-room"
	// LimitPrefillCompute is the compute of reading prompts.
	LimitPrefillCompute Limit = "prefill-compute"
	// LimitKernelTime is time inside a kernel that a tuned or better kernel
	// shortens.
	LimitKernelTime Limit = "kernel-time"
	// LimitHostOverhead is CPU time between GPU steps.
	LimitHostOverhead Limit = "host-overhead"
	// LimitFirstToken is time to the first token.
	LimitFirstToken Limit = "first-token"
	// LimitGPUMemory is memory the weights, the cache, and buffers take.
	LimitGPUMemory Limit = "gpu-memory"
	// LimitHostMemory is the host's RAM a technique holds, for example a
	// pinned copy of a sleeping engine's weights, graphs, and buffers.
	LimitHostMemory Limit = "host-memory"
	// LimitBuildTime is time and GPU spent building an artifact before the
	// technique can serve.
	LimitBuildTime Limit = "build-time"
)

var limits = []Limit{LimitDecodeSteps, LimitKVCacheRoom, LimitPrefillCompute, LimitKernelTime, LimitHostOverhead, LimitFirstToken, LimitGPUMemory, LimitHostMemory, LimitBuildTime}

// Where an artifact build runs.
const (
	// RunsInCustomerAccount is required for anything trained or tuned on the
	// customer's content: the content never leaves their account.
	RunsInCustomerAccount = "customer-account"
	// RunsInDisposableContainer is an isolated GPU container that is thrown
	// away after the build (acceleratorlab's builders).
	RunsInDisposableContainer = "disposable-gpu-container"
)

// Runtime policies.
const (
	// RuntimeStatic serves one setup at every load.
	RuntimeStatic = "static"
	// RuntimeAdaptive changes the setup by load band; each band's setup is
	// judged at that band's load.
	RuntimeAdaptive = "adaptive"
)

// What serves when the technique cannot.
const (
	// FallbackStartingSetup serves the setup the technique changed.
	FallbackStartingSetup = "starting-setup"
	// FallbackEngineDefault serves the engine's own behaviour without the
	// technique's artifact or flags.
	FallbackEngineDefault = "engine-default"
)

// Manifest is a technique's declaration. It is the whole public contract
// besides the candidates a technique returns.
type Manifest struct {
	SchemaVersion string `json:"schema_version"`
	// ID is stable and lowercase, for example "trained-draft".
	ID string `json:"id"`
	// Version is the technique's own semantic version. A candidate and its
	// results name it, so a change of method is a new version.
	Version string `json:"version"`
	Label   string `json:"label"`
	// License is the SPDX expression of the technique's code and every
	// dependency it adds. Anything not Apache-2.0, MIT, or BSD compatible is
	// named here and kept optional.
	License        string   `json:"license"`
	Requires       Requires `json:"requires"`
	Relieves       []Limit  `json:"relieves"`
	Costs          []Limit  `json:"costs"`
	Prediction     Method   `json:"prediction"`
	Artifact       *Build   `json:"artifact,omitempty"`
	ChangesAnswers bool     `json:"changes_answers"`
	Runtime        Runtime  `json:"runtime"`
	Fallback       Fallback `json:"fallback"`
	// Evidence names what every result of this technique must carry besides
	// the five fields, for example kernel-invocation-proof, draft-acceptance,
	// or swap-timings. A result without one of them is "not shown".
	Evidence []string `json:"evidence,omitempty"`
	// Disclosures are sentences every claim of this technique carries, for
	// example a view in which it lost or a cost it showed. A result that
	// does not carry each of them word for word is "not shown".
	Disclosures []string `json:"disclosures,omitempty"`
}

// maxEvidence bounds the evidence a manifest may require.
const maxEvidence = 16

// maxDisclosures and maxDisclosureLength bound what a claim must carry.
const (
	maxDisclosures      = 8
	maxDisclosureLength = 400
)

// imagePattern is an OCI reference pinned by its sha256 digest.
var imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,255}@sha256:[a-f0-9]{64}$`)

// maxSignalCeilings bounds the readings a manifest may put a ceiling on.
const maxSignalCeilings = 8

// Requires is what a starting setup must have for the technique to apply.
// Every list names exact values; an empty list is refused, because a
// wildcard would claim support nobody has measured.
type Requires struct {
	Engines []Engine `json:"engines"`
	// Accelerators are GPU names as InferCrane plans them, for example H100
	// or L4.
	Accelerators []string `json:"accelerators"`
	// ModelFeatures the model must have, for example native_mtp or moe.
	ModelFeatures []string `json:"model_features,omitempty"`
	// SignalsAtMost puts a ceiling on the diagnosis's readings of the
	// traffic. A setup whose reading is above its ceiling, or that has no
	// such reading, is refused: traffic nobody measured is not admitted. A
	// technique that turns a cache off names the reading that shows the
	// traffic needs it, for example runtime_prefix_cache_hit_rate at most
	// 0.02 for one that serves with prefix caching off.
	SignalsAtMost map[string]float64 `json:"signals_at_most,omitempty"`
}

// Engine is one serving engine and the exact versions the technique was
// built and checked against.
type Engine struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
}

// Method says how a technique predicts its effect before anything is
// measured. The prediction only orders and prunes the list; measurement
// decides.
type Method struct {
	// Metric is what the predicted change is stated in, for example
	// output_tokens_per_second or useful_output_tokens_per_second.
	Metric string `json:"metric"`
	// Basis is how it is predicted, for example cache-room,
	// acceptance-offline, amdahl, or none.
	Basis string `json:"basis"`
}

// Build is an artifact the technique builds before it can serve.
type Build struct {
	// Kind is an optimizedartifact kind, for example speculator_checkpoint.
	Kind string `json:"kind"`
	// RunsIn is where the build runs: RunsInCustomerAccount or
	// RunsInDisposableContainer.
	RunsIn string `json:"runs_in"`
	// UsesCustomerContent is true when the build reads prompts or answers.
	// It then runs in the customer's account, always.
	UsesCustomerContent bool `json:"uses_customer_content"`
	// Produces says what comes out and what it is pinned to, in words.
	Produces string `json:"produces"`
}

// Runtime is how the technique behaves while serving.
type Runtime struct {
	Policy string `json:"policy"`
}

// Fallback is what serves when the artifact is missing or the engine
// changed.
type Fallback struct {
	Serves string `json:"serves"`
	Reason string `json:"reason"`
}

// Validate checks a manifest. A manifest that does not validate is never
// registered.
func (m Manifest) Validate() error {
	if m.SchemaVersion != SchemaV1 {
		return fmt.Errorf("technique manifest schema must be %q", SchemaV1)
	}
	if !idPattern.MatchString(m.ID) || !versionPattern.MatchString(m.Version) || strings.TrimSpace(m.Label) == "" || len(m.Label) > 128 || strings.TrimSpace(m.License) == "" || len(m.License) > 128 {
		return errors.New("technique manifest needs a lowercase id, a semantic version, a label, and an SPDX license")
	}
	if len(m.Requires.Engines) == 0 || len(m.Requires.Accelerators) == 0 {
		return fmt.Errorf("technique %s must name its engines and accelerators", m.ID)
	}
	for _, engine := range m.Requires.Engines {
		if !tokenPattern.MatchString(engine.Name) || len(engine.Versions) == 0 {
			return fmt.Errorf("technique %s must name each engine and its exact versions", m.ID)
		}
		for _, version := range engine.Versions {
			if strings.TrimSpace(version) == "" || strings.ContainsAny(version, "*<>=~^ ") {
				return fmt.Errorf("technique %s: engine %s version %q is not exact", m.ID, engine.Name, version)
			}
		}
	}
	for _, name := range append(append([]string(nil), m.Requires.Accelerators...), m.Requires.ModelFeatures...) {
		if strings.TrimSpace(name) == "" || len(name) > 64 {
			return fmt.Errorf("technique %s names an empty or long accelerator or model feature", m.ID)
		}
	}
	if len(m.Requires.SignalsAtMost) > maxSignalCeilings {
		return fmt.Errorf("technique %s puts a ceiling on more than %d readings", m.ID, maxSignalCeilings)
	}
	for name, ceiling := range m.Requires.SignalsAtMost {
		if !tokenPattern.MatchString(name) || math.IsNaN(ceiling) || math.IsInf(ceiling, 0) || ceiling < 0 {
			return fmt.Errorf("technique %s: the ceiling on %q needs a lowercase reading name and a finite value of zero or more", m.ID, name)
		}
	}
	if len(m.Relieves) == 0 {
		return fmt.Errorf("technique %s must say which limit it relieves", m.ID)
	}
	for _, limit := range append(append([]Limit(nil), m.Relieves...), m.Costs...) {
		if !slices.Contains(limits, limit) {
			return fmt.Errorf("technique %s names an unknown limit %q", m.ID, limit)
		}
	}
	if !tokenPattern.MatchString(m.Prediction.Metric) || !tokenPattern.MatchString(m.Prediction.Basis) {
		return fmt.Errorf("technique %s must name its prediction metric and basis", m.ID)
	}
	if build := m.Artifact; build != nil {
		if !tokenPattern.MatchString(build.Kind) || strings.TrimSpace(build.Produces) == "" {
			return fmt.Errorf("technique %s: an artifact names its kind and what it produces", m.ID)
		}
		switch {
		case build.RunsIn != RunsInCustomerAccount && build.RunsIn != RunsInDisposableContainer:
			return fmt.Errorf("technique %s: an artifact runs in %q or %q", m.ID, RunsInCustomerAccount, RunsInDisposableContainer)
		case build.UsesCustomerContent && build.RunsIn != RunsInCustomerAccount:
			return fmt.Errorf("technique %s: a build that reads customer content runs in the customer's account", m.ID)
		}
		if !slices.Contains(m.Costs, LimitBuildTime) {
			return fmt.Errorf("technique %s builds an artifact, so it costs %s", m.ID, LimitBuildTime)
		}
	}
	if m.Runtime.Policy != RuntimeStatic && m.Runtime.Policy != RuntimeAdaptive {
		return fmt.Errorf("technique %s: runtime policy is %q or %q", m.ID, RuntimeStatic, RuntimeAdaptive)
	}
	if m.Fallback.Serves != FallbackStartingSetup && m.Fallback.Serves != FallbackEngineDefault || strings.TrimSpace(m.Fallback.Reason) == "" {
		return fmt.Errorf("technique %s must say what serves when it cannot, and why that is safe", m.ID)
	}
	if len(m.Evidence) > maxEvidence {
		return fmt.Errorf("technique %s requires more than %d kinds of evidence", m.ID, maxEvidence)
	}
	for index, name := range m.Evidence {
		if !tokenPattern.MatchString(name) || slices.Contains(m.Evidence[:index], name) {
			return fmt.Errorf("technique %s names evidence %q twice or not as a lowercase token", m.ID, name)
		}
	}
	if len(m.Disclosures) > maxDisclosures {
		return fmt.Errorf("technique %s declares more than %d disclosures", m.ID, maxDisclosures)
	}
	for index, sentence := range m.Disclosures {
		if strings.TrimSpace(sentence) == "" || len(sentence) > maxDisclosureLength || slices.Contains(m.Disclosures[:index], sentence) {
			return fmt.Errorf("technique %s declares an empty, long or repeated disclosure", m.ID)
		}
	}
	return nil
}

// Setup is a starting setup a technique is asked to change. It carries no
// prompt or answer: only what the diagnosis read.
type Setup struct {
	Model         string            `json:"model"`
	ModelRevision string            `json:"model_revision"`
	ModelFeatures []string          `json:"model_features,omitempty"`
	Engine        string            `json:"engine"`
	EngineVersion string            `json:"engine_version"`
	Accelerator   string            `json:"accelerator"`
	GPUCount      int               `json:"gpu_count"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env,omitempty"`
	// Signals are the diagnosis's readings, for example
	// runtime_requests_running_per_replica.
	Signals map[string]float64 `json:"signals,omitempty"`
	// Loads are the load bands the setup is judged at, in requests at once.
	Loads []int `json:"loads,omitempty"`
	// Objective is the run's goal as the planner names it, for example
	// useful-output; FirstTokenLimitMS is the first-token limit that goal
	// counts output within. A technique proven for one goal proposes nothing
	// for another.
	Objective         string  `json:"objective,omitempty"`
	FirstTokenLimitMS float64 `json:"first_token_limit_ms,omitempty"`
}

// Admits reports whether the technique's requirements hold for the setup,
// or why not.
func (m Manifest) Admits(setup Setup) (bool, string) {
	engine := slices.IndexFunc(m.Requires.Engines, func(engine Engine) bool { return engine.Name == setup.Engine })
	switch {
	case engine < 0:
		return false, fmt.Sprintf("%s does not run on %s", m.Label, setup.Engine)
	case !slices.Contains(m.Requires.Engines[engine].Versions, setup.EngineVersion):
		return false, fmt.Sprintf("%s was not checked on %s %s", m.Label, setup.Engine, setup.EngineVersion)
	case !slices.Contains(m.Requires.Accelerators, setup.Accelerator):
		return false, fmt.Sprintf("%s was not checked on %s", m.Label, setup.Accelerator)
	}
	for _, feature := range m.Requires.ModelFeatures {
		if !slices.Contains(setup.ModelFeatures, feature) {
			return false, fmt.Sprintf("%s needs a model with %s", m.Label, feature)
		}
	}
	names := make([]string, 0, len(m.Requires.SignalsAtMost))
	for name := range m.Requires.SignalsAtMost {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ceiling := m.Requires.SignalsAtMost[name]
		reading, known := setup.Signals[name]
		switch {
		case !known || math.IsNaN(reading):
			return false, fmt.Sprintf("%s needs a reading of %s, which the diagnosis does not have", m.Label, name)
		case reading > ceiling:
			return false, fmt.Sprintf("%s needs %s at most %g; this traffic reads %g", m.Label, name, ceiling, reading)
		}
	}
	return true, ""
}

// Candidate is one setup a technique proposes. It is measured like any other
// setup; nothing in it is a result.
type Candidate struct {
	Technique        string `json:"technique"`
	TechniqueVersion string `json:"technique_version"`
	Label            string `json:"label"`
	// Args and Env are the whole setup: the starting setup with the change.
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
	// Artifact is the optimizedartifact the setup serves, when it needs one.
	Artifact *ArtifactRef `json:"artifact,omitempty"`
	// Bands is set for an adaptive policy: the setup per load band. Each is
	// judged at its own band's load.
	Bands []Band `json:"bands,omitempty"`
	// Prediction is the technique's own prediction, made before measuring.
	Prediction *Prediction `json:"prediction,omitempty"`
	// ChangesAnswers is the manifest's, so the task score and KL gates
	// apply to it.
	ChangesAnswers bool   `json:"changes_answers"`
	Reason         string `json:"reason"`
	// Image is the engine image the setup must run on, when the technique
	// changes the engine itself. The setup never runs on another image.
	Image *RuntimeImage `json:"image,omitempty"`
}

// RuntimeImage is a reviewed engine image and the command that starts it.
type RuntimeImage struct {
	// Image is an OCI reference pinned by digest (name@sha256:...).
	Image string `json:"image"`
	// Command starts the server; the setup's arguments follow it.
	Command []string `json:"command"`
	// Identity says what the image is made of, for example its base image
	// digest and the patch applied to it, so an unpublished image is still
	// named by content.
	Identity string `json:"identity,omitempty"`
	// Source is where the image reference came from: ImageSourcePublished,
	// or ImageSourceQualification for an unpublished image an operator named
	// for a qualification run, which the same request without it would not
	// plan.
	Source string `json:"source,omitempty"`
}

// Where a candidate's engine image reference came from.
const (
	ImageSourcePublished     = "published"
	ImageSourceQualification = "qualification"
)

// ArtifactRef names an immutable optimizedartifact by ID and digest.
type ArtifactRef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

// Band is the setup for one load band: up to UpTo requests at once, or every
// load above the last band when UpTo is 0.
type Band struct {
	UpTo int               `json:"up_to"`
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
}

// Prediction is a number a customer can read, made before measuring.
type Prediction struct {
	Metric string `json:"metric"`
	// Change is the predicted relative change, for example 0.12 for +12%.
	Change float64 `json:"change"`
	// Low and High bound it when the technique can say how sure it is.
	Low   *float64 `json:"low,omitempty"`
	High  *float64 `json:"high,omitempty"`
	Basis string   `json:"basis"`
}

// Technique is a registered technique: its manifest, and the candidates it
// proposes for a starting setup it admits. Candidates may predict; it never
// measures, never starts a server, and never reads prompts or answers. An
// empty list with a reason is a valid answer.
type Technique interface {
	Manifest() Manifest
	Candidates(ctx context.Context, setup Setup) ([]Candidate, string, error)
}

// Registry holds techniques by ID.
type Registry struct {
	techniques map[string]Technique
}

// NewRegistry validates every manifest and refuses duplicate IDs.
func NewRegistry(techniques ...Technique) (*Registry, error) {
	registry := &Registry{techniques: map[string]Technique{}}
	for _, technique := range techniques {
		manifest := technique.Manifest()
		if err := manifest.Validate(); err != nil {
			return nil, err
		}
		if _, taken := registry.techniques[manifest.ID]; taken {
			return nil, fmt.Errorf("technique %s is registered twice", manifest.ID)
		}
		registry.techniques[manifest.ID] = technique
	}
	return registry, nil
}

// Manifests lists every registered manifest by ID.
func (r *Registry) Manifests() []Manifest {
	out := make([]Manifest, 0, len(r.techniques))
	for _, technique := range r.techniques {
		out = append(out, technique.Manifest())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Skipped is a technique that proposed nothing for a setup, and why.
type Skipped struct {
	Technique string `json:"technique"`
	Reason    string `json:"reason"`
}

// Candidates asks every technique that admits the setup for its candidates,
// in technique ID order. Each candidate is checked against its technique's
// manifest: it names the technique and version, carries the manifest's
// answer-changing flag, and holds bands only under an adaptive policy.
func (r *Registry) Candidates(ctx context.Context, setup Setup) ([]Candidate, []Skipped, error) {
	var candidates []Candidate
	var skipped []Skipped
	for _, manifest := range r.Manifests() {
		if ok, why := manifest.Admits(setup); !ok {
			skipped = append(skipped, Skipped{Technique: manifest.ID, Reason: why})
			continue
		}
		proposed, why, err := r.techniques[manifest.ID].Candidates(ctx, setup)
		if err != nil {
			return nil, nil, fmt.Errorf("technique %s: %w", manifest.ID, err)
		}
		if len(proposed) == 0 {
			skipped = append(skipped, Skipped{Technique: manifest.ID, Reason: why})
			continue
		}
		for _, candidate := range proposed {
			if err = manifest.check(candidate); err != nil {
				return nil, nil, err
			}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, skipped, nil
}

func (m Manifest) check(candidate Candidate) error {
	switch {
	case candidate.Technique != m.ID || candidate.TechniqueVersion != m.Version:
		return fmt.Errorf("technique %s returned a candidate naming %s %s", m.ID, candidate.Technique, candidate.TechniqueVersion)
	case candidate.ChangesAnswers != m.ChangesAnswers:
		return fmt.Errorf("technique %s returned a candidate that does not carry its answer-changing flag", m.ID)
	case strings.TrimSpace(candidate.Label) == "" || strings.TrimSpace(candidate.Reason) == "":
		return fmt.Errorf("technique %s returned a candidate without a label or a reason", m.ID)
	case len(candidate.Bands) > 0 && m.Runtime.Policy != RuntimeAdaptive:
		return fmt.Errorf("technique %s is static but returned bands", m.ID)
	case m.Runtime.Policy == RuntimeAdaptive && len(candidate.Bands) < 2:
		return fmt.Errorf("technique %s is adaptive and must return a setup for at least two bands", m.ID)
	case m.Artifact != nil && (candidate.Artifact == nil || candidate.Artifact.Kind != m.Artifact.Kind || candidate.Artifact.ID == "" || !strings.HasPrefix(candidate.Artifact.Digest, "sha256:")):
		return fmt.Errorf("technique %s builds a %s and must name it by ID and sha256 digest", m.ID, m.Artifact.Kind)
	case m.Artifact == nil && candidate.Artifact != nil:
		return fmt.Errorf("technique %s declares no artifact but returned one", m.ID)
	case candidate.Prediction != nil && candidate.Prediction.Metric != m.Prediction.Metric:
		return fmt.Errorf("technique %s predicted %s, not its declared %s", m.ID, candidate.Prediction.Metric, m.Prediction.Metric)
	}
	for index, band := range candidate.Bands {
		last := index == len(candidate.Bands)-1
		if last && band.UpTo != 0 || !last && (band.UpTo <= 0 || index > 0 && band.UpTo <= candidate.Bands[index-1].UpTo) {
			return fmt.Errorf("technique %s returned bands that are not increasing up to an open last band", m.ID)
		}
	}
	if image := candidate.Image; image != nil {
		if !imagePattern.MatchString(image.Image) || len(image.Command) == 0 || strings.TrimSpace(image.Command[0]) == "" || len(image.Command) > 16 {
			return fmt.Errorf("technique %s names an engine image that is not pinned by digest or has no command", m.ID)
		}
		if image.Source != "" && image.Source != ImageSourcePublished && image.Source != ImageSourceQualification {
			return fmt.Errorf("technique %s names an engine image from source %q, not %s or %s", m.ID, image.Source, ImageSourcePublished, ImageSourceQualification)
		}
	}
	return nil
}
