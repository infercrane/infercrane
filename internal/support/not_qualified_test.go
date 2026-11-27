package support

import (
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// notQualifiedPlatforms are the platforms of every accelerator recorded as
// not qualified.
func notQualifiedPlatforms(t *testing.T) map[string]bool {
	t.Helper()
	platforms := map[string]bool{}
	for _, accelerator := range provideridentity.NotQualifiedAccelerators() {
		platforms[accelerator.Platform] = true
	}
	if len(platforms) == 0 {
		t.Fatal("no accelerator is recorded as not qualified")
	}
	return platforms
}

func TestNothingThatSelectsReadsANotQualifiedPlatform(t *testing.T) {
	notQualified := notQualifiedPlatforms(t)
	for _, accelerator := range provideridentity.Accelerators() {
		// A serving accelerator never shares a platform with one that is not
		// qualified, and its platform really serves: a current image of the
		// default runtime and a placement.
		if notQualified[accelerator.Platform] {
			t.Fatalf("serving %s shares the not-qualified platform %s", accelerator.Name, accelerator.Platform)
		}
		if _, ok := PinnedImage(DefaultRuntime, "", accelerator.Platform); !ok {
			t.Fatalf("serving %s has no current %s image", accelerator.Name, DefaultRuntime)
		}
		placed := slices.ContainsFunc(V1().AcceleratorPlatforms(), func(placement PlatformQualification) bool {
			return slices.Contains(placement.Platforms, accelerator.Platform)
		})
		if !placed {
			t.Fatalf("serving %s has no placement", accelerator.Name)
		}
	}
	for _, image := range RuntimeImages() {
		if notQualified[image.Platform] {
			t.Fatalf("a selectable image serves the not-qualified platform: %+v", image)
		}
	}
	for _, variable := range EngineEnvironment() {
		for _, platform := range variable.Platforms {
			if notQualified[platform] {
				t.Fatalf("engine variable %s is reviewed for the not-qualified platform %s", variable.Name, platform)
			}
		}
	}
	for _, placement := range V1().AcceleratorPlatforms() {
		for _, platform := range placement.Platforms {
			if notQualified[platform] {
				t.Fatalf("%s %s places the not-qualified platform %s", placement.Cloud, placement.Mode, platform)
			}
		}
	}
}

func TestEverySpecPathRefusesANotQualifiedAccelerator(t *testing.T) {
	matrix := V1()
	places := []struct{ cloud, mode, adapter string }{
		{"gcp", ElasticMode, "skypilot-gcp"},
		{"kubernetes", ElasticMode, "kubernetes"},
		{"future-cloud", ElasticMode, "skypilot-future-cloud"},
	}
	for _, placement := range matrix.AcceleratorPlatforms() {
		places = append(places, struct{ cloud, mode, adapter string }{placement.Cloud, placement.Mode, ""})
	}
	// A matrix without platform data checks accelerators too.
	bare := New([]string{DefaultRuntime, "sglang", "custom-oci"}, map[string][]string{"future-cloud": {ElasticMode}})
	for _, accelerator := range provideridentity.NotQualifiedAccelerators() {
		for _, runtime := range []string{"", DefaultRuntime, "sglang", "custom-oci"} {
			for _, place := range places {
				for _, policy := range []Matrix{matrix, bare} {
					err := policy.ValidateAccelerator(runtime, place.cloud, place.mode, place.adapter, accelerator.Name)
					if err == nil || !strings.Contains(err.Error(), accelerator.Platform+" platform, which is not qualified") {
						t.Fatalf("%s %s on %+v: %v", runtime, accelerator.Name, place, err)
					}
				}
			}
			if runtime != "custom-oci" {
				if err := ValidateRuntimeVersion(runtime, "", accelerator.Name); err == nil || !strings.Contains(err.Error(), "not qualified") {
					t.Fatalf("%s %s version: %v", runtime, accelerator.Name, err)
				}
			}
			if served := ServedWorkload(runtime, accelerator.Name, runtimecontract.Workload{}); !served.Empty() {
				t.Fatalf("%s on %s served %#v", runtime, accelerator.Name, served)
			}
			if _, ok := RuntimeWorkload(runtime, accelerator.Platform); ok {
				t.Fatalf("%s has a workload on %s", runtime, accelerator.Platform)
			}
		}
		if stored := NormalizeWorkload("sglang", SGLangRuntimeVersion, accelerator.Name, runtimecontract.Workload{}); !stored.Empty() {
			t.Fatalf("SGLang revision on %s stored %#v", accelerator.Name, stored)
		}
		if err := ValidateRuntimeEnv(DefaultRuntime, "", accelerator.Name, runtimecontract.Workload{}, map[string]string{"VLLM_USE_RUST_FRONTEND": "1"}); err == nil {
			t.Fatalf("engine env accepted on %s", accelerator.Name)
		}
	}
}

func TestNotQualifiedImagesAreRecordedButNeverSelected(t *testing.T) {
	notQualified := notQualifiedPlatforms(t)
	images := NotQualifiedRuntimeImages()
	if len(images) == 0 {
		t.Fatal("no candidate image is recorded")
	}
	for _, image := range images {
		if image.Status != ImageNotQualified || !notQualified[image.Platform] || image.Runtime == "" || image.EngineVersion == "" || image.Stack == "" || image.Note == "" {
			t.Fatalf("incomplete candidate image %+v", image)
		}
		if err := runtimecontract.ValidateImage(image.Image); err != nil {
			t.Fatalf("%s on %s: %v", image.Tag, image.Platform, err)
		}
		if !strings.HasPrefix(image.Image, strings.SplitN(image.Tag, ":", 2)[0]+"@sha256:") {
			t.Fatalf("%s is not the digest of its tag's repository: %s", image.Tag, image.Image)
		}
		if _, ok := PinnedImage(image.Runtime, image.EngineVersion, image.Platform); ok {
			t.Fatalf("%s %s on %s is selectable", image.Runtime, image.EngineVersion, image.Platform)
		}
		if slices.Contains(RuntimePlatforms(image.Runtime), image.Platform) {
			t.Fatalf("%s lists %s as a platform", image.Runtime, image.Platform)
		}
	}
	images[0].Targets = append(images[0].Targets, "mutated")
	if slices.Contains(NotQualifiedRuntimeImages()[0].Targets, "mutated") {
		t.Fatal("candidate images can be mutated through their accessor")
	}
}
