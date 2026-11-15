package optimizationcampaign

import (
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/workflows"
)

// candidateLifecycleStore models the deployments and endpoints a new-endpoint
// campaign touches. Child operations complete at once. A converge publishes
// exactly what the deployment workflow publishes for the submitted request,
// and deleting a deployment leaves its endpoints in place, as the real store
// does. An endpoint bound to a deleted deployment is a customer-visible
// leftover.
type candidateLifecycleStore struct {
	*coordinatorRepository
	operations  map[string]domain.Operation
	deployments map[string]domain.Deployment
	endpoints   map[string]string
}

func (s *candidateLifecycleStore) SubmitCloudDeployment(_ context.Context, deployment domain.Deployment, operation domain.Operation) (domain.Deployment, domain.Operation, bool, error) {
	if existing, found := s.operations[operation.IdempotencyKey]; found {
		return s.deployments[deployment.Name], existing, false, nil
	}
	var request workflows.CloudRequest
	if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	if err := request.Validate(); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	deployment.ID, deployment.DesiredState, deployment.ActiveRevisionID = "deployment-"+deployment.Name, "running", "revision-"+deployment.Name
	s.deployments[deployment.Name] = deployment
	if name := request.PublishedEndpointName(); name != "" {
		s.endpoints[name] = deployment.Name
	}
	operation.ID, operation.Status = "operation-"+operation.IdempotencyKey, "succeeded"
	s.operations[operation.IdempotencyKey] = operation
	return deployment, operation, true, nil
}

func (s *candidateLifecycleStore) SubmitDeploymentDelete(_ context.Context, _, name, deploymentID string, operation domain.Operation) (domain.Operation, bool, error) {
	if existing, found := s.operations[operation.IdempotencyKey]; found {
		return existing, false, nil
	}
	deployment, found := s.deployments[name]
	if !found || deployment.ID != deploymentID {
		return domain.Operation{}, false, domain.ErrNotFound
	}
	deployment.DesiredState = "deleted"
	s.deployments[name] = deployment
	operation.ID, operation.Status = "operation-"+operation.IdempotencyKey, "succeeded"
	s.operations[operation.IdempotencyKey] = operation
	return operation, true, nil
}

func (s *candidateLifecycleStore) PublishDeploymentEndpoint(_ context.Context, tenant, endpoint, deployment string) (domain.ResolvedEndpoint, error) {
	if tenant != s.campaign.TenantID {
		return domain.ResolvedEndpoint{}, domain.ErrNotFound
	}
	if bound, found := s.endpoints[endpoint]; found && bound != deployment {
		return domain.ResolvedEndpoint{}, domain.ErrConflict
	}
	s.endpoints[endpoint] = deployment
	return domain.ResolvedEndpoint{Endpoint: domain.Endpoint{Name: endpoint, TenantID: tenant}}, nil
}

func (s *candidateLifecycleStore) ResolveForTenant(_ context.Context, _, name string) (domain.ResolvedDeployment, error) {
	deployment, found := s.deployments[name]
	if !found {
		return domain.ResolvedDeployment{}, domain.ErrNotFound
	}
	return domain.ResolvedDeployment{Deployment: deployment}, nil
}

func (s *candidateLifecycleStore) EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error) {
	return domain.Operation{}, false, domain.ErrConflict
}
func (s *candidateLifecycleStore) ActiveOperationForResource(context.Context, string, string, string) (domain.Operation, error) {
	return domain.Operation{}, domain.ErrNotFound
}
func (s *candidateLifecycleStore) RequestOperationCancel(context.Context, string) error { return nil }
func (s *candidateLifecycleStore) BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error) {
	return nil, nil
}
func (s *candidateLifecycleStore) QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error) {
	return nil, nil
}
func (s *candidateLifecycleStore) ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error) {
	return nil, nil
}
func (s *candidateLifecycleStore) EvaluateReleaseGuard(context.Context, string, string, time.Duration) (domain.ReleaseGuardEvaluation, error) {
	return domain.ReleaseGuardEvaluation{}, domain.ErrNotFound
}

func (s *candidateLifecycleStore) setCandidate(id, state string) domain.OptimizationCandidateRun {
	for index := range s.campaign.Candidates {
		if s.campaign.Candidates[index].ID == id {
			s.campaign.Candidates[index].State = state
			return s.campaign.Candidates[index]
		}
	}
	return domain.OptimizationCandidateRun{}
}

func TestNewEndpointCampaignLeavesOnlyTheActivatedEndpoint(t *testing.T) {
	for _, test := range []struct {
		name     string
		activate bool
	}{
		{name: "without activated winner"},
		{name: "with activated winner", activate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			store := newEndpointCampaignStore(now)
			// An endpoint the customer created is never touched by a campaign.
			store.deployments["chat"] = domain.Deployment{ID: "deployment-chat", Name: "chat", DesiredState: "running"}
			store.endpoints["chat"] = "chat"
			driver := CompositeDriver{Store: store, Costs: freshCost(now), Now: func() time.Time { return now }}
			budget := Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)}

			for index, candidate := range store.campaign.Candidates {
				result, err := driver.Provision(t.Context(), candidate.ID, candidate, budget)
				if err != nil {
					t.Fatalf("provision %s: %v", candidate.ID, err)
				}
				store.campaign.Candidates[index].DeploymentName, store.campaign.Candidates[index].RevisionID = result.DeploymentName, result.RevisionID
			}
			if want := map[string]string{"chat": "chat"}; !maps.Equal(store.endpoints, want) {
				t.Fatalf("candidates published endpoints during the run: %v", store.endpoints)
			}

			winner, loser := store.campaign.Candidates[0], store.campaign.Candidates[1]
			want := map[string]string{"chat": "chat"}
			cleanup := []domain.OptimizationCandidateRun{store.setCandidate(winner.ID, CandidateFailed), store.setCandidate(loser.ID, CandidateRejected)}
			if test.activate {
				store.setCandidate(winner.ID, CandidateQualified)
				request, _ := json.Marshal(ActivateRequest{TenantID: "tenant", CampaignID: store.campaign.ID, CandidateID: winner.ID, Actor: "operator"})
				refresh := func(context.Context) error { return nil }
				if _, err := ActivationHandlers(store, refresh, func() time.Time { return now })[ActivateKind](t.Context(), domain.Operation{RequestJSON: string(request)}); err != nil {
					t.Fatalf("activate winner: %v", err)
				}
				want["qwen-interactive"] = winner.DeploymentName
				cleanup = cleanup[1:]
			}
			for _, candidate := range cleanup {
				if err := driver.Cleanup(t.Context(), candidate.ID, candidate); err != nil {
					t.Fatalf("cleanup %s: %v", candidate.ID, err)
				}
			}

			if !maps.Equal(store.endpoints, want) {
				t.Fatalf("endpoints=%v, want %v", store.endpoints, want)
			}
			for endpoint, deployment := range store.endpoints {
				if store.deployments[deployment].DesiredState != "running" {
					t.Fatalf("endpoint %s is left bound to %s deployment %s", endpoint, store.deployments[deployment].DesiredState, deployment)
				}
			}
			if store.deployments[loser.DeploymentName].DesiredState != "deleted" {
				t.Fatalf("losing candidate deployment was not deleted: %+v", store.deployments[loser.DeploymentName])
			}
			if winnerState := store.deployments[winner.DeploymentName].DesiredState; test.activate != (winnerState == "running") {
				t.Fatalf("winner deployment state=%s with activation=%t", winnerState, test.activate)
			}
		})
	}
}

func newEndpointCampaignStore(now time.Time) *candidateLifecycleStore {
	draft := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: "qwen-interactive"}
	draft.Model.ID, draft.Model.Revision = "Qwen/Qwen3-8B", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	draft.Runtime.Engine, draft.Runtime.Version = "vllm", "0.8.5.post1"
	draft.Compute.Mode, draft.Resources.GPU = "elastic", "L40S"
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = "aws", "aws-ec2", "eu-central-1"
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	encoded, _ := json.Marshal(draft)
	proposal, _ := json.Marshal(optimizer.Proposal{Candidates: []optimizer.Candidate{{ID: "proposal-1", BenchmarkProfile: "interactive", Deployment: draft}}})
	expires := now.Add(time.Hour)
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant", Intent: IntentNewEndpoint, ProposalJSON: string(proposal), ApprovalExpiresAt: &expires}
	for _, id := range []string{"65fd40d3-winner", "e2d88597-loser"} {
		campaign.Candidates = append(campaign.Candidates, domain.OptimizationCandidateRun{ID: id, TenantID: "tenant", CampaignID: campaign.ID, ProposalCandidateID: "proposal-1", DeploymentSpecJSON: string(encoded), State: CandidateProvisioning, UpdatedAt: now})
	}
	return &candidateLifecycleStore{coordinatorRepository: &coordinatorRepository{campaign: campaign}, operations: map[string]domain.Operation{}, deployments: map[string]domain.Deployment{}, endpoints: map[string]string{}}
}
