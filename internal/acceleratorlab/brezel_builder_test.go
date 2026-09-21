package acceleratorlab

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/brezelexecutor"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

type brezelRunnerFunc func(context.Context, brezelexecutor.Job) (brezelexecutor.Result, error)

func (function brezelRunnerFunc) Run(ctx context.Context, job brezelexecutor.Job) (brezelexecutor.Result, error) {
	return function(ctx, job)
}

func TestBrezelBuilderProducesPublishedImmutableEvidence(t *testing.T) {
	request := fixtureBuildRequest()
	runner := brezelRunnerFunc(func(_ context.Context, job brezelexecutor.Job) (brezelexecutor.Result, error) {
		if job.Candidate.ImplementationID != "infercrane-residual-rmsnorm-triton" || job.Candidate.Backend != "triton" || job.TargetSM != "sm89" || len(job.Inputs) != 1 {
			t.Fatalf("job=%+v", job)
		}
		return brezelexecutor.Result{
			SchemaVersion: brezelexecutor.ResultSchemaVersion, JobID: job.ID, Status: "passed",
			Checks:    []brezelexecutor.Check{{Name: "source-screen", Status: "passed"}, {Name: "cpu-correctness", Status: "passed"}, {Name: "compile", Status: "passed"}},
			Artifacts: []brezelexecutor.OutputArtifact{{Kind: "kernel-bundle", Path: "/workspace/infercrane/output/artifacts/kernel.so", SHA256: strings.Repeat("e", 64), Size: 4096, URI: "s3://infercrane-artifacts/kernel.so"}},
			Receipt:   json.RawMessage(`{"signed":true}`),
		}, nil
	})
	evidence, err := (BrezelBuilder{Runner: runner, BuilderVersion: "brezel-1", EnvironmentRevision: "envr_964f832968870a80ca50570b"}).Build(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "passed" || len(evidence.Checks) != 3 || len(evidence.Artifacts) != 1 || evidence.Artifacts[0].SHA256 != digestOf("e") || evidence.ReceiptDigest == "" {
		t.Fatalf("evidence=%+v", evidence)
	}
}

func TestBrezelBuilderFailsClosedWithoutRetrievableSourceOrReceipt(t *testing.T) {
	request := fixtureBuildRequest()
	request.Source.Artifacts = nil
	if _, err := (BrezelBuilder{Runner: brezelRunnerFunc(nil), BuilderVersion: "1", EnvironmentRevision: "envr"}).Build(context.Background(), request); err == nil || !strings.Contains(err.Error(), "source artifacts") {
		t.Fatalf("missing source accepted: %v", err)
	}
	request = fixtureBuildRequest()
	runner := brezelRunnerFunc(func(_ context.Context, job brezelexecutor.Job) (brezelexecutor.Result, error) {
		return brezelexecutor.Result{SchemaVersion: brezelexecutor.ResultSchemaVersion, JobID: job.ID, Status: "passed", Checks: []brezelexecutor.Check{{Name: "compile", Status: "passed"}}, Artifacts: []brezelexecutor.OutputArtifact{{Kind: "kernel", SHA256: strings.Repeat("e", 64), Size: 1, URI: "s3://bucket/kernel"}}}, nil
	})
	if _, err := (BrezelBuilder{Runner: runner, BuilderVersion: "1", EnvironmentRevision: "envr"}).Build(context.Background(), request); err == nil || !strings.Contains(err.Error(), "receipt") {
		t.Fatalf("missing receipt accepted: %v", err)
	}
}

func fixtureBuildRequest() BuildRequest {
	return BuildRequest{
		InputDigest: "sha256:" + strings.Repeat("a", 64), CampaignID: "campaign-1", CandidateID: "candidate-1",
		Experiment: kernelplanner.Candidate{ID: "experiment-1", OperatorFamily: kernelplanner.ResidualRMSNorm, CompilerChoices: []string{"triton"}, ExistingImplementations: []kernelplanner.KernelMatch{{ImplementationID: "infercrane-residual-rmsnorm-triton", Backend: "triton"}}},
		Source:     SourcePin{ImplementationID: "infercrane-residual-rmsnorm-triton", Revision: strings.Repeat("b", 40), License: "Apache-2.0", Artifacts: []Artifact{{Kind: "source", URI: "s3://infercrane-source/kernel.tar.zst", SHA256: digestOf("c"), Size: 1024}}},
		Model:      kernelplanner.ModelIdentity{Repository: "Qwen/Qwen3-0.6B", Revision: strings.Repeat("d", 40)},
		Runtime:    kernelplanner.RuntimeIdentity{Name: "vllm", Version: "0.22.1", ImageDigest: digestOf("f")},
		Hardware:   kernelplanner.HardwareIdentity{Vendor: "nvidia", Accelerator: "L40S", ComputeCapability: "sm89"},
	}
}
