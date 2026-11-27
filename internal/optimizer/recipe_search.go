package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const (
	// SearchSetups adds bounded, unmeasured setups to try to a proposal.
	SearchSetups = "setups"
	// SearchRecipes is an earlier name of SearchSetups, accepted so existing
	// scripts keep working; it is never shown.
	SearchRecipes = "recipes"

	// FeatureNativeMTP means the pinned model configuration declares a native
	// multi-token prediction head. It is a precondition for an experiment, not
	// evidence that a runtime can serve the model correctly.
	FeatureNativeMTP = "native_mtp"

	recipeSearchSource  = "infercrane-recipe-search"
	recipeSearchVersion = "recipe-search-v1"
	recipeRuntime       = "custom-oci"

	maxModelConfigBytes = 4 << 20

	recipeOrderWarning = "Added setups are listed in planning order, not by predicted performance. Each stays unmeasured until the campaign measures it against the untuned starting setup on the same image."
)

// ModelConfigEvidence is what the pinned model configuration declares. Absence
// of a feature means unknown: the search skips the dependent arms without
// asserting that the model lacks the capability.
type ModelConfigEvidence struct {
	Digest   string   `json:"digest"`
	Features []string `json:"features"`
}

// InspectModelConfig detects search preconditions from config.json rather than
// from a model name.
func InspectModelConfig(raw []byte) (ModelConfigEvidence, error) {
	if len(raw) == 0 || len(raw) > maxModelConfigBytes {
		return ModelConfigEvidence{}, fmt.Errorf("model config must be between 1 byte and %d bytes", maxModelConfigBytes)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return ModelConfigEvidence{}, fmt.Errorf("decode model config: %w", err)
	}
	sum := sha256.Sum256(raw)
	evidence := ModelConfigEvidence{Digest: "sha256:" + hex.EncodeToString(sum[:]), Features: []string{}}
	sections := []map[string]any{config}
	if text, ok := config["text_config"].(map[string]any); ok {
		sections = append(sections, text)
	}
	for _, section := range sections {
		for _, key := range []string{"mtp_num_hidden_layers", "num_nextn_predict_layers"} {
			if layers, ok := section[key].(float64); ok && layers > 0 && !contains(evidence.Features, FeatureNativeMTP) {
				evidence.Features = append(evidence.Features, FeatureNativeMTP)
			}
		}
	}
	// Each technique package reads the features its manifest requires from
	// the same pinned config, so a technique is asked only for models it can
	// change (technique.Manifest.Requires.ModelFeatures).
	for _, read := range modelFeatureReaders {
		for _, feature := range read(raw) {
			if !contains(evidence.Features, feature) {
				evidence.Features = append(evidence.Features, feature)
			}
		}
	}
	return evidence, nil
}

// modelFeatureReaders are the technique packages' readers of a pinned
// config.json: kernel tuning names block-FP8 fused MoE.
var modelFeatureReaders = []func(config []byte) []string{kerneltuning.ModelFeatures}

// recipeArm is one launch configuration on a pinned runtime image. Arms stay
// unmeasured for every new model: exercisedBy records where these flags were
// accepted by the same image, which proves the flags exist and nothing more.
type recipeArm struct {
	id               string
	layers           []string
	args             []string
	requires         []string
	minInputTokens   int
	changesPrecision bool
	exercisedBy      string
	// priorityOffset defers variants that add little attribution when the
	// candidate budget is small.
	priorityOffset int
}

type recipeTemplate struct {
	id       string
	engine   string
	version  string
	image    string
	launcher []string
	// accelerators are the accelerators the template's setups are reviewed
	// for, each with its qualification. Any other accelerator gets none.
	accelerators []recipeAccelerator
	arms         []recipeArm
}

// Recipe accelerator statuses. Qualification is data: a listed accelerator
// that is not qualified gets no setups, only a warning saying why.
const (
	// recipeQualified: the template's image runs on the accelerator and its
	// setups were exercised there (exercisedBy).
	recipeQualified = "qualified"
	// recipeNotQualified: the accelerator is listed so the proposal can say
	// what is missing, but no setup is added until its platform has a pinned
	// build of the image and a measured run.
	recipeNotQualified = "not-qualified"
)

// recipeAccelerator is one accelerator a template names. Name is the
// canonical alias: provideridentity's for reviewed accelerators, so every
// provider spelling of an MI300X matches one entry.
type recipeAccelerator struct {
	name   string
	status string
	// reason says why an accelerator is not qualified, in customer words.
	reason string
}

// sglang0520ROCm is why the SGLang 0.5.20 setups are not qualified on AMD
// Instinct GPUs: the template pins the CUDA image only.
const sglang0520ROCm = "the pinned SGLang 0.5.20 image is a CUDA build, no ROCm build of it is pinned, and none of these setups has run on an AMD GPU."

var (
	boundedGraphArgs = []string{
		"--cuda-graph-bs-decode", "1", "2", "4", "8", "12", "16", "24", "32", "33",
		"--cuda-graph-bs-prefill", "256", "512", "1024", "2048", "4096", "8192",
	}
	nextnK2Args  = []string{"--speculative-algorithm", "NEXTN", "--speculative-num-steps", "1", "--speculative-eagle-topk", "1", "--speculative-num-draft-tokens", "2"}
	nextnK3Args  = []string{"--speculative-algorithm", "NEXTN", "--speculative-num-steps", "2", "--speculative-eagle-topk", "1", "--speculative-num-draft-tokens", "3"}
	nextnK4Args  = []string{"--speculative-algorithm", "NEXTN", "--speculative-num-steps", "3", "--speculative-eagle-topk", "1", "--speculative-num-draft-tokens", "4"}
	chunk4KArgs  = []string{"--chunked-prefill-size", "4096"}
	chunk16KArgs = []string{"--chunked-prefill-size", "16384", "--max-prefill-tokens", "32768"}
)

const (
	layerControl     = "control"
	layerPrecision   = "kv-cache-precision"
	layerGraphs      = "bounded-graph-capture"
	layerPrefill     = "chunked-prefill"
	layerSpeculative = "speculative-decoding"
)

func joinArgs(parts ...[]string) []string {
	var out []string
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func recipeTemplates() []recipeTemplate {
	return []recipeTemplate{{
		id:       "sglang-0520",
		engine:   "sglang",
		version:  "0.5.20",
		image:    "lmsysorg/sglang@sha256:06e4f2ed21afde4ff513cda65070124e727ba23ccaeff7712b8c40e1097d611f",
		launcher: []string{"/opt/sglang/bin/python", "-m", "sglang.launch_server"},
		accelerators: []recipeAccelerator{
			{name: "H100", status: recipeQualified},
			{name: "H200", status: recipeQualified},
			{name: "B200", status: recipeQualified},
			{name: "MI300X", status: recipeNotQualified, reason: sglang0520ROCm},
			{name: "MI355X", status: recipeNotQualified, reason: sglang0520ROCm},
		},
		arms: []recipeArm{
			{id: "control", layers: []string{layerControl}, exercisedBy: "sglang-0520-bf16-kv-control"},
			{id: "kv-fp8", layers: []string{layerPrecision}, args: []string{"--kv-cache-dtype", "fp8_e4m3"}, changesPrecision: true, exercisedBy: "sglang-0520-control"},
			{id: "bounded-graphs", layers: []string{layerGraphs}, args: boundedGraphArgs, exercisedBy: "sglang-0520-bf16-kv-nextn-k4-bounded-graphs"},
			{id: "chunk4k", layers: []string{layerPrefill}, args: chunk4KArgs, minInputTokens: 4096, exercisedBy: "sglang-0520-nextn-k4-chunk4k"},
			{id: "chunk16k", layers: []string{layerPrefill}, args: chunk16KArgs, minInputTokens: 16384, exercisedBy: "sglang-0520-nextn-k4-chunk16k"},
			{id: "nextn-k2", layers: []string{layerSpeculative}, args: nextnK2Args, requires: []string{FeatureNativeMTP}, exercisedBy: "sglang-0520-nextn-k2", priorityOffset: -40},
			{id: "nextn-k3", layers: []string{layerSpeculative}, args: nextnK3Args, requires: []string{FeatureNativeMTP}, exercisedBy: "sglang-0520-nextn-k3", priorityOffset: -35},
			{id: "nextn-k4", layers: []string{layerSpeculative}, args: nextnK4Args, requires: []string{FeatureNativeMTP}, exercisedBy: "sglang-0520-nextn-k4"},
			{id: "nextn-k4-bounded-graphs", layers: []string{layerSpeculative, layerGraphs}, args: joinArgs(nextnK4Args, boundedGraphArgs), requires: []string{FeatureNativeMTP}, exercisedBy: "sglang-0520-nextn-k4-bounded-graphs"},
			{id: "nextn-k4-chunk4k", layers: []string{layerSpeculative, layerPrefill}, args: joinArgs(nextnK4Args, chunk4KArgs), requires: []string{FeatureNativeMTP}, minInputTokens: 4096, exercisedBy: "sglang-0520-nextn-k4-chunk4k"},
			{id: "nextn-k4-chunk16k", layers: []string{layerSpeculative, layerPrefill}, args: joinArgs(nextnK4Args, chunk16KArgs), requires: []string{FeatureNativeMTP}, minInputTokens: 16384, exercisedBy: "sglang-0520-nextn-k4-chunk16k"},
		},
	}}
}

// ResolveModel returns the exact model repository and revision for a request,
// using the reviewed catalog pin when the caller named a cataloged model.
func (s CatalogSource) ResolveModel(identity, revision string) (string, string, bool) {
	identity, embedded, _ := splitPinnedModel(identity)
	if revision == "" {
		revision = embedded
	}
	if entry, found := findRecipe(s.Recipes, identity); found && (revision == "" || revision == entry.Revision) {
		return entry.Model, entry.Revision, true
	}
	return identity, revision, identity != "" && revision != ""
}

func normalizedFeatures(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if _, duplicate := seen[value]; value == "" || duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validateSearch(request Request) error {
	switch request.Search {
	case "":
		if request.Diagnosis == nil && (len(request.ModelFeatures) > 0 || request.ModelConfigDigest != "") {
			return errors.New("model features and model config digest require search setups or a diagnosis")
		}
		if request.Diagnosis == nil {
			return nil
		}
	case SearchSetups, SearchRecipes:
	default:
		return errors.New("search must be setups when set")
	}
	for _, feature := range request.ModelFeatures {
		if feature != FeatureNativeMTP && feature != kerneltuning.ModelFeatureFusedMoEFP8Block {
			return fmt.Errorf("unsupported model feature %q", feature)
		}
	}
	if len(request.ModelFeatures) > 0 && request.ModelConfigDigest == "" {
		return errors.New("model features require the digest of the model config that declared them")
	}
	if request.ModelConfigDigest != "" && !sha256Digest(request.ModelConfigDigest) {
		return errors.New("model config digest must be a sha256 digest")
	}
	return nil
}

// expandRecipeSearch places bounded experiments ahead of the existing
// candidates. The search control always ranks first so a comparison on the
// same image survives any candidate bound.
func (s CatalogSource) expandRecipeSearch(proposal Proposal) (Proposal, error) {
	request := proposal.Input
	model, revision, resolved := s.ResolveModel(request.ModelIdentity, request.ModelRevision)
	if !resolved {
		proposal.Warnings = append(proposal.Warnings, "Finding the best setup needs the model pinned to an immutable commit; no setups were added to measure.")
		return proposal, nil
	}
	var experiments []Candidate
	for _, template := range recipeTemplates() {
		built, warnings, err := buildRecipeExperiments(template, model, revision, s.Integrations.Compatibility, s.Integrations.Runtimes, request)
		if err != nil {
			return Proposal{}, err
		}
		experiments = append(experiments, built...)
		proposal.Warnings = append(proposal.Warnings, warnings...)
	}
	if len(experiments) == 0 {
		return proposal, nil
	}
	proposal.Candidates = append(experiments, proposal.Candidates...)
	if len(proposal.Candidates) > request.MaxCandidates {
		proposal.Candidates = proposal.Candidates[:request.MaxCandidates]
	}
	for index := range proposal.Candidates {
		proposal.Candidates[index].Rank = index + 1
	}
	for _, missing := range proposal.Missing {
		proposal.Warnings = append(proposal.Warnings, "The reviewed planner reported "+missing+"; only the added setups can be measured for that boundary.")
	}
	proposal.Missing = nil
	attachStartingPoint(&proposal)
	proposal.Warnings = append(proposal.Warnings, recipeOrderWarning)
	return proposal, nil
}

func buildRecipeExperiments(template recipeTemplate, model, revision string, compatibility []integration.RuntimeCompatibility, runtimes []integration.RuntimeProfile, request Request) ([]Candidate, []string, error) {
	accelerator, listed := recipeAcceleratorFor(template, request.GPU)
	if !listed {
		return nil, []string{fmt.Sprintf("No reviewed %s setups exist for accelerator %s; none were added.", template.id, request.GPU)}, nil
	}
	if accelerator.status != recipeQualified {
		return nil, []string{fmt.Sprintf("The %s setups are not qualified on %s: %s None were added.", template.id, accelerator.name, accelerator.reason)}, nil
	}
	if len(request.Runtimes) > 0 && !contains(request.Runtimes, template.engine) && !contains(request.Runtimes, recipeRuntime) {
		return nil, []string{fmt.Sprintf("The runtime allowlist excludes the %s setups; none were added.", template.id)}, nil
	}
	compatible, found := bestRecipeCompatibility(compatibility, runtimes, request)
	if !found {
		return nil, []string{"Adding setups needs a qualified pinned-image runtime on this provider; none were added."}, nil
	}
	shape, err := performanceprofile.Get(request.WorkloadProfile)
	if err != nil {
		return nil, nil, err
	}
	concurrency := float64(shape.Concurrency)
	if request.TargetConcurrency != nil {
		concurrency = *request.TargetConcurrency
	}
	type rankedArm struct {
		arm      recipeArm
		priority int
	}
	ranked := make([]rankedArm, 0, len(template.arms))
	skippedForFeatures := false
	for _, arm := range template.arms {
		if shape.InputTokens < arm.minInputTokens {
			continue
		}
		eligible := true
		for _, feature := range arm.requires {
			eligible = eligible && contains(request.ModelFeatures, feature)
		}
		if !eligible {
			skippedForFeatures = true
			continue
		}
		ranked = append(ranked, rankedArm{arm: arm, priority: recipeArmPriority(arm, shape, concurrency, request.Objective)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].priority != ranked[j].priority {
			return ranked[i].priority > ranked[j].priority
		}
		return ranked[i].arm.id < ranked[j].arm.id
	})
	candidates := make([]Candidate, 0, len(ranked))
	for _, entry := range ranked {
		candidate, buildErr := buildRecipeCandidate(template, entry.arm, entry.priority, model, revision, compatible, request)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		candidates = append(candidates, candidate)
	}
	var warnings []string
	if skippedForFeatures {
		warnings = append(warnings, "Speculative-decoding setups were skipped because the pinned model config did not declare a native prediction head or was not supplied.")
	}
	return candidates, warnings, nil
}

// recipeArmPriority orders paid exploration only. The control is pinned first;
// other weights reflect where a mechanism can matter for the declared shape.
func recipeArmPriority(arm recipeArm, shape performanceprofile.Profile, concurrency float64, objective string) int {
	if contains(arm.layers, layerControl) {
		return 1000
	}
	priority := 10*len(arm.layers) + arm.priorityOffset
	if contains(arm.layers, layerSpeculative) {
		priority += 30
		if shape.OutputTokens >= 256 {
			priority += 20
		}
		if objective == "interactive" || objective == "latency" {
			priority += 5
		}
	}
	if contains(arm.layers, layerGraphs) && concurrency >= 4 {
		priority += 15
	}
	if contains(arm.layers, layerPrefill) {
		priority += 10
	}
	if arm.changesPrecision {
		priority -= 5
	}
	return priority
}

func bestRecipeCompatibility(entries []integration.RuntimeCompatibility, runtimes []integration.RuntimeProfile, request Request) (integration.RuntimeCompatibility, bool) {
	unrestricted := request
	unrestricted.Runtimes = nil
	var matches []integration.RuntimeCompatibility
	for _, entry := range matchingCompatibility(entries, runtimes, unrestricted) {
		if entry.Runtime == recipeRuntime {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return integration.RuntimeCompatibility{}, false
	}
	sort.SliceStable(matches, func(i, j int) bool {
		left, right := qualificationRank(string(matches[i].State)), qualificationRank(string(matches[j].State))
		if left != right {
			return left < right
		}
		return matches[i].Adapter < matches[j].Adapter
	})
	return matches[0], true
}

// recipeAcceleratorFor finds the template's entry for a GPU. A reviewed
// accelerator matches under every provider spelling; other names match their
// upper-cased alias without an "NVIDIA-" prefix.
func recipeAcceleratorFor(template recipeTemplate, gpu string) (recipeAccelerator, bool) {
	name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(gpu)), "NVIDIA-")
	if accelerator, reviewed := provideridentity.LookupAccelerator(gpu); reviewed {
		name = strings.ToUpper(accelerator.Name)
	}
	for _, accelerator := range template.accelerators {
		if strings.ToUpper(accelerator.name) == name {
			return accelerator, true
		}
	}
	return recipeAccelerator{}, false
}

func buildRecipeCandidate(template recipeTemplate, arm recipeArm, priority int, model, revision string, compatible integration.RuntimeCompatibility, request Request) (Candidate, error) {
	command := append([]string(nil), template.launcher...)
	command = append(command,
		"--model-path", "${MODEL}",
		"--host", "0.0.0.0",
		"--port", "${PORT}",
		"--served-model-name", "${MODEL}",
		"--revision", "${MODEL_REVISION}",
		"--tp-size", strconv.Itoa(request.GPUCount),
		"--mem-fraction-static", "0.90",
		"--enable-metrics",
	)
	command = append(command, arm.args...)
	workload := runtimecontract.Workload{
		Image: template.image, Command: command, Protocol: runtimecontract.OpenAIProtocol, Port: 30000,
		ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics",
		Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 120,
	}
	if err := workload.Validate(); err != nil {
		return Candidate{}, fmt.Errorf("setup %s: %w", arm.id, err)
	}

	draft := DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment"}
	draft.Name = safeName(model + "-" + arm.id + "-" + compatible.Adapter)
	draft.ComputeConnectionID = request.ComputeConnectionID
	draft.Model.ID, draft.Model.Revision = model, revision
	applyModelInputs(&draft, request)
	draft.Runtime.Engine, draft.Runtime.Version = recipeRuntime, template.version
	draft.Runtime.Args = []string{}
	draft.Runtime.Workload = workload
	draft.Compute.Mode = string(compatible.Mode)
	draft.Resources.GPU, draft.Resources.GPUCount = request.GPU, request.GPUCount
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = compatible.Cloud, compatible.Adapter, request.Region
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	draft.Routing.Strategy = "round-robin"

	source := SourceInfo{Name: recipeSearchSource, Version: recipeSearchVersion, EvidenceClass: "configuration-unverified", UpstreamProject: template.engine}
	features := []Feature{
		{Name: "immutable_runtime_artifact", State: "pinned", Source: template.image},
		{Name: "immutable_model_revision", State: "pinned", Source: revision},
		{Name: "recipe_arm", State: "experiment", Source: arm.id},
		{Name: "exploration_priority", State: strconv.Itoa(priority), Source: recipeSearchVersion},
		{Name: "flags_exercised_on_image", State: "not-performance-evidence", Source: arm.exercisedBy},
	}
	for _, layer := range arm.layers {
		features = append(features, Feature{Name: strings.ReplaceAll(layer, "-", "_"), State: "proposed", Source: recipeSearchVersion})
	}
	if len(arm.requires) > 0 {
		features = append(features, Feature{Name: "model_config", State: "declared", Source: request.ModelConfigDigest})
	}

	required := []string{"runtime readiness and exact served-model identity", "model architecture and accelerator memory fit", "license and access review", "AIPerf " + request.WorkloadProfile + " workload against the untuned starting setup on the same image"}
	if arm.changesPrecision {
		required = append(required, "semantic quality evidence because this setup changes runtime precision")
	}
	if contains(arm.layers, layerSpeculative) {
		required = append(required, "output parity and speculative acceptance evidence")
	}
	if request.MaxHourlyCost != nil || request.Objective == "cost-efficiency" {
		required = append(required, "sourced hourly cost")
	}
	if request.MaxTTFTP95MS != nil || request.MaxTPOTP95MS != nil || request.MaxErrorRate != nil || request.MinGoodput != nil || request.MinOutputTokensSecond != nil {
		required = append(required, "measured SLO metrics")
	}
	limitations := []string{
		"This setup is an experiment on a pinned runtime image; its flags were exercised on another model and carry no performance claim for this one.",
		"Reasoning and tool-call parsers are not configured, so model-specific API behavior must be added and verified before production use.",
		"GPU memory fit and model license/access were supplied by the operator and have not been inferred.",
	}
	required, limitations = requireCustomerReplay(request, required, limitations)

	identity, err := json.Marshal(struct {
		Source        SourceInfo                       `json:"source"`
		Deployment    DeploymentDraft                  `json:"deployment"`
		Profile       string                           `json:"profile"`
		Arm           string                           `json:"arm"`
		Compatibility integration.RuntimeCompatibility `json:"compatibility"`
	}{source, draft, request.WorkloadProfile, arm.id, compatible})
	if err != nil {
		return Candidate{}, err
	}
	sum := sha256.Sum256(identity)
	return Candidate{
		ID: hex.EncodeToString(sum[:]), Status: "proposed-unmeasured", EvidenceState: EvidenceUnmeasured, Objective: request.Objective,
		ConfigurationProfile: "recipe-" + template.id + "-" + arm.id, BenchmarkProfile: request.WorkloadProfile,
		CompatibilityState: string(compatible.State), CompatibilityEvidence: compatible.Evidence, Source: source,
		Features: features, RequiredEvidence: required, Limitations: limitations, Deployment: draft,
	}, nil
}

// searchesSetups reports whether a request asks for setups to try.
func searchesSetups(request Request) bool {
	return request.Search == SearchSetups || request.Search == SearchRecipes
}
