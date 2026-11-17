package optimizer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

// amdCheckpointSource is the catalog source with one AMD LLM Compressor build
// of Qwen/Qwen3.8-27B that fits the pinned vLLM ROCm image, as the sync plans
// it for a CDNA 3 and CDNA 4 format.
func amdCheckpointSource(t *testing.T) CatalogSource {
	t.Helper()
	source := catalogSource(t)
	catalog := checkpointcatalog.MustDefault()
	var entry checkpointcatalog.Entry
	for _, candidate := range catalog.Candidates() {
		if candidate.Repository == "nvidia/Qwen3.8-27B-NVFP4" {
			entry = candidate
		}
	}
	if entry.ID == "" {
		t.Fatal("the reviewed Qwen checkpoint used as a template is missing")
	}
	entry.ID, entry.Publisher = "amd-qwen38-27b-w8a8-llmcompressor-e5e5e5e5", "amd"
	entry.Repository, entry.Revision = "amd/Qwen3.8-27B-w8a8-llmcompressor", strings.Repeat("e5", 20)
	entry.SourceURL = "https://huggingface.co/" + entry.Repository
	entry.Quantization = &checkpointcatalog.Quantization{Tool: "llm-compressor", ToolVersion: "0.12.3", Method: "compressed-tensors", Scheme: "w8a8"}
	entry.Hardware = checkpointcatalog.Hardware{Vendor: "amd", Architectures: checkpointcatalog.ResolveArchitectures("amd", []string{"cdna3", "cdna4"}), ProductSupport: checkpointcatalog.SupportRunnable}
	entry.Runtimes, entry.EngineRequirements, entry.PublisherPriors = []string{"vllm"}, nil, nil
	catalog.Entries = []checkpointcatalog.Entry{entry}
	if err := checkpointcatalog.Validate(catalog); err != nil {
		t.Fatalf("AMD checkpoint fixture is not a valid catalog: %v", err)
	}
	source.Checkpoints = catalog
	return source
}

func TestCatalogProposalSeedsAMDCheckpointOnlyOnItsCompilerTargets(t *testing.T) {
	request := Request{
		ModelIdentity: "Qwen/Qwen3.8-27B", ModelRevision: strings.Repeat("a", 40),
		Provider: "runpod-pods", GPU: "AMD Instinct MI300X OAM", Runtimes: []string{"vllm"},
		Objective: "throughput", MaxCandidates: 4,
	}
	seeds := func(gpu string) []*Candidate {
		request.GPU = gpu
		proposal, err := amdCheckpointSource(t).Propose(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if err = ValidateProposal(proposal); err != nil {
			t.Fatalf("proposal on %s failed validation: %v", gpu, err)
		}
		var out []*Candidate
		for index := range proposal.Candidates {
			if proposal.Candidates[index].ArtifactSeed != nil {
				out = append(out, &proposal.Candidates[index])
			}
		}
		return out
	}
	for _, gpu := range []string{"AMD Instinct MI300X OAM", "MI325X", "MI355X"} {
		seeded := seeds(gpu)
		if len(seeded) != 1 {
			t.Fatalf("AMD checkpoint did not seed exactly one candidate on %s: %d", gpu, len(seeded))
		}
		candidate := seeded[0]
		if candidate.Deployment.Model.ID != "amd/Qwen3.8-27B-w8a8-llmcompressor" || candidate.ArtifactSeed.Publisher != "AMD" || candidate.ArtifactSeed.Tool != "llm-compressor" ||
			!slices.Equal(candidate.ArtifactSeed.AcceleratorArchitectures, []string{"gfx942", "gfx950"}) || candidate.EvidenceState != EvidenceUnmeasured {
			t.Fatalf("AMD checkpoint candidate on %s crossed its provenance boundary: %+v", gpu, candidate)
		}
		if candidate.Deployment.Runtime.Engine != "vllm" || candidate.Deployment.Resources.GPU != request.GPU {
			t.Fatalf("AMD checkpoint candidate on %s does not run the pinned vLLM on that GPU: %+v", gpu, candidate.Deployment)
		}
	}
	for _, gpu := range []string{"H100", "B200"} {
		if seeded := seeds(gpu); len(seeded) != 0 {
			t.Fatalf("AMD checkpoint leaked onto %s: %+v", gpu, seeded[0])
		}
	}
}
