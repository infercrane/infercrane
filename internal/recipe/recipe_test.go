package recipe

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestBuildIsDeterministicAndRequiresMatchingMeasuredEvidence(t *testing.T) {
	revision := domain.DeploymentRevision{ID: "rev-1", SpecJSON: `{"model":"org/model","runtime":"vllm","runtime_version":"0.10.2","routing_strategy":"round_robin","min_replicas":1,"max_replicas":1,"compute_mode":"elastic","cloud":"aws","gpu":"H100"}`}
	artifact := domain.ModelArtifact{ID: "artifact-1", Repository: "org/model", ImmutableRevision: strings.Repeat("a", 40), ModelIdentity: "org/model@" + strings.Repeat("a", 40)}
	benchmark := domain.BenchmarkResult{ID: "bench-1", RevisionID: "rev-1", DeploymentName: "prod", ModelIdentity: artifact.ModelIdentity, Tool: "aiperf", ToolVersion: "0.9", WorkloadJSON: `{"requests":100}`}
	first, err := Build("balanced", "1.0.0", "1.7.0", artifact, revision, benchmark)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build("balanced", "1.0.0", "1.7.0", artifact, revision, benchmark)
	if err != nil || first.Digest != second.Digest || len(first.Digest) != 64 {
		t.Fatalf("first=%#v second=%#v err=%v", first, second, err)
	}
	benchmark.RevisionID = "other"
	if _, err = Build("balanced", "1.0.0", "1.7.0", artifact, revision, benchmark); err == nil {
		t.Fatal("mismatched evidence accepted")
	}
}
