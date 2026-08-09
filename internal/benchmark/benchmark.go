// Package benchmark adapts AIPerf. InferCrane deliberately does not generate load itself.
package benchmark

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Binary, Endpoint, APIKey, Model string
	Requests, Concurrency           int
	RandomSeed                      int64
	Timeout                         time.Duration
}

type Result struct {
	Tool, ToolVersion, Command  string
	Requests, Succeeded, Failed int
	DurationSeconds             float64
	RequestThroughput           *float64
	OutputTokenThroughput       *float64
	TTFTP50MS, TTFTP95MS        *float64
	TPOTP50MS, TPOTP95MS        *float64
	LatencyP50MS, LatencyP95MS  *float64
}

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type executable struct{}

func (executable) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func Run(ctx context.Context, cfg Config) (Result, error) { return run(ctx, cfg, executable{}) }

type Runner struct{}

func (Runner) Run(ctx context.Context, cfg Config) (Result, error) { return Run(ctx, cfg) }

func run(ctx context.Context, cfg Config, commands commandRunner) (Result, error) {
	if cfg.Binary == "" {
		cfg.Binary = "aiperf"
	}
	if cfg.Endpoint == "" || cfg.Model == "" || cfg.Requests < 1 || cfg.Concurrency < 1 {
		return Result{}, errors.New("endpoint, model, positive requests, and positive concurrency are required")
	}
	if cfg.RandomSeed == 0 {
		cfg.RandomSeed = 17
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	dir, err := os.MkdirTemp("", "infercrane-aiperf-")
	if err != nil {
		return Result{}, fmt.Errorf("create AIPerf artifact directory: %w", err)
	}
	defer os.RemoveAll(dir)
	prefix := filepath.Join(dir, "infercrane")
	args := []string{"profile", "--model", cfg.Model, "--url", strings.TrimRight(cfg.Endpoint, "/"), "--endpoint-type", "chat", "--streaming", "--use-server-token-count", "--request-count", strconv.Itoa(cfg.Requests), "--concurrency", strconv.Itoa(cfg.Concurrency), "--random-seed", strconv.FormatInt(cfg.RandomSeed, 10), "--ui", "none", "--export-level", "records", "--profile-export-prefix", prefix, "--no-auto-plot", "--no-gpu-telemetry", "--no-server-metrics"}
	if cfg.APIKey != "" {
		args = append(args, "--api-key", cfg.APIKey)
	}
	command := shellCommand(cfg.Binary, args, cfg.APIKey)
	versionOutput, versionErr := commands.Run(ctx, cfg.Binary, "--version")
	if versionErr != nil {
		return Result{}, fmt.Errorf("AIPerf is unavailable (install with pipx install aiperf): %w", versionErr)
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	output, err := commands.Run(runCtx, cfg.Binary, args...)
	if err != nil {
		return Result{}, fmt.Errorf("AIPerf failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	result, err := parseRecords(prefix + ".jsonl")
	if err != nil {
		return Result{}, err
	}
	result.Tool, result.ToolVersion, result.Command = "aiperf", strings.TrimSpace(string(versionOutput)), command
	return result, nil
}

func shellCommand(binary string, args []string, secret string) string {
	parts := []string{binary}
	for i, arg := range args {
		if i > 0 && args[i-1] == "--api-key" {
			arg = "${INFERCRANE_API_KEY}"
		}
		parts = append(parts, quote(arg))
	}
	return strings.Join(parts, " ")
}
func quote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`!&|;()<>*") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type record struct {
	Metadata struct {
		Phase          string `json:"benchmark_phase"`
		RequestStartNS int64  `json:"request_start_ns"`
		RequestEndNS   int64  `json:"request_end_ns"`
	} `json:"metadata"`
	Metrics map[string]struct {
		Value *float64 `json:"value"`
		Unit  string   `json:"unit"`
	} `json:"metrics"`
	Error any `json:"error"`
}

func parseRecords(path string) (Result, error) {
	file, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("read AIPerf records: %w", err)
	}
	defer file.Close()
	var ttft, tpot, latency, outputTokens []float64
	var result Result
	var earliest, latest int64
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var row record
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return Result{}, fmt.Errorf("parse AIPerf record: %w", err)
		}
		if row.Metadata.Phase != "" && row.Metadata.Phase != "profiling" {
			continue
		}
		result.Requests++
		if row.Error != nil {
			result.Failed++
			continue
		}
		result.Succeeded++
		appendMetric := func(name string, destination *[]float64) {
			if metric, ok := row.Metrics[name]; ok && metric.Value != nil {
				*destination = append(*destination, *metric.Value)
			}
		}
		appendMetric("time_to_first_token", &ttft)
		appendMetric("request_latency", &latency)
		if _, ok := row.Metrics["time_per_output_token"]; ok {
			appendMetric("time_per_output_token", &tpot)
		} else {
			appendMetric("inter_token_latency", &tpot)
		}
		appendMetric("output_token_count", &outputTokens)
		if row.Metadata.RequestStartNS > 0 && (earliest == 0 || row.Metadata.RequestStartNS < earliest) {
			earliest = row.Metadata.RequestStartNS
		}
		if row.Metadata.RequestEndNS > latest {
			latest = row.Metadata.RequestEndNS
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, err
	}
	if result.Requests == 0 {
		return Result{}, errors.New("AIPerf produced no profiling records")
	}
	result.TTFTP50MS, result.TTFTP95MS = percentiles(ttft)
	result.TPOTP50MS, result.TPOTP95MS = percentiles(tpot)
	result.LatencyP50MS, result.LatencyP95MS = percentiles(latency)
	if latest > earliest {
		result.DurationSeconds = float64(latest-earliest) / float64(time.Second)
	}
	if result.DurationSeconds > 0 {
		throughput := float64(result.Succeeded) / result.DurationSeconds
		result.RequestThroughput = &throughput
		var tokens float64
		for _, value := range outputTokens {
			tokens += value
		}
		tokenThroughput := tokens / result.DurationSeconds
		result.OutputTokenThroughput = &tokenThroughput
	}
	return result, nil
}

func percentiles(values []float64) (*float64, *float64) {
	if len(values) == 0 {
		return nil, nil
	}
	sort.Float64s(values)
	p50, p95 := percentile(values, .50), percentile(values, .95)
	return &p50, &p95
}
func percentile(values []float64, p float64) float64 { return values[int(float64(len(values)-1)*p)] }
