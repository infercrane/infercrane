package controlapi

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/support"
)

type agentEndpointFake struct {
	Store
	agent domain.Agent
	err   error
}

func (f agentEndpointFake) AgentForEndpoint(context.Context, string, string) (domain.Agent, error) {
	return f.agent, f.err
}

// A run on an agent's endpoint is refused before it is stored when its agent
// could not measure it: no single agent, no starting point, a migration,
// customer-signed evidence, a replayed trace, or a setup an agent cannot
// start.
func TestAgentRunTargetRefusesRunsAnAgentCannotMeasure(t *testing.T) {
	setup := func(env map[string]string) optimizer.Candidate {
		candidate := optimizer.Candidate{ID: "setup"}
		candidate.Deployment.Model.ID, candidate.Deployment.Model.Revision = "Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"
		candidate.Deployment.Runtime.Engine, candidate.Deployment.Runtime.Version, candidate.Deployment.Runtime.Env = "vllm", support.DefaultRuntimeVersion, env
		candidate.Deployment.Resources.GPU = "NVIDIA-L4"
		return candidate
	}
	proposal := func(mutate func(*optimizer.Proposal)) optimizer.Proposal {
		value := optimizer.Proposal{StartingPoint: &optimizer.StartingPoint{CandidateID: "current"}, Candidates: []optimizer.Candidate{{ID: "current"}, setup(nil)}}
		if mutate != nil {
			mutate(&value)
		}
		return value
	}
	api := API{Store: agentEndpointFake{agent: domain.Agent{ID: "agent-1", State: "active"}}}
	if agentID, refused := api.agentRunTarget(t.Context(), "tenant", "support-bot", proposal(nil)); refused != "" || agentID != "agent-1" {
		t.Fatalf("a run the agent can measure: %q %q", agentID, refused)
	}
	for name, test := range map[string]struct {
		api      API
		proposal optimizer.Proposal
		want     string
	}{
		"two agents":      {API{Store: agentEndpointFake{err: domain.ErrConflict}}, proposal(nil), "conflict"},
		"no endpoint":     {API{Store: agentEndpointFake{err: domain.ErrNotFound}}, proposal(nil), "was not found"},
		"no start":        {api, proposal(func(p *optimizer.Proposal) { p.StartingPoint = nil }), "starting point"},
		"migration":       {api, proposal(func(p *optimizer.Proposal) { p.Input.Migration = &optimizer.MigrationBaseline{} }), "migration"},
		"customer signed": {api, proposal(func(p *optimizer.Proposal) { p.Input.QualityCheck = optimizer.QualityCheckCustomer }), "customer-signed"},
		"replayed trace": {api, proposal(func(p *optimizer.Proposal) {
			p.Input.WorkloadSource, p.Input.WorkloadFingerprint = optimizer.WorkloadSourceCustomerObserved, "sha256:abc"
		}), "replayed trace"},
		"environment": {api, proposal(func(p *optimizer.Proposal) { p.Candidates[1] = setup(map[string]string{"VLLM_X": "1"}) }), "environment variables"},
	} {
		if _, refused := test.api.agentRunTarget(t.Context(), "tenant", "support-bot", test.proposal); !strings.Contains(refused, test.want) {
			t.Errorf("%s: refused %q, want %q", name, refused, test.want)
		}
	}
}

// An agent starts plain servers only, so a run whose setups include a Dynamo
// serving graph is refused before it is stored, and the refusal names the
// setup and why. The same setups on the plain Kubernetes adapter are accepted.
func TestAgentRunTargetRefusesASetupThatIsNotAPlainServer(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	api := API{Store: agentEndpointFake{agent: domain.Agent{ID: "agent-1", State: "active"}}, Integrations: registry.Snapshot()}
	setup := func(id, adapter string, serving servingcontract.Topology) optimizer.Candidate {
		candidate := optimizer.Candidate{ID: id}
		candidate.Deployment.Model.ID, candidate.Deployment.Model.Revision = "Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"
		candidate.Deployment.Runtime.Engine, candidate.Deployment.Runtime.Version = "vllm", support.DefaultRuntimeVersion
		candidate.Deployment.Resources.GPU, candidate.Deployment.Resources.GPUCount = "NVIDIA-L4", 1
		candidate.Deployment.Provider.Cloud, candidate.Deployment.Provider.Adapter, candidate.Deployment.Serving = "kubernetes", adapter, serving
		return candidate
	}
	graph := servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
	proposal := optimizer.Proposal{StartingPoint: &optimizer.StartingPoint{CandidateID: "current"},
		Candidates: []optimizer.Candidate{{ID: "current"}, setup("plain", "kubernetes", servingcontract.Topology{}), setup("graph", "kubernetes-dynamo", graph)}}
	if _, refused := api.agentRunTarget(t.Context(), "tenant", "support-bot", proposal); refused != "setup graph: an agent starts plain servers only; this setup needs a serving graph on NVIDIA Dynamo" {
		t.Fatalf("refused %q", refused)
	}
	proposal.Candidates = proposal.Candidates[:2]
	if agentID, refused := api.agentRunTarget(t.Context(), "tenant", "support-bot", proposal); refused != "" || agentID != "agent-1" {
		t.Fatalf("a plain setup: %q %q", agentID, refused)
	}
}

// A declared setup is recorded only for an endpoint one agent serves and
// that has no revision to read it from.
func TestDeclaredSetupIsOnlyForAnAgentsEndpoint(t *testing.T) {
	declared := optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, GPU: "NVIDIA-L4"}
	api := API{Store: agentEndpointFake{agent: domain.Agent{ID: "agent-1", State: "active"}}}
	setup, refused := api.declaredAgentSetup(t.Context(), "tenant", "support-bot", declared, false)
	if refused != "" || setup.Endpoint != "support-bot" || setup.Engine != "vllm" {
		t.Fatalf("declared: %+v %q", setup, refused)
	}
	if _, refused = api.declaredAgentSetup(t.Context(), "tenant", "support-bot", declared, true); !strings.Contains(refused, "active revision") {
		t.Fatalf("a revisioned endpoint: %q", refused)
	}
	if _, refused = (API{Store: agentEndpointFake{err: domain.ErrConflict}}).declaredAgentSetup(t.Context(), "tenant", "support-bot", declared, false); !strings.Contains(refused, "one active InferCrane agent") {
		t.Fatalf("no single agent: %q", refused)
	}
	if _, refused = api.declaredAgentSetup(t.Context(), "tenant", "support-bot", optimizer.DeclaredSetup{Engine: "vllm"}, false); refused == "" {
		t.Fatal("a declaration without a version was recorded")
	}
	// The declaration is checked where it enters: reviewed engine
	// arguments, reviewed environment names, and never a secret.
	for name, test := range map[string]struct {
		declared optimizer.DeclaredSetup
		want     string
	}{
		"refused argument":    {optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, Args: []string{"--trust-remote-code"}}, "--trust-remote-code"},
		"unreviewed argument": {optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, Args: []string{"--tool-parser-plugin", "/p.py"}}, "not reviewed"},
		"token":               {optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, Env: map[string]string{"HF_TOKEN": "hf_x"}}, "model_secret_reference_id"},
		"api key":             {optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, Env: map[string]string{"VLLM_API_KEY": "x"}}, "must not carry a secret"},
		"unreviewed env":      {optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, GPU: "L4", Env: map[string]string{"VLLM_SOMETHING_NEW": "1"}}, "not reviewed"},
		"env without a GPU":   {optimizer.DeclaredSetup{Engine: "vllm", Version: support.DefaultRuntimeVersion, Env: map[string]string{"VLLM_USE_RUST_FRONTEND": "1"}}, "needs a GPU"},
	} {
		if _, refused = api.declaredAgentSetup(t.Context(), "tenant", "support-bot", test.declared, false); !strings.Contains(refused, test.want) {
			t.Errorf("%s: refused %q, want %q", name, refused, test.want)
		}
	}
}
