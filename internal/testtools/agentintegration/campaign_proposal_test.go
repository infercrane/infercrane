package agentintegration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
)

// kindModelRevision pins the model the kind run's fake engines serve.
const kindModelRevision = "c1899de289a04d12100db370d81485cdf75e47ca"

// fakeEngineWorkload is the development fake engine as a custom workload:
// worker names what its answers say, so two workers answer differently, and
// extra arguments (a --delay) make one serve slower.
func fakeEngineWorkload(image, worker string, extra ...string) runtimecontract.Workload {
	return runtimecontract.Workload{Image: image, Command: append([]string{"/fake-vllm", "--port", "8000", "--model", "Qwen/Qwen3-0.6B", "--worker", worker, "--api-key-env", "VLLM_API_KEY"}, extra...),
		Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 5}
}

// agentCampaignProposal is a proposal for a run on the agent's endpoint
// whose setups the kind cluster can start: the endpoint's own server (the
// starting point, never started; it answers 40 ms late), a copy of it, and
// the same answers without the delay, the run's winner. The planner proposes real engine images, which kind cannot
// run, so the proposal is written here and checked by the real validator.
func agentCampaignProposal(image string) (optimizer.Proposal, error) {
	request := optimizer.Request{ModelIdentity: "Qwen/Qwen3-0.6B", ModelRevision: kindModelRevision, Provider: "kubernetes", GPU: "SIMULATED-CPU", GPUCount: 1,
		Objective: "interactive", WorkloadProfile: "interactive", WorkloadSource: optimizer.WorkloadSourceReviewedDefault, MaxCandidates: 3}
	encoded, err := json.Marshal(request)
	if err != nil {
		return optimizer.Proposal{}, err
	}
	digest := sha256.Sum256(encoded)
	candidate := func(name string, rank int, worker string, extra ...string) optimizer.Candidate {
		id := sha256.Sum256([]byte("kind-agent-run:" + name))
		value := optimizer.Candidate{ID: hex.EncodeToString(id[:]), Rank: rank, Objective: request.Objective, BenchmarkProfile: request.WorkloadProfile, EvidenceState: optimizer.EvidenceUnmeasured}
		draft := &value.Deployment
		draft.APIVersion, draft.Kind, draft.Name = "infercrane.dev/v1", "Deployment", "support-bot"
		draft.Model.ID, draft.Model.Revision = request.ModelIdentity, kindModelRevision
		draft.Runtime.Engine, draft.Runtime.Version = "vllm", support.DefaultRuntimeVersion
		draft.Resources.GPU, draft.Resources.GPUCount = request.GPU, 1
		draft.Provider.Cloud = "kubernetes"
		draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
		if worker != "" {
			draft.Runtime.Workload = fakeEngineWorkload(image, worker, extra...)
		}
		return value
	}
	current, copied, faster := candidate("current", 1, ""), candidate("copy", 2, "qwen3", "--delay", "40ms"), candidate("faster", 3, "qwen3")
	return optimizer.Proposal{
		SchemaVersion: optimizer.SchemaVersion, AlgorithmVersion: "kind-agent-run", Mutation: "none", SelectionBoundary: "kind data-plane proof on development fake engines",
		InputDigest: hex.EncodeToString(digest[:]), Input: request, Candidates: []optimizer.Candidate{current, copied, faster},
		StartingPoint: &optimizer.StartingPoint{CandidateID: current.ID, EvidenceState: optimizer.EvidenceUnmeasured, Basis: "the endpoint's own server, measured where it runs", ClaimBoundary: "fake engines prove the path, not performance"},
	}, nil
}

// The kind run's proposal passes the control plane's own validation, and
// every setup but the starting point is one an agent can start.
func TestAgentCampaignProposalIsValid(t *testing.T) {
	proposal, err := agentCampaignProposal("registry.example.test/infercrane-dataplane/engine@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if err = optimizer.ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	if err = optimizer.ValidateMeasurable(proposal.Input); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range proposal.Candidates[1:] {
		if _, setupErr := optimizationcampaign.AgentSetup(candidate.Deployment); setupErr != nil {
			t.Fatalf("%s: %v", candidate.ID, setupErr)
		}
	}
}
