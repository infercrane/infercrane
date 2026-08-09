package planning

import "testing"

func TestBuildProvisionedPlan(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", Cloud: "aws", GPU: "L4"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "qwen3-8b" || p.Mode != "provisioned" || len(p.Actions) != 6 {
		t.Fatalf("unexpected plan: %#v", p)
	}
}

func TestBuildRejectsMixedModes(t *testing.T) {
	_, err := Build(Input{Model: "model", Targets: []string{"gpu-a"}, Cloud: "aws", GPU: "L4"})
	if err == nil {
		t.Fatal("expected mixed mode error")
	}
}

func TestBuildServerlessPlanKeepsWorkersAtZero(t *testing.T) {
	p, err := Build(Input{Model: "Qwen/Qwen3-8B", ComputeMode: "serverless", Cloud: "runpod", GPU: "L40S", MinReplicas: 0, MaxReplicas: 4})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != "serverless" || p.MinReplicas != 0 || p.MaxReplicas != 4 || p.Actions[len(p.Actions)-1].Summary != "Delegate zero-to-4 worker scaling to RunPod" {
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
	p = Compare(p, Current{Model: "model-v1", Runtime: "vllm", Routing: "round-robin", MinReplicas: 1, MaxReplicas: 2, ActiveRevision: "deployment-rev-18", ActiveRevisionNumber: 18})
	if len(p.Changes) != 3 || p.Changes[2].After != "candidate rev-19" || len(p.Actions) != 6 {
		t.Fatalf("unexpected comparison: %#v", p)
	}
}
