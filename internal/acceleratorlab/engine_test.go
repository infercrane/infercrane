package acceleratorlab

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/kernelplanner"
)

type profilerFunc func(context.Context, ProfileIntent) (ProfileEvidence, error)

func (f profilerFunc) Capture(ctx context.Context, input ProfileIntent) (ProfileEvidence, error) {
	return f(ctx, input)
}

type generatorFunc func(context.Context, GenerationRequest) (GenerationEvidence, error)

func (f generatorFunc) Generate(ctx context.Context, input GenerationRequest) (GenerationEvidence, error) {
	return f(ctx, input)
}

type builderFunc func(context.Context, BuildRequest) (BuildEvidence, error)

func (f builderFunc) Build(ctx context.Context, input BuildRequest) (BuildEvidence, error) {
	return f(ctx, input)
}

type qualifierFunc func(context.Context, QualificationRequest) (QualificationEvidence, error)

func (f qualifierFunc) Qualify(ctx context.Context, input QualificationRequest) (QualificationEvidence, error) {
	return f(ctx, input)
}

func TestEngineRunsProfileBuildAndExactTargetQualification(t *testing.T) {
	request := fixtureRequest(ModalityText)
	var stages []string
	engine := Engine{
		Profiler: fixtureProfiler(.10),
		Builder:  fixtureBuilder(.20),
		Qualifier: fixtureQualifier(func(evidence *QualificationEvidence) {
			evidence.CostUSD = .30
		}),
		Progress: func(_ context.Context, progress Progress) error {
			stages = append(stages, progress.Stage)
			return nil
		},
	}

	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "qualified" || result.EvidenceState != "qualified" || result.WinnerID == "" {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Experiments) != 1 || result.Experiments[0].State != "qualified" {
		t.Fatalf("experiments=%+v", result.Experiments)
	}
	if math.Abs(result.CostUSD-.60) > 1e-9 {
		t.Fatalf("cost=%f", result.CostUSD)
	}
	wantStages := []string{"profile", "search", "build", "measure", "prove"}
	if strings.Join(stages, ",") != strings.Join(wantStages, ",") {
		t.Fatalf("stages=%v", stages)
	}
}

func TestEngineRejectsMicrobenchmarkWithoutServingReplay(t *testing.T) {
	request := fixtureRequest(ModalityText)
	engine := Engine{
		Profiler: fixtureProfiler(0),
		Builder:  fixtureBuilder(0),
		Qualifier: fixtureQualifier(func(evidence *QualificationEvidence) {
			evidence.ServingReplayMeasured = false
			evidence.EndToEndSpeedup = 1
			evidence.BenchmarkEvidenceID = ""
		}),
	}

	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "inconclusive" || result.WinnerID != "" || len(result.Experiments) != 1 {
		t.Fatalf("result=%+v", result)
	}
	experiment := result.Experiments[0]
	if experiment.State != "rejected" || experiment.FailureCode != "qualification_gate_failed" {
		t.Fatalf("experiment=%+v", experiment)
	}
	joined := strings.Join(experiment.Reasons, " ")
	for _, expected := range []string{"serving replay", "end-to-end", "benchmark evidence"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("reasons=%v missing %q", experiment.Reasons, expected)
		}
	}
}

func TestEngineRejectsCrossHardwareEvidence(t *testing.T) {
	request := fixtureRequest(ModalityText)
	engine := Engine{
		Profiler: fixtureProfiler(0),
		Builder:  fixtureBuilder(0),
		Qualifier: fixtureQualifier(func(evidence *QualificationEvidence) {
			evidence.Hardware.Accelerator = "A100"
		}),
	}

	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 1 || result.Experiments[0].State != "rejected" || !strings.Contains(strings.Join(result.Experiments[0].Reasons, " "), "identity") {
		t.Fatalf("result=%+v", result)
	}
}

func TestEngineRejectsEvidenceFromDifferentTopologyOrModality(t *testing.T) {
	request := fixtureRequest(ModalityVideo)
	engine := Engine{
		Profiler: fixtureProfiler(0),
		Builder:  fixtureBuilder(0),
		Qualifier: fixtureQualifier(func(evidence *QualificationEvidence) {
			evidence.Topology.Nodes = 2
			evidence.Modality = ModalityText
		}),
	}
	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Experiments) != 1 || result.Experiments[0].State != "rejected" || !strings.Contains(strings.Join(result.Experiments[0].Reasons, " "), "identity") {
		t.Fatalf("result=%+v", result)
	}
}

func TestEngineGeneratesBuildsAndQualifiesProfileBoundKernel(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Sources = nil
	request.Policy.AllowGeneratedKernels = true
	generated := false
	engine := Engine{
		Profiler: fixtureProfiler(.10),
		Generator: generatorFunc(func(_ context.Context, input GenerationRequest) (GenerationEvidence, error) {
			generated = true
			return GenerationEvidence{
				InputDigest: input.InputDigest, CandidateID: input.Candidate.ID,
				Generator: "kernel-agent", GeneratorVersion: "sha256:" + strings.Repeat("c", 64),
				ProfileArtifactDigest: input.Profile.ArtifactDigest,
				Source:                SourcePin{ImplementationID: "generated-" + input.Candidate.ID, Revision: strings.Repeat("d", 40), License: "Apache-2.0", Artifacts: []Artifact{{Kind: "source-bundle", URI: "https://worker.example/v1/artifacts/source", SHA256: digestOf("e"), Size: 1024}}},
				GeneratedAt:           time.Now().UTC(), CostUSD: .05,
			}, nil
		}),
		Builder:   fixtureBuilder(.20),
		Qualifier: fixtureQualifier(nil),
	}
	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !generated || result.State != "qualified" || len(result.Experiments) != 1 || result.Experiments[0].Generation == nil {
		t.Fatalf("result=%+v generated=%v", result, generated)
	}
}

func TestEngineRejectsGeneratedSourceNotBoundToProfile(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Sources = nil
	request.Policy.AllowGeneratedKernels = true
	engine := Engine{
		Profiler: fixtureProfiler(0),
		Generator: generatorFunc(func(_ context.Context, input GenerationRequest) (GenerationEvidence, error) {
			return GenerationEvidence{
				InputDigest: input.InputDigest, CandidateID: input.Candidate.ID,
				Generator: "kernel-agent", GeneratorVersion: "1", ProfileArtifactDigest: digestOf("f"),
				Source:      SourcePin{ImplementationID: "generated", Revision: strings.Repeat("d", 40), License: "Apache-2.0", Artifacts: []Artifact{{Kind: "source-bundle", URI: "https://worker.example/v1/artifacts/source", SHA256: digestOf("e"), Size: 1024}}},
				GeneratedAt: time.Now().UTC(),
			}, nil
		}),
		Builder: fixtureBuilder(0), Qualifier: fixtureQualifier(nil),
	}
	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "inconclusive" || result.Experiments[0].FailureCode != "generation_evidence_invalid" {
		t.Fatalf("result=%+v", result)
	}
}

func TestValidateAllModalitiesRequireMeasuredShapes(t *testing.T) {
	tests := []struct {
		modality Modality
		mutate   func(*Request)
	}{
		{ModalityText, func(request *Request) { request.Workload.InputTokens = 0 }},
		{ModalityImage, func(request *Request) { request.Workload.Media.ImageWidth = 0 }},
		{ModalityVideo, func(request *Request) { request.Workload.Media.VideoFrames = 0 }},
		{ModalityAudio, func(request *Request) { request.Workload.Media.AudioSeconds = 0 }},
	}
	for _, test := range tests {
		t.Run(string(test.modality), func(t *testing.T) {
			request := fixtureRequest(test.modality)
			test.mutate(&request)
			if _, err := (Engine{Profiler: fixtureProfiler(0), Builder: fixtureBuilder(0), Qualifier: fixtureQualifier(nil)}).Run(context.Background(), request); err == nil {
				t.Fatalf("invalid %s workload was accepted", test.modality)
			}
		})
	}
}

func TestEngineStopsWhenAuthorizedBudgetIsExceeded(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Policy.MaxCostUSD = .05
	_, err := (Engine{Profiler: fixtureProfiler(.10), Builder: fixtureBuilder(0), Qualifier: fixtureQualifier(nil)}).Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "exceeds authorized maximum") {
		t.Fatalf("budget violation=%v", err)
	}
}

func fixtureRequest(modality Modality) Request {
	request := Request{
		SchemaVersion: RequestSchemaV1,
		TenantID:      "tenant-1",
		CampaignID:    "campaign-1",
		CandidateID:   "candidate-1",
		Model:         kernelplanner.ModelIdentity{Repository: "Qwen/Qwen3-0.6B", Revision: strings.Repeat("a", 40)},
		Runtime:       kernelplanner.RuntimeIdentity{Name: "vllm", Version: "0.22.1", ImageDigest: digestOf("1")},
		Hardware:      kernelplanner.HardwareIdentity{Vendor: "nvidia", Accelerator: "L40S", ComputeCapability: "sm89"},
		Topology:      Topology{Mode: "aggregated", Nodes: 1, Accelerators: 1, TensorParallel: 1, PipelineParallel: 1, ExpertParallel: 1},
		Workload: Workload{
			Digest:       digestOf("2"),
			Modality:     modality,
			Phase:        kernelplanner.PhaseDecode,
			BatchSize:    1,
			Concurrency:  4,
			InputTokens:  128,
			OutputTokens: 32,
			Media:        MediaShape{ImagesPerRequest: 1, ImageWidth: 1024, ImageHeight: 1024, VideoFrames: 32, VideoFPS: 8, AudioSeconds: 30, AudioSampleRate: 16000},
			QualitySuite: "quality-suite@sha256:" + strings.Repeat("3", 64),
			Replay:       "aiperf-replay@sha256:" + strings.Repeat("4", 64),
		},
		Policy: Policy{
			MaxCandidates:          1,
			MaxCostUSD:             2,
			MinHotspotFraction:     .05,
			MinAmdahlCeiling:       1.05,
			TargetEndToEndSpeedup:  1.02,
			MinKernelSpeedup:       1.01,
			MaxMemoryRegressionPct: 2,
			RequireSanitizer:       true,
			RequireHiddenShapes:    true,
			RequireServingReplay:   true,
			RequireQuality:         true,
			AuthorizedUntil:        time.Now().UTC().Add(time.Hour),
		},
		Sources: []SourcePin{{ImplementationID: "infercrane-residual-rmsnorm-triton", Revision: strings.Repeat("b", 40), License: "Apache-2.0"}},
	}
	return request
}

func fixtureProfiler(cost float64) Profiler {
	return profilerFunc(func(_ context.Context, input ProfileIntent) (ProfileEvidence, error) {
		return ProfileEvidence{
			InputDigest: input.InputDigest, Tool: "nsight-systems+nsight-compute", ToolVersion: "2026.3",
			Hardware: input.Hardware, RuntimeImageDigest: input.Runtime.ImageDigest, Topology: input.Topology, WorkloadDigest: input.Workload.Digest, CapturedAt: time.Now().UTC(), CostUSD: cost,
			ArtifactDigest: digestOf("5"),
			Hotspots:       []kernelplanner.Hotspot{{ID: "norm", Name: "residual_rmsnorm", Family: kernelplanner.ResidualRMSNorm, DeviceTimeFraction: .20, MeanDurationUS: 18, Invocations: 64, Shapes: []string{"4x128x1024"}, DTypes: []string{"bf16"}}},
		}, nil
	})
}

func fixtureBuilder(cost float64) Builder {
	return builderFunc(func(_ context.Context, input BuildRequest) (BuildEvidence, error) {
		return BuildEvidence{
			InputDigest: input.InputDigest, Builder: "brezel", BuilderVersion: "1", Environment: "envr_964f832968870a80ca50570b",
			SourceDigest: digestOf("6"), Status: "passed", Checks: []string{"source-screen", "cpu-correctness", "compile"},
			Artifacts:     []Artifact{{Kind: "kernel-bundle", URI: "https://artifacts.infercrane.test/kernel.so", SHA256: digestOf("7"), Size: 4096}},
			ReceiptDigest: digestOf("8"), CostUSD: cost,
		}, nil
	})
}

func fixtureQualifier(mutate func(*QualificationEvidence)) Qualifier {
	return qualifierFunc(func(_ context.Context, input QualificationRequest) (QualificationEvidence, error) {
		evidence := QualificationEvidence{
			InputDigest: input.InputDigest, Hardware: input.Hardware, RuntimeImageDigest: input.Runtime.ImageDigest,
			Topology: input.Topology, WorkloadDigest: input.Workload.Digest, Modality: input.Workload.Modality, Media: input.Workload.Media, Replay: input.Workload.Replay, QualitySuite: input.Workload.QualitySuite, BuildArtifactDigest: input.Build.Artifacts[0].SHA256,
			CorrectnessPassed: true, HiddenShapesPassed: true, SanitizerPassed: true, QualityPassed: true, ServingReplayMeasured: true,
			KernelSpeedup: 1.15, EndToEndSpeedup: 1.04, MemoryRegressionPct: 0, CostRegressionPct: -4,
			ProfilerArtifactDigest: digestOf("9"), BenchmarkEvidenceID: "aiperf-evidence-1", QualityEvidenceID: "quality-evidence-1", MeasuredAt: time.Now().UTC(),
		}
		if mutate != nil {
			mutate(&evidence)
		}
		return evidence, nil
	})
}

func digestOf(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
