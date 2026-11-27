package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
)

// taskAnswers answers a 1,000-item task set right except on the items in
// wrong, which get a wrong final line.
func taskAnswers(wrong ...[2]int) map[string]string {
	answers := map[string]string{}
	for index := range 1000 {
		answers[fmt.Sprintf("gsm8k.test.%d", index)] = fmt.Sprintf("Some working.\nAnswer: %d", index+1)
	}
	for _, span := range wrong {
		for index := span[0]; index < span[1]; index++ {
			answers[fmt.Sprintf("gsm8k.test.%d", index)] = "Some working.\nAnswer: 999"
		}
	}
	return answers
}

// An FP8 KV cache setup that gained in its first window answered the task
// set on its first servers before they were retired, and again on fresh
// servers. Each pair alone is too wide to show its answers held within a
// point; the check after the fresh window pools both pairs and shows it.
func TestAConfirmedSetupPoolsItsFirstServersAnswers(t *testing.T) {
	prompts := make([]map[string]any, 0, 1000)
	for index := range 1000 {
		gold := fmt.Sprint(index + 1)
		prompts = append(prompts, map[string]any{"id": fmt.Sprintf("gsm8k.test.%d", index), "prompt": "item with gold " + gold, "expected": []string{gold}})
	}
	body, err := json.Marshal(map[string]any{"schema": qualitycheck.EvalSetSchemaV1, "name": "task-set", "version": "1", "max_tokens": 1024, "grading": qualitycheck.GradingAnswerLine, "prompts": prompts})
	if err != nil {
		t.Fatal(err)
	}
	digest := qualitycheck.Digest(body)
	run := newQualityCampaign(t, optimizer.Request{QualityEvalSet: digest})
	run.store.evalSets = map[string]domain.QualityEvalSet{digest: {Digest: digest, Body: string(body)}}
	withArgs(t, &run, "--kv-cache-dtype", "fp8")
	asker, ok := run.driver.Quality.(FirstAnswersAsker)
	if !ok {
		t.Fatal("the built-in check does not ask a confirmed setup's first servers")
	}

	// The first servers, before the confirmation retires them.
	run.completer.answers = map[string]map[string]string{"http://control": taskAnswers([2]int{0, 25}), "http://fast": taskAnswers([2]int{100, 125})}
	first, asked, err := asker.AskFirstPair(t.Context(), run.composite.campaign, run.candidate)
	if err != nil || !asked || first.ReferenceID != "rev-control" || first.CandidateID != "rev-fast" || first.EvalSet != digest || len(first.Candidate) != 1000 {
		t.Fatalf("first pair = %+v asked=%v err=%v", first, asked, err)
	}
	if len(run.store.evidence["qwen-opt-fast"]) != 0 {
		t.Fatal("asking the first servers must record no evidence; the verdict comes after the fresh window")
	}

	// The fresh servers disagree on other items. Alone they are not shown
	// to hold answers; pooled with the first pair they are.
	run.completer.answers = map[string]map[string]string{"http://control": taskAnswers([2]int{200, 225}), "http://fast": taskAnswers([2]int{300, 325})}
	alone, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !alone.Inconclusive || alone.FailureCode != FailureQualityAnswersNotShownHeld {
		t.Fatalf("fresh pair alone = %+v err=%v", alone, err)
	}
	run.store.evidence = map[string][]domain.QualityEvidence{}
	checkpoint := MeasurementCheckpoint{Version: measurementCheckpointVersion, Method: MeasurementMethod, Identity: "sha256:identity", Confirmation: &ConfirmationState{Phase: ConfirmMeasure, FirstAnswers: &first}}
	encoded, _ := json.Marshal(checkpoint)
	run.candidate.MeasurementCheckpointJSON = string(encoded)
	pooled, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !pooled.Passed {
		t.Fatalf("pooled = %+v err=%v", pooled, err)
	}
	var distribution qualityevidence.Distribution
	if err = json.Unmarshal([]byte(run.store.evidence["qwen-opt-fast"][0].DistributionJSON), &distribution); err != nil {
		t.Fatal(err)
	}
	if distribution.TaskRule != qualitycheck.RuleHeld || distribution.TaskVerdict != qualitycheck.CodeTaskHeld || distribution.TaskServers != 2 || distribution.TaskPoolRefused != "" {
		t.Fatalf("distribution = %+v", distribution)
	}

	// A setup that keeps answers is not asked: its one pair is judged by the
	// 3-point margin, and a confirmation adds no task set.
	withArgs(t, &run, "--max-num-seqs", "128")
	if _, asked, err = asker.AskFirstPair(t.Context(), run.composite.campaign, run.candidate); err != nil || asked {
		t.Fatalf("a setup that keeps answers was asked: asked=%v err=%v", asked, err)
	}
}

type firstPairAsker struct {
	pair          qualitycheck.ServerPair
	asked         bool
	err           error
	calls         int
	store         *compositeStoreFixture
	retireExisted bool
}

func (a *firstPairAsker) Check(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun) (domain.QualityEvidence, error) {
	return domain.QualityEvidence{}, errors.New("not reached while measuring")
}

func (a *firstPairAsker) AskFirstPair(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun) (qualitycheck.ServerPair, bool, error) {
	a.calls++
	_, a.retireExisted = a.store.operations[childKey("candidate-12345678", "retire-candidat-1")]
	return a.pair, a.asked, a.err
}

func confirmingHarness(t *testing.T) recoveryHarness {
	t.Helper()
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	measured := h.candidate("candidate-12345678")
	checkpoint, _ := DecodeMeasurementCheckpoint(measured.MeasurementCheckpointJSON)
	checkpoint.Interruptions[0].Kind, checkpoint.Interruptions[0].ReplicaID = RunKindConfirmation, "replica-serving-candidate"
	checkpoint.Confirmation = &ConfirmationState{Phase: ConfirmRetireCandidate}
	encoded, _ := json.Marshal(checkpoint)
	for index := range h.store.campaign.Candidates {
		if h.store.campaign.Candidates[index].ID == measured.ID {
			h.store.campaign.Candidates[index].MeasurementCheckpointJSON = string(encoded)
		}
	}
	return h
}

// The first servers answer before the candidate's is retired, once; the
// checkpoint keeps their answers for the check after the fresh window.
func TestAConfirmationAsksTheFirstServersBeforeRetiringOne(t *testing.T) {
	h := confirmingHarness(t)
	pair := qualitycheck.ServerPair{EvalSet: "sha256:" + fmt.Sprintf("%064d", 1), ReferenceID: "revision-control", CandidateID: "revision-candidate", Reference: []float64{1, 0}, Candidate: []float64{1, 1}}
	asker := &firstPairAsker{pair: pair, asked: true, store: h.store}
	h.driver.Quality = asker

	_, err := h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	if asker.calls != 1 || asker.retireExisted {
		t.Fatalf("asked %d times, retire already queued: %v", asker.calls, asker.retireExisted)
	}
	confirmation := h.checkpoint(t).Confirmation
	if confirmation == nil || confirmation.FirstAnswers == nil || confirmation.FirstAnswers.CandidateID != "revision-candidate" || confirmation.FirstAnswersSkipped != "" {
		t.Fatalf("confirmation = %+v", confirmation)
	}
	if _, queued := h.store.operations[childKey("candidate-12345678", "retire-candidat-1")]; !queued {
		t.Fatal("the candidate's server was not retired after its answers were read")
	}
	// The retirement is pending; the next attempt does not ask again.
	_, err = h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	if asker.calls != 1 {
		t.Fatalf("asked again: %d", asker.calls)
	}
}

// Answers the first servers could not give do not stop the confirmation:
// the checkpoint says why, and the verdict rests on the fresh pair alone.
func TestAConfirmationGoesOnWhenTheFirstServersCannotAnswer(t *testing.T) {
	h := confirmingHarness(t)
	asker := &firstPairAsker{err: errors.New("reference revision: endpoint stopped answering"), store: h.store}
	h.driver.Quality = asker

	_, err := h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	confirmation := h.checkpoint(t).Confirmation
	if confirmation.FirstAnswers != nil || confirmation.FirstAnswersSkipped != "the first servers' answers could not be read: reference revision: endpoint stopped answering" {
		t.Fatalf("confirmation = %+v", confirmation)
	}
	if _, queued := h.store.operations[childKey("candidate-12345678", "retire-candidat-1")]; !queued {
		t.Fatal("the confirmation stopped at answers it could not read")
	}
}
