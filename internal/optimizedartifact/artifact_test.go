package optimizedartifact

import (
	"encoding/json"
	"testing"
)

func validPlan() Plan {
	return Plan{BaseModelArtifactID: "artifact-1", Kind: KindQuantized, Format: "safetensors", Tool: "llm-compressor", ToolVersion: "0.9.0", Algorithm: "w8a8-fp8", BuilderImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CalibrationDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", LicenseSPDX: "Apache-2.0", Configuration: json.RawMessage(`{"scheme":"FP8"}`), HardwareConstraints: json.RawMessage(`{"minimum_compute_capability":"8.9"}`), RequiresQualityReview: true}
}

func TestPlanFailsClosedOnUnpinnedOrMismatchedBuilders(t *testing.T) {
	plan := validPlan()
	first, err := InputDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := InputDigest(plan)
	if first != second || len(first) != 64 {
		t.Fatalf("input digest is not deterministic: %q %q", first, second)
	}
	for name, mutate := range map[string]func(*Plan){
		"unpinned builder":     func(value *Plan) { value.BuilderImageDigest = "latest" },
		"tool kind mismatch":   func(value *Plan) { value.Tool = "tensorrt-llm" },
		"missing quality gate": func(value *Plan) { value.RequiresQualityReview = false },
		"invalid constraints":  func(value *Plan) { value.HardwareConstraints = json.RawMessage(`[]`) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := plan
			mutate(&candidate)
			if err := ValidatePlan(candidate); err == nil {
				t.Fatal("unsafe plan should fail")
			}
		})
	}
}

func TestAttestationCannotConfuseFailureWithImmutableOutput(t *testing.T) {
	ready := Attestation{OutputRepository: "org/model-fp8", OutputImmutableRevision: "commit", OutputDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", BuildEvidence: json.RawMessage(`{"builder":"qualified-job"}`)}
	if err := ValidateAttestation(StateReady, ready); err != nil {
		t.Fatal(err)
	}
	ready.OutputDigest = ""
	if err := ValidateAttestation(StateReady, ready); err == nil {
		t.Fatal("ready output without digest should fail")
	}
	failed := Attestation{FailureCode: "builder_oom", BuildEvidence: json.RawMessage(`{"exit_code":137}`)}
	if err := ValidateAttestation(StateFailed, failed); err != nil {
		t.Fatal(err)
	}
	failed.OutputRepository = "partial-output"
	if err := ValidateAttestation(StateFailed, failed); err == nil {
		t.Fatal("failed build cannot claim output identity")
	}
}
