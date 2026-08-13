package planning

import "testing"

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
