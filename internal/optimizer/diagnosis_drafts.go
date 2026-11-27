package optimizer

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/speculator"
)

// RuleSuppliedDrafts fires when the plan's input names a draft for the exact
// model: the drafts are measured beside the speculation the other rules
// chose, so the judge, not a prior, says which one serves faster.
const RuleSuppliedDrafts = "drafts.supplied"

// Planning priors for supplied drafts. A draft trained on the customer's
// own traffic is measured right after the strongest speculation choice of the
// other rules, a published draft and the model's own prediction head next,
// and no speculation after them when the starting setup speculates. Without
// a speculation choice the drafts follow the changes the diagnosis did point
// at. They choose order only; the judge decides.
const (
	suppliedDraftPriority = 60
	maxSuppliedDrafts     = 2
)

// SignalDraftTokensPerStep is the offline estimate of tokens a verify step
// writes with a trained draft (speculator.Training.ExpectedTokensPerStep).
const SignalDraftTokensPerStep = "draft_expected_tokens_per_step"

var noDraftWhy = map[string]string{
	speculator.SourcePublished: "InferCrane has no reviewed draft model paired with this model yet, so no setup was added.",
	speculator.SourceTrained:   "No draft trained on your traffic was named for this model, so no setup was added. Train one in your own account and name it in speculative_drafts.",
}

// suppliedDrafts indexes the plan input's drafts by source; validation keeps
// at most one per source.
func suppliedDrafts(request Request) map[string]speculator.Draft {
	if len(request.SpeculativeDrafts) == 0 {
		return nil
	}
	out := make(map[string]speculator.Draft, len(request.SpeculativeDrafts))
	for _, draft := range request.SpeculativeDrafts {
		out[draft.Source] = draft
	}
	return out
}

func normalizeDrafts(drafts []speculator.Draft) []speculator.Draft {
	if len(drafts) == 0 {
		return nil
	}
	out := make([]speculator.Draft, len(drafts))
	for index, draft := range drafts {
		out[index] = draft.Normalize()
	}
	return out
}

// validateDrafts checks the plan input's drafts: at most one trained and one
// published, each valid and trained against exactly the requested model and
// commit, planned from an endpoint diagnosis.
func validateDrafts(request Request) error {
	if len(request.SpeculativeDrafts) == 0 {
		return nil
	}
	if request.Diagnosis == nil {
		return errors.New("speculative_drafts are planned from an endpoint diagnosis; set diagnosis_endpoint")
	}
	if len(request.SpeculativeDrafts) > maxSuppliedDrafts {
		return fmt.Errorf("at most %d speculative_drafts are allowed: one trained, one published", maxSuppliedDrafts)
	}
	model, _, _ := splitPinnedModel(request.ModelIdentity)
	if model == "" {
		model = request.ModelIdentity
	}
	seen := map[string]bool{}
	for _, draft := range request.SpeculativeDrafts {
		if err := draft.Validate(); err != nil {
			return err
		}
		if seen[draft.Source] {
			return fmt.Errorf("speculative_drafts holds two %s drafts; name one", draft.Source)
		}
		seen[draft.Source] = true
		if request.ModelRevision == "" || !strings.EqualFold(draft.BaseModel, model) || draft.BaseRevision != request.ModelRevision {
			return fmt.Errorf("draft %q was trained against %s at %s; a draft reads that model's hidden layers, so the plan's model must be exactly that commit", draft.Name, draft.BaseModel, draft.BaseRevision)
		}
	}
	return nil
}

// suppliedDraftOutcome orders the supplied drafts among the other rules'
// speculation choices and adds the setups each draft is compared with.
func suppliedDraftOutcome(outcomes []ruleOutcome, facts planContext) *ruleOutcome {
	trained, hasTrained := facts.supplied[speculator.SourceTrained]
	published, hasPublished := facts.supplied[speculator.SourcePublished]
	if !hasTrained && !hasPublished {
		return nil
	}
	outcome := ruleOutcome{rule: RuleSuppliedDrafts}
	top, chosen := 0, map[string]bool{}
	for _, outcome := range outcomes {
		for _, pick := range outcome.picks {
			chosen[pick.technique] = true
			if candidate := techniques[pick.technique]; candidate.speculative && candidate.draft == "" && pick.technique != TechniqueSpeculationTune {
				top = max(top, pick.priority)
			}
		}
	}
	// A comparison setup another rule already chose keeps that rule's
	// priority; picking it again here would count as corroboration and lift
	// it above the drafts it is compared with.
	compare := func(pick techniquePick) {
		if !chosen[pick.technique] {
			outcome.picks = append(outcome.picks, pick)
		}
	}
	base, context := suppliedDraftPriority, "the diagnosis did not point at speculation, so it is measured after the changes it did point at"
	if top > 0 {
		base, context = min(100, top+1), "speculation was chosen for this load, so it is measured with the speculation setups"
	}
	if hasTrained {
		why := fmt.Sprintf("A %s was named for this exact model; %s.", speculator.Describe(trained), context)
		if trained.Training != nil {
			if expected, ok := trained.Training.ExpectedTokensPerStep(trained.DraftTokens); ok {
				outcome.evidence = append(outcome.evidence, ReasonEvidence{Signal: SignalDraftTokensPerStep, Observed: expected, Unit: "tokens"})
				why += fmt.Sprintf(" On %s held-out prompts its tokens matched the model's for about %s tokens a step, an offline estimate the campaign measures.", numberWords(float64(trained.Training.HeldOutExamples)), oneDecimal(expected))
			}
		}
		outcome.picks = append(outcome.picks, techniquePick{TechniqueSpeculativeTrainedDraft, base, why})
	}
	compared := "it is measured beside the draft trained on your traffic, so the judge compares the two on the same traffic."
	if !hasTrained {
		compared = context + "."
	}
	if hasPublished {
		compare(techniquePick{TechniqueSpeculativeDraft, base - 2, fmt.Sprintf("A %s was named for this exact model; %s", speculator.Describe(published), compared)})
	}
	compare(techniquePick{TechniqueSpeculativeMTP, base - 4, "The model's own prediction head needs no draft; " + compared})
	if facts.drafts > 0 {
		compare(techniquePick{TechniqueSpeculationOff, base - 6, "The starting setup already speculates; serving without speculation shows what any draft is worth here, and " + compared})
	}
	return &outcome
}

// draftArgs derives a setup that serves the named published draft instead of
// whatever speculation the starting setup had.
func draftArgs(current []string, engine, version string, technique technique, request Request) ([]string, string) {
	// A trained draft is served only through its technique, which serves it
	// only where it is pinned (technique_path.go): never from here.
	if technique.draft == speculator.SourceTrained {
		return nil, "A draft trained on your traffic is served only through the trained-draft technique, where it is pinned."
	}
	draft, supplied := suppliedDrafts(request)[technique.draft]
	if !supplied {
		return nil, noDraftWhy[technique.draft]
	}
	if pin := draft.Pinned; pin != nil && !draft.Matches(engine, version, request.GPU) {
		return nil, fmt.Sprintf("The draft is pinned to %s %s on %s and this setup runs %s %s on %s, so it serves without the draft until the draft is qualified there.", pin.Engine, pin.EngineVersion, pin.Accelerator, engine, version, request.GPU)
	}
	flags, err := speculator.RuntimeArgs(engine, draft)
	if err != nil {
		if errors.Is(err, speculator.ErrNotServable) {
			why := strings.TrimPrefix(err.Error(), speculator.ErrNotServable.Error()+": ")
			return nil, strings.ToUpper(why[:1]) + why[1:] + "."
		}
		return nil, err.Error()
	}
	without, _ := withoutFlagValues(current, speculator.SpeculativeFlags...)
	args := make([]runtimeArg, 0, len(flags)/2)
	for index := 0; index+1 < len(flags); index += 2 {
		args = append(args, runtimeArg{flags[index], flags[index+1]})
	}
	derived, _ := withArgs(without, args)
	if slices.Equal(derived, current) {
		return nil, "The starting setup already serves this draft."
	}
	return derived, ""
}
