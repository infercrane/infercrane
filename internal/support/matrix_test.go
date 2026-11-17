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
		if served := ServedWorkload(runtime, "H100", runtimecontract.Workload{}); served.MetricsPath != "/metrics" || served.Image != VLLMWorkload().Image {
			t.Fatalf("runtime %q served %#v", runtime, served)
		}
	}
	if served := ServedWorkload("sglang", "", runtimecontract.Workload{}); served.Image != SGLangWorkload().Image {
		t.Fatalf("sglang served %#v", served)
	}
	// A custom image declares its own contract; an empty one has no paths.
	if served := ServedWorkload("custom-oci", "MI300X", runtimecontract.Workload{}); !served.Empty() {
		t.Fatalf("custom-oci served %#v", served)
	}
	declared := runtimecontract.Workload{Image: "example/server@sha256:abc", Protocol: "openai", MetricsPath: ""}
	if served := ServedWorkload("vllm", "MI300X", declared); served.Image != declared.Image || served.MetricsPath != "" {
		t.Fatalf("an explicit workload was replaced: %#v", served)
	}
}

// The pins below were read from each tag's registry manifest. Changing one is
// a reviewed engine or image change, never a silent drift.
var reviewedImagePins = map[[2]string]string{
	{"vllm", "cuda"}:          "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7",
	{"vllm", "rocm-gfx942"}:   "vllm/vllm-openai-rocm@sha256:368b2992c776ad653e40e9e00d38aa51cf1e9a16558b05d07c42818abd9ec031",
	{"vllm", "rocm-gfx950"}:   "vllm/vllm-openai-rocm@sha256:368b2992c776ad653e40e9e00d38aa51cf1e9a16558b05d07c42818abd9ec031",
	{"sglang", "cuda"}:        "lmsysorg/sglang:v0.5.12@sha256:42194170546745092e74cd5f81ad32a7c6e944c7111fe7bf13588152277ff356",
	{"sglang", "rocm-gfx942"}: "lmsysorg/sglang:v0.5.12-rocm720-mi30x@sha256:1f5c080be74269393bdc07d2cb30127dd2931c45b0b53e448235fb14d854da63",
	{"sglang", "rocm-gfx950"}: "lmsysorg/sglang:v0.5.12-rocm720-mi35x@sha256:3be1556105cdfdede103dc053f002d4d0307bdb229d0e15bfef130d5c53af208",
}

func TestRuntimeImagesArePinnedPerPlatformAtOneEngineVersion(t *testing.T) {
	images := RuntimeImages()
	if len(images) != len(reviewedImagePins) {
		t.Fatalf("images=%d, reviewed pins=%d", len(images), len(reviewedImagePins))
	}
	for _, image := range images {
		want, ok := reviewedImagePins[[2]string{image.Runtime, image.Platform}]
		if !ok || image.Image != want {
			t.Fatalf("%s on %s pins %q, reviewed %q", image.Runtime, image.Platform, image.Image, want)
		}
		if err := runtimecontract.ValidateImage(image.Image); err != nil {
			t.Fatalf("%s on %s: %v", image.Runtime, image.Platform, err)
		}
		// Every platform of a runtime runs the engine version whose defaults
		// the planner recorded, so the defaults table holds on every GPU.
		version := map[string]string{"vllm": DefaultRuntimeVersion, "sglang": SGLangRuntimeVersion}[image.Runtime]
		if image.EngineVersion != version || !strings.Contains(image.Tag, version) || image.Stack == "" {
			t.Fatalf("%s on %s: version %q tag %q stack %q", image.Runtime, image.Platform, image.EngineVersion, image.Tag, image.Stack)
		}
		workload, ok := RuntimeWorkload(image.Runtime, image.Platform)
		if !ok || workload.Image != image.Image {
			t.Fatalf("%s on %s has no workload", image.Runtime, image.Platform)
		}
		if err := workload.Validate(); err != nil {
			t.Fatalf("%s on %s workload: %v", image.Runtime, image.Platform, err)
		}
		// The scrape, readiness, and model identity paths are the same on
		// every platform, so measurement does not change on AMD.
		cuda, _ := RuntimeWorkload(image.Runtime, "cuda")
		if strings.Join(workload.Command, " ") != strings.Join(cuda.Command, " ") || workload.MetricsPath != "/metrics" || workload.ReadinessPath != cuda.ReadinessPath || workload.Port != cuda.Port {
			t.Fatalf("%s on %s changed the contract: %#v", image.Runtime, image.Platform, workload)
		}
	}
	images[0].Targets = append(images[0].Targets, "mutated")
	if slices.Contains(RuntimeImages()[0].Targets, "mutated") {
		t.Fatal("pinned images can be mutated through their accessor")
	}
	if _, ok := RuntimeWorkload("custom-oci", "cuda"); ok || RuntimePlatforms("custom-oci") != nil {
		t.Fatal("custom-oci gained a built-in image")
	}
}

func TestWorkloadsFollowTheGPUPlatform(t *testing.T) {
	for _, test := range []struct{ runtime, gpu, image string }{
		{"sglang", "MI300X", reviewedImagePins[[2]string{"sglang", "rocm-gfx942"}]},
		{"sglang", "AMD_Instinct_MI325X_OAM", reviewedImagePins[[2]string{"sglang", "rocm-gfx942"}]},
		{"sglang", "MI355X", reviewedImagePins[[2]string{"sglang", "rocm-gfx950"}]},
		{"sglang", "H100", reviewedImagePins[[2]string{"sglang", "cuda"}]},
		{"vllm", "AMD Instinct MI300X OAM", reviewedImagePins[[2]string{"vllm", "rocm-gfx942"}]},
		{"", "MI350X", reviewedImagePins[[2]string{"vllm", "rocm-gfx950"}]},
		{"vllm", "L40S", reviewedImagePins[[2]string{"vllm", "cuda"}]},
	} {
		if served := ServedWorkload(test.runtime, test.gpu, runtimecontract.Workload{}); served.Image != test.image {
			t.Fatalf("%s on %s served %q, want %q", test.runtime, test.gpu, served.Image, test.image)
		}
	}
	// SGLang revisions store their image, so it must be the GPU's; vLLM
	// revisions keep an empty workload that provisioning fills.
	if stored := NormalizeWorkload("sglang", "MI300X", runtimecontract.Workload{}); stored.Image != reviewedImagePins[[2]string{"sglang", "rocm-gfx942"}] {
		t.Fatalf("SGLang revision on MI300X stored %q", stored.Image)
	}
	if stored := NormalizeWorkload("vllm", "MI300X", runtimecontract.Workload{}); !stored.Empty() {
		t.Fatalf("vLLM revision stored %#v", stored)
	}
}

func TestV1QualifiesAMDPlatformsOnlyWhereTheyCanRun(t *testing.T) {
	matrix := V1()
	for _, test := range []struct{ runtime, cloud, mode, adapter, gpu string }{
		{"vllm", "runpod", ElasticMode, "", "MI300X"},
		{"vllm", "runpod", ElasticMode, "", "MI350X"},
		{"custom-oci", "runpod", ElasticMode, "", "MI300X"},
		{"vllm", "kubernetes", ElasticMode, "", "AMD_Instinct_MI300X_OAM"},
		{"sglang", "kubernetes", ElasticMode, "", "MI355X"},
		{"vllm", "aws", ElasticMode, "", "H100"},
		{"vllm", "vast", ElasticMode, "skypilot-vast", "H100"},
		{"vllm", "runpod", ServerlessMode, "", "L40S"},
		// Attached compute has no GPU to check.
		{"vllm", "", "existing", "", ""},
	} {
		if err := matrix.ValidateAccelerator(test.runtime, test.cloud, test.mode, test.adapter, test.gpu); err != nil {
			t.Fatalf("%+v rejected: %v", test, err)
		}
	}
	for _, test := range []struct{ runtime, cloud, mode, adapter, gpu, why string }{
		{"vllm", "aws", ElasticMode, "", "MI300X", "not qualified for cloud \"aws\""},
		{"sglang", "gcp", ElasticMode, "", "MI355X", "not qualified for cloud \"gcp\""},
		{"vllm", "runpod", ServerlessMode, "", "MI300X", "in serverless mode"},
	} {
		err := matrix.ValidateAccelerator(test.runtime, test.cloud, test.mode, test.adapter, test.gpu)
		if err == nil || !strings.Contains(err.Error(), test.why) || !strings.Contains(err.Error(), "kubernetes elastic, runpod elastic") {
			t.Fatalf("%+v: %v", test, err)
		}
	}
	// SkyPilot reaches nodes over SSH, which only the CUDA images carry.
	for _, adapter := range []string{"skypilot", "skypilot-vast"} {
		if err := matrix.ValidateAccelerator("vllm", "runpod", ElasticMode, adapter, "MI300X"); err == nil || !strings.Contains(err.Error(), "SkyPilot adapter does not run") {
			t.Fatalf("%s: %v", adapter, err)
		}
	}
	// A matrix built without platform data checks only runtime images.
	if err := New([]string{"vllm"}, map[string][]string{"future-cloud": {ElasticMode}}).ValidateAccelerator("vllm", "future-cloud", ElasticMode, "", "MI300X"); err != nil {
		t.Fatal(err)
	}
}
