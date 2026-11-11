// Command infercrane-kernel-adapter is the executable the accelerator worker
// invokes as profile, generate, or qualify. It reads one JSON request on stdin
// and writes one JSON evidence document on stdout. It expects one GPU, Docker,
// and AIPerf on this host. Configuration comes only
// from the owner-only environment the worker passes to it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/kernelgen"
	"github.com/infercrane/infercrane/internal/kernelliterature"
	"github.com/infercrane/infercrane/internal/kerneltarget"
)

const (
	maxRequestBytes         = 16 << 20
	defaultEvaluatorTimeout = 10 * time.Minute
	defaultMaxOutputTokens  = 8192
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "infercrane-kernel-adapter:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, env func(string) string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: infercrane-kernel-adapter profile|generate|qualify")
	}
	body, err := io.ReadAll(io.LimitReader(stdin, maxRequestBytes+1))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if len(body) > maxRequestBytes {
		return errors.New("request exceeds the size limit")
	}
	switch args[0] {
	case "generate":
		return generate(ctx, env, body, stdout)
	case "profile":
		var intent acceleratorlab.ProfileIntent
		if err = decodeStrict(body, &intent); err != nil {
			return fmt.Errorf("decode profile request: %w", err)
		}
		target, targetErr := newTarget(env)
		if targetErr != nil {
			return targetErr
		}
		evidence, profileErr := target.Capture(ctx, intent)
		if profileErr != nil {
			return profileErr
		}
		return json.NewEncoder(stdout).Encode(evidence)
	case "qualify":
		var request acceleratorlab.QualificationRequest
		if err = decodeStrict(body, &request); err != nil {
			return fmt.Errorf("decode qualification request: %w", err)
		}
		target, targetErr := newTarget(env)
		if targetErr != nil {
			return targetErr
		}
		evidence, qualifyErr := target.Qualify(ctx, request)
		if qualifyErr != nil {
			return qualifyErr
		}
		return json.NewEncoder(stdout).Encode(evidence)
	default:
		return fmt.Errorf("unknown operation %q", args[0])
	}
}

func decodeStrict(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func generate(ctx context.Context, env func(string) string, body []byte, stdout io.Writer) error {
	var request acceleratorlab.GenerationRequest
	if err := decodeStrict(body, &request); err != nil {
		return fmt.Errorf("decode generation request: %w", err)
	}
	generator, err := newGenerator(env)
	if err != nil {
		return err
	}
	evidence, err := generator.Generate(ctx, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(evidence)
}

// newTarget configures Docker-based profiling and qualification on this host.
func newTarget(env func(string) string) (kerneltarget.Target, error) {
	for _, name := range []string{"INFERCRANE_KERNEL_RUNTIMES_FILE", "INFERCRANE_KERNEL_MODEL_CACHE", "INFERCRANE_KERNEL_WORK_DIR", "INFERCRANE_KERNEL_PACK_DIR", "INFERCRANE_KERNEL_REPLAY_DIR"} {
		if !filepath.IsAbs(env(name)) {
			return kerneltarget.Target{}, fmt.Errorf("%s must be an absolute path", name)
		}
	}
	gpuHourly, err := positiveNumber(env, "INFERCRANE_KERNEL_GPU_HOURLY_USD")
	if err != nil {
		return kerneltarget.Target{}, err
	}
	runtimes, err := kerneltarget.LoadRuntimes(env("INFERCRANE_KERNEL_RUNTIMES_FILE"))
	if err != nil {
		return kerneltarget.Target{}, err
	}
	artifacts, err := workerClient(env)
	if err != nil {
		return kerneltarget.Target{}, err
	}
	hfToken, err := readToken(env("INFERCRANE_KERNEL_HF_TOKEN_FILE"))
	if err != nil {
		return kerneltarget.Target{}, err
	}
	environment := []string{"PATH=" + env("PATH"), "HOME=" + env("INFERCRANE_KERNEL_WORK_DIR")}
	if hfToken != "" {
		environment = append(environment, "HF_TOKEN="+hfToken)
	}
	evaluator, err := newEvaluator(env, gpuHourly)
	if err != nil {
		return kerneltarget.Target{}, err
	}
	return kerneltarget.Target{
		Runtimes: runtimes, ModelCache: env("INFERCRANE_KERNEL_MODEL_CACHE"), WorkDir: env("INFERCRANE_KERNEL_WORK_DIR"),
		PackDir: env("INFERCRANE_KERNEL_PACK_DIR"), ReplayDir: env("INFERCRANE_KERNEL_REPLAY_DIR"), ReplayType: env("INFERCRANE_KERNEL_REPLAY_DATASET_TYPE"),
		AIPerf: env("INFERCRANE_KERNEL_AIPERF"), HasHFToken: hfToken != "", GPUHourlyUSD: gpuHourly,
		Commands: kerneltarget.ExecCommander{Environment: environment}, Load: benchmark.Runner{}, Evaluator: evaluator, Artifacts: artifacts,
	}, nil
}

func workerClient(env func(string) string) (*acceleratorlab.WorkerClient, error) {
	client, err := acceleratorlab.NewWorkerClientFromTokenFile(acceleratorlab.WorkerConfig{
		BaseURL: env("INFERCRANE_ACCELERATOR_WORKER_URL"), Client: &http.Client{Timeout: 5 * time.Minute},
	}, env("INFERCRANE_ACCELERATOR_WORKER_TOKEN_FILE"))
	if err != nil {
		return nil, fmt.Errorf("configure artifact store: %w", err)
	}
	return client, nil
}

func evaluatorTimeout(env func(string) string) (time.Duration, error) {
	value := strings.TrimSpace(env("INFERCRANE_KERNEL_EVALUATOR_TIMEOUT"))
	if value == "" {
		return defaultEvaluatorTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, errors.New("INFERCRANE_KERNEL_EVALUATOR_TIMEOUT must be a positive duration")
	}
	return timeout, nil
}

func newEvaluator(env func(string) string, gpuHourly float64) (kernelgen.CommandEvaluator, error) {
	timeout, err := evaluatorTimeout(env)
	if err != nil {
		return kernelgen.CommandEvaluator{}, err
	}
	return kernelgen.CommandEvaluator{
		Executable: env("INFERCRANE_KERNEL_EVALUATOR"), Timeout: timeout, GPUHourlyUSD: gpuHourly,
		Environment: []string{"PATH=" + env("PATH"), "INFERCRANE_KERNEL_EVAL_IMAGE=" + env("INFERCRANE_KERNEL_EVAL_IMAGE"), "INFERCRANE_KERNEL_PACK_DIR=" + env("INFERCRANE_KERNEL_PACK_DIR")},
	}, nil
}

func newGenerator(env func(string) string) (kernelgen.Generator, error) {
	gpuHourly, err := positiveNumber(env, "INFERCRANE_KERNEL_GPU_HOURLY_USD")
	if err != nil {
		return kernelgen.Generator{}, err
	}
	inputPrice, err := nonnegativeNumber(env, "INFERCRANE_KERNEL_MODEL_INPUT_USD_PER_M")
	if err != nil {
		return kernelgen.Generator{}, err
	}
	outputPrice, err := nonnegativeNumber(env, "INFERCRANE_KERNEL_MODEL_OUTPUT_USD_PER_M")
	if err != nil {
		return kernelgen.Generator{}, err
	}
	evaluator, err := newEvaluator(env, gpuHourly)
	if err != nil {
		return kernelgen.Generator{}, err
	}
	token, err := readToken(env("INFERCRANE_KERNEL_MODEL_TOKEN_FILE"))
	if err != nil {
		return kernelgen.Generator{}, err
	}
	publisher, err := workerClient(env)
	if err != nil {
		return kernelgen.Generator{}, err
	}
	literature, err := literatureSource(env("INFERCRANE_KERNEL_LITERATURE"))
	if err != nil {
		return kernelgen.Generator{}, err
	}
	// Two evaluator runs cover a win that needs independent verification.
	attemptCost := 2*evaluator.Timeout.Hours()*gpuHourly + float64(defaultMaxOutputTokens)/1e6*outputPrice
	return kernelgen.Generator{
		PackDirectory: env("INFERCRANE_KERNEL_PACK_DIR"),
		Author: kernelgen.ChatAuthor{
			BaseURL: env("INFERCRANE_KERNEL_MODEL_URL"), Model: env("INFERCRANE_KERNEL_MODEL"), Token: token,
			InputUSDPerMillion: inputPrice, OutputUSDPerMillion: outputPrice, MaxOutputTokens: defaultMaxOutputTokens,
		},
		Evaluator: evaluator,
		Screener:  cpuScreener(env),
		Publisher: publisher, Literature: literature, MaxAttemptCostUSD: attemptCost,
	}, nil
}

// cpuScreener is optional. When set, drafts must pass the CPU correctness
// suite before the GPU evaluator runs.
func cpuScreener(env func(string) string) kernelgen.Evaluator {
	launcher := strings.TrimSpace(env("INFERCRANE_KERNEL_CPU_SCREEN"))
	if launcher == "" {
		return nil
	}
	return kernelgen.CommandEvaluator{
		Executable: launcher, Unpriced: true, Timeout: 20 * time.Minute,
		Environment: []string{"PATH=" + env("PATH"), "HOME=" + env("HOME"), "INFERCRANE_KERNEL_PACK_DIR=" + env("INFERCRANE_KERNEL_PACK_DIR"), "INFERCRANE_KERNEL_CPU_SCREEN_IMAGE=" + env("INFERCRANE_KERNEL_CPU_SCREEN_IMAGE")},
	}
}

// literatureSource defaults to off so that no external query leaves a customer
// boundary unless the operator chose it.
func literatureSource(value string) (kernelliterature.Searcher, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "" || value == "off":
		return nil, nil
	case value == "arxiv":
		return kernelliterature.ArxivSearcher{Delay: 3 * time.Second}, nil
	case filepath.IsAbs(value):
		return kernelliterature.FileSearcher{Path: value}, nil
	default:
		return nil, errors.New("INFERCRANE_KERNEL_LITERATURE must be off, arxiv, or an absolute file path")
	}
}

func readToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect model token file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("model token file must be an owner-only regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read model token file: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}

func positiveNumber(env func(string) string, name string) (float64, error) {
	value, err := nonnegativeNumber(env, name)
	if err == nil && value == 0 {
		return 0, fmt.Errorf("%s must be greater than zero", name)
	}
	return value, err
}

func nonnegativeNumber(env func(string) string, name string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(env(name)), 64)
	if err != nil || value < 0 || value != value || value > 1e9 {
		return 0, fmt.Errorf("%s must be a finite nonnegative number", name)
	}
	return value, nil
}
