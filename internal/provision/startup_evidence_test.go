package provision

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseStartupEvidenceKeepsOnlyClosedMarkers(t *testing.T) {
	raw := "boot secret=do-not-persist\n" +
		"infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
		"infercrane_startup stage=identity_ready at=2026-08-23T10:00:02Z\n" +
		"infercrane_startup stage=image_check at=2026-08-23T10:00:03Z\n" +
		"infercrane_startup stage=image_pull_start at=2026-08-23T10:00:04Z\n" +
		"runtime token=secret\n" +
		"infercrane_startup stage=image_pull_complete at=2026-08-23T10:01:04Z\n" +
		"infercrane_startup stage=runtime_start at=2026-08-23T10:01:05Z\n" +
		"infercrane_startup stage=runtime_container_started at=2026-08-23T10:01:06Z\n"
	evidence, ok := parseStartupEvidence(raw)
	if !ok || evidence.CurrentStage != "runtime_container_started" || evidence.ImageCache != "miss" || len(evidence.Stages) != 7 {
		t.Fatalf("evidence=%#v ok=%v", evidence, ok)
	}
	for _, stage := range evidence.Stages {
		if stage.Name == "secret" {
			t.Fatal("arbitrary console content was retained")
		}
	}
}

func TestParseStartupEvidenceSeparatesImageAndArtifactCache(t *testing.T) {
	raw := "infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
		"infercrane_startup stage=identity_ready at=2026-08-23T10:00:01Z\n" +
		"infercrane_startup stage=image_check at=2026-08-23T10:00:02Z\n" +
		"infercrane_startup stage=image_cache_hit at=2026-08-23T10:00:03Z\n" +
		"infercrane_startup stage=artifact_check at=2026-08-23T10:00:04Z\n" +
		"infercrane_startup stage=artifact_cache_hit at=2026-08-23T10:00:05Z\n" +
		"infercrane_startup stage=runtime_start at=2026-08-23T10:00:06Z\n"
	evidence, ok := parseStartupEvidence(raw)
	if !ok || evidence.SchemaVersion != 2 || evidence.ImageCache != "hit" || evidence.ArtifactCache != "hit" || evidence.CurrentStage != "runtime_start" {
		t.Fatalf("evidence=%#v ok=%v", evidence, ok)
	}

	raw = "infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
		"infercrane_startup stage=artifact_check at=2026-08-23T10:00:01Z\n" +
		"infercrane_startup stage=artifact_cache_mount_failed at=2026-08-23T10:00:02Z\n"
	evidence, ok = parseStartupEvidence(raw)
	if !ok || evidence.ArtifactCache != "mount_failed" || evidence.CurrentStage != "artifact_cache_mount_failed" {
		t.Fatalf("failed evidence=%#v ok=%v", evidence, ok)
	}
}

func TestParseStartupEvidenceDecodesAWSBase64AndSelectsLatestBoot(t *testing.T) {
	raw := "infercrane_startup stage=identity_start at=2026-08-23T09:00:00Z\n" +
		"infercrane_startup stage=runtime_start at=2026-08-23T09:01:00Z\n" +
		"infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
		"infercrane_startup stage=image_check at=2026-08-23T10:00:01Z\n" +
		"infercrane_startup stage=image_cache_hit at=2026-08-23T10:00:02Z\n" +
		"infercrane_startup stage=runtime_start at=2026-08-23T10:00:03Z\n"
	evidence, ok := parseStartupEvidence(base64.StdEncoding.EncodeToString([]byte(raw)))
	if !ok || evidence.ImageCache != "hit" || len(evidence.Stages) != 4 || evidence.Stages[0].At.Hour() != 10 {
		t.Fatalf("evidence=%#v ok=%v", evidence, ok)
	}
}

func TestParseStartupEvidenceRejectsMalformedAndUnknownMarkers(t *testing.T) {
	for _, raw := range []string{
		"ordinary console output",
		"infercrane_startup stage=identity_start at=not-a-time\n",
		"infercrane_startup stage=credential_dump at=2026-08-23T10:00:00Z\n",
	} {
		if evidence, ok := parseStartupEvidence(raw); ok {
			t.Fatalf("unexpected evidence for %q: %#v", raw, evidence)
		}
	}
}

func TestParseStartupEvidenceRetainsProviderAcceleratorStages(t *testing.T) {
	for _, test := range []struct {
		name, stages, current string
	}{
		{name: "aws", stages: "accelerator_check|accelerator_ready", current: "accelerator_ready"},
		{name: "gcp", stages: "gpu_driver_start|gpu_driver_ready", current: "gpu_driver_ready"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := strings.Split(test.stages, "|")
			raw := "infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
				"infercrane_startup stage=identity_ready at=2026-08-23T10:00:01Z\n" +
				"infercrane_startup stage=" + parts[0] + " at=2026-08-23T10:00:02Z\n" +
				"infercrane_startup stage=" + parts[1] + " at=2026-08-23T10:00:12Z\n"
			evidence, ok := parseStartupEvidence(raw)
			if !ok || evidence.CurrentStage != test.current || len(evidence.Stages) != 4 {
				t.Fatalf("evidence=%#v ok=%v", evidence, ok)
			}
		})
	}
}
