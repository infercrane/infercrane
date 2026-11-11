package kernelgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/kernelresearch"
)

const (
	EvaluationSchemaV1 = "infercrane.kernel-evaluation/v1"

	maxEvaluatorOutputBytes = 1 << 20
	defaultEvaluatorTimeout = 30 * time.Minute
)

// EvaluationRequest crosses into the isolated evaluator. It carries source and
// identity only, never credentials or customer content.
type EvaluationRequest struct {
	SchemaVersion      string                         `json:"schema_version"`
	InputDigest        string                         `json:"input_digest"`
	CandidateID        string                         `json:"candidate_id"`
	Iteration          int                            `json:"iteration"`
	Family             kernelplanner.OperatorFamily   `json:"family"`
	Backend            string                         `json:"backend"`
	Filename           string                         `json:"filename"`
	Source             string                         `json:"source"`
	SourceRevision     string                         `json:"source_revision"`
	Hardware           kernelplanner.HardwareIdentity `json:"hardware"`
	RuntimeImageDigest string                         `json:"runtime_image_digest"`
	Shapes             []string                       `json:"shapes,omitempty"`
	DTypes             []string                       `json:"dtypes,omitempty"`
	Verification       bool                           `json:"verification"`
}

type EvaluationResult struct {
	Correctness         kernelresearch.Correctness `json:"correctness"`
	FailedStage         string                     `json:"failed_stage,omitempty"`
	BaselineMedianUS    float64                    `json:"baseline_median_us"`
	CandidateMedianUS   float64                    `json:"candidate_median_us"`
	RooflineUtilization float64                    `json:"roofline_utilization"`
	CostUSD             float64                    `json:"cost_usd"`
}

type Evaluator interface {
	Evaluate(context.Context, EvaluationRequest) (EvaluationResult, error)
}

// CommandEvaluator invokes an absolute launcher without a shell. The launcher
// owns isolation; tools/kernel-lab/run-isolated-evaluation.sh is the reference.
type CommandEvaluator struct {
	Executable  string
	Environment []string
	Timeout     time.Duration
	// GPUHourlyUSD converts wall time into attempt cost.
	GPUHourlyUSD float64
	// Unpriced marks a local CPU screen whose wall time costs nothing.
	Unpriced bool
}

func (e CommandEvaluator) Evaluate(ctx context.Context, request EvaluationRequest) (EvaluationResult, error) {
	if !filepath.IsAbs(e.Executable) {
		return EvaluationResult{}, errors.New("kernel evaluator executable must be absolute")
	}
	if info, err := os.Stat(e.Executable); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return EvaluationResult{}, errors.New("kernel evaluator must be an executable regular file")
	}
	if !e.Unpriced && (e.GPUHourlyUSD <= 0 || math.IsNaN(e.GPUHourlyUSD) || math.IsInf(e.GPUHourlyUSD, 0)) {
		return EvaluationResult{}, errors.New("kernel evaluator requires a positive GPU hourly cost")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return EvaluationResult{}, err
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultEvaluatorTimeout
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runContext, e.Executable)
	command.Env = append([]string(nil), e.Environment...)
	command.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	command.Stdout = &limitedWriter{buffer: &stdout, remaining: maxEvaluatorOutputBytes}
	started := time.Now()
	if err = command.Run(); err != nil {
		return EvaluationResult{}, fmt.Errorf("kernel evaluator failed: %w", err)
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	var result EvaluationResult
	if err = decoder.Decode(&result); err != nil {
		return EvaluationResult{}, fmt.Errorf("decode kernel evaluation: %w", err)
	}
	result.CostUSD = 0
	if !e.Unpriced {
		result.CostUSD = time.Since(started).Hours() * e.GPUHourlyUSD
	}
	return result, nil
}

type limitedWriter struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(value []byte) (int, error) {
	if len(value) > w.remaining {
		return 0, errors.New("kernel evaluator output exceeds the size limit")
	}
	w.remaining -= len(value)
	return w.buffer.Write(value)
}
