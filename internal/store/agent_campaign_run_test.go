package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/support"
)

// inputDigest is the digest a proposal states for its input.
func inputDigest(request optimizer.Request) string {
	encoded, _ := json.Marshal(request)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// setupID is a proposal candidate's ID for a named setup.
func setupID(name string) string {
	sum := sha256.Sum256([]byte("agent-run-test:" + name))
	return hex.EncodeToString(sum[:])
}

// windowsAtOnce answers every measured window as soon as it is queued: the
// endpoint's own server at 1,000 output tokens a second, and a setup with a
// larger batch 10% faster on its first server and 9.5% on its fresh one.
type windowsAtOnce struct {
	windows map[string]optimizationcampaign.AgentWindow
	count   int
}

func (w *windowsAtOnce) QueueWindow(_ context.Context, _, _, key string, spec agentprotocol.WindowSpec) (optimizationcampaign.AgentWindow, error) {
	if window, ok := w.windows[key]; ok {
		return window, nil
	}
	if err := spec.Validate(); err != nil {
		return optimizationcampaign.AgentWindow{}, err
	}
	w.count++
	candidate := 1000.0
	if spec.Candidate != nil && slices.Contains(spec.Candidate.RuntimeArgs, "--max-num-seqs") {
		candidate = map[bool]float64{false: 1100, true: 1095}[spec.Fresh]
	}
	result := &agentprotocol.WindowResult{Tool: "aiperf", ToolVersion: "0.12.0", CleanedUp: true}
	drift := []float64{1, 1.004, 0.997, 1.002, 0.999, 1.003, 0.998, 1.001, 1}
	run := func(index int, arm string, throughput float64) agentprotocol.WindowRun {
		value, ttft, latency := throughput*drift[index%len(drift)], 120.0, 4000.0
		node := "node-cccccccccccccccc"
		if arm == agentprotocol.ArmCandidate {
			node = map[bool]string{false: "node-aaaaaaaaaaaaaaaa", true: "node-bbbbbbbbbbbbbbbb"}[spec.Fresh]
		}
		return agentprotocol.WindowRun{Arm: arm, Seed: spec.SeedBase + int64(index+2), Requests: spec.Load.Requests, Succeeded: spec.Load.Requests, DurationSeconds: 60,
			OutputTokenThroughput: &value, TTFTP95MS: &ttft, LatencyP95MS: &latency, Node: node, GPU: "unknown"}
	}
	if spec.Candidate == nil {
		for index := range spec.Rounds {
			result.Runs = append(result.Runs, run(index, agentprotocol.ArmControl, 1000))
		}
	} else {
		result.Runs = append(result.Runs, run(0, agentprotocol.ArmControl, 1000))
		for round := range spec.Rounds {
			result.Runs = append(result.Runs, run(2*round+1, agentprotocol.ArmCandidate, candidate), run(2*round+2, agentprotocol.ArmControl, 1000))
		}
	}
	result.Quality = &agentprotocol.WindowAnswers{EvalSet: spec.Quality.EvalSet, EvalSetName: "infercrane-default", EvalSetVersion: "1", EvalSetDigest: "sha256:" + strings.Repeat("a", 64),
		PairingDigest: "sha256:" + strings.Repeat("b", 64), Items: 2, CandidateScores: []float64{1, 1}, ResultDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC().Add(-time.Minute)}
	now := time.Now().UTC()
	started := now.Add(-10 * time.Minute)
	window := optimizationcampaign.AgentWindow{ID: fmt.Sprintf("window-%d", w.count), State: agentprotocol.CommandSucceeded, StartedAt: &started, FinishedAt: &now, Result: result}
	w.windows[key] = window
	return window, nil
}

func (w *windowsAtOnce) CancelWindow(context.Context, string, string, string) error { return nil }

// A run on an agent's endpoint goes from approval to its decisions on
// PostgreSQL through the same coordinator a hosted run uses: every setup is
// stored under its agent identity, the faster setup is confirmed on a fresh
// node and passes Release Guard with a stored decision, and the others are
// decided too. The memory stores the driver's own tests use cannot show
// what the database refuses.
func TestAgentEndpointRunIsDecidedOnPostgres(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	request := optimizer.Request{ModelIdentity: "Qwen/Qwen3-8B", ModelRevision: "b968826d9c46dd6066d109eabc6255188de91218", Provider: "kubernetes", GPU: "NVIDIA-L4", GPUCount: 1,
		Objective: "throughput", WorkloadProfile: "throughput", WorkloadSource: optimizer.WorkloadSourceReviewedDefault, MaxCandidates: 3}
	setup := func(name string, rank int, args ...string) optimizer.Candidate {
		candidate := optimizer.Candidate{ID: setupID(name), Rank: rank, Objective: request.Objective, BenchmarkProfile: "throughput", EvidenceState: optimizer.EvidenceUnmeasured}
		draft := &candidate.Deployment
		draft.APIVersion, draft.Kind, draft.Name = "infercrane.dev/v1", "Deployment", "support-bot"
		draft.Model.ID, draft.Model.Revision = "Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"
		draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args = "vllm", support.DefaultRuntimeVersion, args
		draft.Resources.GPU, draft.Resources.GPUCount = "NVIDIA-L4", 1
		draft.Provider.Cloud = "kubernetes"
		draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
		return candidate
	}
	proposal := optimizer.Proposal{SchemaVersion: optimizer.SchemaVersion, AlgorithmVersion: "agent-run-test", Mutation: "none", SelectionBoundary: "a stand-in agent's windows",
		InputDigest: inputDigest(request), Input: request, Candidates: []optimizer.Candidate{setup("current", 1), setup("copy", 2), setup("faster", 3, "--max-num-seqs", "256")},
		StartingPoint: &optimizer.StartingPoint{CandidateID: setupID("current"), EvidenceState: optimizer.EvidenceUnmeasured, Basis: "the endpoint's own server", ClaimBoundary: "stand-in numbers"}}
	if err = optimizer.ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	proposalJSON, _ := json.Marshal(proposal)
	names := map[string]string{setupID("current"): "current", setupID("copy"): "copy", setupID("faster"): "faster"}
	var runs []domain.OptimizationCandidateRun
	for _, candidate := range proposal.Candidates {
		deployment, _ := json.Marshal(candidate.Deployment)
		runs = append(runs, domain.OptimizationCandidateRun{ProposalCandidateID: candidate.ID, Rank: candidate.Rank, EvidenceState: "unmeasured", DeploymentSpecJSON: string(deployment), PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`})
	}
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "tenant-a", IdempotencyKey: "agent-run", InputDigest: proposal.InputDigest, ModelIdentity: request.ModelIdentity,
		Objective: request.Objective, Source: proposal.AlgorithmVersion, Intent: optimizationcampaign.IntentEvolveEndpoint, TargetEndpoint: "support-bot", AgentID: agent.ID, ProposalJSON: string(proposalJSON)}, runs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveOptimizationCampaign(ctx, "tenant-a", campaign.ID, "owner", 1, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	windows := &windowsAtOnce{windows: map[string]optimizationcampaign.AgentWindow{}}
	driver := optimizationcampaign.AgentDriver{Store: s, Windows: windows, Ranker: optimizationcampaign.PersistedRanker{Store: s}, Signer: key}
	coordinator := optimizationcampaign.Coordinator{Repository: s, Driver: driver}
	decided := map[string]bool{optimizationcampaign.CandidateGuardPassed: true, optimizationcampaign.CandidateRejected: true, optimizationcampaign.CandidateInconclusive: true,
		optimizationcampaign.CandidateFailed: true, optimizationcampaign.CandidateCleaned: true}
	lastErr := map[string]string{}
	for range 60 {
		current, readErr := s.OptimizationCampaign(ctx, "tenant-a", campaign.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		done := true
		for _, candidate := range current.Candidates {
			if decided[candidate.State] {
				continue
			}
			done = false
			if _, stepErr := coordinator.Step(ctx, "tenant-a", campaign.ID, candidate.ID); stepErr != nil {
				lastErr[candidate.ProposalCandidateID] = candidate.State + ": " + stepErr.Error()
			}
		}
		if done {
			break
		}
	}
	current, err := s.OptimizationCampaign(ctx, "tenant-a", campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]domain.OptimizationCandidateRun{}
	for _, candidate := range current.Candidates {
		states[names[candidate.ProposalCandidateID]] = candidate
		if !decided[candidate.State] {
			t.Fatalf("%s stopped at %s: %s", candidate.ProposalCandidateID, candidate.State, lastErr[candidate.ProposalCandidateID])
		}
		if !strings.HasPrefix(candidate.RevisionID, "agent") || candidate.DeploymentName != "support-bot" || candidate.BenchmarkID == "" {
			t.Fatalf("%s is not stored under its agent identity: %+v", candidate.ProposalCandidateID, candidate)
		}
	}
	faster := states["faster"]
	if faster.State != optimizationcampaign.CandidateGuardPassed || faster.ReleaseGuardEvaluationID == "" || faster.QualityEvidenceID == "" || faster.LabEvaluationID == "" {
		summary := ""
		for name, candidate := range states {
			summary += fmt.Sprintf("\n%s: %s %s (%s)", name, candidate.State, candidate.FailureCode, lastErr[candidate.ProposalCandidateID])
		}
		if evaluations, labErr := s.LabEvaluationsByIDs(ctx, "tenant-a", []string{faster.LabEvaluationID}); labErr == nil {
			summary += "\nLab: " + evaluations[faster.LabEvaluationID].ResultsJSON
		}
		t.Fatalf("the faster setup did not pass:%s", summary)
	}
	guards, err := s.ReleaseGuardEvaluationsByIDs(ctx, "tenant-a", []string{faster.ReleaseGuardEvaluationID})
	if guard := guards[faster.ReleaseGuardEvaluationID]; err != nil || guard.Decision != "PASS" || guard.CandidateRevisionID != faster.RevisionID || !strings.HasPrefix(guard.ActiveRevisionID, "agentserver-") {
		t.Fatalf("the stored guard decision: %+v %v", guards, err)
	}
	// One window each for the live server and the copy, two for the faster
	// setup: its first and the one on a fresh node.
	if windows.count != 4 {
		t.Fatalf("windows measured: %d", windows.count)
	}
	// Another tenant cannot record a guard decision on this run's setup.
	if _, err = s.RecordAgentReleaseGuardEvaluation(ctx, domain.AgentReleaseGuardEvaluation{TenantID: "global", AgentID: agent.ID, CampaignID: campaign.ID, CandidateID: faster.ID,
		ActiveSetupID: guards[faster.ReleaseGuardEvaluationID].ActiveRevisionID, CandidateSetupID: faster.RevisionID, BenchmarkID: faster.BenchmarkID, Decision: "PASS", ReasonCodesJSON: `[]`, MetricsJSON: `{}`, PolicyJSON: `{}`}); err == nil {
		t.Fatal("another tenant recorded a guard decision on this run's setup")
	}
}
