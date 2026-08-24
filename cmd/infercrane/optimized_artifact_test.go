package main

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizedartifact"
)

func TestOptimizedArtifactPresetsRemainExternalPinnedAndQualityGated(t *testing.T) {
	for _, preset := range []string{"fp8", "awq", "gptq", "nvfp4", "eagle3", "mtp", "dflash", "tensorrt"} {
		t.Run(preset, func(t *testing.T) {
			plan, err := optimizedArtifactPreset("base-artifact", preset, "1.2.3", "sha256:"+strings.Repeat("a", 64), "", "Apache-2.0")
			if err != nil {
				t.Fatal(err)
			}
			if !plan.RequiresQualityReview || plan.ToolVersion != "1.2.3" || optimizedartifact.ValidatePlan(plan) != nil {
				t.Fatalf("unsafe preset plan: %+v", plan)
			}
		})
	}
}

func TestOptimizedArtifactPresetRejectsUnknownOrMutableBuilder(t *testing.T) {
	if _, err := optimizedArtifactPreset("base", "future", "1", "sha256:"+strings.Repeat("a", 64), "", "Apache-2.0"); err == nil {
		t.Fatal("unknown preset accepted")
	}
	plan, err := optimizedArtifactPreset("base", "fp8", "1", "builder:latest", "", "Apache-2.0")
	if err != nil {
		t.Fatal(err)
	}
	if err = optimizedartifact.ValidatePlan(plan); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("mutable builder accepted: %v", err)
	}
}
