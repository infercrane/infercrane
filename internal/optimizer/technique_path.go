package optimizer

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/optimizedartifact"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/support"
	contract "github.com/infercrane/infercrane/internal/technique"
)

// The one path a technique built on the technique contract (internal/technique)
// takes into a proposal. Its package makes its setups: the registry asks every
// technique that admits the starting setup, and contractCandidate turns each
// setup it offers into a proposal candidate the same way. The diagnosis rules
// still decide whether and where a technique a rule can choose is measured
// (trained drafts); a technique no rule chooses is measured after the rules'
// choices (kernels tuned for your shapes, applyUnruledTechniques).

// techniqueSource names candidates a contract technique made.
const techniqueSource = "technique-contract"

// CandidateTechnique names the contract technique that made a setup: its
// manifest ID and version, and the artifact the setup serves.
type CandidateTechnique struct {
	ID       string                `json:"id"`
	Version  string                `json:"version"`
	Artifact *contract.ArtifactRef `json:"artifact,omitempty"`
}

// builtinTechniques are the contract techniques InferCrane ships, by
// manifest ID: the ones a proposal can name.
func builtinTechniques() []contract.Technique {
	return []contract.Technique{kerneltuning.Technique{}, speculator.Technique{}, AdaptiveSpeculation{}, AdaptiveSpeculationOneEngine{}}
}

// TechniqueManifest returns a built-in technique's manifest by its manifest
// ID or an earlier planner name.
func TechniqueManifest(id string) (contract.Manifest, bool) {
	id = canonicalTechnique(id)
	for _, technique := range builtinTechniques() {
		if manifest := technique.Manifest(); manifest.ID == id {
			return manifest, true
		}
	}
	return contract.Manifest{}, false
}

func validateCandidateTechnique(technique CandidateTechnique) error {
	manifest, known := TechniqueManifest(technique.ID)
	if !known || technique.Version != manifest.Version {
		return fmt.Errorf("proposal candidate names technique %s %s, which this release does not ship", technique.ID, technique.Version)
	}
	if artifact := technique.Artifact; artifact != nil && (artifact.ID == "" || artifact.Kind == "" || !strings.HasPrefix(artifact.Digest, "sha256:")) {
		return fmt.Errorf("proposal candidate technique %s names an artifact without an ID, kind, and sha256 digest", technique.ID)
	}
	return nil
}

// contractTechniques are the techniques whose setups come from the contract.
var contractTechniques = map[string]bool{TechniqueSpeculativeTrainedDraft: true, TechniqueTunedKernels: true, TechniqueAdaptiveSpeculationOneEngine: true}

// techniqueAliases are earlier names of contract techniques, kept so stored
// proposals that name them still validate.
var techniqueAliases = map[string]string{"speculative-trained-draft": TechniqueSpeculativeTrainedDraft, "tuned-kernels": TechniqueTunedKernels}

// canonicalTechnique is a technique's current name.
func canonicalTechnique(id string) string {
	if current, alias := techniqueAliases[id]; alias {
		return current
	}
	return id
}

// unruledTechnique is how a contract technique no rule chooses is placed: its
// priority, below every rule's choice, and the rule its reason names.
type unruledTechnique struct {
	id       string
	priority int
	rule     string
	// leftOut opens the warning when its setup cannot be added.
	leftOut string
}

// unruledTechniques in the order they are added.
var unruledTechniques = []unruledTechnique{{id: TechniqueTunedKernels, priority: tunedKernelsPriority, rule: "kernels.tuned_set", leftOut: "The tuned kernel set was left out"}}

// techniqueEvidence is the required-evidence sentence for each evidence name a
// manifest declares (technique.Manifest.Evidence).
var techniqueEvidence = map[string]string{
	kerneltuning.EvidenceKernelInvocation: "the engine's log showing it read the tuned file, and the starting setup's showing its default for that file",
	speculator.EvidenceDraftAcceptance:    "output parity and speculative acceptance evidence",
	"swap-timings":                        "the speculation switch's swap timings for every change of band",
	EvidenceStateByDraftReceipt:           "the engine's own check of the patched image's receipt, reported on its metrics in every measured run",
	EvidenceReservationMix:                "the engine's gauges showing requests reserving three draft tokens and fewer running together",
}

func evidenceSentence(name string) string {
	if sentence, known := techniqueEvidence[name]; known {
		return sentence
	}
	return "the technique's own evidence: " + name
}

// proposalRegistry holds the contract techniques a request's own inputs feed:
// trained drafts from speculative_drafts, and the tuned set kernel_configs
// carries, on the starting setup's GPU; and speculation that changes with
// load in one engine, which offers a setup only when its image is available.
func proposalRegistry(request Request, control Candidate) (*contract.Registry, error) {
	list := []contract.Technique{speculator.Technique{Drafts: request.SpeculativeDrafts}, AdaptiveSpeculationOneEngine{}}
	if request.KernelConfigs != nil {
		list = append(list, kerneltuning.Technique{Sets: requestSets{request.KernelConfigs}, Accelerators: []string{control.Deployment.Resources.GPU}})
	}
	return contract.NewRegistry(list...)
}

// requestSets offers kernel tuning the tuned set the request carries.
type requestSets struct{ set *domain.KernelConfigSet }

func (s requestSets) Find(_ context.Context, _, _, runtime, _, device string) (*domain.KernelConfigSet, error) {
	if s.set == nil || s.set.Runtime != runtime || s.set.Device != device {
		return nil, nil
	}
	return s.set, nil
}

// contractSetup is the starting setup as the contract states it: no prompt or
// answer, only what the plan and the diagnosis read.
func contractSetup(control Candidate, request Request) contract.Setup {
	deployment := control.Deployment
	setup := contract.Setup{
		Model: deployment.Model.ID, ModelRevision: deployment.Model.Revision, ModelFeatures: slices.Clone(request.ModelFeatures),
		Engine: deployment.Runtime.Engine, EngineVersion: deployment.Runtime.Version, Accelerator: deployment.Resources.GPU, GPUCount: deployment.Resources.GPUCount,
		Args: slices.Clone(deployment.Runtime.Args), Env: maps.Clone(deployment.Runtime.Env),
	}
	// A tuned fused-MoE set for block-FP8 weights was built from this model's
	// own MoE shape, so the model runs that kernel.
	if set := request.KernelConfigs; set != nil && !slices.Contains(setup.ModelFeatures, kerneltuning.ModelFeatureFusedMoEFP8Block) && blockFP8MoE(*set) {
		setup.ModelFeatures = append(setup.ModelFeatures, kerneltuning.ModelFeatureFusedMoEFP8Block)
	}
	if request.Diagnosis != nil {
		setup.Signals = maps.Clone(request.Diagnosis.Signals)
	}
	if request.TargetConcurrency != nil {
		setup.Loads = []int{int(math.Ceil(*request.TargetConcurrency))}
	}
	setup.Objective = request.Objective
	setup.FirstTokenLimitMS = lab.FirstTokenLimitMS(request.Objective, request.MaxTTFTP95MS)
	return setup
}

// engineWorkload is the deployment workload for a setup that names its own
// engine image: the built-in vLLM workload with that image, and the image's
// command in place of "vllm serve" (the model, revision, host and port follow
// it as they follow "vllm serve").
func engineWorkload(image contract.RuntimeImage) (runtimecontract.Workload, error) {
	workload := support.VLLMWorkload()
	if len(workload.Command) < 2 || workload.Command[0] != "vllm" || workload.Command[1] != "serve" {
		return runtimecontract.Workload{}, fmt.Errorf("the built-in vLLM workload does not start with vllm serve: %v", workload.Command)
	}
	workload.Image = image.Image
	workload.Command = append(slices.Clone(image.Command), workload.Command[2:]...)
	return workload, workload.Validate()
}

func blockFP8MoE(set domain.KernelConfigSet) bool {
	for _, file := range set.Files {
		if strings.Contains(file.Name, "dtype=fp8_w8a8") && strings.Contains(file.Name, "block_shape=") {
			return true
		}
	}
	return false
}

// contractOffers asks the registry once for a proposal: the setups each
// technique offers, and why a technique offered none.
func contractOffers(request Request, control Candidate) (map[string][]contract.Candidate, map[string]string, map[string]contract.Manifest, error) {
	registry, err := proposalRegistry(request, control)
	if err != nil {
		return nil, nil, nil, err
	}
	offered, skipped, err := registry.Candidates(context.Background(), contractSetup(control, request))
	if err != nil {
		return nil, nil, nil, err
	}
	offers, why, manifests := map[string][]contract.Candidate{}, map[string]string{}, map[string]contract.Manifest{}
	for _, manifest := range registry.Manifests() {
		manifests[manifest.ID] = manifest
	}
	for _, candidate := range offered {
		offers[candidate.Technique] = append(offers[candidate.Technique], candidate)
	}
	for _, skip := range skipped {
		why[skip.Technique] = sentence(skip.Reason)
	}
	return offers, why, manifests, nil
}

// sentence turns a contract reason (a lowercase clause) into a sentence.
func sentence(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	if !strings.HasSuffix(reason, ".") {
		reason += "."
	}
	return strings.ToUpper(reason[:1]) + reason[1:]
}

// contractCandidate turns one setup a contract technique offers into a
// proposal candidate: the starting setup with the setup's flags and
// environment, the artifact it serves, the technique's provenance, and the
// evidence its manifest requires. A setup that cannot serve here is skipped
// with the reason.
func contractCandidate(control Candidate, manifest contract.Manifest, offered contract.Candidate, request Request) (Candidate, string, error) {
	technique, known := techniques[manifest.ID]
	if !known {
		return Candidate{}, "", fmt.Errorf("technique %s is not one the planner lists", manifest.ID)
	}
	var workload *runtimecontract.Workload
	if image := offered.Image; image != nil {
		// A setup on its own engine image runs only there, with no engine
		// environment: the product reviews environment for the pinned images.
		if control.Deployment.Runtime.Engine != "vllm" {
			return Candidate{}, "its engine image is a vLLM image, and the starting setup runs " + control.Deployment.Runtime.Engine, nil
		}
		if len(offered.Env) > 0 {
			return Candidate{}, "a setup on its own engine image takes no engine environment", nil
		}
		built, err := engineWorkload(*image)
		if err != nil {
			return Candidate{}, "", fmt.Errorf("technique %s: %w", manifest.ID, err)
		}
		workload = &built
	}
	var set *domain.KernelConfigSet
	if artifact := offered.Artifact; artifact != nil && artifact.Kind == optimizedartifact.KindKernelConfigs {
		if request.KernelConfigs == nil || request.KernelConfigs.Digest != artifact.Digest {
			return Candidate{}, "the tuned set is not the one the request carries", nil
		}
		runtime := control.Deployment.Runtime
		if err := kerneltuning.ValidateForDeployment(request.KernelConfigs, runtime.Engine, runtime.Version, control.Deployment.Resources.GPU, runtime.Workload.Empty()); err != nil {
			return Candidate{}, err.Error(), nil
		}
		set = request.KernelConfigs
	}
	provenance := manifest.ID + "@" + manifest.Version
	candidate, err := deriveCandidate(control, technique, offered.Args, offered.Env, request, func(candidate *Candidate) {
		candidate.Technique = &CandidateTechnique{ID: manifest.ID, Version: manifest.Version, Artifact: offered.Artifact}
		candidate.Source = SourceInfo{Name: techniqueSource, Version: provenance, EvidenceClass: "technique-unverified", UpstreamProject: control.Deployment.Runtime.Engine}
		candidate.Features = append(candidate.Features, Feature{Name: "technique", State: provenance, Source: techniqueSource})
		if workload != nil {
			candidate.Deployment.Runtime.Workload = *workload
			candidate.Features = append(candidate.Features, Feature{Name: "engine_image", State: offered.Image.Identity, Source: techniqueSource})
			// An image a qualification run named is not in the request, so
			// the candidate says where it came from.
			if offered.Image.Source != "" {
				candidate.Features = append(candidate.Features, Feature{Name: "engine_image_source", State: offered.Image.Source, Source: techniqueSource})
			}
		}
		if set != nil {
			candidate.Deployment.Runtime.KernelConfigs = set
			candidate.Features = append(candidate.Features, Feature{Name: "kernel_configs", State: set.Digest, Source: kerneltuning.Tuner})
		}
		// The rules' sentence says a diagnosis chose the change; this one
		// names the technique that made it.
		if count := len(candidate.Limitations); count > 0 {
			candidate.Limitations = candidate.Limitations[:count-1]
		}
		candidate.Limitations = append(candidate.Limitations, fmt.Sprintf("This setup was made by the %s technique (%s): %s The technique explains why it is worth measuring, not that it is faster.", manifest.Label, provenance, sentence(offered.Reason)))
		// Every claim of the technique carries its disclosures.
		candidate.Limitations = append(candidate.Limitations, manifest.Disclosures...)
		for _, name := range manifest.Evidence {
			if required := evidenceSentence(name); !slices.Contains(candidate.RequiredEvidence, required) {
				candidate.RequiredEvidence = append(candidate.RequiredEvidence, required)
			}
		}
		if manifest.ChangesAnswers && !slices.Contains(candidate.RequiredEvidence, TaskScoreEvidence) {
			candidate.RequiredEvidence = append(candidate.RequiredEvidence, TaskScoreEvidence)
		}
	})
	return candidate, "", err
}

// applyUnruledTechniques adds the setups of contract techniques no diagnosis
// rule chooses, after every other setup, within the requested bound. A
// technique that offers none, or a setup that cannot serve here, adds a
// warning instead.
func applyUnruledTechniques(proposal Proposal) (Proposal, error) {
	request := proposal.Input
	if len(proposal.Candidates) == 0 || request.KernelConfigs == nil {
		return proposal, nil
	}
	control := proposal.Candidates[0]
	offers, why, manifests, err := contractOffers(request, control)
	if err != nil {
		return Proposal{}, err
	}
	for _, unruled := range unruledTechniques {
		list := offers[unruled.id]
		if len(list) == 0 {
			if reason := why[unruled.id]; reason != "" {
				proposal.Warnings = append(proposal.Warnings, unruled.leftOut+": "+strings.TrimSuffix(lowerFirst(reason), ".")+".")
			}
			continue
		}
		for _, offered := range list {
			if len(proposal.Candidates) >= request.MaxCandidates {
				proposal.Warnings = append(proposal.Warnings, fmt.Sprintf("%s: the proposal already holds %d setups, the requested bound.", unruled.leftOut, request.MaxCandidates))
				break
			}
			candidate, skip, err := contractCandidate(control, manifests[unruled.id], offered, request)
			if err != nil {
				return Proposal{}, err
			}
			if skip != "" {
				proposal.Warnings = append(proposal.Warnings, unruled.leftOut+": "+strings.TrimSuffix(skip, ".")+".")
				continue
			}
			technique := techniques[unruled.id]
			candidate.Reason = &CandidateReason{Role: ReasonDiagnosis, Technique: technique.id, Label: technique.label, Priority: unruled.priority, ExpectedBenefit: expectedBenefit(unruled.priority),
				Rules: []string{unruled.rule}, Evidence: []ReasonEvidence{}, Summary: sentence(offered.Reason)}
			candidate.Rank = len(proposal.Candidates) + 1
			proposal.Candidates = append(proposal.Candidates, candidate)
		}
	}
	return proposal, nil
}

func lowerFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToLower(text[:1]) + text[1:]
}
