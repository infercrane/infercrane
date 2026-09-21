package acceleratorlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/brezelexecutor"
)

type BrezelRunner interface {
	Run(context.Context, brezelexecutor.Job) (brezelexecutor.Result, error)
}

// BrezelBuilder adapts the isolated Brezel job contract to Accelerator Lab.
// Brezel compiles and screens source; it never substitutes for qualification
// on the exact target accelerator.
type BrezelBuilder struct {
	Runner              BrezelRunner
	BuilderVersion      string
	EnvironmentRevision string
	TimeoutSeconds      int
}

func (builder BrezelBuilder) Build(ctx context.Context, request BuildRequest) (BuildEvidence, error) {
	if builder.Runner == nil || strings.TrimSpace(builder.BuilderVersion) == "" || strings.TrimSpace(builder.EnvironmentRevision) == "" {
		return BuildEvidence{}, errors.New("Brezel builder requires a runner, version, and immutable environment revision")
	}
	if len(request.Source.Artifacts) == 0 {
		return BuildEvidence{}, errors.New("Brezel build requires content-addressed source artifacts")
	}
	inputs := make([]brezelexecutor.ArtifactRef, 0, len(request.Source.Artifacts))
	for index, artifact := range request.Source.Artifacts {
		inputs = append(inputs, brezelexecutor.ArtifactRef{Name: fmt.Sprintf("source-%02d", index+1), URI: artifact.URI, SHA256: strings.TrimPrefix(strings.ToLower(artifact.SHA256), "sha256:")})
	}
	timeout := builder.TimeoutSeconds
	if timeout == 0 {
		timeout = 1800
	}
	job := brezelexecutor.Job{
		SchemaVersion: brezelexecutor.JobSchemaVersion,
		ID:            boundedID("lab", request.InputDigest+":"+request.Experiment.ID),
		CampaignID:    boundedID("campaign", request.CampaignID),
		CandidateID:   boundedID("candidate", request.CandidateID),
		Kind:          brezelexecutor.KindArtifactBuild,
		Model:         request.Model.Repository,
		ModelRevision: request.Model.Revision,
		TargetSM:      request.Hardware.ComputeCapability,
		Candidate: brezelexecutor.Candidate{
			ImplementationID: request.Source.ImplementationID,
			OperatorFamily:   string(request.Experiment.OperatorFamily),
			Backend:          sourceBackend(request),
			SourceRevision:   request.Source.Revision,
			License:          request.Source.License,
		},
		Inputs:      inputs,
		TimeoutSecs: timeout,
	}
	result, err := builder.Runner.Run(ctx, job)
	if err != nil {
		return BuildEvidence{}, err
	}
	if result.Status != "passed" {
		return BuildEvidence{}, errors.New("Brezel rejected the source or artifact build")
	}
	artifacts := make([]Artifact, 0, len(result.Artifacts))
	for _, output := range result.Artifacts {
		artifacts = append(artifacts, Artifact{Kind: output.Kind, URI: output.URI, SHA256: "sha256:" + strings.TrimPrefix(strings.ToLower(output.SHA256), "sha256:"), Size: output.Size})
	}
	checks := make([]string, 0, len(result.Checks))
	for _, check := range result.Checks {
		if check.Status != "passed" {
			return BuildEvidence{}, fmt.Errorf("Brezel check %q did not pass", check.Name)
		}
		checks = append(checks, check.Name)
	}
	if len(checks) == 0 || len(artifacts) == 0 || len(result.Receipt) == 0 {
		return BuildEvidence{}, errors.New("Brezel build omitted checks, published artifacts, or lifecycle receipt")
	}
	return BuildEvidence{
		InputDigest: request.InputDigest, Builder: "brezel", BuilderVersion: builder.BuilderVersion,
		Environment: builder.EnvironmentRevision, SourceDigest: hashJSON(request.Source), Status: "passed", Checks: checks,
		Artifacts: artifacts, ReceiptDigest: hashBytes(result.Receipt),
	}, nil
}

func sourceBackend(request BuildRequest) string {
	for _, implementation := range request.Experiment.ExistingImplementations {
		if implementation.ImplementationID == request.Source.ImplementationID {
			return implementation.Backend
		}
	}
	if len(request.Experiment.CompilerChoices) > 0 {
		return request.Experiment.CompilerChoices[0]
	}
	return "unknown"
}

func boundedID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func hashJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return hashBytes(encoded)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
