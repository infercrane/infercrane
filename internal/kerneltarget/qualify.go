package kerneltarget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/brezelexecutor"
	"github.com/infercrane/infercrane/internal/kernelgen"
)

const (
	kernelBundleKind   = "kernel-bundle"
	maxKernelBytes     = 8 << 20
	maxControlDrift    = 0.05
	replayRunsPerArm   = 2
	replayWarmup       = 16
	minReplayRequests  = 100
	defaultMaxTokens   = 128
	invokedMarkerName  = "invoked"
	defaultReplayType  = "single_turn"
	maxQualitySuiteLen = 16 << 20
)

type qualitySuite struct {
	Prompts   []string `json:"prompts"`
	MaxTokens int      `json:"max_tokens,omitempty"`
}

// contentAddressedFile resolves an identity such as name@sha256:<hex> to a
// local file the operator placed inside the customer boundary. Nothing is
// downloaded, and a file that does not match its identity is refused.
func (t Target) contentAddressedFile(identity string) (string, error) {
	index := strings.LastIndex(identity, "@sha256:")
	if index < 0 || !digestPattern.MatchString(identity[index+1:]) {
		return "", fmt.Errorf("%q is not a content-addressed identity", identity)
	}
	digest := identity[index+len("@sha256:"):]
	// Rooting the digest keeps the path inside the replay directory.
	path := filepath.Join(t.ReplayDir, filepath.Clean("/"+digest))
	actual, err := hashFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", identity, err)
	}
	if actual != digest {
		return "", fmt.Errorf("%s does not match its content digest", identity)
	}
	return path, nil
}

func (t Target) fetchKernel(ctx context.Context, build acceleratorlab.BuildEvidence) (string, acceleratorlab.Artifact, error) {
	for _, artifact := range build.Artifacts {
		if artifact.Kind != kernelBundleKind {
			continue
		}
		reader, err := t.Artifacts.Open(ctx, brezelexecutor.ArtifactRef{Name: "kernel", URI: artifact.URI, SHA256: strings.TrimPrefix(artifact.SHA256, "sha256:")})
		if err != nil {
			return "", artifact, fmt.Errorf("fetch built kernel: %w", err)
		}
		defer reader.Close()
		body, err := io.ReadAll(io.LimitReader(reader, maxKernelBytes+1))
		if err != nil {
			return "", artifact, fmt.Errorf("read built kernel: %w", err)
		}
		if len(body) > maxKernelBytes {
			return "", artifact, errors.New("built kernel exceeds the size limit")
		}
		sum := sha256.Sum256(body)
		if "sha256:"+hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return "", artifact, errors.New("built kernel does not match its digest")
		}
		return string(body), artifact, nil
	}
	return "", acceleratorlab.Artifact{}, errors.New("build evidence has no kernel bundle")
}

type armResult struct {
	result        benchmark.Result
	outputs       []string
	peakMiB       float64
	profileDigest string
}

type armRequest struct {
	server serverRequest
	suite  qualitySuite
	load   benchmark.Config
	// capture profiles the parity prompts, after the timed replay has finished
	// so that profiler overhead cannot affect the throughput comparison.
	capture bool
}

func (t Target) runArm(ctx context.Context, request armRequest) (armResult, error) {
	instance, err := t.start(ctx, request.server)
	if err != nil {
		return armResult{}, err
	}
	defer instance.stop()
	load := request.load
	load.Endpoint = instance.baseURL
	stopSampling := t.peakMemory(ctx)
	result, err := t.Load.Run(ctx, load)
	peak := stopSampling()
	if err != nil {
		return armResult{}, fmt.Errorf("replay workload: %w", err)
	}
	if result.Failed > 0 || result.Succeeded == 0 || result.OutputTokenThroughput == nil || *result.OutputTokenThroughput <= 0 {
		return armResult{}, fmt.Errorf("replay was not clean: %d failed of %d", result.Failed, result.Requests)
	}
	if request.capture {
		if _, err = instance.post(ctx, "/start_profile", t.profileRequestBody(request.server.Spec)); err != nil {
			return armResult{}, err
		}
	}
	outputs, err := instance.complete(ctx, request.server.Model.Repository, request.suite)
	if err != nil {
		return armResult{}, err
	}
	arm := armResult{result: result, outputs: outputs, peakMiB: peak}
	if request.capture {
		if _, err = instance.post(ctx, "/stop_profile", nil); err != nil {
			return armResult{}, err
		}
		if arm.profileDigest, err = digestTraces(request.server.ProfileDir); err != nil {
			return armResult{}, err
		}
	}
	return arm, nil
}

// complete runs the deterministic parity prompts. Output text is compared and
// hashed in memory and is never stored.
func (s *server) complete(ctx context.Context, model string, suite qualitySuite) ([]string, error) {
	maxTokens := suite.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	outputs := make([]string, 0, len(suite.Prompts))
	for _, prompt := range suite.Prompts {
		body, err := json.Marshal(map[string]any{
			"model": model, "temperature": 0, "seed": 17, "max_tokens": maxTokens, "stream": false,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
		})
		if err != nil {
			return nil, err
		}
		data, err := s.post(ctx, "/v1/chat/completions", body)
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err = json.Unmarshal(data, &decoded); err != nil || len(decoded.Choices) == 0 {
			return nil, errors.New("quality prompt returned no completion")
		}
		outputs = append(outputs, decoded.Choices[0].Message.Content)
	}
	return outputs, nil
}

func equalOutputs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func digestJSON(prefix string, value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return prefix + hex.EncodeToString(sum[:])
}

func (t Target) loadSuite(identity string) (qualitySuite, error) {
	path, err := t.contentAddressedFile(identity)
	if err != nil {
		return qualitySuite{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) > maxQualitySuiteLen {
		return qualitySuite{}, errors.New("quality suite is unreadable or too large")
	}
	var suite qualitySuite
	if err = json.Unmarshal(body, &suite); err != nil {
		return qualitySuite{}, fmt.Errorf("decode quality suite: %w", err)
	}
	if len(suite.Prompts) == 0 {
		return qualitySuite{}, errors.New("quality suite has no prompts")
	}
	return suite, nil
}

// Qualify replays the customer workload around the candidate as control,
// candidate, control. Control drift or a broken control is an infrastructure
// error; a candidate that fails is returned as evidence so the rejection is kept.
func (t Target) Qualify(ctx context.Context, request acceleratorlab.QualificationRequest) (acceleratorlab.QualificationEvidence, error) {
	started := t.now()
	var empty acceleratorlab.QualificationEvidence
	if err := validateBoundary(request.Model, request.Topology); err != nil {
		return empty, err
	}
	spec, err := t.runtime(request.Runtime)
	if err != nil {
		return empty, err
	}
	pack, err := kernelgen.LoadPack(t.PackDir, request.Experiment.OperatorFamily)
	if err != nil {
		return empty, err
	}
	integration, err := t.packIntegration(pack, spec)
	if err != nil {
		return empty, err
	}
	replayPath, err := t.contentAddressedFile(request.Workload.Replay)
	if err != nil {
		return empty, err
	}
	suite, err := t.loadSuite(request.Workload.QualitySuite)
	if err != nil {
		return empty, err
	}
	if err = t.verifyAccelerator(ctx, request.Hardware); err != nil {
		return empty, err
	}
	source, artifact, err := t.fetchKernel(ctx, request.Build)
	if err != nil {
		return empty, err
	}
	evidence := acceleratorlab.QualificationEvidence{
		InputDigest: request.InputDigest, Hardware: request.Hardware, RuntimeImageDigest: request.Runtime.ImageDigest, Topology: request.Topology,
		WorkloadDigest: request.Workload.Digest, Modality: request.Workload.Modality, Media: request.Workload.Media, Replay: request.Workload.Replay,
		QualitySuite: request.Workload.QualitySuite, BuildArtifactDigest: artifact.SHA256,
	}
	finish := func() (acceleratorlab.QualificationEvidence, error) {
		evidence.CostUSD, evidence.MeasuredAt = t.elapsedCost(started), t.now()
		return evidence, nil
	}

	hidden, err := t.runHiddenSuite(ctx, request, pack, source)
	if err != nil {
		return empty, err
	}
	evidence.CorrectnessPassed = hidden.correct
	evidence.HiddenShapesPassed = hidden.verified
	evidence.SanitizerPassed = hidden.sanitizer
	evidence.KernelSpeedup = hidden.speedup
	// No server ran yet, so the record that ended the run is the artifact.
	evidence.ProfilerArtifactDigest = digestJSON("sha256:", hidden.record)
	if !hidden.correct || !hidden.verified {
		return finish()
	}

	image := spec.Repository + "@" + request.Runtime.ImageDigest
	if err = t.materialize(ctx, spec, image, request.Model); err != nil {
		return empty, err
	}
	if err = t.ensureNetwork(ctx); err != nil {
		return empty, err
	}
	profileDir, err := t.privateDirectory("qualify-profile-")
	if err != nil {
		return empty, err
	}
	kernelDir, err := t.privateDirectory("qualify-kernel-")
	if err != nil {
		return empty, err
	}
	if err = os.WriteFile(filepath.Join(kernelDir, pack.Filename), []byte(source), 0o644); err != nil {
		return empty, fmt.Errorf("stage kernel: %w", err)
	}
	stateDir, err := t.privateDirectory("qualify-state-")
	if err != nil {
		return empty, err
	}
	replayType := t.ReplayType
	if replayType == "" {
		replayType = defaultReplayType
	}
	concurrency := max(1, request.Workload.Concurrency)
	load := benchmark.Config{
		Binary: t.AIPerf, Model: request.Model.Repository, Tokenizer: t.snapshotPath(request.Model), Requests: max(minReplayRequests, concurrency*16),
		Concurrency: concurrency, ProfileRuns: replayRunsPerArm, WarmupRequests: replayWarmup, InputFile: replayPath, DatasetType: replayType,
	}
	arm := func(name string, kernel *kernelMount) (armResult, error) {
		return t.runArm(ctx, armRequest{
			server: serverRequest{Spec: spec, Image: image, Model: request.Model, Name: containerName(name, request.InputDigest), ProfileDir: profileDir, Kernel: kernel},
			suite:  suite, load: load, capture: kernel != nil,
		})
	}

	controlBefore, err := arm("control-a", nil)
	if err != nil {
		return empty, fmt.Errorf("control arm: %w", err)
	}
	candidate, candidateErr := arm("candidate", &kernelMount{SourceDirectory: kernelDir, Filename: pack.Filename, StateDirectory: stateDir, Integration: integration, Args: pack.CandidateArgs})
	controlAfter, err := arm("control-b", nil)
	if err != nil {
		return empty, fmt.Errorf("control arm: %w", err)
	}
	before, after := *controlBefore.result.OutputTokenThroughput, *controlAfter.result.OutputTokenThroughput
	if math.Abs(before-after)/before > maxControlDrift {
		return empty, fmt.Errorf("control throughput drifted from %.1f to %.1f tok/s, so the comparison is invalid", before, after)
	}
	if !equalOutputs(controlBefore.outputs, controlAfter.outputs) {
		return empty, errors.New("control outputs were not deterministic, so parity cannot be judged")
	}
	if candidateErr != nil {
		return finish()
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, invokedMarkerName)); statErr != nil {
		return empty, errors.New("the generated kernel was never called during replay, so the result would measure the unmodified runtime")
	}

	baseline := (before + after) / 2
	evidence.ServingReplayMeasured = true
	evidence.ProfilerArtifactDigest = candidate.profileDigest
	evidence.EndToEndSpeedup = *candidate.result.OutputTokenThroughput / baseline
	evidence.CostRegressionPct = (1/evidence.EndToEndSpeedup - 1) * 100
	if control := max(controlBefore.peakMiB, controlAfter.peakMiB); control > 0 && candidate.peakMiB > 0 {
		evidence.MemoryRegressionPct = (candidate.peakMiB - control) / control * 100
	}
	evidence.QualityPassed = equalOutputs(controlBefore.outputs, candidate.outputs)
	evidence.BenchmarkEvidenceID = digestJSON("aiperf-", []benchmark.Result{controlBefore.result, candidate.result, controlAfter.result})
	evidence.QualityEvidenceID = digestJSON("parity-", candidate.outputs)
	return finish()
}

func (t Target) packIntegration(pack kernelgen.Pack, spec RuntimeSpec) (string, error) {
	if err := pack.SupportsRuntime(spec.Name, spec.Version); err != nil {
		return "", err
	}
	path := filepath.Join(t.PackDir, filepath.Clean("/"+string(pack.Family)), filepath.Clean("/"+pack.Integration))
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("pack %s integration directory is missing", pack.Family)
	}
	return path, nil
}

type hiddenOutcome struct {
	correct, verified, sanitizer bool
	speedup                      float64
	record                       []kernelgen.EvaluationResult
}

// runHiddenSuite re-runs the exact built bytes, on the original inputs and on
// the separate verification inputs the search loop rarely reached.
func (t Target) runHiddenSuite(ctx context.Context, request acceleratorlab.QualificationRequest, pack kernelgen.Pack, source string) (hiddenOutcome, error) {
	sum := sha256.Sum256([]byte(source))
	input := kernelgen.EvaluationRequest{
		SchemaVersion: kernelgen.EvaluationSchemaV1, InputDigest: request.InputDigest, CandidateID: request.Experiment.ID, Iteration: 1,
		Family: pack.Family, Backend: pack.Backend, Filename: pack.Filename, Source: source, SourceRevision: hex.EncodeToString(sum[:]),
		Hardware: request.Hardware, RuntimeImageDigest: request.Runtime.ImageDigest,
	}
	primary, err := t.Evaluator.Evaluate(ctx, input)
	if err != nil {
		return hiddenOutcome{}, fmt.Errorf("hidden suite: %w", err)
	}
	input.Verification = true
	verification, err := t.Evaluator.Evaluate(ctx, input)
	if err != nil {
		return hiddenOutcome{}, fmt.Errorf("hidden verification: %w", err)
	}
	stages := func(result kernelgen.EvaluationResult) bool {
		return result.Correctness.Smoke && result.Correctness.ShapeSweep && result.Correctness.NumericalStability && result.Correctness.Determinism && result.Correctness.EdgeCases
	}
	outcome := hiddenOutcome{correct: stages(primary), verified: stages(verification), sanitizer: primary.Correctness.Sanitizer && verification.Correctness.Sanitizer, record: []kernelgen.EvaluationResult{primary, verification}}
	if primary.BaselineMedianUS > 0 && primary.CandidateMedianUS > 0 {
		outcome.speedup = primary.BaselineMedianUS / primary.CandidateMedianUS
	}
	return outcome, nil
}
