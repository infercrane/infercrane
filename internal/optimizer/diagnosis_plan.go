package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
	contract "github.com/infercrane/infercrane/internal/technique"
)

const (
	diagnosisPlannerSource = "infercrane-diagnosis-planner"
	diagnosisOrderWarning  = "Setups are ordered by what the endpoint's diagnosis says is most likely to help. The order is a plan, not a measured result; each setup is measured against the starting setup."
	maxReasonText          = 2048
	maxReasonItems         = 16
)

// Diagnosis plan states.
const (
	DiagnosisApplied             = "applied"
	DiagnosisNotReady            = "not_ready"
	DiagnosisReliabilityBlocked  = "reliability_blocked"
	DiagnosisNoActionableFinding = "no_actionable_findings"
	DiagnosisNoStartingSetup     = "no_starting_setup"
)

// Candidate reason roles.
const (
	ReasonControl   = "control"
	ReasonDiagnosis = "diagnosis"
)

// Evidence a plan asks for when it cannot choose a change.
const (
	NeededFreshTraffic   = "fresh traffic on the endpoint: at least 20 requests in the last hour"
	NeededEngineMetrics  = "serving-engine metrics: running and waiting requests, KV-cache usage, and preemptions"
	NeededFewerErrors    = "an error rate at or below 2%, so failed requests do not bias a speed comparison"
	engineSignalPrefix   = "runtime_"
	differentArtifactWhy = "This setup serves different model files from the starting setup. A diagnosis measures only a reviewed publisher's FP8 checkpoint of the exact model, beside FP8 at load, so it was left out."
	moreGPUsWhy          = "This setup spreads the model over more GPUs per server than the starting setup, and the GPU trace shows communication between GPUs already holding the GPU idle, so it was left out."
)

// DiagnosisPlan records how a diagnosis chose and ordered the candidates:
// every technique a rule chose, its priority and reason, and either the
// candidates that apply it or why none does.
type DiagnosisPlan struct {
	RulesVersion      string             `json:"rules_version"`
	Endpoint          string             `json:"endpoint"`
	State             string             `json:"state"`
	PrimaryBottleneck string             `json:"primary_bottleneck"`
	Summary           string             `json:"summary"`
	Techniques        []PlannedTechnique `json:"techniques"`
	// NeededEvidence names what to collect before a diagnosis can choose a
	// change. It is set whenever the proposal measures only the control.
	NeededEvidence []string `json:"needed_evidence"`
	// CacheRoom says how many requests fit at once with each draft length,
	// beside the load the server is asked to hold. Absent when the engine
	// did not report the readings it needs.
	CacheRoom *CacheRoom `json:"cache_room,omitempty"`
}

// PlannedTechnique is one technique the diagnosis chose, in planning order.
type PlannedTechnique struct {
	Technique       string           `json:"technique"`
	Label           string           `json:"label"`
	Priority        int              `json:"priority"`
	ExpectedBenefit string           `json:"expected_benefit"`
	Rules           []string         `json:"rules"`
	Reason          string           `json:"reason"`
	Evidence        []ReasonEvidence `json:"evidence"`
	CandidateIDs    []string         `json:"candidate_ids"`
	// Skipped says why no candidate in the proposal applies the technique.
	Skipped string `json:"skipped,omitempty"`
}

// CandidateReason says why a candidate is in a diagnosis-ordered proposal and
// where it ranks. Priority and expected benefit are planning priors from the
// diagnosis, never a measured or predicted gain.
type CandidateReason struct {
	Role            string           `json:"role"`
	Technique       string           `json:"technique,omitempty"`
	Label           string           `json:"label"`
	Priority        int              `json:"priority"`
	ExpectedBenefit string           `json:"expected_benefit"`
	Rules           []string         `json:"rules"`
	Summary         string           `json:"summary"`
	Evidence        []ReasonEvidence `json:"evidence"`
}

// validateCandidateReason bounds a reason in an untrusted proposal. A reason
// orders measurement; it can never mark a candidate measured or qualified.
func validateCandidateReason(reason CandidateReason) error {
	if reason.Role != ReasonControl && reason.Role != ReasonDiagnosis && reason.Role != ReasonReference && reason.Role != ReasonCopy || reason.Label == "" || len(reason.Label) > 256 || len(reason.Summary) > maxReasonText || len(reason.Rules) > maxReasonItems || len(reason.Evidence) > maxReasonItems || reason.Priority < 0 || reason.Priority > 100 {
		return errors.New("proposal candidate reason is malformed")
	}
	if reason.Role == ReasonDiagnosis {
		if _, known := techniques[canonicalTechnique(reason.Technique)]; !known {
			return errors.New("proposal candidate reason names an unknown technique")
		}
	} else if reason.Technique != "" {
		return errors.New("only a diagnosis reason names a technique")
	}
	for _, evidence := range reason.Evidence {
		if !diagnosisName.MatchString(evidence.Signal) || !finite(evidence.Observed) || evidence.Threshold != nil && !finite(*evidence.Threshold) {
			return errors.New("proposal candidate reason evidence is malformed")
		}
	}
	return nil
}

type rankedCandidate struct {
	candidate Candidate
	choice    *plannedChoice
	derived   bool
	order     int
}

// applyDiagnosis keeps the starting setup first and adds only setups a rule
// chose: one derived from the starting setup per chosen change, plus
// existing setups that already apply a chosen change, in order of expected
// benefit. Setups no rule chose are left out. When nothing applies, the
// proposal measures only the starting setup and names the evidence to
// collect. Gates are unchanged: every added setup is unmeasured and carries
// the starting setup's required evidence.
func applyDiagnosis(proposal Proposal) (Proposal, error) {
	request := proposal.Input
	// The plan starts from what the endpoint runs, when the control can take it.
	if why := applyStartingSetup(&proposal); why != "" {
		proposal.Warnings = append(proposal.Warnings, why)
	}
	// The plan is made for the load the server holds now; the recorded
	// diagnosis stays in the proposal's input.
	diagnosis, note := presentDiagnosis(*request.Diagnosis)
	facts := planContext{objective: request.Objective, gpu: request.GPU}
	var controlArgs []string
	if len(proposal.Candidates) > 0 {
		facts.engine, facts.version = proposal.Candidates[0].Deployment.Runtime.Engine, proposal.Candidates[0].Deployment.Runtime.Version
		controlArgs = proposal.Candidates[0].Deployment.Runtime.Args
	}
	facts.drafts = currentDrafts(diagnosis, controlArgs)
	if len(proposal.Candidates) > 0 {
		facts.fp8Weights, facts.fp8Cache = precisionOf(proposal.Candidates[0].Deployment.Model.ID, controlArgs)
	}
	facts.supplied = suppliedDrafts(request)
	choices := applyPrior(request, diagnosis, proposal, planTechniques(diagnosis, facts))
	plan := &DiagnosisPlan{RulesVersion: DiagnosisRulesVersion, Endpoint: diagnosis.Endpoint, PrimaryBottleneck: diagnosis.PrimaryBottleneck, Techniques: []PlannedTechnique{}, NeededEvidence: []string{},
		CacheRoom: cacheRoom(diagnosis, facts.drafts, facts.fp8Weights, facts.fp8Cache)}
	proposal.Diagnosis = plan
	planned := make([]PlannedTechnique, len(choices))
	for index, choice := range choices {
		planned[index] = PlannedTechnique{
			Technique: choice.technique.id, Label: choice.technique.label, Priority: choice.priority, ExpectedBenefit: expectedBenefit(choice.priority),
			Rules: choice.rules, Reason: choice.reason, Evidence: choice.evidence, CandidateIDs: []string{},
		}
	}
	if len(proposal.Candidates) == 0 {
		for index := range planned {
			planned[index].Skipped = "No starting setup could be planned for this model and infrastructure, so there is nothing to change."
		}
		finishPlan(&proposal, planned, diagnosis, nil, note)
		return proposal, nil
	}

	control := proposal.Candidates[0]
	skipped := map[string]string{}
	usable := map[string]*plannedChoice{}
	for index := range choices {
		choice := &choices[index]
		if why := techniqueGate(choice.technique, control, request); why != "" {
			skipped[choice.technique.id] = why
			continue
		}
		if choice.skip != "" {
			skipped[choice.technique.id] = choice.skip
			continue
		}
		usable[choice.technique.id] = choice
	}
	var ranked []rankedCandidate
	var checkpoints []ArtifactSeed
	covered := map[string]bool{}
	for index, candidate := range proposal.Candidates[1:] {
		var best *plannedChoice
		for _, id := range classifyCandidate(candidate) {
			if choice := usable[id]; choice != nil && (best == nil || choice.priority > best.priority) {
				best = choice
			}
		}
		if best == nil {
			continue
		}
		if !sameArtifact(candidate, control) {
			// A published FP8 checkpoint of the starting setup's exact model is
			// measured beside FP8 weights quantized at load, not instead of them.
			if best.technique.id == TechniqueFP8Weights && publishedFP8(candidate, control) {
				if !slices.ContainsFunc(checkpoints, func(seed ArtifactSeed) bool { return seed.CatalogID == candidate.ArtifactSeed.CatalogID }) {
					checkpoints = append(checkpoints, *candidate.ArtifactSeed)
				}
				continue
			}
			if skipped[best.technique.id] == "" && !covered[best.technique.id] {
				skipped[best.technique.id] = differentArtifactWhy
			}
			continue
		}
		if _, exposed := diagnosis.traceFinding(TraceRuleCommunicationExposed); exposed && gpusPerServer(candidate) > gpusPerServer(control) {
			if skipped[best.technique.id] == "" && !covered[best.technique.id] {
				skipped[best.technique.id] = moreGPUsWhy
			}
			continue
		}
		covered[best.technique.id] = true
		delete(skipped, best.technique.id)
		ranked = append(ranked, rankedCandidate{candidate: candidate, choice: best, order: index})
	}
	var offers map[string][]contract.Candidate
	var offerWhy map[string]string
	var manifests map[string]contract.Manifest
	for index := range choices {
		choice := &choices[index]
		if usable[choice.technique.id] == nil || covered[choice.technique.id] {
			continue
		}
		// A contract technique's setups come from its own package, asked
		// once per proposal (technique_path.go).
		if contractTechniques[choice.technique.id] {
			if offers == nil {
				var err error
				if offers, offerWhy, manifests, err = contractOffers(request, control); err != nil {
					return Proposal{}, err
				}
			}
			added := false
			for _, offered := range offers[choice.technique.id] {
				derived, skip, err := contractCandidate(control, manifests[choice.technique.id], offered, request)
				if err != nil {
					return Proposal{}, err
				}
				if skip != "" {
					offerWhy[choice.technique.id] = sentence(skip)
					continue
				}
				ranked = append(ranked, rankedCandidate{candidate: derived, choice: choice, derived: true, order: len(ranked)})
				added = true
			}
			if added {
				delete(skipped, choice.technique.id)
			} else if skipped[choice.technique.id] == "" {
				skipped[choice.technique.id] = offerWhy[choice.technique.id]
			}
			continue
		}
		args, env, reason := control.Deployment.Runtime.Args, map[string]string(nil), ""
		if choice.technique.env != nil {
			env, reason = deriveEnv(control, choice.technique, request)
		} else {
			args, reason = deriveArgs(control, choice.technique, diagnosis, request)
		}
		if reason != "" {
			if skipped[choice.technique.id] == "" {
				skipped[choice.technique.id] = reason
			}
			continue
		}
		delete(skipped, choice.technique.id)
		derived, err := diagnosisCandidate(control, choice.technique, args, env, request)
		if err != nil {
			return Proposal{}, err
		}
		ranked = append(ranked, rankedCandidate{candidate: derived, choice: choice, derived: true, order: len(ranked)})
	}
	if weights := usable[TechniqueFP8Weights]; weights != nil && len(checkpoints) > 0 {
		choice := checkpointChoice(*weights)
		for _, seed := range checkpoints {
			derived, err := checkpointSetup(control, seed, request)
			if err != nil {
				return Proposal{}, err
			}
			ranked = append(ranked, rankedCandidate{candidate: derived, choice: &choice, derived: true, order: len(ranked)})
		}
		planned = append(planned, PlannedTechnique{Technique: choice.technique.id, Label: choice.technique.label, Priority: choice.priority,
			ExpectedBenefit: expectedBenefit(choice.priority), Rules: choice.rules, Reason: choice.reason, Evidence: choice.evidence, CandidateIDs: []string{}})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].choice.priorOrder != ranked[j].choice.priorOrder {
			return ranked[i].choice.priorOrder < ranked[j].choice.priorOrder
		}
		if ranked[i].choice.priority != ranked[j].choice.priority {
			return ranked[i].choice.priority > ranked[j].choice.priority
		}
		if techniqueRank(ranked[i].choice.technique.id) != techniqueRank(ranked[j].choice.technique.id) {
			return techniqueRank(ranked[i].choice.technique.id) < techniqueRank(ranked[j].choice.technique.id)
		}
		if ranked[i].derived != ranked[j].derived {
			return !ranked[i].derived
		}
		return ranked[i].order < ranked[j].order
	})

	// The setups that test the plan itself come right after its first choice
	// or after its last (diagnosis_standing.go).
	standing, err := standingCandidates(control, ranked, diagnosis, request, plan.CacheRoom)
	if err != nil {
		return Proposal{}, err
	}
	control.Reason = &CandidateReason{Role: ReasonControl, Label: "Starting setup", ExpectedBenefit: "baseline", Rules: []string{}, Evidence: []ReasonEvidence{},
		Summary: "The setup every other setup must beat; it is measured in turns with each of them on the same traffic."}
	ordered := []Candidate{control}
	for index, entry := range ranked {
		candidate := entry.candidate
		candidate.Reason = &CandidateReason{
			Role: ReasonDiagnosis, Technique: entry.choice.technique.id, Label: entry.choice.technique.label, Priority: entry.choice.priority,
			ExpectedBenefit: expectedBenefit(entry.choice.priority), Rules: entry.choice.rules, Summary: entry.choice.reason, Evidence: entry.choice.evidence,
		}
		ordered = append(ordered, candidate)
		if index == 0 {
			ordered = appendStanding(ordered, standing, false)
		}
	}
	ordered = appendStanding(ordered, standing, true)
	proposal.Candidates = ordered
	finishPlan(&proposal, planned, diagnosis, skipped, note)
	return proposal, nil
}

// finishPlan bounds the candidates, links each planned technique to its
// candidates or its skip reason, and sets the plan state, summary, and the
// evidence to collect when only the starting setup is measured. A note from
// reading the diagnosis for the present load opens the summary.
func finishPlan(proposal *Proposal, planned []PlannedTechnique, diagnosis Diagnosis, skipped map[string]string, note string) {
	plan, request := proposal.Diagnosis, proposal.Input
	if len(proposal.Candidates) > request.MaxCandidates {
		proposal.Candidates = proposal.Candidates[:request.MaxCandidates]
	}
	for index := range proposal.Candidates {
		proposal.Candidates[index].Rank = index + 1
	}
	if len(proposal.Candidates) > 0 {
		attachStartingPoint(proposal)
	}
	kept := map[string][]string{}
	for _, candidate := range proposal.Candidates {
		if candidate.Reason != nil && candidate.Reason.Role == ReasonDiagnosis {
			kept[candidate.Reason.Technique] = append(kept[candidate.Reason.Technique], candidate.ID)
		}
	}
	for index := range planned {
		id := planned[index].Technique
		switch {
		case planned[index].Skipped != "":
		case len(kept[id]) > 0:
			planned[index].CandidateIDs = kept[id]
		case skipped[id] != "":
			planned[index].Skipped = skipped[id]
		default:
			planned[index].Skipped = fmt.Sprintf("Not measured in this run: the limit of %d setups was reached by setups ranked higher.", request.MaxCandidates)
		}
	}
	plan.Techniques = planned
	switch {
	case diagnosis.State == optimizationreadiness.StateCollecting:
		plan.State = DiagnosisNotReady
		plan.NeededEvidence = append(plan.NeededEvidence, NeededFreshTraffic)
	case diagnosis.State == optimizationreadiness.StateReliabilityBlocked:
		plan.State = DiagnosisReliabilityBlocked
		plan.NeededEvidence = append(plan.NeededEvidence, NeededFewerErrors)
	case len(proposal.Candidates) == 0:
		plan.State = DiagnosisNoStartingSetup
	case len(kept) == 0:
		plan.State = DiagnosisNoActionableFinding
	default:
		plan.State = DiagnosisApplied
	}
	if plan.State != DiagnosisApplied && !diagnosis.hasEngineSignals() {
		plan.NeededEvidence = append(plan.NeededEvidence, NeededEngineMetrics)
	}
	plan.Summary = diagnosisSummary(plan, diagnosis)
	if note != "" {
		plan.Summary = note + " " + plan.Summary
	}
	if plan.State == DiagnosisApplied {
		warnings := []string{}
		for _, warning := range proposal.Warnings {
			if warning != recipeOrderWarning {
				warnings = append(warnings, warning)
			}
		}
		proposal.Warnings = append(warnings, diagnosisOrderWarning)
	} else {
		proposal.Warnings = append(proposal.Warnings, plan.Summary)
	}
}

func (d Diagnosis) hasEngineSignals() bool {
	for name := range d.Signals {
		if strings.HasPrefix(name, engineSignalPrefix) {
			return true
		}
	}
	return false
}

func diagnosisSummary(plan *DiagnosisPlan, diagnosis Diagnosis) string {
	bottleneck, known := bottleneckWords[diagnosis.PrimaryBottleneck]
	if !known {
		bottleneck = strings.ReplaceAll(diagnosis.PrimaryBottleneck, "-", " ")
	}
	collect := ""
	if len(plan.NeededEvidence) > 0 {
		collect = " Collect " + strings.Join(plan.NeededEvidence, "; and ") + ", then plan again."
	}
	switch plan.State {
	case DiagnosisNotReady:
		return "The endpoint's diagnosis is not ready, so no change is proposed and only the starting setup is measured." + collect
	case DiagnosisReliabilityBlocked:
		return "The endpoint is failing too many requests to compare speed, so no change is proposed and only the starting setup is measured." + collect
	case DiagnosisNoStartingSetup:
		return "No starting setup could be planned for this model and infrastructure, so there is nothing to change."
	case DiagnosisNoActionableFinding:
		if len(plan.Techniques) == 0 {
			return "The diagnosis points at " + bottleneck + ", but no rule supports a setup change, so only the starting setup is measured." + collect
		}
		return "The diagnosis points at " + bottleneck + ", but none of the changes it chose applies here, so only the starting setup is measured; each change says why." + collect
	}
	var labels []string
	for _, technique := range plan.Techniques {
		if len(technique.CandidateIDs) > 0 {
			labels = append(labels, sentenceCase(technique.Label))
		}
	}
	return "The diagnosis points at " + bottleneck + ". Trying, in order: " + strings.Join(labels, ", ") + "."
}

var bottleneckWords = map[string]string{
	optimizationreadiness.BottleneckKVCachePressure: "a full KV cache",
	optimizationreadiness.BottleneckHostScheduler:   "the server's CPU side not keeping the GPU fed",
	optimizationreadiness.BottleneckQueueing:        "requests waiting in line",
	optimizationreadiness.BottleneckPrefill:         "time to first token",
	optimizationreadiness.BottleneckDecode:          "writing answers",
	optimizationreadiness.BottleneckUnattributed:    "serving time it cannot attribute yet",
}

// sentenceCase lowercases a label's first letter unless it starts an
// acronym such as FP8 or KV.
func sentenceCase(label string) string {
	if len(label) > 1 && label[1] >= 'a' && label[1] <= 'z' {
		return strings.ToLower(label[:1]) + label[1:]
	}
	return label
}

// techniqueGate says why a chosen change must not be measured for this model
// at all, whoever would apply it.
func techniqueGate(technique technique, control Candidate, request Request) string {
	if technique.outOfScope != "" && technique.id != TechniqueInt4Weights {
		return technique.outOfScope
	}
	if technique.draft != "" {
		if _, supplied := suppliedDrafts(request)[technique.draft]; !supplied {
			return noDraftWhy[technique.draft]
		}
	}
	size, sized := modelSizeBillions(control.Deployment.Model.ID)
	switch {
	case technique.speculative && sized && size <= speculationMinimumBillions:
		return fmt.Sprintf("The model name marks about %sB parameters; speculation does not pay on models of %sB or fewer.", numberWords(size), numberWords(speculationMinimumBillions))
	case technique.lowersWeights && sized && size <= precisionMinimumBillions:
		return fmt.Sprintf("The model name marks about %sB parameters; lower weight precision is not proposed for models of %sB or fewer.", numberWords(size), numberWords(precisionMinimumBillions))
	}
	return ""
}

// gpusPerServer is how many GPUs one server of a setup uses.
func gpusPerServer(candidate Candidate) int {
	return max(1, candidate.Deployment.Resources.GPUCount)
}

func sameArtifact(candidate, control Candidate) bool {
	return candidate.Deployment.Model.ID == control.Deployment.Model.ID && candidate.Deployment.Model.Revision == control.Deployment.Model.Revision
}

// publishedFP8 reports whether candidate serves a reviewed publisher's FP8
// checkpoint of the starting setup's exact model: the catalog's base is the
// control's model, at the control's revision: the revision the publisher
// declares, or else the one the catalog screened the build against. A build
// the publisher declares only by repository matches no other revision.
func publishedFP8(candidate, control Candidate) bool {
	seed := candidate.ArtifactSeed
	if seed == nil || !strings.Contains(strings.ToLower(seed.Algorithm), "fp8") || strings.Contains(strings.ToLower(seed.Algorithm), "fp4") || !strings.EqualFold(seed.BaseRepository, control.Deployment.Model.ID) {
		return false
	}
	if seed.BaseRevision != "" {
		return seed.BaseRevision == control.Deployment.Model.Revision
	}
	return seed.BaseScreeningRevision != "" && seed.BaseScreeningRevision == control.Deployment.Model.Revision
}

// checkpointSetup is the starting setup with only its weights replaced by a
// published checkpoint: the same flags and environment, the checkpoint's
// files, and the catalog's provenance.
func checkpointSetup(control Candidate, seed ArtifactSeed, request Request) (Candidate, error) {
	base := control
	base.Deployment.Model.ID, base.Deployment.Model.Revision = seed.OutputRepository, seed.OutputRevision
	derived, err := diagnosisCandidate(base, techniques[TechniqueFP8Checkpoint], control.Deployment.Runtime.Args, nil, request)
	if err != nil {
		return Candidate{}, err
	}
	derived.ArtifactSeed = &seed
	derived.Deployment.Name = safeName(control.Deployment.Name + "-" + TechniqueFP8Checkpoint + "-" + strings.ToLower(seed.Publisher))
	return derived, nil
}

// checkpointChoice is the FP8 weights choice carried to a published FP8
// checkpoint: the same priority, ranked right after FP8 at load.
func checkpointChoice(weights plannedChoice) plannedChoice {
	return plannedChoice{
		technique: techniques[TechniqueFP8Checkpoint], priority: weights.priority,
		rules:    append(append([]string(nil), weights.rules...), "weights.published_checkpoint"),
		reason:   weights.reason + " A reviewed publisher's FP8 checkpoint of this exact model is measured beside FP8 at load: its calibration may keep answers closer, and each must keep the task score on its own.",
		evidence: append([]ReasonEvidence(nil), weights.evidence...),
	}
}

// classifyCandidate names the techniques an existing candidate applies, from
// its recorded provenance rather than its name.
func classifyCandidate(candidate Candidate) []string {
	var out []string
	for _, id := range techniqueOrder {
		variables := techniques[id].env[candidate.Deployment.Runtime.Engine]
		applies := len(variables) > 0
		for _, variable := range variables {
			applies = applies && candidate.Deployment.Runtime.Env[variable.name] == variable.value
		}
		if applies {
			out = append(out, id)
		}
	}
	if seed := candidate.ArtifactSeed; seed != nil {
		scheme := strings.ToLower(seed.Algorithm)
		switch {
		case strings.Contains(scheme, "fp4") || strings.Contains(scheme, "int4") || strings.Contains(scheme, "w4") || strings.Contains(scheme, "awq") || strings.Contains(scheme, "gptq"):
			out = append(out, TechniqueInt4Weights)
		case strings.Contains(scheme, "fp8"):
			out = append(out, TechniqueFP8Weights)
		}
	}
	layers := map[string]string{
		strings.ReplaceAll(layerPrecision, "-", "_"):   TechniqueFP8KVCache,
		strings.ReplaceAll(layerSpeculative, "-", "_"): TechniqueSpeculativeMTP,
		strings.ReplaceAll(layerPrefill, "-", "_"):     TechniqueLargePrefill,
		strings.ReplaceAll(layerGraphs, "-", "_"):      TechniqueCUDAGraphs,
	}
	for _, feature := range candidate.Features {
		if id, ok := layers[feature.Name]; ok && feature.State == "proposed" && !contains(out, id) {
			out = append(out, id)
		}
		if feature.Name == "diagnosis_technique" && !contains(out, feature.State) {
			out = append(out, feature.State)
		}
	}
	return out
}

// deriveArgs returns the starting setup's runtime arguments with the
// technique applied, or why the technique cannot be applied to it. A flag
// equal to the engine's resolved default on this accelerator is dropped, and
// a change whose default is unknown is not planned.
func deriveArgs(control Candidate, technique technique, diagnosis Diagnosis, request Request) ([]string, string) {
	if technique.outOfScope != "" {
		return nil, technique.outOfScope
	}
	engine, version := control.Deployment.Runtime.Engine, control.Deployment.Runtime.Version
	if _, builtIn := reviewedEngineVersions[engine]; !builtIn || !control.Deployment.Runtime.Workload.Empty() {
		return nil, "The starting setup runs a pinned custom image; InferCrane changes its flags only through setups reviewed for that image."
	}
	if !control.Deployment.Serving.Empty() {
		return nil, "The starting setup is a Dynamo serving graph; its runtime changes go through the serving topology, not engine flags."
	}
	if technique.draft != "" {
		return draftArgs(control.Deployment.Runtime.Args, engine, version, technique, request)
	}
	if technique.modelFeature != "" && !contains(request.ModelFeatures, technique.modelFeature) {
		return nil, "The model's pinned config was not read or declares no native prediction head, which this technique needs."
	}
	if technique.nativeFP8 && !nativeFP8Accelerator(request.GPU) {
		return nil, request.GPU + " is not an accelerator InferCrane knows runs FP8 in hardware (NVIDIA Ada, Hopper, or Blackwell, or AMD Instinct MI300X, MI325X, MI350X, or MI355X)."
	}
	if (technique.id == TechniqueFP8Weights || contains(technique.parts, TechniqueFP8Weights)) && quantizedModelName(control.Deployment.Model.ID) {
		return nil, "The model's weights already look quantized, so FP8 weights would not apply."
	}
	current := append([]string(nil), control.Deployment.Runtime.Args...)
	for _, part := range technique.parts {
		if _, changed := withArgs(current, techniques[part].args[engine]); !changed {
			return nil, fmt.Sprintf("The starting setup already uses %s, so the pair is %s alone.", strings.ToLower(techniques[part].label), otherPart(technique, part))
		}
	}
	if technique.id == TechniqueCUDAGraphs {
		return cudaGraphArgs(current, engine, version, request.GPU, diagnosis)
	}
	// vLLM 0.34.0's Rust front end is one multithreaded process and ignores
	// --api-server-count (entrypoints/cli/serve.py).
	if technique.id == TechniqueAPIServers && engine == "vllm" && control.Deployment.Runtime.Env["VLLM_USE_RUST_FRONTEND"] == "1" {
		return nil, "The starting setup serves through vLLM's Rust front end, which runs one multithreaded process and ignores the API server count."
	}
	if technique.id == TechniqueSpeculationOff {
		without, removed := withoutFlagValues(current, "--speculative-config", "--speculative-algorithm", "--speculative-num-steps", "--speculative-eagle-topk", "--speculative-num-draft-tokens")
		if !removed {
			return nil, "The starting setup does not speculate."
		}
		return without, ""
	}
	if technique.id == TechniqueAsyncScheduling && hasSpeculation(current) {
		return nil, fmt.Sprintf("The starting setup speculates, and its speculation method decides whether %s schedules ahead, so this cannot be changed on its own.", engineName(engine))
	}
	if technique.defaultOn[engine] {
		for _, flag := range technique.disables[engine] {
			if contains(current, flag) {
				return withoutFlags(current, technique.disables[engine]), ""
			}
		}
		return nil, fmt.Sprintf("%s already runs with this on by default, and the starting setup keeps it on.", engineName(engine))
	}
	args := technique.args[engine]
	if sized := technique.sizedArgs[engine]; sized != nil {
		class, known := acceleratorMemoryClass(request.GPU, version)
		if !known {
			return nil, fmt.Sprintf("InferCrane does not know %s's memory, so it cannot tell %s's default batch limits.", request.GPU, engineName(engine))
		}
		args = sized[class]
	}
	if technique.id == TechniqueBatchBudget {
		var why string
		if args, why = capBatchToCache(args, current, engine, version, request.GPU, diagnosis); why != "" {
			return nil, why
		}
	}
	if len(args) == 0 {
		return nil, fmt.Sprintf("No reviewed %s flag applies this technique yet.", engineName(engine))
	}
	if technique.id == TechniqueKVSequenceCap {
		running, ok := diagnosis.signal(optimizationreadiness.SignalRunningPerReplica)
		if !ok {
			return nil, "This needs the engine's count of running requests per server, which the diagnosis window did not report."
		}
		limit, lowers := sequenceCap(running)
		if !lowers {
			return nil, "Too few requests ran at once for a lower cap to change anything."
		}
		// The cap sits below the observed concurrency, which never exceeds the
		// engine's limit, so it always changes the setup.
		args = []runtimeArg{{args[0].flag, strconv.Itoa(limit)}}
	} else {
		var why string
		if args, why = withoutNoOps(engine, version, request.GPU, args); why != "" {
			return nil, why
		}
	}
	derived, changed := withArgs(current, args)
	if !changed {
		return nil, "The starting setup already uses this change."
	}
	return derived, ""
}

// deriveEnv returns the starting setup's engine environment with the
// technique's switches set, or why they cannot be set. Each switch must be
// reviewed for the engine, its version, and the accelerator's platform
// (support.EngineEnvironment), and a switch already at the value the setup
// runs with, its own setting or else the engine's recorded default, is a
// no-op that is not planned.
func deriveEnv(control Candidate, technique technique, request Request) (map[string]string, string) {
	engine, version := control.Deployment.Runtime.Engine, control.Deployment.Runtime.Version
	if _, builtIn := reviewedEngineVersions[engine]; !builtIn || !control.Deployment.Runtime.Workload.Empty() {
		return nil, "The starting setup runs a pinned custom image; InferCrane changes its settings only through setups reviewed for that image."
	}
	if !control.Deployment.Serving.Empty() {
		return nil, "The starting setup is a Dynamo serving graph; its runtime changes go through the serving topology, not engine settings."
	}
	variables := technique.env[engine]
	if len(variables) == 0 {
		return nil, fmt.Sprintf("No reviewed %s setting applies this technique yet.", engineName(engine))
	}
	accelerator, reviewed := provideridentity.LookupAccelerator(request.GPU)
	if !reviewed {
		return nil, request.GPU + " is not an accelerator InferCrane has reviewed this setting for."
	}
	env := maps.Clone(control.Deployment.Runtime.Env)
	if env == nil {
		env = map[string]string{}
	}
	changed, setByControl := false, false
	for _, variable := range variables {
		reviewedVariable, ok := support.LookupEngineEnvironment(engine, version, accelerator.Platform, variable.name)
		if !ok {
			return nil, fmt.Sprintf("This setting is not reviewed for %s %s on %s.", engineName(engine), version, request.GPU)
		}
		current, set := env[variable.name]
		if !set {
			current = reviewedVariable.Default
		}
		if current == variable.value {
			setByControl = setByControl || set
			continue
		}
		env[variable.name] = variable.value
		changed = true
	}
	switch {
	case changed:
		return env, ""
	case setByControl:
		return nil, "The starting setup already uses this change."
	default:
		return nil, fmt.Sprintf("This is already %s %s's default on %s, so the setup would equal the starting setup.", engineName(engine), version, request.GPU)
	}
}

func engineName(engine string) string {
	switch engine {
	case "vllm":
		return "vLLM"
	case "sglang":
		return "SGLang"
	default:
		return engine
	}
}

// withArgs sets each flag to its value, replacing an existing value in place
// and appending a missing flag. It reports whether anything changed.
func withArgs(current []string, args []runtimeArg) ([]string, bool) {
	out := append([]string(nil), current...)
	changed := false
	for _, arg := range args {
		index := -1
		for position, value := range out {
			if value == arg.flag {
				index = position
			}
		}
		switch {
		case index < 0:
			out = append(out, arg.flag)
			if arg.value != "" {
				out = append(out, arg.value)
			}
			changed = true
		case arg.value != "" && index+1 < len(out) && !strings.HasPrefix(out[index+1], "--"):
			if out[index+1] != arg.value {
				out[index+1] = arg.value
				changed = true
			}
		case arg.value != "":
			out = append(out[:index+1], append([]string{arg.value}, out[index+1:]...)...)
			changed = true
		}
	}
	return out, changed
}

func withoutFlags(current, flags []string) []string {
	out := make([]string, 0, len(current))
	for _, value := range current {
		if !contains(flags, value) {
			out = append(out, value)
		}
	}
	return out
}

// diagnosisCandidate copies the starting setup and applies one technique's
// flags. The copy keeps every required evidence item of the starting setup.
func diagnosisCandidate(control Candidate, technique technique, args []string, env map[string]string, request Request) (Candidate, error) {
	return deriveCandidate(control, technique, args, env, request, nil)
}

// deriveCandidate builds a setup from the starting setup with technique's
// change; adjust, when given, finishes it before its identity is computed.
func deriveCandidate(control Candidate, technique technique, args []string, env map[string]string, request Request, adjust func(*Candidate)) (Candidate, error) {
	encoded, err := json.Marshal(control)
	if err != nil {
		return Candidate{}, err
	}
	var candidate Candidate
	if err = json.Unmarshal(encoded, &candidate); err != nil {
		return Candidate{}, err
	}
	engine := control.Deployment.Runtime.Engine
	candidate.ID, candidate.Rank, candidate.Reason = "", 0, nil
	candidate.Status, candidate.EvidenceState, candidate.ModeledEvidence = "proposed-unmeasured", EvidenceUnmeasured, nil
	candidate.ConfigurationProfile = control.ConfigurationProfile + "-" + technique.id
	candidate.Source = SourceInfo{Name: diagnosisPlannerSource, Version: DiagnosisRulesVersion, EvidenceClass: "configuration-unverified", UpstreamProject: engine}
	candidate.Deployment.Name = safeName(control.Deployment.Name + "-" + technique.id)
	candidate.Deployment.Runtime.Args = args
	if env != nil {
		candidate.Deployment.Runtime.Env = env
	}
	// The copy is no longer the untuned baseline, so baseline-only claims go.
	features := []Feature{}
	for _, feature := range candidate.Features {
		if feature.Name != "runtime_baseline" {
			features = append(features, feature)
		}
	}
	candidate.Features = append(features, Feature{Name: "diagnosis_technique", State: technique.id, Source: featureSource(request)})
	limitations := []string{}
	for _, limitation := range candidate.Limitations {
		if limitation != genericBaselineLimitation {
			limitations = append(limitations, limitation)
		}
	}
	if technique.env != nil {
		candidate.Limitations = append(limitations, "This setup changes engine settings the endpoint's diagnosis chose; the diagnosis explains why it is worth measuring, not that it is faster.")
	} else {
		candidate.Limitations = append(limitations, "This setup changes runtime flags the endpoint's diagnosis chose; the diagnosis explains why it is worth measuring, not that it is faster.")
	}
	candidate.RequiredEvidence = append(candidate.RequiredEvidence, "paired measurement against the starting setup on the same traffic")
	if technique.changesPrecision {
		candidate.RequiredEvidence = append(candidate.RequiredEvidence, "answer quality check against the starting setup because this setup changes numeric precision")
	}
	if technique.speculative {
		candidate.RequiredEvidence = append(candidate.RequiredEvidence, "output parity and speculative acceptance evidence")
	}
	if technique.changesKernels {
		candidate.RequiredEvidence = append(candidate.RequiredEvidence, "answer quality check against the starting setup because this setup swaps the GPU kernels that compute each layer")
	}
	if technique.changesAnswers() {
		candidate.RequiredEvidence = append(candidate.RequiredEvidence, TaskScoreEvidence)
	}
	if adjust != nil {
		adjust(&candidate)
	}
	identity, err := json.Marshal(struct {
		Source     SourceInfo      `json:"source"`
		Deployment DeploymentDraft `json:"deployment"`
		Profile    string          `json:"profile"`
		Technique  string          `json:"technique"`
		Workload   string          `json:"workload"`
		Control    string          `json:"control"`
	}{candidate.Source, candidate.Deployment, candidate.ConfigurationProfile, technique.id, request.WorkloadProfile, control.ID})
	if err != nil {
		return Candidate{}, err
	}
	sum := sha256.Sum256(identity)
	candidate.ID = hex.EncodeToString(sum[:])
	return candidate, nil
}

// precisionOf says whether a starting setup already serves quantized weights
// (by the model's name or a --quantization flag) or an FP8 KV cache.
func precisionOf(model string, args []string) (weights, cache bool) {
	weights = quantizedModelName(model)
	for index, arg := range args {
		flag, value, inline := strings.Cut(arg, "=")
		if !inline && index+1 < len(args) {
			value = args[index+1]
		}
		switch flag {
		case "--quantization":
			weights = weights || value != ""
		case "--kv-cache-dtype":
			cache = cache || strings.HasPrefix(strings.ToLower(value), "fp8")
		}
	}
	return weights, cache
}

// otherPart names the part of a combined change that is not `part`.
func otherPart(technique technique, part string) string {
	for _, other := range technique.parts {
		if other != part {
			return strings.ToLower(techniques[other].label)
		}
	}
	return ""
}

// featureSource names where a planned change came from: the diagnosed
// endpoint, or the request itself for a technique no diagnosis chose.
func featureSource(request Request) string {
	if request.Diagnosis != nil {
		return request.Diagnosis.Endpoint
	}
	return "request"
}
