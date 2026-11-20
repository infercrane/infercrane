package continualoptimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// OriginStayOptimized marks an optimization run that a Stay optimized check
// proposed. A person created every other run.
const OriginStayOptimized = "stay_optimized"

// RunCostHours is the approval window a proposed run's cost is estimated for:
// the dashboard's default approval lifetime.
const RunCostHours = 1.0

// Origin is why a check proposed a run. It is stored with the run and shown
// with it: the dashboard's suggestion, the CLI, and notifications read it.
type Origin struct {
	Kind            string          `json:"kind"`
	Deployment      string          `json:"deployment"`
	DecisionID      string          `json:"decision_id"`
	DecisionDigest  string          `json:"decision_digest"`
	Triggers        []Trigger       `json:"triggers"`
	Reason          string          `json:"reason"`
	ExpectedChange  ExpectedChange  `json:"expected_change"`
	RunCost         RunCostEstimate `json:"run_cost"`
	UnknownEvidence []string        `json:"unknown_evidence,omitempty"`
	// AutoRun records whether the policy approved the run without a person.
	AutoRun AutoRunResult `json:"auto_run"`
	// Kernels lists custom kernels the check marked for re-qualification or
	// switched off because the engine changed.
	Kernels []KernelUpkeep `json:"kernels,omitempty"`
}

// ExpectedChange is what the run may change, from measured evidence only.
// Basis "unknown" means no measured evidence exists: the run measures it.
// Basis "past_runs" is the range of cost-per-token changes earlier runs on
// this endpoint measured; it is history, not a prediction.
type ExpectedChange struct {
	Basis  string   `json:"basis"`
	Metric string   `json:"metric,omitempty"`
	Low    *float64 `json:"low,omitempty"`
	High   *float64 `json:"high,omitempty"`
	Runs   int      `json:"runs,omitempty"`
	Detail string   `json:"detail"`
}

// RunCostEstimate is the spend cap the run needs to measure every setup for
// RunCostHours at today's sourced prices. USD is nil when any setup's price
// is unknown. It is an estimate for the approval form, never a charge.
type RunCostEstimate struct {
	Basis  string   `json:"basis"`
	USD    *float64 `json:"usd,omitempty"`
	Hours  float64  `json:"hours"`
	Setups int      `json:"setups"`
	Detail string   `json:"detail"`
}

// AutoRunResult says what the opt-in auto-run did with a proposed run.
type AutoRunResult struct {
	Approved bool     `json:"approved"`
	CapUSD   *float64 `json:"cap_usd,omitempty"`
	Reason   string   `json:"reason"`
}

// KernelUpkeep is one custom kernel the check changed.
type KernelUpkeep struct {
	ID            string `json:"id"`
	Operator      string `json:"operator"`
	EngineVersion string `json:"engine_version"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
}

// DecodeOrigin reads a stored origin. A run without one, or with one that
// cannot be read, has no Stay optimized origin.
func DecodeOrigin(raw string) (Origin, bool) {
	var origin Origin
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &origin) != nil || origin.Kind != OriginStayOptimized || origin.Deployment == "" {
		return Origin{}, false
	}
	return origin, true
}

var candidateDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// HypothesisFingerprint is the history identity of a planner setup. It
// matches the fingerprint campaign memory records for the same setup.
func HypothesisFingerprint(candidateID string) string {
	if candidateDigest.MatchString(candidateID) {
		return "sha256:" + candidateID
	}
	sum := sha256.Sum256([]byte(candidateID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// StartingSetupID is the planner's control: the setup every other setup is
// measured against. It is not a hypothesis.
func StartingSetupID(proposal optimizer.Proposal) string {
	if proposal.StartingPoint != nil {
		return proposal.StartingPoint.CandidateID
	}
	for _, candidate := range proposal.Candidates {
		if candidate.Reason != nil && candidate.Reason.Role == optimizer.ReasonControl {
			return candidate.ID
		}
	}
	return ""
}

// HypothesesFromProposal turns the planner's setups, except the control, into
// hypotheses. The planner has no measured gain, so the gain stays unknown;
// a diagnosis priority sets confidence, and an undiagnosed setup gets the
// neutral 0.5. costs holds each setup's estimated run cost (absent: unknown).
func HypothesesFromProposal(proposal optimizer.Proposal, costs map[string]float64) []Hypothesis {
	control := StartingSetupID(proposal)
	out := make([]Hypothesis, 0, len(proposal.Candidates))
	for _, candidate := range proposal.Candidates {
		if candidate.ID == control {
			continue
		}
		layer, description, confidence := candidate.ConfigurationProfile, candidate.ConfigurationProfile, .5
		if reason := candidate.Reason; reason != nil {
			if reason.Technique != "" {
				layer = reason.Technique
			}
			if reason.Label != "" {
				description = reason.Label
			}
			if reason.Priority > 0 {
				confidence = math.Min(1, float64(reason.Priority)/100)
			}
		}
		if layer == "" {
			layer = "setup"
		}
		if description == "" {
			description = candidate.ID
		}
		out = append(out, Hypothesis{ID: candidate.ID, Fingerprint: HypothesisFingerprint(candidate.ID), Layer: layer, Description: description, Confidence: confidence, Risk: .1, EstimatedCostUSD: costs[candidate.ID]})
	}
	return out
}

// NarrowProposal keeps the control and the selected setups, in the planner's
// order and renumbered, so the run measures only what the check chose.
func NarrowProposal(proposal optimizer.Proposal, selected []RankedHypothesis) (optimizer.Proposal, error) {
	keep := map[string]struct{}{}
	for _, item := range selected {
		keep[item.ID] = struct{}{}
	}
	control := StartingSetupID(proposal)
	narrowed := proposal
	narrowed.Candidates = nil
	for _, candidate := range proposal.Candidates {
		if _, chosen := keep[candidate.ID]; !chosen && candidate.ID != control {
			continue
		}
		candidate.Rank = len(narrowed.Candidates) + 1
		narrowed.Candidates = append(narrowed.Candidates, candidate)
	}
	if len(narrowed.Candidates) == 0 || len(narrowed.Candidates) == 1 && narrowed.Candidates[0].ID == control {
		return optimizer.Proposal{}, errors.New("no selected setup is in the proposal")
	}
	if narrowed.Diagnosis != nil {
		plan := *narrowed.Diagnosis
		plan.Techniques = append([]optimizer.PlannedTechnique(nil), plan.Techniques...)
		for index := range plan.Techniques {
			ids := plan.Techniques[index].CandidateIDs[:0:0]
			for _, id := range plan.Techniques[index].CandidateIDs {
				if _, chosen := keep[id]; chosen || id == control {
					ids = append(ids, id)
				}
			}
			plan.Techniques[index].CandidateIDs = ids
		}
		narrowed.Diagnosis = &plan
	}
	return narrowed, nil
}

// EstimateRunCost is the spend cap that covers every setup for RunCostHours
// at today's sourced prices. Approval splits a cap evenly across setups, so
// each share must cover the most expensive setup. One unknown price makes the
// estimate unknown.
func EstimateRunCost(setups int, hourly map[string]float64, ids []string) RunCostEstimate {
	estimate := RunCostEstimate{Basis: "unknown", Hours: RunCostHours, Setups: setups, Detail: "unknown: a setup has no current sourced price; set the spend cap yourself"}
	highest := 0.0
	for _, id := range ids {
		price, known := hourly[id]
		if !known || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return estimate
		}
		highest = math.Max(highest, price)
	}
	if len(ids) == 0 {
		return estimate
	}
	total := math.Ceil(highest*float64(len(ids))*RunCostHours*100) / 100
	estimate.Basis, estimate.USD = "current_prices", &total
	estimate.Detail = fmt.Sprintf("about $%.2f: an equal share for each of %d setups for %g hour at today's sourced prices", total, len(ids), RunCostHours)
	return estimate
}

// KernelChange is a kernel the check moves out of qualified or serving, or a
// serving kernel it only warns about (Change false).
type KernelChange struct {
	Kernel domain.EndpointCustomKernel
	Change bool
	Upkeep KernelUpkeep
}

// KernelUpkeepFor applies the engine pin rule to an endpoint's kernels. A
// serving kernel pinned to another release than the one the endpoint runs is
// switched off. A qualified kernel pinned to another release than the one
// InferCrane now pins needs re-qualification. A serving kernel on the running
// release keeps serving, with a warning that a rollout to the new release
// switches it off unless it is re-qualified.
func KernelUpkeepFor(kernels []domain.EndpointCustomKernel, setup Setup) []KernelChange {
	runtime := strings.ToLower(strings.TrimSpace(setup.Runtime))
	running := strings.TrimPrefix(strings.TrimSpace(setup.RuntimeVersion), "v")
	pinned, hasPinned := PinnedEngine(setup)
	var out []KernelChange
	for _, kernel := range kernels {
		if !strings.EqualFold(kernel.Engine, runtime) || kernel.State != "serving" && kernel.State != "qualified" {
			continue
		}
		upkeep := KernelUpkeep{ID: kernel.ID, Operator: kernel.Operator, EngineVersion: kernel.EngineVersion}
		switch {
		case kernel.State == "serving" && running != "" && kernel.EngineVersion != running:
			upkeep.State, upkeep.Reason = "off", fmt.Sprintf("this endpoint runs %s %s; the kernel was qualified on %s, so it is switched off", kernel.Engine, running, kernel.EngineVersion)
			out = append(out, KernelChange{Kernel: kernel, Change: true, Upkeep: upkeep})
		case hasPinned && kernel.EngineVersion != pinned.EngineVersion && kernel.State == "qualified":
			upkeep.State, upkeep.Reason = "requalification_needed", fmt.Sprintf("%s %s is the pinned engine release; re-qualify the kernel on it before it can serve", pinned.Runtime, pinned.EngineVersion)
			out = append(out, KernelChange{Kernel: kernel, Change: true, Upkeep: upkeep})
		case hasPinned && kernel.EngineVersion != pinned.EngineVersion:
			upkeep.State, upkeep.Reason = "serving", fmt.Sprintf("serves on %s %s; a setup on %s %s rolls out without it unless it is re-qualified", kernel.Engine, kernel.EngineVersion, pinned.Runtime, pinned.EngineVersion)
			out = append(out, KernelChange{Kernel: kernel, Upkeep: upkeep})
		}
	}
	return out
}

// PastRunChange is the measured cost-per-token change one earlier run on the
// endpoint found for its chosen setup, as a fraction (-0.2 is 20% cheaper).
type PastRunChange struct {
	CampaignID string
	Change     float64
	DecidedAt  time.Time
}

// ExpectedChangeFrom summarizes measured history. With none it says unknown.
func ExpectedChangeFrom(history []PastRunChange) ExpectedChange {
	if len(history) == 0 {
		return ExpectedChange{Basis: "unknown", Detail: "unknown until measured: no earlier run on this endpoint measured a change"}
	}
	low, high := history[0].Change, history[0].Change
	for _, item := range history[1:] {
		low, high = math.Min(low, item.Change), math.Max(high, item.Change)
	}
	detail := fmt.Sprintf("earlier runs on this endpoint measured cost per output token changes from %+.0f%% to %+.0f%% (%d runs); this run measures its own", low*100, high*100, len(history))
	if len(history) == 1 {
		detail = fmt.Sprintf("one earlier run on this endpoint measured a %+.0f%% cost per output token change; this run measures its own", low*100)
	}
	return ExpectedChange{Basis: "past_runs", Metric: "cost_per_output_token", Low: &low, High: &high, Runs: len(history), Detail: detail}
}

// ProposalReason is the one sentence a proposed run is shown with.
func ProposalReason(triggers []Trigger, setups int) string {
	if len(triggers) == 0 {
		return fmt.Sprintf("Stay optimized proposes measuring %d new setups against your current one.", setups)
	}
	details := make([]string, 0, len(triggers))
	for _, trigger := range triggers {
		details = append(details, trigger.Detail)
	}
	noun := "setups"
	if setups == 1 {
		noun = "setup"
	}
	return fmt.Sprintf("Stay optimized proposes measuring %d new %s against your current one because %s.", setups, noun, strings.Join(details, "; "))
}
