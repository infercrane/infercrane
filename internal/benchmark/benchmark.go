// Package benchmark adapts AIPerf. InferCrane deliberately does not generate load itself.
package benchmark

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Binary, Endpoint, APIKey, APIKeyEnv, Model, Tokenizer string
	Requests, Concurrency, InputTokens, OutputTokens      int
	RandomSeed                                            int64
	Timeout                                               time.Duration
	// Streaming is a pointer so callers that predate buffered qualification
	// retain the safe historical default (streaming enabled).
	Streaming                          *bool
	TTFTSLOMS, TPOTSLOMS, LatencySLOMS float64
}

type Result struct {
	Tool, ToolVersion, Command  string
	Requests, Succeeded, Failed int
	DurationSeconds             float64
	RequestThroughput           *float64
	OutputTokenThroughput       *float64
	Goodput                     *float64
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
	if cfg.OutputTokens <= 0 {
		cfg.OutputTokens = 32
	}
	if cfg.InputTokens <= 0 {
		cfg.InputTokens = 128
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	dir, err := os.MkdirTemp("", "infercrane-aiperf-")
	if err != nil {
		return Result{}, fmt.Errorf("create AIPerf artifact directory: %w", err)
	}
	defer os.RemoveAll(dir)
	prefix := "infercrane"
	streaming := true
	if cfg.Streaming != nil {
		streaming = *cfg.Streaming
	}
	args := []string{"profile", "--model", cfg.Model, "--url", strings.TrimRight(cfg.Endpoint, "/"), "--endpoint-type", "chat"}
	// AIPerf 0.9 exposes --streaming as an opt-in boolean flag. It does not
	// expose a --no-streaming inverse; omitting --streaming is the buffered
	// request mode. Keep this construction explicit so CLI compatibility is
	// covered independently of the profile defaults.
	if streaming {
		args = append(args, "--streaming")
	}
	args = append(args, "--use-server-token-count", "--request-count", strconv.Itoa(cfg.Requests), "--concurrency", strconv.Itoa(cfg.Concurrency), "--random-seed", strconv.FormatInt(cfg.RandomSeed, 10), "--prompt-input-tokens-mean", strconv.Itoa(cfg.InputTokens), "--prompt-input-tokens-stddev", "0", "--prompt-output-tokens-mean", strconv.Itoa(cfg.OutputTokens), "--prompt-output-tokens-stddev", "0", "--ui", "none", "--export-level", "records", "--output-artifact-dir", dir, "--profile-export-prefix", prefix, "--no-auto-plot", "--no-gpu-telemetry", "--no-server-metrics")
	if cfg.Tokenizer != "" {
		args = append(args, "--tokenizer", cfg.Tokenizer)
	}
	if cfg.APIKey != "" {
		args = append(args, "--api-key", cfg.APIKey)
	}
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "INFERCRANE_API_KEY"
	}
	command := portableReproductionCommand(shellCommand(cfg.Binary, args, cfg.APIKey, cfg.APIKeyEnv), dir)
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
	result, err := parseRecordsWithSLO(filepath.Join(dir, prefix+".jsonl"), cfg)
	if err != nil {
		return Result{}, err
	}
	result.Tool, result.ToolVersion, result.Command = "aiperf", strings.TrimSpace(string(versionOutput)), command
	return result, nil
}

func portableReproductionCommand(command, temporaryDirectory string) string {
	return strings.Replace(command, quote(temporaryDirectory), "./infercrane-benchmark-artifacts", 1)
}

func shellCommand(binary string, args []string, secret, secretEnv string) string {
	parts := []string{binary}
	for i, arg := range args {
		if i > 0 && args[i-1] == "--api-key" {
			arg = "${" + secretEnv + "}"
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
		Value json.RawMessage `json:"value"`
		Unit  string          `json:"unit"`
	} `json:"metrics"`
	Error any `json:"error"`
}

func parseRecords(path string) (Result, error) {
	return parseRecordsWithSLO(path, Config{})
}

func parseRecordsWithSLO(path string, cfg Config) (Result, error) {
	file, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("read AIPerf records: %w", err)
	}
	defer file.Close()
	var ttft, tpot, latency, outputTokens []float64
	var result Result
	var earliest, latest int64
	goodRequests := 0
	hasSLO := cfg.TTFTSLOMS > 0 || cfg.TPOTSLOMS > 0 || cfg.LatencySLOMS > 0
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
		requestGood := hasSLO
		observedSLOMetrics := map[string]bool{}
		appendMetric := func(name string, destination *[]float64) error {
			if metric, ok := row.Metrics[name]; ok {
				var value float64
				if len(metric.Value) == 0 || json.Unmarshal(metric.Value, &value) != nil {
					return fmt.Errorf("AIPerf metric %q is not numeric", name)
				}
				*destination = append(*destination, value)
			}
			return nil
		}
		appendLatencyMetric := func(name string, destination *[]float64) error {
			metric, ok := row.Metrics[name]
			if !ok {
				return nil
			}
			var value float64
			if len(metric.Value) == 0 || json.Unmarshal(metric.Value, &value) != nil {
				return fmt.Errorf("AIPerf metric %q is not numeric", name)
			}
			value, err = milliseconds(value, metric.Unit)
			if err != nil {
				return fmt.Errorf("AIPerf metric %q: %w", name, err)
			}
			*destination = append(*destination, value)
			observedSLOMetrics[name] = true
			switch name {
			case "time_to_first_token":
				if cfg.TTFTSLOMS > 0 && value > cfg.TTFTSLOMS {
					requestGood = false
				}
			case "request_latency":
				if cfg.LatencySLOMS > 0 && value > cfg.LatencySLOMS {
					requestGood = false
				}
			case "time_per_output_token", "inter_token_latency":
				if cfg.TPOTSLOMS > 0 && value > cfg.TPOTSLOMS {
					requestGood = false
				}
			}
			return nil
		}
		if err := appendLatencyMetric("time_to_first_token", &ttft); err != nil {
			return Result{}, err
		}
		if err := appendLatencyMetric("request_latency", &latency); err != nil {
			return Result{}, err
		}
		if _, ok := row.Metrics["time_per_output_token"]; ok {
			if err := appendLatencyMetric("time_per_output_token", &tpot); err != nil {
				return Result{}, err
			}
		} else {
			if err := appendLatencyMetric("inter_token_latency", &tpot); err != nil {
				return Result{}, err
			}
		}
		if err := appendMetric("output_token_count", &outputTokens); err != nil {
			return Result{}, err
		}
		if row.Metadata.RequestStartNS > 0 && (earliest == 0 || row.Metadata.RequestStartNS < earliest) {
			earliest = row.Metadata.RequestStartNS
		}
		if row.Metadata.RequestEndNS > latest {
			latest = row.Metadata.RequestEndNS
		}
		if cfg.TTFTSLOMS > 0 && !observedSLOMetrics["time_to_first_token"] {
			requestGood = false
		}
		if cfg.LatencySLOMS > 0 && !observedSLOMetrics["request_latency"] {
			requestGood = false
		}
		if cfg.TPOTSLOMS > 0 && !observedSLOMetrics["time_per_output_token"] && !observedSLOMetrics["inter_token_latency"] {
			requestGood = false
		}
		if requestGood {
			goodRequests++
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
		if hasSLO {
			goodput := float64(goodRequests) / result.DurationSeconds
			result.Goodput = &goodput
		}
	}
	return result, nil
}

func milliseconds(value float64, unit string) (float64, error) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "ms", "millisecond", "milliseconds":
		return value, nil
	case "s", "sec", "second", "seconds":
		return value * 1_000, nil
	case "us", "µs", "μs", "microsecond", "microseconds":
		return value / 1_000, nil
	case "ns", "nanosecond", "nanoseconds":
		return value / 1_000_000, nil
	default:
		return 0, fmt.Errorf("unsupported latency unit %q", unit)
	}
}

func percentiles(values []float64) (*float64, *float64) {
	if len(values) == 0 {
		return nil, nil
	}
	sort.Float64s(values)
	p50, p95 := percentile(values, .50), percentile(values, .95)
	return &p50, &p95
}
func percentile(values []float64, p float64) float64 {
	// Use the nearest-rank definition. A floor-index calculation understates
	// tail latency for small samples; for example, p95 of [2, 4] must be 4.
	index := int(math.Ceil(p*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}
