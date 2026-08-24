// Package optimizedartifact validates immutable artifacts produced by external
// optimization builders. InferCrane owns provenance and release policy, not the
// CUDA/model transformation implementation.
package optimizedartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	KindQuantized      = "quantized_checkpoint"
	KindSpeculator     = "speculator_checkpoint"
	KindTensorRTEngine = "tensorrt_engine"

	StatePlanned  = "planned"
	StateBuilding = "building"
	StateReady    = "ready"
	StateFailed   = "failed"
	StateStale    = "stale"
)

var supportedTools = map[string]map[string]struct{}{
	KindQuantized:      {"llm-compressor": {}, "modelopt": {}},
	KindSpeculator:     {"vllm-speculators": {}},
	KindTensorRTEngine: {"tensorrt-llm": {}},
}

type Plan struct {
	BaseModelArtifactID   string          `json:"base_model_artifact_id"`
	Kind                  string          `json:"kind"`
	Format                string          `json:"format"`
	Tool                  string          `json:"tool"`
	ToolVersion           string          `json:"tool_version"`
	Algorithm             string          `json:"algorithm"`
	BuilderImageDigest    string          `json:"builder_image_digest"`
	CalibrationDigest     string          `json:"calibration_digest,omitempty"`
	LicenseSPDX           string          `json:"license_spdx"`
	Configuration         json.RawMessage `json:"configuration"`
	HardwareConstraints   json.RawMessage `json:"hardware_constraints"`
	RequiresQualityReview bool            `json:"requires_quality_review"`
}

type Attestation struct {
	OutputRepository        string          `json:"output_repository"`
	OutputImmutableRevision string          `json:"output_immutable_revision"`
	OutputDigest            string          `json:"output_digest"`
	BuildEvidence           json.RawMessage `json:"build_evidence"`
	FailureCode             string          `json:"failure_code,omitempty"`
}

func ValidatePlan(plan Plan) error {
	if plan.BaseModelArtifactID == "" || plan.Kind == "" || plan.Format == "" || plan.Tool == "" || plan.ToolVersion == "" || plan.Algorithm == "" || plan.LicenseSPDX == "" {
		return errors.New("optimized artifact plan requires base artifact, kind, format, exact tool version, algorithm, and SPDX license")
	}
	tools, ok := supportedTools[plan.Kind]
	if !ok {
		return fmt.Errorf("unsupported optimized artifact kind %q", plan.Kind)
	}
	if _, ok = tools[plan.Tool]; !ok {
		return fmt.Errorf("tool %q cannot produce artifact kind %q", plan.Tool, plan.Kind)
	}
	if !isDigest(plan.BuilderImageDigest) {
		return errors.New("builder image must be pinned by sha256 digest")
	}
	if plan.CalibrationDigest != "" && !isDigest(plan.CalibrationDigest) {
		return errors.New("calibration dataset must be represented by a sha256 digest")
	}
	if !boundedObject(plan.Configuration) || !boundedObject(plan.HardwareConstraints) {
		return errors.New("configuration and hardware constraints must be bounded JSON objects")
	}
	if !plan.RequiresQualityReview {
		return errors.New("optimized inference artifacts must require semantic quality review")
	}
	return nil
}

func ValidateAttestation(state string, attestation Attestation) error {
	if !boundedObject(attestation.BuildEvidence) {
		return errors.New("build evidence must be a bounded JSON object")
	}
	switch state {
	case StateReady:
		if attestation.OutputRepository == "" || attestation.OutputImmutableRevision == "" || !isDigest(attestation.OutputDigest) || attestation.FailureCode != "" {
			return errors.New("ready artifact requires immutable output repository, revision, and sha256 digest without a failure code")
		}
	case StateFailed:
		if attestation.FailureCode == "" || attestation.OutputDigest != "" || attestation.OutputRepository != "" || attestation.OutputImmutableRevision != "" {
			return errors.New("failed artifact requires a failure code and cannot claim output identity")
		}
	default:
		return errors.New("attestation state must be ready or failed")
	}
	return nil
}

func InputDigest(plan Plan) (string, error) {
	if err := ValidatePlan(plan); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func isDigest(value string) bool {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func boundedObject(value json.RawMessage) bool {
	if len(value) < 2 || len(value) > 1<<20 || !json.Valid(value) {
		return false
	}
	var object map[string]any
	return json.Unmarshal(value, &object) == nil && object != nil
}
