package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

func endpointNames(endpoints []domain.Endpoint) []string {
	names := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		names = append(names, endpoint.Name)
	}
	return names
}

// A run's progress is recorded on its candidates: when each state was first
// entered, the live measurement (only while measuring), and the first quote
// that authorized its servers. Replica lifetimes are read back for spend.
func TestOptimizationRunProgressRecordsStateTimesMeasurementAndQuote(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	model := "Qwen/Qwen3-0.6B"
	target, err := s.AddTarget(ctx, domain.Target{Name: "progress-target-" + suffix, URL: "http://progress-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: model})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "progress-" + suffix, Model: model}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "progress-" + suffix, InputDigest: strings.Repeat("4", 64), ModelIdentity: model, Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`}, []domain.OptimizationCandidateRun{
		{ProposalCandidateID: strings.Repeat("a", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"qwen-rtx-optimized"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
		{ProposalCandidateID: strings.Repeat("b", 64), Rank: 2, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"qwen-rtx-optimized"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposedAt := optimizationcampaign.DecodeStateEnteredAt(campaign.Candidates[0].StateEnteredAtJSON)[optimizationcampaign.CandidateProposed]
	if proposedAt.IsZero() || campaign.Candidates[0].MeasurementProgressJSON != "{}" || campaign.Candidates[0].CostQuoteJSON != "{}" {
		t.Fatalf("new candidate progress = %+v", campaign.Candidates[0])
	}
	if campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	first := campaign.Candidates[0].ID
	if err = s.RecordOptimizationCandidateCostQuote(ctx, "global", campaign.ID, first, `{"hourly_usd":0.4,"max_replicas":1,"source":"vast-offers"}`); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordOptimizationCandidateCostQuote(ctx, "global", campaign.ID, first, `{"hourly_usd":9,"max_replicas":1,"source":"later"}`); err != nil {
		t.Fatal(err)
	}
	if written, recordErr := s.RecordOptimizationMeasurementProgress(ctx, "global", campaign.ID, first, `{"method":"m"}`); recordErr != nil || written {
		t.Fatalf("progress was written before the candidate measured: written=%t err=%v", written, recordErr)
	}
	state := optimizationcampaign.CandidateProposed
	for _, step := range []struct {
		to      string
		updates domain.OptimizationCandidateRun
	}{
		{optimizationcampaign.CandidateProvisioning, domain.OptimizationCandidateRun{}},
		{optimizationcampaign.CandidateReady, domain.OptimizationCandidateRun{DeploymentName: deployment.Name, RevisionID: deployment.ActiveRevisionID}},
		{optimizationcampaign.CandidateMeasuring, domain.OptimizationCandidateRun{}},
	} {
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, first, state, step.to, step.updates); err != nil {
			t.Fatalf("transition to %s: %v", step.to, err)
		}
		state = step.to
	}
	progress := `{"method":"campaign-interleaved-control-v1","turns_planned":3,"runs":[{"kind":"turn","arm":"candidate","turn":1}]}`
	if written, recordErr := s.RecordOptimizationMeasurementProgress(ctx, "global", campaign.ID, first, progress); recordErr != nil || !written {
		t.Fatalf("progress while measuring: written=%t err=%v", written, recordErr)
	}
	if _, recordErr := s.RecordOptimizationMeasurementProgress(ctx, "global", campaign.ID, first, strings.Repeat(" ", 40<<10)+"{}"); recordErr == nil {
		t.Fatal("unbounded measurement progress was accepted")
	}
	if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, first, state, optimizationcampaign.CandidateValidating, domain.OptimizationCandidateRun{BenchmarkID: "bench-1"}); err != nil {
		t.Fatal(err)
	}
	if written, _ := s.RecordOptimizationMeasurementProgress(ctx, "global", campaign.ID, first, `{"method":"late"}`); written {
		t.Fatal("a late write replaced the progress of a candidate that finished measuring")
	}
	if campaign, err = s.CancelOptimizationCampaign(ctx, "global", campaign.ID); err != nil {
		t.Fatal(err)
	}
	measured, waiting := campaign.Candidates[0], campaign.Candidates[1]
	entered := optimizationcampaign.DecodeStateEnteredAt(measured.StateEnteredAtJSON)
	for _, name := range []string{"proposed", "provisioning", "ready", "measuring", "validating", "cancelled"} {
		if entered[name].IsZero() {
			t.Fatalf("state %s has no entry time: %s", name, measured.StateEnteredAtJSON)
		}
	}
	if !entered["proposed"].Equal(proposedAt) || entered["measuring"].Before(entered["ready"]) || entered["cancelled"].Before(entered["validating"]) {
		t.Fatalf("entry times were rewritten or out of order: %s", measured.StateEnteredAtJSON)
	}
	if !strings.Contains(measured.MeasurementProgressJSON, `"turns_planned": 3`) || !strings.Contains(measured.CostQuoteJSON, `"vast-offers"`) || strings.Contains(measured.CostQuoteJSON, "later") {
		t.Fatalf("measurement=%s quote=%s", measured.MeasurementProgressJSON, measured.CostQuoteJSON)
	}
	if optimizationcampaign.DecodeStateEnteredAt(waiting.StateEnteredAtJSON)["cancelled"].IsZero() {
		t.Fatalf("a cancelled candidate has no cancellation time: %s", waiting.StateEnteredAtJSON)
	}

	replica, _, err := s.EnsureReplicaIntent(ctx, domain.Replica{TenantID: "global", DeploymentID: deployment.ID, RevisionID: deployment.ActiveRevisionID, Ordinal: 0, ExternalKey: deployment.ID + "-progress-r0", Provider: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	spans, err := s.OptimizationReplicaSpans(ctx, "global", "", deployment.ActiveRevisionID, time.Time{})
	if err != nil || len(spans) != 1 || spans[0].ReplicaID != replica.ID || spans[0].DeletedAt != nil || spans[0].CreatedAt.IsZero() {
		t.Fatalf("live replica spans=%+v err=%v", spans, err)
	}
	if err = s.MarkReplicaDeleted(ctx, replica.ID); err != nil {
		t.Fatal(err)
	}
	spans, err = s.OptimizationReplicaSpans(ctx, "global", deployment.Name, "", time.Time{})
	if err != nil || len(spans) != 1 || spans[0].DeletedAt == nil || spans[0].DeletedAt.Before(spans[0].CreatedAt) {
		t.Fatalf("deleted replica spans by deployment=%+v err=%v", spans, err)
	}
	if spans, err = s.OptimizationReplicaSpans(ctx, "global", deployment.Name, "", time.Now().Add(time.Hour)); err != nil || len(spans) != 0 {
		t.Fatalf("a deployment created before the candidate was attributed to it: %+v err=%v", spans, err)
	}
	if spans, err = s.OptimizationReplicaSpans(ctx, "other-tenant", "", deployment.ActiveRevisionID, time.Time{}); err != nil || len(spans) != 0 {
		t.Fatalf("another tenant read replica spans: %+v err=%v", spans, err)
	}
}

// Every new deployment row gets an endpoint of the same name from the
// deployments_create_legacy_endpoint trigger, including campaign candidates
// that converge never publishes. The customer list leaves those out, during
// provisioning and after; routing still sees them.
func TestCustomerEndpointsLeaveOutNewEndpointCandidates(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405000")
	model := "Qwen/Qwen3-0.6B"
	target, err := s.AddTarget(ctx, domain.Target{Name: "list-target-" + suffix, URL: "http://list-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: model})
	if err != nil {
		t.Fatal(err)
	}
	customer, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "qwen-prod-" + suffix, Model: model}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"name":"qwen-rtx-optimized"}`
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "list-" + suffix, InputDigest: strings.Repeat("5", 64), ModelIdentity: model, Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`}, []domain.OptimizationCandidateRun{
		{ProposalCandidateID: strings.Repeat("a", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: spec, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
		{ProposalCandidateID: strings.Repeat("b", 64), Rank: 2, EvidenceState: "unmeasured", DeploymentSpecJSON: spec, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The first candidate reached ready, so its deployment name is recorded;
	// the second is still provisioning under the name the driver gives it.
	ready, provisioning := campaign.Candidates[0], campaign.Candidates[1]
	readyName, provisioningName := "qwen-rtx-optimized-opt-"+ready.ID[:8], "qwen-rtx-optimized-opt-"+provisioning.ID[:8]
	readyDeployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: readyName, Model: model}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyDeployment(ctx, domain.Deployment{Name: provisioningName, Model: model}, []string{target.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, ready.ID, optimizationcampaign.CandidateProposed, optimizationcampaign.CandidateProvisioning, domain.OptimizationCandidateRun{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, ready.ID, optimizationcampaign.CandidateProvisioning, optimizationcampaign.CandidateReady, domain.OptimizationCandidateRun{DeploymentName: readyName, RevisionID: readyDeployment.ActiveRevisionID}); err != nil {
		t.Fatal(err)
	}
	// Activation publishes the winner under the requested name.
	if _, err = s.PublishDeploymentEndpoint(ctx, "global", "qwen-rtx-optimized", readyName); err != nil {
		t.Fatal(err)
	}
	listed, err := s.CustomerEndpointsForTenant(ctx, "global")
	if err != nil {
		t.Fatal(err)
	}
	if names := endpointNames(listed); !slices.Equal(names, []string{customer.Name, "qwen-rtx-optimized"}) {
		t.Fatalf("customer endpoints = %v", names)
	}
	routed, err := s.EndpointsForTenant(ctx, "global")
	if err != nil {
		t.Fatal(err)
	}
	if names := endpointNames(routed); !slices.Contains(names, readyName) || !slices.Contains(names, provisioningName) {
		t.Fatalf("routing lost candidate endpoints: %v", names)
	}
}
