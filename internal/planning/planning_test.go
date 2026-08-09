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

func TestIncompletePlanIsActionable(t *testing.T) {
	p, err := Build(Input{Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != "incomplete" || len(p.Warnings) != 1 {
		t.Fatalf("unexpected plan: %#v", p)
	}
}
