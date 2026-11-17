package support

import (
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

func TestVLLMWorkloadPinsImageAndResolvedModelRevision(t *testing.T) {
	workload := VLLMWorkload()
	if err := workload.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(workload.Image, "@sha256:") || workload.Command[2] != "${MODEL}" || workload.Command[4] != "${MODEL_REVISION}" {
		t.Fatalf("workload=%#v", workload)
	}
}

func TestSGLangWorkloadPinsResolvedModelRevision(t *testing.T) {
	command := SGLangWorkload().Command
	for _, pair := range [][2]string{{"--model-path", "${MODEL}"}, {"--revision", "${MODEL_REVISION}"}} {
		index := slices.Index(command, pair[0])
		if index < 0 || index+1 >= len(command) || command[index+1] != pair[1] {
			t.Fatalf("SGLang command does not preserve %s identity: %v", pair[0], command)
		}
	}
}

func TestMatrixCanQualifyNewAdaptersWithoutChangingValidationCode(t *testing.T) {
	matrix := New([]string{"vllm", "future-runtime"}, map[string][]string{
		"runpod":       {"elastic", "serverless"},
		"future-cloud": {"elastic"},
	})
	if err := matrix.Validate("future-runtime", "future-cloud", "elastic"); err != nil {
		t.Fatal(err)
	}
	if err := matrix.Validate("future-runtime", "future-cloud", "serverless"); err == nil {
		t.Fatal("unqualified provider mode was accepted")
	}
}

func TestV1QualificationIsExact(t *testing.T) {
	matrix := V1()
	qualified := [][3]string{
		{"vllm", "runpod", "elastic"},
		{"vllm", "runpod", "serverless"},
		{"custom-oci", "runpod", "elastic"},
		{"vllm", "aws", "elastic"},
		{"sglang", "gcp", "elastic"},
		{"custom-oci", "kubernetes", "elastic"},
	}
	for _, combination := range qualified {
		if err := matrix.Validate(combination[0], combination[1], combination[2]); err != nil {
			t.Fatalf("qualified combination %v was rejected: %v", combination, err)
		}
	}
	unqualified := [][3]string{
		{"sglang", "runpod", "elastic"},
		{"custom-oci", "runpod", "serverless"},
		{"vllm", "aws", "serverless"},
		{"vllm", "kubernetes", "serverless"},
		{"vllm", "unknown", "elastic"},
	}
	for _, combination := range unqualified {
		if err := matrix.Validate(combination[0], combination[1], combination[2]); err == nil {
			t.Fatalf("unqualified combination accepted: %v", combination)
		}
	}
}

func TestValidateProviderAdmitsOnlyDeclaredSkyPilotAdapters(t *testing.T) {
	matrix := V1()
	if err := matrix.ValidateProvider(DefaultRuntime, "vast", ElasticMode, "skypilot-vast"); err != nil {
		t.Fatalf("declared SkyPilot cloud rejected: %v", err)
	}
	for _, tc := range []struct{ runtime, cloud, mode, adapter string }{
		{DefaultRuntime, "vast", ElasticMode, ""},
		{DefaultRuntime, "vast", ElasticMode, "skypilot-lambda"},
		{DefaultRuntime, "vast", ServerlessMode, "skypilot-vast"},
		{"made-up-runtime", "vast", ElasticMode, "skypilot-vast"},
		{DefaultRuntime, "Vast_AI", ElasticMode, "skypilot-Vast_AI"},
	} {
		if err := matrix.ValidateProvider(tc.runtime, tc.cloud, tc.mode, tc.adapter); err == nil {
			t.Fatalf("ValidateProvider(%+v) admitted an undeclared combination", tc)
		}
	}
	if err := matrix.ValidateProvider(DefaultRuntime, "aws", ElasticMode, ""); err != nil {
		t.Fatalf("qualified cloud rejected: %v", err)
	}
}

func TestServedWorkloadIsTheBuiltInDefaultForAnEmptyDraft(t *testing.T) {
	for _, runtime := range []string{"", "vllm"} {
		if served := ServedWorkload(runtime, runtimecontract.Workload{}); served.MetricsPath != "/metrics" || served.Image != VLLMWorkload().Image {
			t.Fatalf("runtime %q served %#v", runtime, served)
		}
	}
	if served := ServedWorkload("sglang", runtimecontract.Workload{}); served.Image != SGLangWorkload().Image {
		t.Fatalf("sglang served %#v", served)
	}
	// A custom image declares its own contract; an empty one has no paths.
	if served := ServedWorkload("custom-oci", runtimecontract.Workload{}); !served.Empty() {
		t.Fatalf("custom-oci served %#v", served)
	}
	declared := runtimecontract.Workload{Image: "example/server@sha256:abc", Protocol: "openai", MetricsPath: ""}
	if served := ServedWorkload("vllm", declared); served.Image != declared.Image || served.MetricsPath != "" {
		t.Fatalf("an explicit workload was replaced: %#v", served)
	}
}
