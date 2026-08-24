package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizedartifact"
)

type optimizedArtifactView struct {
	ID                      string `json:"id"`
	BaseModelArtifactID     string `json:"base_model_artifact_id"`
	Kind                    string `json:"kind"`
	Format                  string `json:"format"`
	Tool                    string `json:"tool"`
	ToolVersion             string `json:"tool_version"`
	Algorithm               string `json:"algorithm"`
	BuilderImageDigest      string `json:"builder_image_digest"`
	State                   string `json:"state"`
	EvidenceState           string `json:"evidence_state"`
	OutputRepository        string `json:"output_repository"`
	OutputImmutableRevision string `json:"output_immutable_revision"`
	OutputDigest            string `json:"output_digest"`
	QualityEvidenceID       string `json:"quality_evidence_id"`
	FailureCode             string `json:"failure_code"`
	RequiresQualityReview   bool   `json:"requires_quality_review"`
}

func optimizedArtifactCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane artifact optimize plan BASE_ARTIFACT | list | inspect|start|attest|qualify OPTIMIZED_ARTIFACT")
	}
	action := args[0]
	if action == "plan" {
		return optimizedArtifactPlanCommand(ctx, cfg, args[1:])
	}
	fs := flag.NewFlagSet("artifact optimize "+action, flag.ContinueOnError)
	limit := fs.Int("limit", 20, "maximum optimized artifacts")
	state := fs.String("state", "ready", "ready or failed")
	outputRepository := fs.String("output-repository", "", "immutable optimized output repository")
	outputRevision := fs.String("output-revision", "", "immutable optimized output revision")
	outputDigest := fs.String("output-digest", "", "optimized output sha256 digest")
	evidenceFile := fs.String("evidence-file", "", "bounded content-free build evidence JSON")
	failureCode := fs.String("failure-code", "", "stable builder failure code")
	qualityEvidenceID := fs.String("quality-evidence", "", "passing signed revision quality-evidence ID")
	candidateRunID := fs.String("candidate-run", "", "exact optimization candidate run evaluated")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if action == "list" {
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane artifact optimize list [--limit N]")
		}
		var response struct {
			Data []optimizedArtifactView `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimized-artifacts?limit="+strconv.Itoa(*limit), "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		if len(response.Data) == 0 {
			fmt.Println("No optimized artifacts.")
			return nil
		}
		for _, artifact := range response.Data {
			fmt.Printf("%-36s  %-22s  %-12s  %s@%s\n", artifact.ID, artifact.Kind, artifact.EvidenceState, artifact.Tool, artifact.ToolVersion)
		}
		return nil
	}
	if fs.NArg() != 1 {
		return errors.New("usage: infercrane artifact optimize " + action + " OPTIMIZED_ARTIFACT [flags]")
	}
	id := fs.Arg(0)
	path := "/api/v1/optimized-artifacts/" + url.PathEscape(id)
	var response struct {
		Artifact optimizedArtifactView `json:"artifact"`
	}
	var err error
	switch action {
	case "inspect":
		err = controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response)
	case "start":
		err = controlJSON(ctx, cfg, http.MethodPost, path+"/build", "", map[string]any{}, &response)
	case "attest":
		buildEvidence, readErr := readJSONObject(*evidenceFile)
		if readErr != nil {
			return readErr
		}
		attestation := optimizedartifact.Attestation{OutputRepository: *outputRepository, OutputImmutableRevision: *outputRevision, OutputDigest: *outputDigest, BuildEvidence: buildEvidence, FailureCode: *failureCode}
		if validationErr := optimizedartifact.ValidateAttestation(*state, attestation); validationErr != nil {
			return validationErr
		}
		err = controlJSON(ctx, cfg, http.MethodPost, path+"/attest", "", map[string]any{"state": *state, "attestation": attestation}, &response)
	case "qualify":
		if *qualityEvidenceID == "" || *candidateRunID == "" {
			return errors.New("qualify requires --candidate-run and --quality-evidence")
		}
		err = controlJSON(ctx, cfg, http.MethodPost, path+"/qualify", "", map[string]any{"candidate_run_id": *candidateRunID, "quality_evidence_id": *qualityEvidenceID}, &response)
	default:
		return fmt.Errorf("unknown optimized artifact action %q", action)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	printOptimizedArtifact(response.Artifact)
	return nil
}

func optimizedArtifactPlanCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane artifact optimize plan BASE_ARTIFACT --preset fp8|awq|gptq|nvfp4|eagle3|mtp|dflash|tensorrt --tool-version VERSION --builder-image-digest sha256:... --license-spdx SPDX")
	}
	baseArtifactID := args[0]
	fs := flag.NewFlagSet("artifact optimize plan", flag.ContinueOnError)
	preset := fs.String("preset", "", "fp8, awq, gptq, nvfp4, eagle3, mtp, dflash, or tensorrt")
	planFile := fs.String("plan-file", "", "advanced complete optimized-artifact plan JSON")
	toolVersion := fs.String("tool-version", "", "exact external builder version")
	builderDigest := fs.String("builder-image-digest", "", "builder OCI sha256 digest")
	calibrationDigest := fs.String("calibration-digest", "", "content-free calibration dataset sha256 digest")
	licenseSPDX := fs.String("license-spdx", "", "output model/artifact SPDX expression")
	idempotencyKey := fs.String("idempotency-key", "", "stable safe-retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected optimized artifact plan arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var plan optimizedartifact.Plan
	var err error
	if *planFile != "" {
		plan, err = readOptimizedArtifactPlan(*planFile)
		if err != nil {
			return err
		}
		if plan.BaseModelArtifactID != baseArtifactID {
			return errors.New("plan base_model_artifact_id must match the command argument")
		}
	} else {
		plan, err = optimizedArtifactPreset(baseArtifactID, *preset, *toolVersion, *builderDigest, *calibrationDigest, *licenseSPDX)
		if err != nil {
			return err
		}
	}
	if err = optimizedartifact.ValidatePlan(plan); err != nil {
		return err
	}
	if *idempotencyKey == "" {
		digest, digestErr := optimizedartifact.InputDigest(plan)
		if digestErr != nil {
			return digestErr
		}
		*idempotencyKey = "optimized-artifact-" + digest[:24]
	}
	var response struct {
		Artifact         optimizedArtifactView `json:"artifact"`
		Created          bool                  `json:"created"`
		BuilderExecution string                `json:"builder_execution"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/optimized-artifacts", *idempotencyKey, plan, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	printOptimizedArtifact(response.Artifact)
	fmt.Printf("Builder     external · not started\n\nNext: run the digest-pinned builder, then `infercrane artifact optimize start %s` and attest its immutable output.\n", response.Artifact.ID)
	return nil
}

func optimizedArtifactPreset(base, preset, toolVersion, builderDigest, calibrationDigest, licenseSPDX string) (optimizedartifact.Plan, error) {
	if toolVersion == "" || builderDigest == "" || licenseSPDX == "" {
		return optimizedartifact.Plan{}, errors.New("preset requires --tool-version, --builder-image-digest, and --license-spdx")
	}
	plan := optimizedartifact.Plan{BaseModelArtifactID: base, ToolVersion: toolVersion, BuilderImageDigest: builderDigest, CalibrationDigest: calibrationDigest, LicenseSPDX: licenseSPDX, RequiresQualityReview: true}
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "fp8":
		plan.Kind, plan.Format, plan.Tool, plan.Algorithm = optimizedartifact.KindQuantized, "safetensors", "llm-compressor", "w8a8-fp8"
		plan.Configuration, plan.HardwareConstraints = json.RawMessage(`{"scheme":"FP8"}`), json.RawMessage(`{"minimum_compute_capability":"8.9"}`)
	case "awq", "gptq":
		plan.Kind, plan.Format, plan.Tool, plan.Algorithm = optimizedartifact.KindQuantized, "safetensors", "llm-compressor", strings.ToLower(preset)
		plan.Configuration, plan.HardwareConstraints = json.RawMessage(`{"weight_only":true,"bits":4}`), json.RawMessage(`{"runtime":"vllm"}`)
	case "nvfp4":
		plan.Kind, plan.Format, plan.Tool, plan.Algorithm = optimizedartifact.KindQuantized, "safetensors", "modelopt", "nvfp4"
		plan.Configuration, plan.HardwareConstraints = json.RawMessage(`{"scheme":"NVFP4"}`), json.RawMessage(`{"vendor":"nvidia","exact_gpu_required":true,"runtime_qualification_required":true}`)
	case "eagle3", "mtp", "dflash":
		method := strings.ToLower(strings.TrimSpace(preset))
		plan.Kind, plan.Format, plan.Tool, plan.Algorithm = optimizedartifact.KindSpeculator, "safetensors", "vllm-speculators", method
		plan.Configuration, plan.HardwareConstraints = json.RawMessage(`{"method":"`+method+`"}`), json.RawMessage(`{"runtime":"vllm","exact_verifier_required":true}`)
	case "tensorrt":
		plan.Kind, plan.Format, plan.Tool, plan.Algorithm = optimizedartifact.KindTensorRTEngine, "tensorrt-engine", "tensorrt-llm", "engine-build"
		plan.Configuration, plan.HardwareConstraints = json.RawMessage(`{"strongly_typed":true}`), json.RawMessage(`{"vendor":"nvidia","exact_gpu_required":true}`)
	default:
		return optimizedartifact.Plan{}, errors.New("preset must be fp8, awq, gptq, nvfp4, eagle3, mtp, dflash, or tensorrt")
	}
	return plan, nil
}

func readOptimizedArtifactPlan(path string) (optimizedartifact.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return optimizedartifact.Plan{}, fmt.Errorf("read plan: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var plan optimizedartifact.Plan
	if err = decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("decode plan: %w", err)
	}
	if err = requireJSONEOF(decoder); err != nil {
		return plan, err
	}
	return plan, nil
}

func readJSONObject(path string) (json.RawMessage, error) {
	if path == "" {
		return json.RawMessage(`{}`), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read evidence: %w", err)
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err = decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("build evidence must be one JSON object")
	}
	if err = requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON file must contain exactly one value")
	}
	return nil
}

func printOptimizedArtifact(artifact optimizedArtifactView) {
	fmt.Printf("Optimized artifact %s · %s\n", artifact.ID, artifact.State)
	fmt.Printf("Base        %s\nKind        %s\nBuilder     %s@%s\nAlgorithm   %s\nEvidence    %s\n", artifact.BaseModelArtifactID, artifact.Kind, artifact.Tool, artifact.ToolVersion, artifact.Algorithm, artifact.EvidenceState)
	if artifact.OutputDigest != "" {
		fmt.Printf("Output      %s@%s\nDigest      %s\n", artifact.OutputRepository, artifact.OutputImmutableRevision, artifact.OutputDigest)
	}
	if artifact.FailureCode != "" {
		fmt.Printf("Failure     %s\n", artifact.FailureCode)
	}
}
