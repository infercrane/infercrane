package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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

func TestEndpointGuardTopologyOnlyQualifiesMeasuredPrimaryOrGovernedAppend(t *testing.T) {
	primary := domain.ServingPlanBinding{BindingID: "primary", Priority: 0, Weight: 100}
	fallback := domain.ServingPlanBinding{BindingID: "fallback", Priority: 1, Weight: 100}
	resolved := domain.ResolvedEndpoint{
		ActivePlan:    domain.ServingPlan{RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{primary}},
		CandidatePlan: &domain.ServingPlan{RoutingPolicy: "primary-fallback", Bindings: []domain.ServingPlanBinding{primary, fallback}},
		Bindings: []domain.BackendBinding{
			{ID: "primary", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: "deployment"},
			{ID: "fallback", Kind: "external", OwnershipMode: "traffic-managed", ConfigJSON: `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`},
		},
	}
	if qualified, evidence := endpointPlanTopologyEvidence(resolved); !qualified || !strings.Contains(evidence, "append-only") {
		t.Fatalf("qualified=%t evidence=%q", qualified, evidence)
	}
	resolved.CandidatePlan.RoutingPolicy = "weighted"
	if qualified, _ := endpointPlanTopologyEvidence(resolved); qualified {
		t.Fatal("weighted topology reused primary-only evidence")
	}
	resolved.CandidatePlan.RoutingPolicy = "primary-fallback"
	resolved.Bindings[1].ConfigJSON = `{}`
	if qualified, _ := endpointPlanTopologyEvidence(resolved); qualified {
		t.Fatal("ungoverned fallback was qualified")
	}
}

func TestManagedExternalBindingIsImmutableTenantSafeAndHardBudgeted(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "managed-binding-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Managed Binding"); err != nil {
		t.Fatal(err)
	}
	environment, err := s.CreateEnvironment(ctx, tenant, domain.Environment{Name: "production", PolicyJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, tenant, domain.LogicalModel{Name: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, tenant, domain.Endpoint{Name: "coder-production", LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.AddTargetForTenant(ctx, tenant, domain.Target{Name: "managed-api", URL: "https://provider.invalid/v1", Provider: "openai-compatible-external", Runtime: "openai-compatible", UpstreamModel: "provider/coder"})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreateSecretReference(ctx, tenant, "managed-api", "env", "MANAGED_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"adapter":"openai-compatible-external","secret_reference_id":%q,"enabled":true,"privacy_acknowledged":true,"request_limit":5,"cost_limit_microusd":500,"max_request_cost_microusd":100}`, secret.ID)
	binding, err := s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: endpoint.ID, Name: "external-primary", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID, ConfigJSON: config})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.ManagedExternalBindingPolicy(ctx, tenant, binding.ID)
	if err != nil || policy.TargetID != target.ID || policy.SecretReferenceID != secret.ID || policy.RequestsReserved != 0 {
		t.Fatalf("policy=%#v err=%v", policy, err)
	}
	first, err := s.LeaseManagedExternalBindingBudget(ctx, tenant, binding.ID, 4)
	if err != nil || first.Requests != 4 || first.ReservedCostMicrousd != 400 {
		t.Fatalf("first lease=%#v err=%v", first, err)
	}
	second, err := s.LeaseManagedExternalBindingBudget(ctx, tenant, binding.ID, 4)
	if err != nil || second.Requests != 1 || second.ReservedCostMicrousd != 100 {
		t.Fatalf("second lease=%#v err=%v", second, err)
	}
	if _, err = s.LeaseManagedExternalBindingBudget(ctx, tenant, binding.ID, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("exhausted budget error=%v", err)
	}
	if _, err = s.ManagedExternalBindingPolicy(ctx, "global", binding.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant policy visible: %v", err)
	}
	if _, err = s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: endpoint.ID, Name: "raw-secret", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID, ConfigJSON: `{"adapter":"openai-compatible-external","secret_reference_id":"secret","api_key":"must-not-persist","enabled":true,"privacy_acknowledged":true,"request_limit":1,"cost_limit_microusd":1,"max_request_cost_microusd":1}`}); err == nil {
		t.Fatal("raw external credential field was accepted")
	}

	concurrentConfig := fmt.Sprintf(`{"adapter":"openai-compatible-external","secret_reference_id":%q,"enabled":true,"privacy_acknowledged":true,"request_limit":8,"cost_limit_microusd":800,"max_request_cost_microusd":100}`, secret.ID)
	concurrentBinding, err := s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: endpoint.ID, Name: "concurrent-budget", Kind: "external", OwnershipMode: "traffic-managed", TargetID: target.ID, ConfigJSON: concurrentConfig})
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if lease, leaseErr := s.LeaseManagedExternalBindingBudget(ctx, tenant, concurrentBinding.ID, 1); leaseErr == nil && lease.Requests == 1 {
				successes.Add(1)
			} else if !errors.Is(leaseErr, ErrConflict) {
				t.Errorf("unexpected concurrent lease: lease=%#v err=%v", lease, leaseErr)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 8 {
		t.Fatalf("distributed hard budget authorized %d requests, want 8", successes.Load())
	}
}

func TestProviderConnectionIsTenantScopedReferenceOnlyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "provider-connection-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Provider Connection"); err != nil {
		t.Fatal(err)
	}
	target, err := s.AddTargetForTenant(ctx, tenant, domain.Target{Name: "openrouter-main", URL: "https://openrouter.ai/api/v1", Provider: "openrouter", Runtime: "openai-compatible-api", UpstreamModel: "provider/model"})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.CreateSecretReference(ctx, tenant, "openrouter", "env", "OPENROUTER_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	input := domain.ProviderConnection{Name: "primary", Adapter: "openrouter", TargetID: target.ID, SecretReferenceID: secret.ID}
	created, err := s.CreateProviderConnection(ctx, tenant, input)
	if err != nil || created.TargetName != target.Name || created.SecretReferenceName != secret.Name {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	retry, err := s.CreateProviderConnection(ctx, tenant, input)
	if err != nil || retry.ID != created.ID {
		t.Fatalf("retry=%#v err=%v", retry, err)
	}
	if _, err = s.ProviderConnectionForTenant(ctx, "global", created.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant connection visible: %v", err)
	}
	rows, err := s.ProviderConnectionsForTenant(ctx, tenant)
	if err != nil || len(rows) != 1 || rows[0].SecretReferenceID != secret.ID {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	if _, err = s.CreateProviderConnection(ctx, tenant, domain.ProviderConnection{Name: "wrong-adapter", Adapter: "openai-compatible-external", TargetID: target.ID, SecretReferenceID: secret.ID}); err == nil {
		t.Fatal("adapter/target mismatch accepted")
	}
	if err = s.DeleteProviderConnectionForTenant(ctx, tenant, created.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProviderConnectionForTenant(ctx, tenant, created.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted connection still visible: %v", err)
	}
	if _, err = s.TargetForTenantByName(ctx, tenant, target.Name); err != nil {
		t.Fatalf("deleting connection mutated its external target: %v", err)
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
	replacementPlan, err := s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: endpoint.ID, RoutingPolicy: "weighted", Bindings: []domain.ServingPlanBinding{{BindingID: candidateBinding.ID, Weight: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointPlan(ctx, "global", endpoint.Name, replacementPlan.ID, "candidate"); err != nil {
		t.Fatal(err)
	}
	weightedEvaluation, err := s.EvaluateEndpointReleaseGuard(ctx, "global", endpoint.Name, time.Hour)
	if err != nil || weightedEvaluation.Decision != "INCONCLUSIVE" || !strings.Contains(weightedEvaluation.ReasonCodesJSON, "serving_plan_topology_unqualified") {
		t.Fatalf("weighted evaluation=%#v err=%v", weightedEvaluation, err)
	}
	if accepted, err := s.EndpointReleaseGuardAccepted(ctx, "global", endpoint.Name, replacementPlan.ID); err != nil || accepted {
		t.Fatalf("unmeasured weighted candidate accepted=%t err=%v", accepted, err)
	}
	if err = s.SetEndpointPlan(ctx, "global", endpoint.Name, candidatePlan.ID, "active"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale accepted plan activation error=%v, want conflict", err)
	}
	history, err = s.EndpointReleaseGuardEvaluations(ctx, "global", endpoint.Name, 10)
	if err != nil || len(history) != 3 || history[0].ID != weightedEvaluation.ID || history[1].ID != acceptedEvaluation.ID || history[2].ID != evaluation.ID {
		t.Fatalf("history=%#v err=%v", history, err)
	}
}

func TestEnvironmentPromotionStagesAtomicallyAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	environment, err := s.CreateEnvironment(ctx, "global", domain.Environment{Name: "production", PolicyJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	logical, err := s.CreateLogicalModel(ctx, "global", domain.LogicalModel{Name: "promotion-model-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	createDeployment := func(name string) domain.Deployment {
		target, createErr := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "coder"})
		if createErr != nil {
			t.Fatal(createErr)
		}
		deployment, createErr := s.CreateDeployment(ctx, domain.Deployment{Name: name, Model: "coder", Runtime: "vllm"}, []string{target.Name})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return deployment
	}
	sourceDeployment := createDeployment("promotion-source-dep-" + suffix)
	destinationDeployment := createDeployment("promotion-dest-dep-" + suffix)
	createEndpoint := func(name string, deployment domain.Deployment) (domain.Endpoint, domain.ServingPlan) {
		endpoint, createErr := s.CreateEndpoint(ctx, "global", domain.Endpoint{Name: name, LogicalModelID: logical.ID, EnvironmentID: environment.ID})
		if createErr != nil {
			t.Fatal(createErr)
		}
		binding, createErr := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "primary", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: deployment.ID})
		if createErr != nil {
			t.Fatal(createErr)
		}
		plan, createErr := s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: binding.ID, Weight: 100}}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if createErr = s.SetEndpointPlan(ctx, "global", endpoint.Name, plan.ID, "active"); createErr != nil {
			t.Fatal(createErr)
		}
		return endpoint, plan
	}
	sourceEndpoint, sourcePlan := createEndpoint("promotion-staging-"+suffix, sourceDeployment)
	destinationEndpoint, oldDestinationPlan := createEndpoint("promotion-production-"+suffix, destinationDeployment)
	promotion, created, err := s.StageEnvironmentPromotion(ctx, "global", sourceEndpoint.Name, destinationEndpoint.Name, "promotion-key-"+suffix)
	if err != nil || !created {
		t.Fatalf("stage promotion=%+v created=%t err=%v", promotion, created, err)
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, "global", destinationEndpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint.ActiveServingPlanID != oldDestinationPlan.ID || resolved.Endpoint.CandidateServingPlanID != promotion.DestinationPlanID {
		t.Fatalf("destination active/candidate changed unsafely: %+v", resolved.Endpoint)
	}
	if promotion.SourcePlanID != sourcePlan.ID || len(resolved.CandidatePlan.Bindings) != 1 {
		t.Fatalf("promotion did not clone source plan: promotion=%+v candidate=%+v", promotion, resolved.CandidatePlan)
	}
	firstPromotedBindingID := resolved.CandidatePlan.Bindings[0].BindingID
	again, created, err := s.StageEnvironmentPromotion(ctx, "global", sourceEndpoint.Name, destinationEndpoint.Name, "promotion-key-"+suffix)
	if err != nil || created || again.ID != promotion.ID || again.DestinationPlanID != promotion.DestinationPlanID {
		t.Fatalf("retry=%+v created=%t err=%v", again, created, err)
	}
	if _, _, err = s.StageEnvironmentPromotion(ctx, "global", sourceEndpoint.Name, destinationEndpoint.Name, "different-key-"+suffix); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected existing-candidate conflict, got %v", err)
	}
	if _, _, err = s.StageEnvironmentPromotion(ctx, "global", sourceEndpoint.Name, destinationEndpoint.Name, strings.Repeat("x", 256)); err == nil {
		t.Fatal("accepted an idempotency key beyond the API contract")
	}

	// A later immutable source plan must receive a distinct destination binding
	// snapshot. Sharing the binding would allow a future ownership transition to
	// alter the semantics of both historical destination plans.
	secondSourcePlan, err := s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: sourceEndpoint.ID, RoutingPolicy: "weighted", Bindings: sourcePlan.Bindings})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE endpoints SET active_serving_plan_id=? WHERE id=?`, secondSourcePlan.ID, sourceEndpoint.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE endpoints SET candidate_serving_plan_id=NULL WHERE id=?`, destinationEndpoint.ID); err != nil {
		t.Fatal(err)
	}
	second, created, err := s.StageEnvironmentPromotion(ctx, "global", sourceEndpoint.Name, destinationEndpoint.Name, "second-promotion-"+suffix)
	if err != nil || !created {
		t.Fatalf("second promotion=%+v created=%t err=%v", second, created, err)
	}
	resolved, err = s.ResolveEndpointForTenant(ctx, "global", destinationEndpoint.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.CandidatePlan.Bindings) != 1 || resolved.CandidatePlan.Bindings[0].BindingID == firstPromotedBindingID {
		t.Fatalf("second promotion reused a mutable destination binding snapshot: %+v", resolved.CandidatePlan.Bindings)
	}
}

func TestAdoptInspectDiagnoseAndAlertPolicyAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	name := "adopted-" + suffix
	resolved, adoption, err := s.AdoptEndpoint(ctx, "global", name, "coder-"+suffix, "coder", "https://inference.example.test/v1/", "vllm", "observe-only", "vllm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateAlertPolicy(ctx, "global", name, domain.AlertPolicy{Name: "unsafe-query", WebhookURL: "https://alerts.example.test/hook?token=secret", SecretReferenceID: "missing", MinimumSeverity: "warning", Enabled: true, MaxAttempts: 3}); err == nil || !strings.Contains(err.Error(), "query parameters") {
		t.Fatalf("webhook query credential was accepted: %v", err)
	}
	if adoption.OwnershipMode != "observe-only" || len(resolved.Bindings) != 1 || resolved.Bindings[0].OwnershipMode != "observe-only" || resolved.Bindings[0].DeploymentID != "" {
		t.Fatalf("adoption=%#v resolved=%#v", adoption, resolved)
	}
	emptyPolicies, err := s.AlertPoliciesForEndpoint(ctx, "global", name)
	if err != nil || emptyPolicies == nil || len(emptyPolicies) != 0 {
		t.Fatalf("empty alert policies=%#v err=%v, want non-nil empty collection", emptyPolicies, err)
	}
	_, repeated, err := s.AdoptEndpoint(ctx, "global", name, "coder-"+suffix, "coder", "https://inference.example.test", "vllm", "observe-only", "vllm")
	if err != nil || repeated.ID != adoption.ID {
		t.Fatalf("idempotent adoption=%#v err=%v", repeated, err)
	}
	if _, _, err = s.AdoptEndpoint(ctx, "global", name, "coder-"+suffix, "coder", "https://inference.example.test/v1", "vllm", "traffic-managed", "vllm"); !errors.Is(err, ErrConflict) {
		t.Fatalf("ownership escalation error=%v", err)
	}
	promoted, err := s.PromoteAdoptionOwnership(ctx, "global", name, "traffic-managed")
	if err != nil || promoted.OwnershipMode != "traffic-managed" {
		t.Fatalf("promoted=%#v err=%v", promoted, err)
	}
	resolved, err = s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil || resolved.Bindings[0].OwnershipMode != "traffic-managed" {
		t.Fatalf("promoted binding=%#v err=%v", resolved.Bindings, err)
	}
	record := domain.InferenceRecord{RequestID: "request-" + suffix, TenantID: "global", TargetID: adoption.TargetID, LogicalModelID: resolved.LogicalModel.ID, EnvironmentID: resolved.Environment.ID, EndpointID: resolved.Endpoint.ID, ServingPlanID: resolved.ActivePlan.ID, BindingID: adoption.BindingID, Provider: "external", Runtime: "vllm", ComputeMode: "external", OperationName: "chat", StartedAt: time.Now().UTC(), StatusCode: 503, LatencyMS: 250, ErrorType: "upstream_unavailable"}
	if err = s.RecordRequest(ctx, record); err != nil {
		t.Fatal(err)
	}
	inspection, err := s.RequestInspectionForTenant(ctx, "global", record.RequestID)
	if err != nil || inspection.Endpoint != name || inspection.Deployment != "" || inspection.ErrorType != "upstream_unavailable" {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	if _, err = s.RequestInspectionForTenant(ctx, "another-tenant", record.RequestID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant inspection error=%v", err)
	}
	for i := 1; i < 20; i++ {
		record.RequestID = fmt.Sprintf("request-%s-%d", suffix, i)
		if err = s.RecordRequest(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	findings, err := s.DiagnoseEndpoint(ctx, "global", name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["endpoint_not_serving"] || !codes["elevated_error_rate"] {
		t.Fatalf("findings=%#v", findings)
	}
	secret, err := s.CreateSecretReference(ctx, "global", "alert-"+suffix, "env", "INFERCRANE_ALERT_SIGNING_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.CreateAlertPolicy(ctx, "global", name, domain.AlertPolicy{Name: "operations", WebhookURL: "https://alerts.example.test/infercrane", SecretReferenceID: secret.ID, MinimumSeverity: "warning", Enabled: true, MaxAttempts: 3})
	if err != nil || policy.EndpointID != resolved.Endpoint.ID {
		t.Fatalf("policy=%#v err=%v", policy, err)
	}
	var critical domain.DiagnosticFinding
	for _, finding := range findings {
		if finding.Severity == "critical" {
			critical = finding
			break
		}
	}
	delivery, created, err := s.BeginAlertDelivery(ctx, policy, critical, "sha256:body")
	if err != nil || !created || delivery.Status != "pending" {
		t.Fatalf("delivery=%#v created=%t err=%v", delivery, created, err)
	}
	if err = s.RecordAlertDeliveryAttempt(ctx, delivery.ID, false, 503, "http_503", policy.MaxAttempts); err != nil {
		t.Fatal(err)
	}
	if repeated, createdAgain, err := s.BeginAlertDelivery(ctx, policy, critical, "sha256:body"); err != nil || createdAgain || repeated.Attempts != 1 || repeated.Status != "pending" {
		t.Fatalf("repeated delivery=%#v created=%t err=%v", repeated, createdAgain, err)
	}
	if err = s.DeleteEndpointForTenant(ctx, "global", name); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TargetForTenantByID(ctx, "global", adoption.TargetID); err != nil {
		t.Fatalf("adopted target was deleted: %v", err)
	}
}

func TestPublishDeploymentEndpointIsAtomicIdempotentAndNeverRebinds(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	createDeployment := func(name string) domain.Deployment {
		t.Helper()
		target, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "meta-llama/Llama-3.1-8B-Instruct"})
		if err != nil {
			t.Fatal(err)
		}
		deployment, err := s.CreateDeployment(ctx, domain.Deployment{Name: name, Model: "meta-llama/Llama-3.1-8B-Instruct"}, []string{target.Name})
		if err != nil {
			t.Fatal(err)
		}
		return deployment
	}
	selected := createDeployment("candidate-a-" + suffix)
	other := createDeployment("candidate-b-" + suffix)
	alias := "llama-production-" + suffix

	published, err := s.PublishDeploymentEndpoint(ctx, "global", alias, selected.Name)
	if err != nil {
		t.Fatal(err)
	}
	if published.Endpoint.Name != alias || published.Environment.Name != "production" || published.LogicalModel.Name != alias || published.ActivePlan.RoutingPolicy != "manual" || len(published.Bindings) != 1 || published.Bindings[0].DeploymentID != selected.ID || published.Bindings[0].OwnershipMode != "lifecycle-managed" {
		t.Fatalf("unexpected published endpoint: %#v", published)
	}
	repeated, err := s.PublishDeploymentEndpoint(ctx, "global", alias, selected.Name)
	if err != nil || repeated.Endpoint.ID != published.Endpoint.ID || repeated.ActivePlan.ID != published.ActivePlan.ID {
		t.Fatalf("idempotent publication changed identity: first=%#v repeated=%#v err=%v", published, repeated, err)
	}
	concurrentAlias := "llama-concurrent-" + suffix
	type publicationResult struct {
		endpoint domain.ResolvedEndpoint
		err      error
	}
	results := make(chan publicationResult, 2)
	var publishers sync.WaitGroup
	for range 2 {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			endpoint, publishErr := s.PublishDeploymentEndpoint(ctx, "global", concurrentAlias, selected.Name)
			results <- publicationResult{endpoint: endpoint, err: publishErr}
		}()
	}
	publishers.Wait()
	close(results)
	concurrentEndpointID := ""
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent identical publication failed: %v", result.err)
		}
		if concurrentEndpointID == "" {
			concurrentEndpointID = result.endpoint.Endpoint.ID
		} else if result.endpoint.Endpoint.ID != concurrentEndpointID {
			t.Fatalf("concurrent publication created two endpoint identities: %s and %s", concurrentEndpointID, result.endpoint.Endpoint.ID)
		}
	}
	if _, err = s.PublishDeploymentEndpoint(ctx, "global", alias, other.Name); !errors.Is(err, ErrConflict) {
		t.Fatalf("existing endpoint alias was rebound: %v", err)
	}
	crossTenant, err := s.PublishDeploymentEndpoint(ctx, "other-tenant", alias, selected.Name)
	if !errors.Is(err, ErrNotFound) || crossTenant.Endpoint.ID != "" {
		t.Fatalf("cross-tenant deployment was published: endpoint=%#v err=%v", crossTenant, err)
	}
}
