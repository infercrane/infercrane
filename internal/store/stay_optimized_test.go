package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/continualoptimizer"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// TestStayOptimizedChecksAreLeasedProposedAndPinned runs the durable side of
// Stay optimized against PostgreSQL: due checks are leased to one instance at
// a time, a completed check schedules the next one, a proposed run keeps its
// origin, auto-run spend is summed per month, and custom kernels move out of
// qualified or serving only once.
func TestStayOptimizedChecksAreLeasedProposedAndPinned(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	model := "Qwen/Qwen3-8B"
	name := "stay-" + suffix
	if _, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://stay-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: model}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: model, Runtime: "vllm"}, []string{name + "-target"})
	if err != nil {
		t.Fatal(err)
	}

	// No policy: nothing is due.
	if claims, claimErr := s.ClaimDueStayOptimizedChecks(ctx, "instance-a", time.Minute, 10); claimErr != nil || len(claims) != 0 {
		t.Fatalf("claims=%+v err=%v", claims, claimErr)
	}
	policy := continualoptimizer.DefaultPolicy()
	if _, err = s.SetContinualOptimizationPolicy(ctx, "global", name, policy); err != nil {
		t.Fatal(err)
	}

	// Two instances race for the one due check; exactly one wins it.
	var mu sync.Mutex
	var won []domain.StayOptimizedCheck
	var wg sync.WaitGroup
	for _, owner := range []string{"instance-a", "instance-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			claims, claimErr := s.ClaimDueStayOptimizedChecks(ctx, owner, time.Minute, 10)
			if claimErr != nil {
				t.Errorf("claim %s: %v", owner, claimErr)
				return
			}
			mu.Lock()
			won = append(won, claims...)
			mu.Unlock()
		}(owner)
	}
	wg.Wait()
	if len(won) != 1 || won[0].DeploymentID != deployment.ID || won[0].DeploymentName != name || won[0].ObservedJSON != "{}" {
		t.Fatalf("won=%+v", won)
	}
	claim := won[0]
	// A person's Check now waits for the running check.
	if _, err = s.ClaimStayOptimizedCheck(ctx, "global", name, "check-now", time.Minute); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("concurrent check now err=%v", err)
	}
	// A check whose lease another owner holds cannot complete it.
	stolen := claim
	stolen.LeaseOwner = "someone-else"
	if err = s.CompleteStayOptimizedCheck(ctx, stolen, `{}`, time.Now().Add(time.Hour), ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("foreign completion err=%v", err)
	}
	next := time.Now().UTC().Add(time.Hour)
	if err = s.CompleteStayOptimizedCheck(ctx, claim, `{"engine_release":"vllm@0.22.1"}`, next, "price unavailable"); err != nil {
		t.Fatal(err)
	}
	state, err := s.ContinualOptimizationState(ctx, "global", name)
	if err != nil || state.NextCheckAt == nil || state.NextCheckAt.Sub(next).Abs() > time.Second || state.LastCheckAt == nil || state.LastCheckError != "price unavailable" || !strings.Contains(state.ObservedJSON, "vllm@0.22.1") {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	// Not due again until the next check time.
	if claims, claimErr := s.ClaimDueStayOptimizedChecks(ctx, "instance-b", time.Minute, 10); claimErr != nil || len(claims) != 0 {
		t.Fatalf("claimed before due: %+v err=%v", claims, claimErr)
	}
	// Check now still runs, and releases on completion.
	manual, err := s.ClaimStayOptimizedCheck(ctx, "global", name, "check-now", time.Minute)
	if err != nil || manual.ObservedJSON == "{}" {
		t.Fatalf("manual=%+v err=%v", manual, err)
	}
	if err = s.CompleteStayOptimizedCheck(ctx, manual, manual.ObservedJSON, next, ""); err != nil {
		t.Fatal(err)
	}
	// A turned-off policy is never due.
	policy.Enabled = false
	if _, err = s.SetContinualOptimizationPolicy(ctx, "global", name, policy); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE continual_optimization_states SET next_check_at=NOW()-INTERVAL '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if claims, claimErr := s.ClaimDueStayOptimizedChecks(ctx, "instance-a", time.Minute, 10); claimErr != nil || len(claims) != 0 {
		t.Fatalf("disabled policy claimed: %+v err=%v", claims, claimErr)
	}

	// A proposed run keeps its origin; only runs that evolve an endpoint may
	// carry one.
	origin := `{"kind":"stay_optimized","deployment":"` + name + `","reason":"vllm 0.22.1 is the new pinned engine release"}`
	candidates := []domain.OptimizationCandidateRun{{ProposalCandidateID: strings.Repeat("2", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"stay"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`}}
	proposed := domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "stay-optimized-" + suffix, InputDigest: strings.Repeat("1", 64), ModelIdentity: model, Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentEvolveEndpoint, TargetDeployment: name, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`, OriginJSON: origin}
	campaign, created, err := s.CreateOptimizationCampaign(ctx, proposed, candidates)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if decoded, ok := continualoptimizer.DecodeOrigin(campaign.OriginJSON); !ok || decoded.Deployment != name || campaign.State != optimizationcampaign.CampaignAwaitingApproval {
		t.Fatalf("campaign=%+v", campaign)
	}
	misplaced := proposed
	misplaced.IdempotencyKey, misplaced.Intent, misplaced.TargetDeployment = "misplaced-"+suffix, optimizationcampaign.IntentNewEndpoint, ""
	if _, _, err = s.CreateOptimizationCampaign(ctx, misplaced, candidates); err == nil {
		t.Fatal("an origin on a new-endpoint run must be refused")
	}
	person := proposed
	person.IdempotencyKey, person.OriginJSON = "person-"+suffix, ""
	if _, _, err = s.CreateOptimizationCampaign(ctx, person, candidates); err != nil {
		t.Fatal(err)
	}
	listed, err := s.StayOptimizedProposals(ctx, "global", name, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != campaign.ID {
		t.Fatalf("proposals=%+v err=%v", listed, err)
	}
	if err = s.SetOptimizationCampaignOrigin(ctx, "global", campaign.ID, strings.Replace(origin, `"reason"`, `"auto_run":{"approved":true,"reason":"within cap"},"reason"`, 1)); err != nil {
		t.Fatal(err)
	}
	monthStart := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if spent, spendErr := s.StayOptimizedAutoRunSpend(ctx, "global", name, monthStart); spendErr != nil || spent != 0 {
		t.Fatalf("spent=%f err=%v", spent, spendErr)
	}
	if _, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, StayOptimizedActor, 12.5, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if spent, spendErr := s.StayOptimizedAutoRunSpend(ctx, "global", name, monthStart); spendErr != nil || spent != 12.5 {
		t.Fatalf("spent=%f err=%v", spent, spendErr)
	}

	// Kernel pins move once, only from qualified or serving.
	kernel, err := s.RecordEndpointCustomKernel(ctx, domain.EndpointCustomKernel{TenantID: "global", DeploymentID: deployment.ID, Operator: "residual-rmsnorm", Family: "norm", Engine: "vllm", EngineVersion: "0.21.0", ImageDigest: "sha256:" + strings.Repeat("c", 64), State: "serving"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointCustomKernelState(ctx, "global", kernel.ID, "serving", "off", "this endpoint runs vllm 0.22.1"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetEndpointCustomKernelState(ctx, "global", kernel.ID, "serving", "off", "again"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second move err=%v", err)
	}
	if err = s.SetEndpointCustomKernelState(ctx, "global", kernel.ID, "off", "serving", "back"); err == nil {
		t.Fatal("Stay optimized never turns a kernel back on")
	}
	kernels, err := s.EndpointCustomKernels(ctx, "global", deployment.ID)
	if err != nil || len(kernels) != 1 || kernels[0].State != "off" || kernels[0].StateReason != "this endpoint runs vllm 0.22.1" {
		t.Fatalf("kernels=%+v err=%v", kernels, err)
	}
	// Re-qualifying on the same engine release records it again.
	if kernel, err = s.RecordEndpointCustomKernel(ctx, domain.EndpointCustomKernel{TenantID: "global", DeploymentID: deployment.ID, Operator: "residual-rmsnorm", Family: "norm", Engine: "vllm", EngineVersion: "0.21.0", ImageDigest: "sha256:" + strings.Repeat("c", 64), State: "qualified"}); err != nil || kernel.State != "qualified" {
		t.Fatalf("kernel=%+v err=%v", kernel, err)
	}
}
