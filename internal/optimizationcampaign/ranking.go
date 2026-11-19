package optimizationcampaign

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/optimizer"
)

const (
	RankSelect       = "SELECT"
	RankSupersede    = "SUPERSEDE"
	RankReject       = "REJECT"
	RankInconclusive = "INCONCLUSIVE"
)

// RankingAlgorithmVersion is appended to the Lab algorithm version on every
// persisted campaign ranking. It names the control and noise-floor gate; v3
// adds the failed-request and answer-length guards.
const RankingAlgorithmVersion = "campaign-control-noise-floor-v3"

// FailureNoMeasurableDifference marks a candidate whose objective result is
// inside the measurement noise of the campaign control. It is reported as no
// measurable difference, never as a win.
const FailureNoMeasurableDifference = "measured_no_measurable_difference"

// MeasuredRanking is a deterministic, side-effect-free ranking result. The
// caller persists Evaluation before allowing the coordinator to cross the
// ranking boundary. FailureCodes overrides the default code for a decision.
type MeasuredRanking struct {
	Evaluation   domain.LabEvaluation
	Decisions    map[string]string
	Reasons      map[string]string
	FailureCodes map[string]string
}

// RankMeasuredCampaign composes the existing exact-workload Inference Lab
// evaluator with durable campaign identities. Proposal rank is deliberately
// ignored once measurements exist.
func RankMeasuredCampaign(campaign domain.OptimizationCampaign, benchmarks []domain.BenchmarkResult) (MeasuredRanking, error) {
	var proposal optimizer.Proposal
	if err := json.Unmarshal([]byte(campaign.ProposalJSON), &proposal); err != nil {
		return MeasuredRanking{}, fmt.Errorf("decode immutable campaign proposal: %w", err)
	}
	if err := optimizer.ValidateProposal(proposal); err != nil {
		return MeasuredRanking{}, fmt.Errorf("validate immutable campaign proposal: %w", err)
	}
	if campaign.ModelIdentity != proposal.Input.ModelIdentity || campaign.Objective != proposal.Input.Objective || campaign.InputDigest != proposal.InputDigest {
		return MeasuredRanking{}, errors.New("campaign identity does not match its immutable proposal")
	}
	lineage, err := optimizer.ProposalLineage(proposal)
	if err != nil {
		return MeasuredRanking{}, err
	}

	byBenchmark := make(map[string]domain.BenchmarkResult, len(benchmarks))
	for _, row := range benchmarks {
		if row.ID != "" {
			byBenchmark[row.ID] = row
		}
	}
	selectedEvidence := make([]domain.BenchmarkResult, 0, len(campaign.Candidates))
	evidenceToCandidate := make(map[string]string, len(campaign.Candidates))
	decisions := make(map[string]string, len(campaign.Candidates))
	reasons := make(map[string]string, len(campaign.Candidates))
	derivedLineage := map[string]string{}
	for _, candidate := range campaign.Candidates {
		if !atMeasuredRankingBarrier(candidate.State) {
			return MeasuredRanking{}, fmt.Errorf("candidate %s has not reached the measured ranking barrier", candidate.ID)
		}
		// A candidate rejected by quality before ranking has no Lab identity and
		// cannot participate. A candidate already rejected/inconclusive by this
		// ranker remains in subsequent evaluations so processing order cannot
		// turn unlike workloads into a false winner.
		if (candidate.State == CandidateRejected || candidate.State == CandidateInconclusive || candidate.State == CandidateCleaned) && candidate.LabEvaluationID == "" {
			continue
		}
		if candidate.BenchmarkID == "" || candidate.QualityEvidenceID == "" || candidate.RevisionID == "" {
			return MeasuredRanking{}, fmt.Errorf("candidate %s lacks benchmark, revision, or quality evidence", candidate.ID)
		}
		row, ok := byBenchmark[candidate.BenchmarkID]
		if !ok {
			return MeasuredRanking{}, fmt.Errorf("candidate %s benchmark evidence is unavailable", candidate.ID)
		}
		// Each row must record the exact artifact its own proposal candidate
		// serves, so a derived artifact's evidence cannot stand in for the
		// base model's or the reverse.
		artifact, ok := lineage.Candidates[candidate.ProposalCandidateID]
		if !ok || row.RevisionID != candidate.RevisionID || row.ModelIdentity != artifact.Identity {
			return MeasuredRanking{}, fmt.Errorf("candidate %s benchmark identity does not match campaign and revision", candidate.ID)
		}
		selectedEvidence = append(selectedEvidence, row)
		evidenceToCandidate[row.ID] = candidate.ID
		if artifact.Lineage != optimizer.LineageBase {
			derivedLineage[candidate.ID] = artifact.Lineage
		}
	}
	if len(selectedEvidence) == 0 {
		return MeasuredRanking{}, errors.New("campaign has no measured candidates to rank")
	}

	evaluation, err := lab.Evaluate(lab.Input{
		ModelIdentity:          lineage.Base,
		DerivedModelIdentities: lineage.Derived(),
		Objective:              proposal.Input.Objective,
		WorkloadProfile:        proposal.Input.WorkloadProfile,
		MaxTTFTP95MS:           proposal.Input.MaxTTFTP95MS,
		MaxTPOTP95MS:           proposal.Input.MaxTPOTP95MS,
		MaxErrorRate:           proposal.Input.MaxErrorRate,
		MinGoodput:             proposal.Input.MinGoodput,
		MinOutputTPS:           proposal.Input.MinOutputTokensSecond,
		MaxHourlyCost:          proposal.Input.MaxHourlyCost,
		Region:                 proposal.Input.Region,
	}, selectedEvidence)
	if err != nil {
		return MeasuredRanking{}, err
	}
	var rows []lab.Candidate
	if err = json.Unmarshal([]byte(evaluation.ResultsJSON), &rows); err != nil {
		return MeasuredRanking{}, fmt.Errorf("decode deterministic Lab result: %w", err)
	}
	if proposal.Input.WorkloadSource == optimizer.WorkloadSourcePublicPrior {
		for _, candidateID := range evidenceToCandidate {
			decisions[candidateID] = RankInconclusive
			reasons[candidateID] = "public workload prior is screening evidence; representative customer replay is required before qualification or promotion"
		}
		return MeasuredRanking{Evaluation: evaluation, Decisions: decisions, Reasons: reasons}, nil
	}
	selectedCandidate := ""
	allComparable, allFailedConstraints := len(rows) > 0, len(rows) > 0
	for _, row := range rows {
		candidateID := evidenceToCandidate[row.EvidenceID]
		if candidateID == "" {
			return MeasuredRanking{}, errors.New("Lab returned evidence outside the bounded campaign")
		}
		if row.Selected {
			if selectedCandidate != "" {
				return MeasuredRanking{}, errors.New("Lab selected more than one candidate")
			}
			selectedCandidate = candidateID
		}
		allComparable = allComparable && row.Comparable
		allFailedConstraints = allFailedConstraints && row.MeetsSLO != nil && !*row.MeetsSLO
		if row.SelectionReason != "" {
			reasons[candidateID] = row.SelectionReason
		} else if len(row.ConstraintReasons) > 0 {
			reasons[candidateID] = row.ConstraintReasons[0]
		}
	}
	gate := applyControlGate(campaign, proposal.Input.Objective, rows, evidenceToCandidate, byBenchmark, selectedCandidate)
	selectedCandidate = gate.selected
	for candidateID, reason := range gate.reasons {
		reasons[candidateID] = reason
	}
	// With the control outside the constraints, a guard can leave nothing to
	// select. Every candidate is then rejected, as when all miss constraints.
	if selectedCandidate == "" && len(gate.held) > 0 {
		allFailedConstraints = true
		for _, row := range rows {
			allFailedConstraints = allFailedConstraints && (gate.held[evidenceToCandidate[row.EvidenceID]] || row.MeetsSLO != nil && !*row.MeetsSLO)
		}
	}
	for _, candidate := range campaign.Candidates {
		if (candidate.State == CandidateRejected || candidate.State == CandidateInconclusive || candidate.State == CandidateCleaned) && candidate.LabEvaluationID == "" {
			continue
		}
		switch {
		case selectedCandidate != "" && candidate.ID == selectedCandidate:
			decisions[candidate.ID] = RankSelect
			if gate.reasons[candidate.ID] == "" {
				reasons[candidate.ID] = "best measured exact-workload candidate satisfying the requested objective and constraints"
			}
		case selectedCandidate != "":
			decisions[candidate.ID] = RankSupersede
			if reasons[candidate.ID] == "" {
				reasons[candidate.ID] = "another measured exact-workload candidate ranked higher"
			}
		case gate.held[candidate.ID]:
			// A guard kept it from selection and nothing else qualified.
			decisions[candidate.ID] = RankReject
		case allComparable && allFailedConstraints:
			decisions[candidate.ID] = RankReject
			if reasons[candidate.ID] == "" {
				reasons[candidate.ID] = "measured candidate violates the requested SLO or cost constraints"
			}
		default:
			decisions[candidate.ID] = RankInconclusive
			if reasons[candidate.ID] == "" {
				reasons[candidate.ID] = "measured candidates are not safely comparable or required evidence is unavailable"
			}
		}
	}
	annotated, err := annotateEvaluation(evaluation, rows, evidenceToCandidate, derivedLineage, gate, decisions, reasons)
	if err != nil {
		return MeasuredRanking{}, err
	}
	return MeasuredRanking{Evaluation: annotated, Decisions: decisions, Reasons: reasons, FailureCodes: gate.codes}, nil
}

// atMeasuredRankingBarrier reports whether a candidate's measured evidence is
// final for ranking. A winner ranked before its peers has already moved on:
// to qualified and then promoted for a new endpoint, or to guarding for an
// evolution. It stays at the barrier, so ranking a later peer replays the same
// evaluation instead of waiting forever.
func atMeasuredRankingBarrier(state string) bool {
	switch state {
	case CandidateRanked, CandidateQualified, CandidateGuarding, CandidateGuardPassed, CandidatePromoted, CandidateObserved, CandidateRejected, CandidateInconclusive, CandidateCleaned:
		return true
	default:
		return false
	}
}
