// Package artifact resolves mutable model references into immutable identities.
package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, binary string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, args...).CombinedOutput()
}

type HuggingFace struct {
	Python string
	Runner Runner
}

const resolverScript = `
import json, sys
from huggingface_hub import HfApi
repo, revision = sys.argv[1], sys.argv[2]
info = HfApi().model_info(repo_id=repo, revision=revision, files_metadata=True)
sizes = [getattr(item, "size", None) for item in (info.siblings or [])]
if sizes and all(item is not None for item in sizes):
    size = sum(sizes)
    size_source = "exact_revision_file_sum"
else:
    size = getattr(info, "used_storage", None)
    size_source = "repository_storage_fallback" if size is not None else "unknown"
compat = {
    "library_name": getattr(info, "library_name", None),
    "pipeline_tag": getattr(info, "pipeline_tag", None),
    "tags": sorted((getattr(info, "tags", None) or []))[:100],
    "artifact_size_source": size_source,
}
print(json.dumps({"repository": info.id, "requested_revision": revision, "immutable_revision": info.sha, "approximate_size_bytes": size, "runtime_compatibility": compat}))
`

func (h HuggingFace) Resolve(ctx context.Context, repository, revision string) (domain.ModelArtifact, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" || !strings.Contains(repository, "/") {
		return domain.ModelArtifact{}, errors.New("Hugging Face repository must be namespace/name")
	}
	if revision == "" {
		revision = "main"
	}
	python := h.Python
	if python == "" {
		python = os.Getenv("INFERCRANE_HF_PYTHON")
		if python == "" {
			python = "python3"
		}
	}
	runner := h.Runner
	if runner == nil {
		runner = execRunner{}
	}
	output, err := runner.Run(ctx, python, "-c", resolverScript, repository, revision)
	if err != nil {
		return domain.ModelArtifact{}, fmt.Errorf("resolve Hugging Face model: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var response struct {
		Repository           string         `json:"repository"`
		RequestedRevision    string         `json:"requested_revision"`
		ImmutableRevision    string         `json:"immutable_revision"`
		ApproximateSizeBytes *int64         `json:"approximate_size_bytes"`
		RuntimeCompatibility map[string]any `json:"runtime_compatibility"`
	}
	if err = json.Unmarshal(output, &response); err != nil {
		return domain.ModelArtifact{}, fmt.Errorf("decode Hugging Face model identity: %w", err)
	}
	if response.Repository == "" || !commitPattern.MatchString(response.ImmutableRevision) {
		return domain.ModelArtifact{}, errors.New("Hugging Face returned an invalid immutable model identity")
	}
	compatibility, _ := json.Marshal(response.RuntimeCompatibility)
	return domain.ModelArtifact{Source: "huggingface", Repository: response.Repository, RequestedRevision: response.RequestedRevision, ImmutableRevision: response.ImmutableRevision, ModelIdentity: response.Repository + "@" + response.ImmutableRevision, ApproximateSizeBytes: response.ApproximateSizeBytes, CacheState: "unknown", RuntimeCompatibilityJSON: string(compatibility)}, nil
}
