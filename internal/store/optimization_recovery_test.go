package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// A measuring candidate's checkpoint is decision evidence written only while
// it measures, and a setup's replaced machines are counted once per lost
// replica up to the limit.
func TestOptimizationRecoveryRecordsCheckpointAndBoundedReplacements(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	model := "Qwen/Qwen3-0.6B"
	target, err := s.AddTarget(ctx, domain.Target{Name: "recovery-target-" + suffix, URL: "http://recovery-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: model})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "recovery-" + suffix, Model: model}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "recovery-" + suffix, InputDigest: strings.Repeat("5", 64), ModelIdentity: model, Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`}, []domain.OptimizationCandidateRun{
		{ProposalCandidateID: strings.Repeat("c", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"qwen"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if campaign.Candidates[0].MeasurementCheckpointJSON != "{}" || campaign.Candidates[0].MachineReplacementsJSON != "[]" {
		t.Fatalf("new candidate recovery state = %+v", campaign.Candidates[0])
	}
	if campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	id := campaign.Candidates[0].ID
	if written, recordErr := s.RecordOptimizationMeasurementCheckpoint(ctx, "global", campaign.ID, id, `{"version":1}`); recordErr != nil || written {
		t.Fatalf("checkpoint written before measuring: written=%t err=%v", written, recordErr)
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
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, id, state, step.to, step.updates); err != nil {
			t.Fatalf("transition to %s: %v", step.to, err)
		}
		state = step.to
	}
	before, err := s.OptimizationCampaign(ctx, "global", campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := `{"version":1,"identity":"sha256:x","interruptions":[{"sequence":1,"arm":"candidate"}]}`
	if written, recordErr := s.RecordOptimizationMeasurementCheckpoint(ctx, "global", campaign.ID, id, checkpoint); recordErr != nil || !written {
		t.Fatalf("checkpoint while measuring: written=%t err=%v", written, recordErr)
	}
	if _, recordErr := s.RecordOptimizationMeasurementCheckpoint(ctx, "global", campaign.ID, id, strings.Repeat(" ", 300<<10)+"{}"); recordErr == nil {
		t.Fatal("unbounded checkpoint was accepted")
	}
	after, err := s.OptimizationCampaign(ctx, "global", campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Candidates[0].MeasurementCheckpointJSON, `"sha256:x"`) || !after.Candidates[0].UpdatedAt.Equal(before.Candidates[0].UpdatedAt) {
		t.Fatalf("checkpoint=%s updated_at moved from %s to %s", after.Candidates[0].MeasurementCheckpointJSON, before.Candidates[0].UpdatedAt, after.Candidates[0].UpdatedAt)
	}

	record := func(entry string, limit int) (string, error) {
		return s.RecordOptimizationMachineReplacement(ctx, "global", campaign.ID, id, entry, limit)
	}
	if _, err = record(`{"lost_replica_id":"first","number":1,"state":"provisioning"}`, 2); err != nil {
		t.Fatal(err)
	}
	updated, err := record(`{"lost_replica_id":"first","state":"replaced","operation_id":"op-1"}`, 2)
	entries := optimizationcampaign.DecodeMachineReplacements(updated)
	if err != nil || len(entries) != 1 || entries[0].State != optimizationcampaign.ReplacementReplaced || entries[0].Number != 1 || entries[0].OperationID != "op-1" {
		t.Fatalf("a retried entry must update itself, not count twice: %+v err=%v", entries, err)
	}
	if _, err = record(`{"lost_replica_id":"second","number":2,"state":"provisioning"}`, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = record(`{"lost_replica_id":"third","number":3,"state":"provisioning"}`, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("a third replacement past the limit: %v", err)
	}
	if _, err = record(`{"lost_replica_id":"stopped:candidate","state":"given_up"}`, 128); err != nil {
		t.Fatalf("a stop marker must not be refused at the replacement limit: %v", err)
	}
	if _, err = record(`{"state":"provisioning"}`, 2); err == nil {
		t.Fatal("an entry without a lost replica was accepted")
	}
	final, err := s.OptimizationCampaign(ctx, "global", campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries = optimizationcampaign.DecodeMachineReplacements(final.Candidates[0].MachineReplacementsJSON)
	if len(entries) != 3 || optimizationcampaign.MachineReplacementsUsed(entries) != 2 || entries[0].LostReplicaID != "first" || entries[2].State != optimizationcampaign.ReplacementGivenUp {
		t.Fatalf("entries = %+v", entries)
	}
}

// A lost machine's replica row stays deleted with its provider identity, and
// its replacement takes the same revision ordinal under a new identity. Two
// live replicas can never share an ordinal.
func TestReplacementReplicaTakesTheSameOrdinalWithANewIdentity(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	target, err := s.AddTarget(ctx, domain.Target{Name: "replace-target-" + suffix, URL: "http://replace-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: "Qwen/Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "replace-" + suffix, Model: "Qwen/Qwen3-0.6B"}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	intent := func(key string) (domain.Replica, error) {
		replica, _, ensureErr := s.EnsureReplicaIntent(ctx, domain.Replica{TenantID: "global", DeploymentID: deployment.ID, RevisionID: deployment.ActiveRevisionID, Ordinal: 0, ExternalKey: key, Provider: "existing"})
		return replica, ensureErr
	}
	lost, err := intent(deployment.ID + "-r0")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetReplicaProviderIdentity(ctx, lost.ID, "", "resource-lost"); err != nil {
		t.Fatal(err)
	}
	if _, err = intent(deployment.ID + "-r0-g1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second live replica took the same ordinal: %v", err)
	}
	if err = s.MarkReplicaDeleted(ctx, lost.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := intent(deployment.ID + "-r0-g1")
	if err != nil || replacement.ID == lost.ID || replacement.Ordinal != 0 {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
	replicas, err := s.ReplicasForDeployment(ctx, "global", deployment.ID)
	if err != nil || len(replicas) != 2 {
		t.Fatalf("replicas=%+v err=%v", replicas, err)
	}
	for _, replica := range replicas {
		if replica.ID == lost.ID && (replica.LifecycleState != "deleted" || replica.ProviderResourceID != "resource-lost") {
			t.Fatalf("the lost replica's audit row changed: %+v", replica)
		}
	}
}

// Releasing a lost machine's target removes its deployment binding, so the
// replacement can register the same name at a new address.
func TestReleaseProvisionedTargetFreesTheNameForTheReplacement(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	name := "release-" + suffix + "-r0"
	target, err := s.AddTargetForTenant(ctx, "global", domain.Target{Name: name, URL: "http://lost-" + suffix, Provider: "fixture", Runtime: "vllm", UpstreamModel: "Qwen/Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyDeploymentForTenant(ctx, "global", domain.Deployment{Name: "release-" + suffix, Model: "Qwen/Qwen3-0.6B"}, []string{target.Name}); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteProvisionedTarget(ctx, "global", name, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddTargetForTenant(ctx, "global", domain.Target{Name: name, URL: "http://replacement-" + suffix, Provider: "fixture", Runtime: "vllm"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a bound target was replaced without being released: %v", err)
	}
	if err = s.ReleaseProvisionedTarget(ctx, "global", name, "other-provider"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseProvisionedTarget(ctx, "global", name, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseProvisionedTarget(ctx, "global", name, "fixture"); err != nil {
		t.Fatalf("releasing a missing target must be a no-op: %v", err)
	}
	if replacement, err := s.AddTargetForTenant(ctx, "global", domain.Target{Name: name, URL: "http://replacement-" + suffix, Provider: "fixture", Runtime: "vllm"}); err != nil || replacement.ID == target.ID {
		t.Fatalf("replacement target=%+v err=%v", replacement, err)
	}
}
