// Package recipe creates immutable serving recipes from qualified repository evidence.
package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

type Payload struct {
	SchemaVersion     string          `json:"schema_version"`
	Name              string          `json:"name"`
	Version           string          `json:"version"`
	ModelRepository   string          `json:"model_repository"`
	ModelRevision     string          `json:"model_revision"`
	ModelIdentity     string          `json:"model_identity"`
	Runtime           string          `json:"runtime"`
	RuntimeVersion    string          `json:"runtime_version"`
	RuntimeArgs       []string        `json:"runtime_args,omitempty"`
	Provider          string          `json:"provider"`
	ProviderAdapter   string          `json:"provider_adapter,omitempty"`
	Region            string          `json:"region,omitempty"`
	GPU               string          `json:"gpu"`
	ComputeMode       string          `json:"compute_mode"`
	Workload          any             `json:"workload,omitempty"`
	BenchmarkID       string          `json:"benchmark_id"`
	BenchmarkWorkload json.RawMessage `json:"benchmark_workload"`
}

type Provenance struct {
	SourceDeployment  string `json:"source_deployment"`
	SourceRevision    string `json:"source_revision"`
	ArtifactID        string `json:"artifact_id"`
	BenchmarkTool     string `json:"benchmark_tool"`
	BenchmarkVersion  string `json:"benchmark_version"`
	InferCraneVersion string `json:"infercrane_version"`
	EvidenceClass     string `json:"evidence_class"`
}

func Build(name, version, infercraneVersion string, artifact domain.ModelArtifact, revision domain.DeploymentRevision, benchmark domain.BenchmarkResult) (domain.ModelRecipe, error) {
	name, version = strings.TrimSpace(name), strings.TrimSpace(version)
	if name == "" || version == "" || len(name) > 128 || len(version) > 64 {
		return domain.ModelRecipe{}, errors.New("bounded recipe name and version are required")
	}
	if artifact.Repository == "" || artifact.ImmutableRevision == "" || artifact.ModelIdentity == "" || benchmark.ID == "" || benchmark.ModelIdentity != artifact.ModelIdentity || benchmark.RevisionID != revision.ID {
		return domain.ModelRecipe{}, errors.New("recipe requires matching immutable artifact, revision, and benchmark evidence")
	}
	var spec domain.DeploymentRevisionSpec
	if json.Unmarshal([]byte(revision.SpecJSON), &spec) != nil || spec.Runtime == "" || spec.RuntimeVersion == "" {
		return domain.ModelRecipe{}, errors.New("recipe requires a versioned runtime revision")
	}
	if !json.Valid([]byte(benchmark.WorkloadJSON)) {
		return domain.ModelRecipe{}, errors.New("recipe benchmark workload is invalid")
	}
	payload := Payload{SchemaVersion: "infercrane.recipe/v1", Name: name, Version: version, ModelRepository: artifact.Repository, ModelRevision: artifact.ImmutableRevision, ModelIdentity: artifact.ModelIdentity, Runtime: spec.Runtime, RuntimeVersion: spec.RuntimeVersion, RuntimeArgs: spec.RuntimeArgs, Provider: spec.Cloud, ProviderAdapter: spec.ProviderAdapter, Region: spec.Region, GPU: spec.GPU, ComputeMode: spec.ComputeMode, Workload: spec.Workload, BenchmarkID: benchmark.ID, BenchmarkWorkload: json.RawMessage(benchmark.WorkloadJSON)}
	provenance := Provenance{SourceDeployment: benchmark.DeploymentName, SourceRevision: revision.ID, ArtifactID: artifact.ID, BenchmarkTool: benchmark.Tool, BenchmarkVersion: benchmark.ToolVersion, InferCraneVersion: infercraneVersion, EvidenceClass: "measured"}
	payloadJSON, _ := json.Marshal(payload)
	provenanceJSON, _ := json.Marshal(provenance)
	digestInput, _ := json.Marshal(struct {
		Payload    json.RawMessage `json:"payload"`
		Provenance json.RawMessage `json:"provenance"`
	}{Payload: payloadJSON, Provenance: provenanceJSON})
	digest := sha256.Sum256(digestInput)
	return domain.ModelRecipe{Name: name, Version: version, Digest: hex.EncodeToString(digest[:]), PayloadJSON: string(payloadJSON), ProvenanceJSON: string(provenanceJSON)}, nil
}
