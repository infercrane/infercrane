package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestNormalizeServingPlanIsCanonicalAndBounded(t *testing.T) {
	bindings := []domain.ServingPlanBinding{
		{BindingID: "secondary", Priority: 1, Weight: 25},
		{BindingID: "primary", Priority: 0, Weight: 75},
	}
	plan, body, err := normalizePlan("weighted", bindings)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Bindings[0].BindingID != "primary" || string(body) != `{"routing_policy":"weighted","bindings":[{"binding_id":"primary","priority":0,"weight":75},{"binding_id":"secondary","priority":1,"weight":25}]}` {
		t.Fatalf("canonical plan = %s", body)
	}
	for name, candidate := range map[string][]domain.ServingPlanBinding{
		"duplicate ID":       {{BindingID: "a", Weight: 1}, {BindingID: "a", Priority: 1, Weight: 1}},
		"duplicate priority": {{BindingID: "a", Weight: 1}, {BindingID: "b", Weight: 1}},
		"unbounded weight":   {{BindingID: "a", Weight: 10001}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := normalizePlan("weighted", candidate); err == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
	if _, _, err = normalizePlan("manual", bindings); err == nil {
		t.Fatal("manual plan accepted multiple bindings")
	}
}

func TestLegacyDeploymentBackfillsStableEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "endpoint-" + time.Now().UTC().Format("150405.000000000")
	target, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "upstream"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.CreateDeployment(ctx, domain.Deployment{Name: name, Model: "logical"}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint.Name != name || resolved.Environment.Name != "production" || resolved.LogicalModel.Name != name {
		t.Fatalf("backfilled endpoint = %#v", resolved)
	}
	if len(resolved.Bindings) != 1 || resolved.Bindings[0].DeploymentID != deployment.ID || resolved.Bindings[0].OwnershipMode != "lifecycle-managed" {
		t.Fatalf("backfilled bindings = %#v", resolved.Bindings)
	}
	if resolved.ActivePlan.ID == "" || resolved.ActivePlan.RoutingPolicy != "manual" || len(resolved.ActivePlan.Bindings) != 1 {
		t.Fatalf("backfilled plan = %#v", resolved.ActivePlan)
	}
	if !strings.HasPrefix(resolved.ActivePlan.SpecDigest, "sha256:") || len(resolved.ActivePlan.SpecDigest) != 71 {
		t.Fatalf("backfilled plan digest = %q", resolved.ActivePlan.SpecDigest)
	}
	if _, err = s.ExecContext(ctx, `UPDATE serving_plans SET routing_policy='weighted' WHERE id=?`, resolved.ActivePlan.ID); err == nil {
		t.Fatal("immutable serving plan accepted an update")
	}
	if _, err = s.ResolveEndpointForTenant(ctx, "another-tenant", name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant resolve error = %v", err)
	}
}

func TestEndpointServingPlanRejectsCrossTenantBinding(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	if err := s.CreateTenant(ctx, "endpoint-tenant-"+suffix, "Endpoint Tenant "+suffix); err != nil {
		t.Fatal(err)
	}
	tenant := "endpoint-tenant-" + suffix
	environment, err := s.CreateEnvironment(ctx, tenant, domain.Environment{Name: "staging", PolicyJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, tenant, domain.LogicalModel{Name: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, tenant, domain.Endpoint{Name: "coder-staging", LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	globalDeployments, err := s.DeploymentsForTenant(ctx, "global")
	if err != nil || len(globalDeployments) == 0 {
		t.Skip("test needs a global fixture deployment")
	}
	if _, err = s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: endpoint.ID, Name: "foreign", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: globalDeployments[0].ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant binding error = %v", err)
	}
}

func TestEndpointReleaseGuardPersistsDeterministicPlanDecision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	createDeployment := func(name string) domain.Deployment {
		target, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "coder"})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.SetTargetHealth(ctx, target.ID, "healthy"); err != nil {
			t.Fatal(err)
		}
		deployment, err := s.CreateDeployment(ctx, domain.Deployment{Name: name, Model: "coder", Runtime: "vllm"}, []string{target.Name})
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := s.ResolveForTenant(ctx, "global", deployment.Name)
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Deployment
	}
	activeDeployment := createDeployment("guard-active-" + suffix)
	candidateDeployment := createDeployment("guard-candidate-" + suffix)
	environment, err := s.EnvironmentForTenant(ctx, "global", "production")
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, "global", domain.LogicalModel{Name: "guard-model-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, "global", domain.Endpoint{Name: "guard-endpoint-" + suffix, LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	activeBinding, err := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "active", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: activeDeployment.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidateBinding, err := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "candidate", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: candidateDeployment.ID})
	if err != nil {
		t.Fatal(err)
	}
	activePlan, err := s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: activeBinding.ID, Weight: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointPlan(ctx, "global", endpoint.Name, activePlan.ID, "active"); err != nil {
		t.Fatal(err)
	}
	candidatePlan, err := s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: candidateBinding.ID, Weight: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointPlan(ctx, "global", endpoint.Name, candidatePlan.ID, "candidate"); err != nil {
		t.Fatal(err)
	}
	activeTTFT, candidateTTFT := 100.0, 150.0
	for _, record := range []domain.InferenceRecord{{RequestID: "endpoint-active-" + suffix, DeploymentID: activeDeployment.ID, RevisionID: activeDeployment.ActiveRevisionID, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 200, TTFTMS: &activeTTFT}, {RequestID: "endpoint-candidate-" + suffix, DeploymentID: candidateDeployment.ID, RevisionID: candidateDeployment.ActiveRevisionID, OperationName: "chat", StartedAt: time.Now(), StatusCode: 200, LatencyMS: 250, TTFTMS: &candidateTTFT}} {
		if err = s.RecordRequest(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := s.EndpointReleaseGuardPolicy(ctx, "global", endpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	policy.MinimumRequests = 1
	policy.MaxTTFTRegressionPercent = 10
	if _, err = s.SetEndpointReleaseGuardPolicy(ctx, "global", endpoint.Name, policy); err != nil {
		t.Fatal(err)
	}
	evaluation, err := s.EvaluateEndpointReleaseGuard(ctx, "global", endpoint.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Decision != "REJECT" || evaluation.ActiveServingPlanID != activePlan.ID || evaluation.CandidateServingPlanID != candidatePlan.ID {
		t.Fatalf("evaluation = %#v", evaluation)
	}
	if accepted, err := s.EndpointReleaseGuardAccepted(ctx, "global", endpoint.Name, candidatePlan.ID); err != nil || accepted {
		t.Fatalf("rejected candidate accepted=%t err=%v", accepted, err)
	}
	history, err := s.EndpointReleaseGuardEvaluations(ctx, "global", endpoint.Name, 10)
	if err != nil || len(history) != 1 || history[0].ID != evaluation.ID {
		t.Fatalf("history=%#v err=%v", history, err)
	}

	policy.MaxTTFTRegressionPercent = 100
	policy.MaxLatencyRegressionPercent = 100
	if _, err = s.SetEndpointReleaseGuardPolicy(ctx, "global", endpoint.Name, policy); err != nil {
		t.Fatal(err)
	}
	acceptedEvaluation, err := s.EvaluateEndpointReleaseGuard(ctx, "global", endpoint.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if acceptedEvaluation.Decision != "PASS" {
		t.Fatalf("accepted evaluation = %#v", acceptedEvaluation)
	}
	if accepted, err := s.EndpointReleaseGuardAccepted(ctx, "global", endpoint.Name, candidatePlan.ID); err != nil || !accepted {
		t.Fatalf("passing candidate accepted=%t err=%v", accepted, err)
	}
	history, err = s.EndpointReleaseGuardEvaluations(ctx, "global", endpoint.Name, 10)
	if err != nil || len(history) != 2 || history[0].ID != acceptedEvaluation.ID || history[1].ID != evaluation.ID {
		t.Fatalf("history=%#v err=%v", history, err)
	}
}
