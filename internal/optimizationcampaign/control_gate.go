package optimizationcampaign

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/lab"
)

const (
	controlDecisionControl = "control"
	controlDecisionUnknown = "unknown"

	basisInterleaved = "interleaved_control"
	basisIntervals   = "confidence_intervals"
)

// ControlAssessment is the objective-metric verdict for one candidate against
// the campaign control. It is persisted beside each Lab row.
type ControlAssessment struct {
	Control    bool              `json:"control"`
	Metric     string            `json:"objective_metric,omitempty"`
	Decision   string            `json:"control_decision"`
	Basis      string            `json:"control_basis,omitempty"`
	Comparison *MetricComparison `json:"objective_comparison,omitempty"`
	Reason     string            `json:"control_reason,omitempty"`
}

type controlGate struct {
	selected    string
	reasons     map[string]string
	codes       map[string]string
	assessments map[string]ControlAssessment
}

type rankingWorkload struct {
	Confidence map[string]benchmark.ConfidenceInterval `json:"confidence_95"`
	Measured   *MeasuredEvidence                       `json:"measured"`
}

type rankingCost struct {
	Available bool     `json:"available"`
	Hourly    *float64 `json:"hourly"`
	Source    string   `json:"source"`
}

// applyControlGate selects the best eligible candidate that measurably beats
// the control on the objective metric, or the control when none does. Lab's
// choice stands unchanged only when no eligible control was measured.
// Candidates inside the noise floor, or never shown to beat the control, are
// reported as no measurable difference. The control's role comes from the
// proposal, but a candidate displaces it only on evidence.
func applyControlGate(campaign domain.OptimizationCampaign, objective string, rows []lab.Candidate, evidenceToCandidate map[string]string, benchmarks map[string]domain.BenchmarkResult, labSelected string) controlGate {
	gate := controlGate{selected: labSelected, reasons: map[string]string{}, codes: map[string]string{}, assessments: map[string]ControlAssessment{}}
	metric, higherBetter, costScaled := objectiveMetric(objective)
	control, hasControl := campaignControl(campaign)
	byCandidate := map[string]lab.Candidate{}
	for _, row := range rows {
		byCandidate[evidenceToCandidate[row.EvidenceID]] = row
	}
	controlRow, controlMeasured := byCandidate[control.ID]
	controlMeasured = hasControl && controlMeasured
	for _, row := range rows {
		candidateID := evidenceToCandidate[row.EvidenceID]
		gate.assessments[candidateID] = assessAgainstControl(candidateID, row, control.ID, controlRow, controlMeasured, metric, higherBetter, costScaled, benchmarks)
	}
	if labSelected == "" {
		// Nothing is selected, so Lab's comparability and constraint reasons
		// stand; the assessments are still persisted for audit.
		return gate
	}
	if !controlMeasured || !eligible(controlRow) {
		if controlMeasured && labSelected != control.ID {
			gate.reasons[labSelected] = "best measured exact-workload candidate; the control does not satisfy the requested constraints, so no performance gain over it is claimed"
		}
		return gate
	}
	// Lab returns comparable rows best first. The winner is the best eligible
	// candidate that measurably beats the control; when none does, the
	// control stays. A noisy point-estimate leader therefore cannot hide a
	// candidate that won in its own window, and a cross-window point estimate
	// cannot keep the control over one.
	gate.selected = control.ID
	for _, row := range rows {
		candidateID := evidenceToCandidate[row.EvidenceID]
		if candidateID != control.ID && eligible(row) && gate.assessments[candidateID].Decision == DecisionImproved {
			gate.selected = candidateID
			break
		}
	}
	for _, row := range rows {
		candidateID := evidenceToCandidate[row.EvidenceID]
		assessment := gate.assessments[candidateID]
		switch {
		case candidateID == gate.selected && candidateID == control.ID:
			gate.reasons[candidateID] = "control kept: no candidate beat it by more than the measurement noise"
		case candidateID == gate.selected:
			gate.reasons[candidateID] = "measurably better than the control: " + assessment.Reason
		case !eligible(row):
			// Lab's constraint or comparability reason stands.
		case candidateID == control.ID:
			gate.reasons[candidateID] = "a candidate measurably beat the control"
		case assessment.Decision == DecisionImproved:
			gate.reasons[candidateID] = "measurably better than the control, but a better-ranked candidate also was: " + assessment.Reason
		case assessment.Decision == DecisionRegressed:
			gate.reasons[candidateID] = "worse than the control: " + assessment.Reason
		case assessment.Decision == DecisionNoMeasurableDifference:
			gate.codes[candidateID] = FailureNoMeasurableDifference
			gate.reasons[candidateID] = "no measurable difference from the control: " + assessment.Reason
		default:
			gate.codes[candidateID] = FailureNoMeasurableDifference
			gate.reasons[candidateID] = "not shown to beat the control: " + assessment.Reason
		}
	}
	return gate
}

// eligible mirrors Lab's selection rule: comparable, with an objective value,
// and not outside a declared constraint.
func eligible(row lab.Candidate) bool {
	return row.Comparable && row.ObjectiveValue != nil && (row.MeetsSLO == nil || *row.MeetsSLO)
}

// assessAgainstControl prefers the interleaved comparison measured in the
// candidate's own window. An invalid interleaved comparison stays unknown. A
// candidate measured without a live control is compared through both arms'
// 95% intervals, which is weaker because the arms ran at different times;
// overlap is no measurable difference.
func assessAgainstControl(candidateID string, row lab.Candidate, controlID string, controlRow lab.Candidate, controlMeasured bool, metric string, higherBetter, costScaled bool, benchmarks map[string]domain.BenchmarkResult) ControlAssessment {
	assessment := ControlAssessment{Metric: metric, Decision: controlDecisionUnknown}
	if candidateID == controlID && controlID != "" {
		assessment.Control, assessment.Decision = true, controlDecisionControl
		return assessment
	}
	if metric == "" {
		assessment.Reason = "the objective has no comparable metric"
		return assessment
	}
	if !controlMeasured {
		assessment.Reason = "no control was measured for this campaign"
		return assessment
	}
	workload := decodeRankingWorkload(benchmarks[row.EvidenceID].WorkloadJSON)
	ratio := 1.0
	if costScaled {
		candidateCost, controlCost := sourcedHourly(row.Cost), sourcedHourly(controlRow.Cost)
		if candidateCost == nil || controlCost == nil {
			assessment.Reason = "sourced hourly cost is unavailable for the candidate or the control"
			return assessment
		}
		ratio = *controlCost / *candidateCost
	}
	if workload.Measured != nil && workload.Measured.Control.Role == ControlRoleBracketed && workload.Measured.Control.CandidateID == controlID {
		comparison, found := workload.Measured.Comparison[metric]
		if !found {
			assessment.Reason = metric + " was not comparable between the interleaved runs"
			return assessment
		}
		if costScaled {
			comparison.Delta = (1+comparison.Delta)*ratio - 1
			comparison.Decision = DecisionNoMeasurableDifference
			if math.Abs(comparison.Delta) > comparison.NoiseFloor {
				comparison.Decision = DecisionRegressed
				if comparison.Delta > 0 {
					comparison.Decision = DecisionImproved
				}
			}
		}
		assessment.Basis, assessment.Comparison, assessment.Decision = basisInterleaved, &comparison, comparison.Decision
		assessment.Reason = fmt.Sprintf("%s changed %+.1f%% against a %.1f%% noise floor in interleaved runs", objectiveLabel(metric, costScaled), comparison.Delta*100, comparison.NoiseFloor*100)
		return assessment
	}
	if workload.Measured != nil && workload.Measured.Control.Role == ControlRoleInvalid && workload.Measured.Control.CandidateID == controlID {
		// The control ran alongside but its runs drifted or failed. That window
		// is unusable, and a comparison across windows cannot replace it.
		assessment.Reason = "the interleaved comparison was invalid (" + workload.Measured.Control.Reason + ")"
		return assessment
	}
	if workload.Measured != nil && workload.Measured.Control.Reason != "" {
		assessment.Reason = "the control was not measured alongside it (" + workload.Measured.Control.Reason + ")"
	} else {
		assessment.Reason = "the control was not measured alongside it"
	}
	controlWorkload := decodeRankingWorkload(benchmarks[controlRow.EvidenceID].WorkloadJSON)
	candidateInterval, candidateFound := workload.Confidence[metric]
	controlInterval, controlFound := controlWorkload.Confidence[metric]
	if !candidateFound || !controlFound || candidateInterval.Samples < 2 || controlInterval.Samples < 2 {
		assessment.Reason += " and repeated-run intervals are unavailable"
		return assessment
	}
	candidateScale := 1.0
	if costScaled {
		candidateScale = ratio
	}
	lowCandidate, highCandidate := candidateInterval.Lower95*candidateScale, candidateInterval.Upper95*candidateScale
	better := lowCandidate > controlInterval.Upper95
	worse := highCandidate < controlInterval.Lower95
	if !higherBetter {
		better, worse = highCandidate < controlInterval.Lower95, lowCandidate > controlInterval.Upper95
	}
	assessment.Basis = basisIntervals
	switch {
	case better:
		assessment.Decision = DecisionImproved
		assessment.Reason += "; its 95% interval is separated from the control's"
	case worse:
		assessment.Decision = DecisionRegressed
		assessment.Reason += "; its 95% interval is worse than the control's"
	default:
		assessment.Decision = DecisionNoMeasurableDifference
		assessment.Reason += "; its 95% interval overlaps the control's"
	}
	return assessment
}

// objectiveMetric mirrors Lab's objective value: TTFT p95 for latency and
// interactive objectives, output throughput for throughput, and output
// throughput per sourced hourly cost for cost efficiency.
func objectiveMetric(objective string) (string, bool, bool) {
	switch objective {
	case lab.ObjectiveLatency, lab.ObjectiveInteractive:
		return "ttft_p95_ms", false, false
	case lab.ObjectiveThroughput:
		return "output_token_throughput", true, false
	case lab.ObjectiveCostEfficiency:
		return "output_token_throughput", true, true
	default:
		return "", false, false
	}
}

func objectiveLabel(metric string, costScaled bool) string {
	if costScaled {
		return metric + " per sourced hourly cost"
	}
	return metric
}

func decodeRankingWorkload(value string) rankingWorkload {
	var workload rankingWorkload
	_ = json.Unmarshal([]byte(value), &workload)
	return workload
}

func sourcedHourly(raw json.RawMessage) *float64 {
	var cost rankingCost
	if json.Unmarshal(raw, &cost) != nil || !cost.Available || cost.Source == "" || cost.Hourly == nil || *cost.Hourly <= 0 || math.IsNaN(*cost.Hourly) || math.IsInf(*cost.Hourly, 0) {
		return nil
	}
	return cost.Hourly
}

type annotatedRow struct {
	lab.Candidate
	CandidateID      string `json:"candidate_id"`
	CampaignDecision string `json:"campaign_decision,omitempty"`
	ControlAssessment
}

// annotateEvaluation persists the gate beside Lab's rows. Selection follows
// the campaign decision, so a reader never sees Lab's point-estimate winner
// marked selected after the gate kept the control.
func annotateEvaluation(evaluation domain.LabEvaluation, rows []lab.Candidate, evidenceToCandidate map[string]string, gate controlGate, decisions, reasons map[string]string) (domain.LabEvaluation, error) {
	annotated := make([]annotatedRow, 0, len(rows))
	for _, row := range rows {
		candidateID := evidenceToCandidate[row.EvidenceID]
		row.Selected = candidateID == gate.selected && decisions[candidateID] == RankSelect
		if reason := reasons[candidateID]; reason != "" && row.Comparable {
			row.SelectionReason = reason
		}
		annotated = append(annotated, annotatedRow{Candidate: row, CandidateID: candidateID, CampaignDecision: decisions[candidateID], ControlAssessment: gate.assessments[candidateID]})
	}
	encoded, err := json.Marshal(annotated)
	if err != nil {
		return domain.LabEvaluation{}, fmt.Errorf("encode campaign ranking: %w", err)
	}
	evaluation.ResultsJSON = string(encoded)
	evaluation.AlgorithmVersion = lab.AlgorithmVersion + "+" + RankingAlgorithmVersion
	return evaluation, nil
}
