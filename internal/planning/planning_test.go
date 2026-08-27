package planning

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/servingcontract"
)

func TestBuildProvisionedPlan(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", Cloud: "runpod", GPU: "L40S"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "qwen3-8b" || p.Mode != "provisioned" || len(p.Actions) != 6 || p.Readiness.EstimateStatus != "unavailable" || p.Readiness.ArtifactCacheState != "unknown" || len(p.Readiness.Stages) != 5 {
		t.Fatalf("unexpected plan: %#v", p)
	}
}

func TestBuildRejectsExcludedCloud(t *testing.T) {
	_, err := Build(Input{Model: "model", Cloud: "unqualified-cloud", GPU: "L4"})
	if err == nil {
		t.Fatal("support policy must reject unqualified clouds")
	}
}
func TestBuildRejectsExcludedRuntime(t *testing.T) {
	_, err := Build(Input{Model: "model", Runtime: "sglang", Cloud: "runpod", GPU: "L40S"})
	if err == nil {
		t.Fatal("support policy must reject unsupported runtimes")
	}
}

func TestBuildAWSElasticPlan(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", Cloud: "aws", GPU: "L40S", Region: "eu-central-1"})
	if err != nil || p.Mode != "provisioned" || p.Cloud != "aws" {
		t.Fatalf("plan=%#v err=%v", p, err)
	}
}

func TestBuildKubernetesElasticPlan(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", Cloud: "kubernetes", GPU: "NVIDIA-L40S"})
	if err != nil || p.Mode != "provisioned" || p.Cloud != "kubernetes" {
		t.Fatalf("plan=%#v err=%v", p, err)
	}
}

func TestBuildDynamoPlanPreservesTopologyAndRejectsCompetingOuterScale(t *testing.T) {
	topology := servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
	p, err := Build(Input{Model: "meta-llama/Llama-3.1-8B-Instruct", Runtime: "vllm", Cloud: "kubernetes", ProviderAdapter: "kubernetes-dynamo", GPU: "NVIDIA-L40S", MinReplicas: 1, MaxReplicas: 1, Serving: topology})
	if err != nil || p.ProviderAdapter != "kubernetes-dynamo" || p.Serving.Normalize() != topology.Normalize() {
		t.Fatalf("plan=%#v err=%v", p, err)
	}
	_, err = Build(Input{Model: "model", Runtime: "vllm", Cloud: "kubernetes", ProviderAdapter: "kubernetes-dynamo", GPU: "NVIDIA-L40S", MinReplicas: 1, MaxReplicas: 2, Serving: topology})
	if err == nil || !strings.Contains(err.Error(), "outer replica bounds") {
		t.Fatalf("competing scale ownership accepted: %v", err)
	}
}

func TestCompareDetectsServingTopologyAndAdapterChanges(t *testing.T) {
	topology := servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
	p, err := Build(Input{Model: "model", Runtime: "vllm", Cloud: "kubernetes", ProviderAdapter: "kubernetes-dynamo", GPU: "NVIDIA-L40S", MinReplicas: 1, MaxReplicas: 1, Serving: topology})
	if err != nil {
		t.Fatal(err)
	}
	p = Compare(p, Current{Model: "model", Runtime: "vllm", Routing: "round-robin", ComputeMode: "elastic", Cloud: "kubernetes", ProviderAdapter: "kubernetes", GPU: "NVIDIA-L40S", MinReplicas: 1, MaxReplicas: 1, ActiveRevision: "rev-1", ActiveRevisionNumber: 1})
	if len(p.Changes) != 3 || p.Changes[0].Field != "provider adapter" || p.Changes[1].Field != "serving topology" || p.Changes[2].Field != "revision" {
		t.Fatalf("changes=%#v", p.Changes)
	}
}

func TestBuildRejectsMixedModes(t *testing.T) {
	_, err := Build(Input{Model: "model", Targets: []string{"gpu-a"}, Cloud: "aws", GPU: "L4"})
	if err == nil {
		t.Fatal("expected mixed mode error")
	}
}

func TestExistingTargetPlanDoesNotClaimProviderReadinessEvidence(t *testing.T) {
	p, err := Build(Input{Model: "model", Targets: []string{"worker-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Readiness.EstimateStatus != "externally-managed" || p.Readiness.ArtifactCacheState != "not-observed" || p.Readiness.CapacityState != "not-observed" || len(p.Readiness.Stages) != 0 {
		t.Fatalf("unexpected readiness evidence: %#v", p.Readiness)
	}
}

func TestApplyCapacityEvidenceRequiresMatchingStatisticalEvidence(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", Cloud: "aws", GPU: "L40S", Region: "eu-central-1", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	p50, p95 := 410.0, 720.0
	p = ApplyCapacityEvidence(p, []CapacityEvidence{
		{Provider: "aws-ec2", Runtime: "sglang", ComputeMode: "elastic", Region: "eu-central-1", GPU: "L40S", Attempts: 30, Succeeded: 30, SuccessRate: 1, DurationP50Seconds: &p50, DurationP95Seconds: &p95},
		{Provider: "aws-ec2", Runtime: "vllm", ComputeMode: "elastic", Region: "eu-central-1", GPU: "L40S", Attempts: 23, Succeeded: 20, Pending: 1, SuccessRate: 20.0 / 22.0, DurationP50Seconds: &p50, DurationP95Seconds: &p95},
	})
	if p.Readiness.EstimateStatus != "observed" || p.Readiness.EstimateP50Seconds == nil || *p.Readiness.EstimateP50Seconds != p50 || p.Readiness.EstimateP95Seconds == nil || p.Readiness.SuccessfulSamples != 20 {
		t.Fatalf("matching readiness evidence was not applied: %#v", p.Readiness)
	}
	if !strings.Contains(p.Readiness.CapacityState, "22 terminal") || p.Readiness.EvidenceBoundary == "" {
		t.Fatalf("readiness provenance is incomplete: %#v", p.Readiness)
	}
}

func TestApplyCapacityEvidenceUsesOnlyExactTupleStageHistory(t *testing.T) {
	p, err := Build(Input{Model: "org/model", ModelRevision: strings.Repeat("a", 40), Runtime: "vllm", RuntimeVersion: "1.2.3", RuntimeArgs: []string{"--tp", "2"}, Cloud: "aws", GPU: "H200", GPUCount: 2, Region: "eu-central-1", MinReplicas: 1, MaxReplicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	p50 := 120.0
	wrong := CapacityEvidence{Provider: "aws-ec2", Runtime: "vllm", RuntimeVersion: "1.2.3", RuntimeArgsDigest: p.RuntimeArgsDigest, ModelIdentity: "org/other@" + strings.Repeat("a", 40), ComputeMode: "elastic", Region: "eu-central-1", GPU: "H200", GPUCount: 2, Attempts: 20, Succeeded: 20, SuccessRate: 1, DurationP50Seconds: &p50, StartupStages: []StageEstimate{{Name: "image_pull", SuccessfulSamples: 20, EstimateP50Seconds: &p50}}}
	matching := wrong
	matching.ModelIdentity = "org/model@" + strings.Repeat("a", 40)
	p = ApplyCapacityEvidence(p, []CapacityEvidence{wrong, matching})
	if len(p.Readiness.StageEstimates) != 1 || p.Readiness.StageEstimates[0].Name != "image_pull" {
		t.Fatalf("exact stage evidence was not applied: %#v", p.Readiness.StageEstimates)
	}
	matching.RuntimeArgsDigest = "sha256:other"
	p.Readiness.StageEstimates = nil
	p = ApplyCapacityEvidence(p, []CapacityEvidence{matching})
	if len(p.Readiness.StageEstimates) != 0 {
		t.Fatalf("runtime argument mismatch leaked stage evidence: %#v", p.Readiness.StageEstimates)
	}
}

func TestApplyCapacityEvidenceDoesNotCrossAcceleratorCounts(t *testing.T) {
	p, err := Build(Input{Model: "model", Cloud: "aws", GPU: "H200", GPUCount: 4, Region: "eu-central-1", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	p50 := 100.0
	p = ApplyCapacityEvidence(p, []CapacityEvidence{{Provider: "aws-ec2", Runtime: "vllm", ComputeMode: "elastic", Region: "eu-central-1", GPU: "H200", GPUCount: 1, Attempts: 10, Succeeded: 10, SuccessRate: 1, DurationP50Seconds: &p50}})
	if p.Readiness.EstimateP50Seconds != nil || p.Readiness.EstimateStatus != "unavailable" {
		t.Fatalf("single-GPU evidence leaked into a four-GPU plan: %+v", p.Readiness)
	}
}

func TestApplyCapacityEvidenceWithholdsWeakPrediction(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", Cloud: "aws", GPU: "L40S", Region: "eu-central-1", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	p50 := 410.0
	p = ApplyCapacityEvidence(p, []CapacityEvidence{{Provider: "aws-ec2", Runtime: "vllm", ComputeMode: "elastic", Region: "eu-central-1", GPU: "L40S", Attempts: 2, Succeeded: 2, SuccessRate: 1, DurationP50Seconds: &p50}})
	if p.Readiness.EstimateStatus != "unavailable" || p.Readiness.EstimateP50Seconds != nil || !strings.Contains(p.Readiness.Reason, "at least 3") {
		t.Fatalf("weak readiness evidence was presented as a prediction: %#v", p.Readiness)
	}
}

func TestBuildServerlessPlanKeepsWorkersAtZero(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", ComputeMode: "serverless", Cloud: "runpod", GPU: "L40S", MinReplicas: 0, MaxReplicas: 4})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != "serverless" || p.MinReplicas != 0 || p.MaxReplicas != 4 || p.Actions[len(p.Actions)-1].Summary != "Delegate zero-to-4 worker scaling to the provider backend" {
		t.Fatalf("unexpected plan: %#v", p)
	}
}

func TestIncompletePlanIsActionable(t *testing.T) {
	p, err := Build(Input{Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != "incomplete" || len(p.Warnings) != 1 {
		t.Fatalf("unexpected plan: %#v", p)
	}
}

func TestCompareProducesSemanticRevisionPlan(t *testing.T) {
	p, err := Build(Input{Model: "model-v2", Cloud: "runpod", GPU: "L40S", MinReplicas: 2, MaxReplicas: 4})
	if err != nil {
		t.Fatal(err)
	}
	p = Compare(p, Current{Model: "model-v1", Runtime: "vllm", ComputeMode: "elastic", Cloud: "runpod", GPU: "A40", Routing: "round-robin", MinReplicas: 1, MaxReplicas: 2, ActiveRevision: "deployment-rev-18", ActiveRevisionNumber: 18})
	if len(p.Changes) != 4 || p.Changes[1] != (Change{Field: "GPU", Before: "A40", After: "L40S"}) || p.Changes[3] != (Change{Field: "revision", Before: "rev-18", After: "candidate rev-19"}) || len(p.Actions) != 6 {
		t.Fatalf("unexpected comparison: %#v", p)
	}
}
