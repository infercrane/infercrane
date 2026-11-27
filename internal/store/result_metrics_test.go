package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/resultmetrics"
)

// TestResultMetricsReadsWorkspaceResults seeds one workspace's release
// history, a weighted endpoint, and optimization runs, plus another
// workspace's records, and checks the snapshot reads only the first
// workspace and counts every stored record.
func TestResultMetricsReadsWorkspaceResults(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	deployment := func(tenant, name string) domain.Deployment {
		t.Helper()
		target, err := s.AddTargetForTenant(ctx, tenant, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "model"})
		if err != nil {
			t.Fatal(err)
		}
		created, err := s.CreateDeploymentForTenant(ctx, tenant, domain.Deployment{Name: name, Model: "model", Runtime: "vllm"}, []string{target.Name})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	if err := s.CreateTenant(ctx, "results-other", "Results Other"); err != nil {
		t.Fatal(err)
	}
	prod := deployment("global", "results-prod")
	deployment("global", "results-quiet")
	other := deployment("results-other", "results-other-prod")
	now := time.Now().UTC().Truncate(time.Millisecond)

	guard := func(id string, d domain.Deployment, decision, quality string, at time.Time) {
		t.Helper()
		metrics := `{"active":{},"candidate":{}}`
		if quality != "" {
			metrics = `{"active":{},"candidate":{},"quality":{"verdict":"` + quality + `"}}`
		}
		exec(`INSERT INTO release_guard_evaluations(id,deployment_id,active_revision_id,candidate_revision_id,decision,reason_codes_json,metrics_json,policy_json,created_at) VALUES(?,?,?,?,?,'[]'::jsonb,?::jsonb,'{}'::jsonb,?)`, id, d.ID, d.ActiveRevisionID, d.ActiveRevisionID, decision, metrics, at)
	}
	guard("rg-1", prod, "ACCEPT", "PASS", now.Add(-3*time.Hour))
	guard("rg-2", prod, "ACCEPT", "WATCH", now.Add(-2*time.Hour))
	guard("rg-3", prod, "WAIT", "", now.Add(-90*time.Minute))
	guard("rg-4", prod, "REJECT", "BLOCK", now.Add(-time.Hour))
	guard("rg-other", other, "REJECT", "BLOCK", now)
	// Each observation watches its own promoted revision: an earlier
	// promotion that was accepted, then the latest one that rolled back.
	exec(`INSERT INTO deployment_revisions(id,deployment_id,revision_number,status,spec_json,reason,created_at) SELECT 'rev-earlier',deployment_id,revision_number+100,'superseded',spec_json,'test',created_at FROM deployment_revisions WHERE id=?`, prod.ActiveRevisionID)
	exec(`INSERT INTO release_guard_monitors(id,tenant_id,deployment_id,promoted_revision_id,rollback_revision_id,status,deadline,policy_json,created_at,updated_at) VALUES('mon-1','global',?,'rev-earlier',?,'accepted',?,'{}'::jsonb,?,?),('mon-2','global',?,?,'rev-earlier','rolled_back',?,'{}'::jsonb,?,?)`,
		prod.ID, prod.ActiveRevisionID, now, now.Add(-2*time.Hour), now.Add(-2*time.Hour),
		prod.ID, prod.ActiveRevisionID, now, now.Add(-time.Hour), now.Add(-50*time.Minute))
	rolledBack := now.Add(-50 * time.Minute)
	exec(`INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES('ev-1',?,'revision_auto_rolled_back','auto','{}'::jsonb,?),('ev-2',?,'revision_rolled_back','manual','{}'::jsonb,?),('ev-3',?,'revision_promoted','promoted','{}'::jsonb,?),('ev-4',?,'revision_auto_rolled_back','auto','{}'::jsonb,?)`,
		prod.ID, rolledBack, prod.ID, now.Add(-4*time.Hour), prod.ID, now, other.ID, now)

	environment, err := s.EnvironmentForTenant(ctx, "global", "production")
	if err != nil {
		t.Fatal(err)
	}
	model, err := s.CreateLogicalModel(ctx, "global", domain.LogicalModel{Name: "results-model"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.CreateEndpoint(ctx, "global", domain.Endpoint{Name: "results-canary", LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if err != nil {
		t.Fatal(err)
	}
	// A canary shape: 95% to a closed API behind an external binding, 5% to
	// the owned deployment. A weighted plan needs two distinct providers.
	closedTarget, err := s.AddTarget(ctx, domain.Target{Name: "results-closed-api", URL: "http://results-closed-api", Provider: "closed-api-provider", Runtime: "vllm", UpstreamModel: "model"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "closed-api", Kind: "external", OwnershipMode: "observe-only", TargetID: closedTarget.ID})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := s.CreateBackendBinding(ctx, "global", domain.BackendBinding{EndpointID: endpoint.ID, Name: "owned", Kind: "deployment", OwnershipMode: "lifecycle-managed", DeploymentID: prod.ID})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreateServingPlan(ctx, "global", domain.ServingPlan{EndpointID: endpoint.ID, RoutingPolicy: "weighted", Bindings: []domain.ServingPlanBinding{{BindingID: closed.ID, Priority: 0, Weight: 95}, {BindingID: owned.ID, Priority: 1, Weight: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointPlan(ctx, "global", endpoint.Name, plan.ID, "active"); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO endpoint_release_guard_evaluations(id,tenant_id,endpoint_id,active_serving_plan_id,candidate_serving_plan_id,decision,reason_codes_json,metrics_json,policy_json,created_at) VALUES('eg-1','global',?,?,?,'INCONCLUSIVE','[]'::jsonb,'{}'::jsonb,'{}'::jsonb,?),('eg-2','global',?,?,?,'PASS','[]'::jsonb,'{}'::jsonb,'{}'::jsonb,?)`,
		endpoint.ID, plan.ID, plan.ID, now.Add(-time.Hour), endpoint.ID, plan.ID, plan.ID, now.Add(-time.Minute))

	campaign := func(tenant, id, state, target, proposal string, at time.Time) {
		t.Helper()
		intent, targetName := "new_endpoint", any(nil)
		if target != "" {
			intent, targetName = "evolve_endpoint", target
		}
		exec(`INSERT INTO optimization_campaigns(id,tenant_id,idempotency_key,input_digest,model_identity,objective,source,state,proposal_json,max_candidates,intent,target_deployment_name,created_at,updated_at) VALUES(?,?,?,?,'model','cost-efficiency','test',?,?::jsonb,1,?,?,?,?)`,
			id, tenant, id, strings.Repeat("a", 64), state, proposal, intent, targetName, at, at)
	}
	campaign("global", "camp-old", "rejected", "results-prod", `{}`, now.Add(-2*time.Hour))
	campaign("global", "camp-new", "running", "results-prod", `{}`, now.Add(-time.Hour))
	campaign("global", "camp-migration", "guard_passed", "", `{"input":{"migration":{"endpoint":"results-canary","binding":"openai"}}}`, now.Add(-time.Hour))
	campaign("global", "camp-new-endpoint", "cancelled", "", `{}`, now)
	campaign("results-other", "camp-other", "promoted", "results-other-prod", `{}`, now)

	snapshot, err := s.ResultMetrics(ctx, "global")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != resultmetrics.SchemaVersion || snapshot.Truncated {
		t.Fatalf("snapshot header = %+v", snapshot)
	}
	byName := map[string]resultmetrics.Deployment{}
	for _, d := range snapshot.Deployments {
		byName[d.Name] = d
	}
	if _, leaked := byName["results-other-prod"]; leaked {
		t.Fatal("another workspace's deployment was read")
	}
	got := byName["results-prod"]
	if got.LatestGuard == nil || got.LatestGuard.Decision != "REJECT" || got.LatestGuard.QualityVerdict == nil || *got.LatestGuard.QualityVerdict != "BLOCK" || !got.LatestGuard.EvaluatedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("latest guard = %+v", got.LatestGuard)
	}
	if got.GuardDecisions["ACCEPT"] != 2 || got.GuardDecisions["WAIT"] != 1 || got.GuardDecisions["REJECT"] != 1 {
		t.Fatalf("guard decisions = %v", got.GuardDecisions)
	}
	if got.QualityVerdicts["PASS"] != 1 || got.QualityVerdicts["WATCH"] != 1 || got.QualityVerdicts["BLOCK"] != 1 || got.QualityVerdicts["UNKNOWN"] != 0 {
		t.Fatalf("quality verdicts = %v", got.QualityVerdicts)
	}
	if got.Observation == nil || got.Observation.State != "rolled_back" {
		t.Fatalf("observation = %+v", got.Observation)
	}
	if got.Rollbacks["automatic"] != 1 || got.Rollbacks["operator"] != 1 || got.LastRollbackAt == nil || !got.LastRollbackAt.Equal(rolledBack) {
		t.Fatalf("rollbacks = %v last=%v", got.Rollbacks, got.LastRollbackAt)
	}
	quietGot := byName["results-quiet"]
	if quietGot.LatestGuard != nil || quietGot.Observation != nil || quietGot.LastRollbackAt != nil || quietGot.Rollbacks["automatic"] != 0 || len(quietGot.GuardDecisions) != 3 {
		t.Fatalf("deployment without history = %+v", quietGot)
	}

	var canaryEndpoint *resultmetrics.Endpoint
	for i := range snapshot.Endpoints {
		if snapshot.Endpoints[i].Name == "results-canary" {
			canaryEndpoint = &snapshot.Endpoints[i]
		}
	}
	if canaryEndpoint == nil || canaryEndpoint.RoutingPolicy != "weighted" || canaryEndpoint.CandidateStaged {
		t.Fatalf("endpoint = %+v", canaryEndpoint)
	}
	shares := map[string]string{}
	for _, b := range canaryEndpoint.Bindings {
		shares[b.Name] = fmt.Sprintf("%s %g", b.Kind, b.Share)
	}
	if shares["closed-api"] != "external 0.95" || shares["owned"] != "deployment 0.05" {
		t.Fatalf("shares = %v", shares)
	}
	if canaryEndpoint.LatestGuard == nil || canaryEndpoint.LatestGuard.Decision != "PASS" || canaryEndpoint.GuardDecisions["INCONCLUSIVE"] != 1 || canaryEndpoint.GuardDecisions["PASS"] != 1 {
		t.Fatalf("endpoint guard = %+v %v", canaryEndpoint.LatestGuard, canaryEndpoint.GuardDecisions)
	}

	runs := map[string]resultmetrics.Campaign{}
	for _, c := range snapshot.Campaigns {
		runs[c.Endpoint] = c
	}
	if len(runs) != 2 || runs["results-prod"].ID != "camp-new" || runs["results-prod"].State != "running" || runs["results-canary"].ID != "camp-migration" {
		t.Fatalf("latest runs = %+v", snapshot.Campaigns)
	}
	if snapshot.CampaignStates["running"] != 1 || snapshot.CampaignStates["rejected"] != 1 || snapshot.CampaignStates["cancelled"] != 1 || snapshot.CampaignStates["promoted"] != 0 {
		t.Fatalf("campaign states = %v", snapshot.CampaignStates)
	}
}
