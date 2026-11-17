package optimizationcampaign

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// Base model revisions the reviewed checkpoint catalog matches. DeepSeek's
// publisher states the exact source commit; the Qwen build names only its base
// repository.
const (
	exactLineageModel       = "deepseek-ai/DeepSeek-V4.1-Flash"
	exactLineageRevision    = "dba1be0a40aa45a94ad051997016db3960a90277"
	declaredLineageModel    = "Qwen/Qwen3.8-27B"
	declaredLineageRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// quantizedFixture builds a real catalog proposal for model on B200, keeps its
// unmodified baseline (the control) and its first publisher checkpoint, and
// measures the checkpoint interleaved with the control. comparison is the
// checkpoint's objective result against the control.
func quantizedFixture(t *testing.T, model, revision string, comparison MetricComparison) (domain.OptimizationCampaign, []domain.BenchmarkResult, optimizer.Proposal) {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	request := optimizer.Request{ModelIdentity: model, ModelRevision: revision, Provider: "runpod-pods", GPU: "B200", Runtimes: []string{"vllm"}, Objective: "throughput", MaxCandidates: 8}
	proposal, err := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot()).Propose(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var baseline, quantized *optimizer.Candidate
	for index := range proposal.Candidates {
		candidate := &proposal.Candidates[index]
		switch {
		case candidate.ArtifactSeed == nil && baseline == nil:
			baseline = candidate
		case candidate.ArtifactSeed != nil && quantized == nil:
			quantized = candidate
		}
	}
	if baseline == nil || quantized == nil {
		t.Fatalf("catalog no longer proposes a baseline and a publisher checkpoint for %s: %+v", model, proposal.Candidates)
	}
	if baseline.Deployment.Model.ID == quantized.Deployment.Model.ID {
		t.Fatalf("checkpoint does not serve its own artifact: %+v", quantized.Deployment.Model)
	}
	first, second := *baseline, *quantized
	first.Rank, second.Rank = 1, 2
	proposal.Candidates = []optimizer.Candidate{first, second}
	proposal.StartingPoint.CandidateID = first.ID
	if err = optimizer.ValidateProposal(proposal); err != nil {
		t.Fatalf("trimmed checkpoint proposal is invalid: %v", err)
	}
	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	campaign := domain.OptimizationCampaign{ID: "campaign", TenantID: "tenant", InputDigest: proposal.InputDigest, ModelIdentity: proposal.Input.ModelIdentity, Objective: proposal.Input.Objective, ProposalJSON: string(encoded)}
	for index, candidate := range proposal.Candidates {
		suffix := string(rune('a' + index))
		campaign.Candidates = append(campaign.Candidates, domain.OptimizationCandidateRun{ID: "candidate-" + suffix, TenantID: campaign.TenantID, CampaignID: campaign.ID, ProposalCandidateID: candidate.ID, State: CandidateRanked, RevisionID: "revision-" + suffix, BenchmarkID: "benchmark-" + suffix, QualityEvidenceID: "quality-" + suffix})
	}
	workload := `{"profile":"` + proposal.Input.WorkloadProfile + `","request_count":100,"random_seed":17}`
	controlWorkload := measuredWorkload(t, workload, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleCandidate, CandidateID: "candidate-a"}}, nil)
	candidateWorkload := measuredWorkload(t, workload, MeasuredEvidence{Method: MeasurementMethod, Control: ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 4}, Comparison: map[string]MetricComparison{"output_token_throughput": comparison}}, nil)
	ttft := 200.0
	controlThroughput, candidateThroughput := comparison.ControlMean, comparison.CandidateMean
	row := func(id, revision string, candidate optimizer.Candidate, throughput *float64, workload string) domain.BenchmarkResult {
		return domain.BenchmarkResult{ID: id, RevisionID: revision, ModelIdentity: candidate.Deployment.Model.ID + "@" + candidate.Deployment.Model.Revision, Provider: "runpod", GPU: "B200", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttft, OutputTokenThroughput: throughput, WorkloadJSON: workload, CostMetadataJSON: costJSON(4)}
	}
	benchmarks := []domain.BenchmarkResult{
		row("benchmark-a", "revision-a", first, &controlThroughput, controlWorkload),
		row("benchmark-b", "revision-b", second, &candidateThroughput, candidateWorkload),
	}
	return campaign, benchmarks, proposal
}

var quantizedWin = MetricComparison{HigherIsBetter: true, ControlMean: 60, CandidateMean: 90, Delta: 0.5, NoiseFloor: 0.03, Decision: DecisionImproved}

// A measured checkpoint whose answers passed (it reached the ranked state with
// quality evidence) and that clearly beat the unmodified control wins, and the
// persisted evaluation names the artifact it served and how it derives from
// the control's model.
func TestRankMeasuredCampaignSelectsAQuantizedCheckpointThatBeatsItsBase(t *testing.T) {
	for _, test := range []struct{ name, model, revision, lineage string }{
		{"exact lineage", exactLineageModel, exactLineageRevision, optimizer.LineageExact},
		{"publisher-declared lineage", declaredLineageModel, declaredLineageRevision, optimizer.LineagePublisherDeclared},
	} {
		t.Run(test.name, func(t *testing.T) {
			campaign, benchmarks, proposal := quantizedFixture(t, test.model, test.revision, quantizedWin)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatalf("a quantized checkpoint of the control's own model could not be ranked: %v", err)
			}
			if result.Decisions["candidate-b"] != RankSelect || result.Decisions["candidate-a"] != RankSupersede {
				t.Fatalf("the faster quantized checkpoint did not win: decisions=%v reasons=%v", result.Decisions, result.Reasons)
			}
			base := proposal.Candidates[0].Deployment.Model.ID + "@" + proposal.Candidates[0].Deployment.Model.Revision
			derived := proposal.Candidates[1].Deployment.Model.ID + "@" + proposal.Candidates[1].Deployment.Model.Revision
			var input lab.Input
			if err = json.Unmarshal([]byte(result.Evaluation.InputJSON), &input); err != nil {
				t.Fatal(err)
			}
			if result.Evaluation.ModelIdentity != base || input.ModelIdentity != base || len(input.DerivedModelIdentities) != 1 || input.DerivedModelIdentities[0] != derived {
				t.Fatalf("the evaluation does not record the base model and the derived artifact it admitted: identity=%q input=%+v", result.Evaluation.ModelIdentity, input)
			}
			var rows []struct {
				CandidateID     string `json:"candidate_id"`
				ModelIdentity   string `json:"model_identity"`
				ArtifactLineage string `json:"artifact_lineage"`
				Selected        bool   `json:"selected"`
			}
			if err = json.Unmarshal([]byte(result.Evaluation.ResultsJSON), &rows); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				switch row.CandidateID {
				case "candidate-a":
					if row.ModelIdentity != "" || row.ArtifactLineage != "" || row.Selected {
						t.Fatalf("the base-model control row changed shape: %+v", row)
					}
				case "candidate-b":
					if row.ModelIdentity != derived || row.ArtifactLineage != test.lineage || !row.Selected {
						t.Fatalf("the winning checkpoint row does not name its artifact and lineage: %+v", row)
					}
				default:
					t.Fatalf("unexpected evaluation row: %+v", row)
				}
			}
		})
	}
}

// reencode stores an edited proposal back on the campaign. The input digest
// covers the request, not the candidates, so it stays valid.
func reencode(t *testing.T, campaign *domain.OptimizationCampaign, proposal optimizer.Proposal) {
	t.Helper()
	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	campaign.ProposalJSON = string(encoded)
}

// A checkpoint that is slower than its base, or inside the noise, loses to the
// control like any other candidate.
func TestRankMeasuredCampaignKeepsTheBaseWhenAQuantizedCheckpointLoses(t *testing.T) {
	for _, test := range []struct {
		name       string
		comparison MetricComparison
		code       string
	}{
		{"slower", MetricComparison{HigherIsBetter: true, ControlMean: 60, CandidateMean: 45, Delta: -0.25, NoiseFloor: 0.03, Decision: DecisionRegressed}, FailureRegressed},
		{"inside the noise", MetricComparison{HigherIsBetter: true, ControlMean: 60, CandidateMean: 61, Delta: 1.0 / 60, NoiseFloor: 0.03, Decision: DecisionNoMeasurableDifference}, FailureNoMeasurableDifference},
	} {
		t.Run(test.name, func(t *testing.T) {
			campaign, benchmarks, _ := quantizedFixture(t, exactLineageModel, exactLineageRevision, test.comparison)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions["candidate-a"] != RankSelect || result.Decisions["candidate-b"] != RankSupersede || result.FailureCodes["candidate-b"] != test.code {
				t.Fatalf("a checkpoint that did not beat its base was not superseded by the control: decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
			}
		})
	}
}

// Lineage admits only artifacts built from the control's own model. A
// checkpoint of another model, another revision of the base, or an unrelated
// model in the same proposal is refused before anything is ranked.
func TestRankMeasuredCampaignRefusesArtifactsOfADifferentBaseModel(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*optimizer.Proposal)
		want error
	}{
		{"checkpoint built from another model", func(p *optimizer.Proposal) { p.Candidates[1].ArtifactSeed.BaseRepository = "Qwen/Qwen3.8-27B" }, optimizer.ErrDifferentBaseModel},
		{"exact checkpoint of another base revision", func(p *optimizer.Proposal) {
			p.Candidates[1].ArtifactSeed.BaseRevision = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}, optimizer.ErrDifferentBaseModel},
		{"unmodified candidate of another model", func(p *optimizer.Proposal) {
			p.Candidates[1].ArtifactSeed = nil
			p.Candidates[1].Deployment.Model.ID = "Qwen/Qwen3.8-27B"
		}, optimizer.ErrDifferentBaseModel},
		{"checkpoint as the control", func(p *optimizer.Proposal) { p.StartingPoint.CandidateID = p.Candidates[1].ID }, optimizer.ErrDerivedControl},
	} {
		t.Run(test.name, func(t *testing.T) {
			campaign, benchmarks, proposal := quantizedFixture(t, exactLineageModel, exactLineageRevision, quantizedWin)
			test.edit(&proposal)
			reencode(t, &campaign, proposal)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if !errors.Is(err, test.want) {
				t.Fatalf("ranking did not refuse it with %v: decisions=%v err=%v", test.want, result.Decisions, err)
			}
			store := &rankingStoreFixture{campaign: campaign, benchmarks: benchmarks}
			if _, err = (PersistedRanker{Store: store}).Rank(t.Context(), campaign.Candidates[1]); !errors.Is(err, test.want) || len(store.recorded) != 0 {
				t.Fatalf("the persisted ranker did not refuse it before recording an evaluation: err=%v recorded=%d", err, len(store.recorded))
			}
		})
	}
}

// Lineage never replaces the answer check. A checkpoint whose answers failed
// against the control's was rejected before ranking and cannot win however
// fast it measured, and one that reached ranking without quality evidence
// stops the ranking.
func TestRankMeasuredCampaignNeverSelectsAQuantizedCheckpointWithoutPassingAnswers(t *testing.T) {
	campaign, benchmarks, _ := quantizedFixture(t, exactLineageModel, exactLineageRevision, quantizedWin)
	campaign.Candidates[1].State, campaign.Candidates[1].FailureCode = CandidateRejected, FailureQualityRejected
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if _, ranked := result.Decisions["candidate-b"]; ranked || result.Decisions["candidate-a"] != RankSelect {
		t.Fatalf("a checkpoint whose answers failed took part in the ranking: decisions=%v reasons=%v", result.Decisions, result.Reasons)
	}
	var rows []struct {
		CandidateID string `json:"candidate_id"`
	}
	if err = json.Unmarshal([]byte(result.Evaluation.ResultsJSON), &rows); err != nil || len(rows) != 1 || rows[0].CandidateID != "candidate-a" {
		t.Fatalf("the evaluation compared a checkpoint whose answers failed: rows=%+v err=%v", rows, err)
	}

	campaign, benchmarks, _ = quantizedFixture(t, exactLineageModel, exactLineageRevision, quantizedWin)
	campaign.Candidates[1].QualityEvidenceID = ""
	if _, err = RankMeasuredCampaign(campaign, benchmarks); err == nil {
		t.Fatal("a checkpoint without answer-check evidence was ranked")
	}
}

// A row must record the artifact its own candidate served: the base model's
// evidence cannot stand in for the checkpoint's.
func TestRankMeasuredCampaignRejectsEvidenceForTheWrongArtifact(t *testing.T) {
	campaign, benchmarks, _ := quantizedFixture(t, exactLineageModel, exactLineageRevision, quantizedWin)
	benchmarks[1].ModelIdentity = benchmarks[0].ModelIdentity
	if _, err := RankMeasuredCampaign(campaign, benchmarks); err == nil {
		t.Fatal("base-model evidence was ranked as the checkpoint's")
	}
}

// The persisted ranker reads the evidence stored under each artifact and
// records the same evaluation on replay.
func TestPersistedRankerReadsEvidenceForEveryDerivedArtifact(t *testing.T) {
	campaign, benchmarks, _ := quantizedFixture(t, declaredLineageModel, declaredLineageRevision, quantizedWin)
	store := &rankingStoreFixture{campaign: campaign, benchmarks: benchmarks}
	ranker := PersistedRanker{Store: store}
	first, err := ranker.Rank(t.Context(), campaign.Candidates[1])
	if err != nil {
		t.Fatal(err)
	}
	second, err := ranker.Rank(t.Context(), campaign.Candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision != RankSelect || second.Decision != RankSupersede || first.LabEvaluationID != second.LabEvaluationID || len(store.recorded) != 1 {
		t.Fatalf("the checkpoint's win was not one replay-safe evaluation: first=%+v second=%+v rows=%d", first, second, len(store.recorded))
	}
}
