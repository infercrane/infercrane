// Package kernelresearch implements the bounded, model-neutral inner loop for
// authored GPU-kernel research. It decides keep/reject/stop from immutable
// correctness and timing evidence; it never edits source or promotes a runtime.
package kernelresearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	IntentSchemaV1  = "infercrane.kernel-research-intent/v1"
	ReceiptSchemaV1 = "infercrane.kernel-research-receipt/v1"
)

type Identity struct {
	InputDigest           string `json:"input_digest"`
	CandidateID           string `json:"candidate_id"`
	ProfileArtifactDigest string `json:"profile_artifact_digest"`
	ModelRevision         string `json:"model_revision"`
	RuntimeImageDigest    string `json:"runtime_image_digest"`
	Hardware              string `json:"hardware"`
	WorkloadDigest        string `json:"workload_digest"`
}

type Policy struct {
	Enabled                  bool    `json:"enabled"`
	MaxIterations            int     `json:"max_iterations"`
	MaxConsecutiveRejections int     `json:"max_consecutive_rejections"`
	MaxDurationSeconds       int     `json:"max_duration_seconds"`
	MaxCostUSD               float64 `json:"max_cost_usd"`
	MinRelativeImprovement   float64 `json:"min_relative_improvement"`
	TargetKernelSpeedup      float64 `json:"target_kernel_speedup"`
	MaxRooflineUtilization   float64 `json:"max_roofline_utilization"`
	HotspotFraction          float64 `json:"hotspot_fraction"`
}

type Intent struct {
	SchemaVersion string   `json:"schema_version"`
	Identity      Identity `json:"identity"`
	Policy        Policy   `json:"policy"`
}

type Correctness struct {
	Smoke              bool `json:"smoke"`
	ShapeSweep         bool `json:"shape_sweep"`
	NumericalStability bool `json:"numerical_stability"`
	Determinism        bool `json:"determinism"`
	EdgeCases          bool `json:"edge_cases"`
	Sanitizer          bool `json:"sanitizer"`
}

func (c Correctness) Passed() bool {
	return c.Smoke && c.ShapeSweep && c.NumericalStability && c.Determinism && c.EdgeCases && c.Sanitizer
}

type Attempt struct {
	Iteration           int         `json:"iteration"`
	SourceRevision      string      `json:"source_revision"`
	Backend             string      `json:"backend"`
	ArtifactDigest      string      `json:"artifact_digest"`
	Correctness         Correctness `json:"correctness"`
	BaselineMedianUS    float64     `json:"baseline_median_us"`
	CandidateMedianUS   float64     `json:"candidate_median_us"`
	RooflineUtilization float64     `json:"roofline_utilization"`
	CostUSD             float64     `json:"cost_usd"`
	CompletedAt         time.Time   `json:"completed_at"`
}

type Observation struct {
	Attempt
	KernelSpeedup   float64 `json:"kernel_speedup"`
	EndToEndSpeedup float64 `json:"end_to_end_speedup"`
	Decision        string  `json:"decision"`
	Reason          string  `json:"reason"`
}

type Receipt struct {
	SchemaVersion         string        `json:"schema_version"`
	Identity              Identity      `json:"identity"`
	Policy                Policy        `json:"policy"`
	StartedAt             time.Time     `json:"started_at"`
	CompletedAt           time.Time     `json:"completed_at,omitempty"`
	State                 string        `json:"state"`
	BestSourceRevision    string        `json:"best_source_revision,omitempty"`
	BestKernelSpeedup     float64       `json:"best_kernel_speedup"`
	BestEndToEndSpeedup   float64       `json:"best_end_to_end_speedup"`
	ConsecutiveRejections int           `json:"consecutive_rejections"`
	CostUSD               float64       `json:"cost_usd"`
	Observations          []Observation `json:"observations"`
	ReceiptDigest         string        `json:"receipt_digest,omitempty"`
}

func NormalizePolicy(policy Policy) Policy {
	if policy.MaxIterations == 0 {
		policy.MaxIterations = 40
	}
	if policy.MaxConsecutiveRejections == 0 {
		policy.MaxConsecutiveRejections = 5
	}
	if policy.MaxDurationSeconds == 0 {
		policy.MaxDurationSeconds = 2 * 60 * 60
	}
	if policy.MinRelativeImprovement == 0 {
		policy.MinRelativeImprovement = 1.01
	}
	if policy.TargetKernelSpeedup == 0 {
		policy.TargetKernelSpeedup = 2
	}
	if policy.MaxRooflineUtilization == 0 {
		policy.MaxRooflineUtilization = .90
	}
	return policy
}

func NewIntent(identity Identity, policy Policy) (Intent, error) {
	intent := Intent{SchemaVersion: IntentSchemaV1, Identity: normalizeIdentity(identity), Policy: NormalizePolicy(policy)}
	if err := intent.Validate(); err != nil {
		return Intent{}, err
	}
	return intent, nil
}

func (i Intent) Validate() error {
	if i.SchemaVersion != IntentSchemaV1 {
		return fmt.Errorf("schema_version must be %q", IntentSchemaV1)
	}
	if err := validateIdentity(i.Identity); err != nil {
		return err
	}
	return ValidatePolicy(i.Policy, true)
}

// ValidatePolicy validates bounded search authority. The hotspot is supplied
// by measured profiler evidence inside Accelerator Lab, so request validation
// may defer that single field until an intent is constructed.
func ValidatePolicy(policy Policy, requireHotspot bool) error {
	if !policy.Enabled {
		return errors.New("kernel research intent must be enabled")
	}
	if policy.MaxIterations < 1 || policy.MaxIterations > 1000 || policy.MaxConsecutiveRejections < 1 || policy.MaxConsecutiveRejections > policy.MaxIterations || policy.MaxDurationSeconds < 1 || policy.MaxDurationSeconds > 24*60*60 {
		return errors.New("kernel research iteration, plateau, or duration bound is invalid")
	}
	if invalidPositive(policy.MaxCostUSD) || policy.MinRelativeImprovement <= 1 || policy.MinRelativeImprovement > 2 || policy.TargetKernelSpeedup <= 1 || policy.MaxRooflineUtilization <= 0 || policy.MaxRooflineUtilization > 1 {
		return errors.New("kernel research cost, improvement, roofline, or hotspot policy is invalid")
	}
	if requireHotspot && (policy.HotspotFraction <= 0 || policy.HotspotFraction >= 1) {
		return errors.New("kernel research hotspot fraction is invalid")
	}
	return nil
}

func Start(intent Intent, now time.Time) (Receipt, error) {
	if err := intent.Validate(); err != nil {
		return Receipt{}, err
	}
	if now.IsZero() {
		return Receipt{}, errors.New("kernel research start time is required")
	}
	return Receipt{
		SchemaVersion:       ReceiptSchemaV1,
		Identity:            intent.Identity,
		Policy:              intent.Policy,
		StartedAt:           now.UTC(),
		State:               "searching",
		BestKernelSpeedup:   1,
		BestEndToEndSpeedup: 1,
	}, nil
}

// Observe applies one immutable experiment result. The caller edits and builds
// source; this state machine only retains a candidate when all correctness
// stages pass and it beats the current best by the configured margin.
func Observe(receipt Receipt, attempt Attempt) (Receipt, Observation, error) {
	if receipt.State != "searching" {
		return receipt, Observation{}, errors.New("kernel research session is not accepting attempts")
	}
	if attempt.Iteration != len(receipt.Observations)+1 {
		return receipt, Observation{}, errors.New("kernel research iterations must be contiguous")
	}
	if err := validateAttempt(attempt); err != nil {
		return receipt, Observation{}, err
	}
	deadline := receipt.StartedAt.Add(time.Duration(receipt.Policy.MaxDurationSeconds) * time.Second)
	if attempt.CompletedAt.After(deadline) {
		return finish(receipt, "duration_exhausted", attempt.CompletedAt), Observation{}, errors.New("kernel research duration authority expired")
	}
	if receipt.CostUSD+attempt.CostUSD > receipt.Policy.MaxCostUSD {
		return finish(receipt, "budget_exhausted", attempt.CompletedAt), Observation{}, errors.New("kernel research cost authority exhausted")
	}

	speedup := attempt.BaselineMedianUS / attempt.CandidateMedianUS
	endToEnd := 1 / ((1 - receipt.Policy.HotspotFraction) + receipt.Policy.HotspotFraction/speedup)
	observation := Observation{Attempt: attempt, KernelSpeedup: speedup, EndToEndSpeedup: endToEnd}
	receipt.CostUSD += attempt.CostUSD

	switch {
	case !attempt.Correctness.Passed():
		observation.Decision, observation.Reason = "reject", "five_stage_correctness_failed"
		receipt.ConsecutiveRejections++
	case speedup < receipt.BestKernelSpeedup*receipt.Policy.MinRelativeImprovement:
		observation.Decision, observation.Reason = "reject", "improvement_below_keep_threshold"
		receipt.ConsecutiveRejections++
	default:
		observation.Decision, observation.Reason = "keep", "correct_and_materially_faster"
		receipt.BestSourceRevision = attempt.SourceRevision
		receipt.BestKernelSpeedup = speedup
		receipt.BestEndToEndSpeedup = endToEnd
		receipt.ConsecutiveRejections = 0
	}
	receipt.Observations = append(receipt.Observations, observation)

	switch {
	case receipt.BestKernelSpeedup >= receipt.Policy.TargetKernelSpeedup:
		receipt = finish(receipt, "target_reached", attempt.CompletedAt)
	case attempt.RooflineUtilization >= receipt.Policy.MaxRooflineUtilization && observation.Decision == "keep":
		receipt = finish(receipt, "roofline_reached", attempt.CompletedAt)
	case receipt.ConsecutiveRejections >= receipt.Policy.MaxConsecutiveRejections:
		receipt = finish(receipt, "plateau", attempt.CompletedAt)
	case len(receipt.Observations) >= receipt.Policy.MaxIterations:
		receipt = finish(receipt, "iteration_exhausted", attempt.CompletedAt)
	}
	return receipt, observation, nil
}

func Complete(receipt Receipt, now time.Time) (Receipt, error) {
	if receipt.State != "searching" {
		return receipt, errors.New("kernel research session is already complete")
	}
	if now.IsZero() {
		return receipt, errors.New("completion time is required")
	}
	return finish(receipt, "completed", now), nil
}

func ValidateReceipt(intent Intent, receipt Receipt) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if receipt.SchemaVersion != ReceiptSchemaV1 || receipt.Identity != intent.Identity || receipt.Policy != intent.Policy || receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() || receipt.State == "searching" || len(receipt.Observations) == 0 {
		return errors.New("kernel research receipt is incomplete or not bound to its intent")
	}
	want := receiptDigest(receipt)
	if receipt.ReceiptDigest != want {
		return errors.New("kernel research receipt digest mismatch")
	}
	return nil
}

func finish(receipt Receipt, state string, now time.Time) Receipt {
	receipt.State = state
	receipt.CompletedAt = now.UTC()
	receipt.ReceiptDigest = receiptDigest(receipt)
	return receipt
}

func receiptDigest(receipt Receipt) string {
	receipt.ReceiptDigest = ""
	body, _ := json.Marshal(receipt)
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateAttempt(attempt Attempt) error {
	if attempt.Iteration < 1 || !hexRevision(attempt.SourceRevision) || strings.TrimSpace(attempt.Backend) == "" || !digest(attempt.ArtifactDigest) || attempt.CompletedAt.IsZero() {
		return errors.New("kernel research attempt requires immutable source, backend, artifact, iteration, and completion time")
	}
	if invalidPositive(attempt.BaselineMedianUS) || invalidPositive(attempt.CandidateMedianUS) || attempt.RooflineUtilization < 0 || attempt.RooflineUtilization > 1 || math.IsNaN(attempt.RooflineUtilization) || math.IsInf(attempt.RooflineUtilization, 0) || attempt.CostUSD < 0 || math.IsNaN(attempt.CostUSD) || math.IsInf(attempt.CostUSD, 0) {
		return errors.New("kernel research timing, roofline, or cost evidence is invalid")
	}
	return nil
}

func normalizeIdentity(identity Identity) Identity {
	identity.InputDigest = strings.ToLower(strings.TrimSpace(identity.InputDigest))
	identity.CandidateID = strings.TrimSpace(identity.CandidateID)
	identity.ProfileArtifactDigest = strings.ToLower(strings.TrimSpace(identity.ProfileArtifactDigest))
	identity.ModelRevision = strings.ToLower(strings.TrimSpace(identity.ModelRevision))
	identity.RuntimeImageDigest = strings.ToLower(strings.TrimSpace(identity.RuntimeImageDigest))
	identity.Hardware = strings.TrimSpace(identity.Hardware)
	identity.WorkloadDigest = strings.ToLower(strings.TrimSpace(identity.WorkloadDigest))
	return identity
}

func validateIdentity(identity Identity) error {
	if !digest(identity.InputDigest) || identity.CandidateID == "" || !digest(identity.ProfileArtifactDigest) || !hexRevision(identity.ModelRevision) || !digest(identity.RuntimeImageDigest) || identity.Hardware == "" || !digest(identity.WorkloadDigest) {
		return errors.New("kernel research identity must bind the input, candidate, profile, model, runtime, hardware, and workload")
	}
	return nil
}

func invalidPositive(value float64) bool {
	return value <= 0 || math.IsNaN(value) || math.IsInf(value, 0)
}

func digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func hexRevision(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
