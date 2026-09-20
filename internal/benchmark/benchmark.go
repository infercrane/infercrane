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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const AIPerfVersion = "0.12.0"

type Config struct {
	Binary, Endpoint, APIKey, APIKeyEnv, Model, Tokenizer string
	Requests, Concurrency, InputTokens, OutputTokens      int
	ProfileRuns, WarmupRequests                           int
	ProfileRunCooldown                                    time.Duration
	ArrivalPattern                                        string
	RequestRate                                           float64
	InputFile, DatasetType                                string
	RandomSeed                                            int64
	Timeout                                               time.Duration
	// Streaming is a pointer so callers that predate buffered qualification
	// retain the safe historical default (streaming enabled).
	Streaming                          *bool
	TTFTSLOMS, TPOTSLOMS, LatencySLOMS float64
}

type ConfidenceInterval struct {
	Samples              int     `json:"samples"`
	Mean                 float64 `json:"mean"`
	StandardDeviation    float64 `json:"standard_deviation"`
	CoefficientVariation float64 `json:"coefficient_of_variation"`
	Lower95              float64 `json:"lower_95"`
	Upper95              float64 `json:"upper_95"`
}

type Result struct {
	Tool, ToolVersion, Command  string
	Requests, Succeeded, Failed int
	ProfileRuns                 int
	DurationSeconds             float64
	RequestThroughput           *float64
	OutputTokenThroughput       *float64
	Goodput                     *float64
	TTFTP50MS, TTFTP95MS        *float64
	TPOTP50MS, TPOTP95MS        *float64
	LatencyP50MS, LatencyP95MS  *float64
	Confidence                  map[string]ConfidenceInterval
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
	if cfg.ProfileRuns == 0 {
		cfg.ProfileRuns = 1
	}
	if cfg.ProfileRuns < 1 || cfg.ProfileRuns > 10 {
		return Result{}, errors.New("profile runs must be between 1 and 10")
	}
	if cfg.WarmupRequests < 0 || cfg.WarmupRequests > 100000 {
		return Result{}, errors.New("warmup requests must be between 0 and 100000")
	}
	if cfg.ProfileRunCooldown < 0 || cfg.ProfileRunCooldown > time.Hour {
		return Result{}, errors.New("profile run cooldown must be between 0 and 1 hour")
	}
	cfg.ArrivalPattern = strings.ToLower(strings.TrimSpace(cfg.ArrivalPattern))
	if cfg.ArrivalPattern != "" && cfg.ArrivalPattern != "constant" && cfg.ArrivalPattern != "poisson" && cfg.ArrivalPattern != "gamma" {
		return Result{}, errors.New("arrival pattern must be constant, poisson, or gamma")
	}
	if math.IsNaN(cfg.RequestRate) || math.IsInf(cfg.RequestRate, 0) || cfg.RequestRate < 0 || (cfg.ArrivalPattern == "") != (cfg.RequestRate == 0) {
		return Result{}, errors.New("arrival pattern and a finite positive request rate must be supplied together")
	}
	cfg.DatasetType = strings.ToLower(strings.TrimSpace(cfg.DatasetType))
	if cfg.InputFile != "" {
		if _, err := os.Stat(cfg.InputFile); err != nil {
			return Result{}, fmt.Errorf("read AIPerf trace input: %w", err)
		}
		if !supportedDatasetType(cfg.DatasetType) {
			return Result{}, errors.New("trace input requires a supported dataset type")
		}
	} else if cfg.DatasetType != "" {
		return Result{}, errors.New("dataset type requires a trace input file")
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
		cfg.Timeout = time.Duration(cfg.ProfileRuns) * 30 * time.Minute
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
	// AIPerf exposes --streaming as an opt-in boolean flag. It does not
	// expose a --no-streaming inverse; omitting --streaming is the buffered
	// request mode. Keep this construction explicit so CLI compatibility is
	// covered independently of the profile defaults.
	if streaming {
		args = append(args, "--streaming")
	}
	args = append(args, "--use-server-token-count", "--request-count", strconv.Itoa(cfg.Requests), "--concurrency", strconv.Itoa(cfg.Concurrency), "--random-seed", strconv.FormatInt(cfg.RandomSeed, 10))
	if cfg.InputFile != "" {
		args = append(args, "--input-file", cfg.InputFile, "--custom-dataset-type", cfg.DatasetType)
	} else {
		args = append(args, "--synthetic-input-tokens-mean", strconv.Itoa(cfg.InputTokens), "--synthetic-input-tokens-stddev", "0", "--output-tokens-mean", strconv.Itoa(cfg.OutputTokens), "--output-tokens-stddev", "0")
	}
	if cfg.ProfileRuns > 1 {
		args = append(args, "--num-profile-runs", strconv.Itoa(cfg.ProfileRuns))
	}
	if cfg.WarmupRequests > 0 {
		args = append(args, "--warmup-request-count", strconv.Itoa(cfg.WarmupRequests))
	}
	if cfg.ProfileRunCooldown > 0 {
		args = append(args, "--profile-run-cooldown-seconds", strconv.Itoa(int(cfg.ProfileRunCooldown.Seconds())))
	}
	if cfg.ArrivalPattern != "" {
		args = append(args, "--arrival-pattern", cfg.ArrivalPattern, "--request-rate", strconv.FormatFloat(cfg.RequestRate, 'f', -1, 64))
	}
	args = append(args, "--ui", "none", "--export-level", "records", "--artifact-dir", dir, "--profile-export-prefix", prefix, "--no-auto-plot", "--no-gpu-telemetry", "--no-server-metrics")
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
	version, versionErr := aiperfVersion(string(versionOutput))
	if versionErr != nil || version != AIPerfVersion {
		return Result{}, fmt.Errorf("AIPerf %s is required; found %q", AIPerfVersion, strings.TrimSpace(string(versionOutput)))
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	output, err := commands.Run(runCtx, cfg.Binary, args...)
	if err != nil {
		return Result{}, fmt.Errorf("AIPerf failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	records, err := findRecordExports(dir, prefix)
	if err != nil {
		return Result{}, err
	}
	if len(records) != cfg.ProfileRuns {
		return Result{}, fmt.Errorf("AIPerf produced %d profile record exports; expected %d", len(records), cfg.ProfileRuns)
	}
	result, err := parseRecordFilesWithSLO(records, cfg)
	if err != nil {
		return Result{}, err
	}
	result.ProfileRuns = len(records)
	result.Confidence = profileConfidence(records, cfg)
	result.Tool, result.ToolVersion, result.Command = "aiperf", version, command
	return result, nil
}

func supportedDatasetType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "single_turn", "multi_turn", "mooncake_trace", "baseten_trace", "burst_gpt_trace", "sagemaker_data_capture", "inputs_json":
		return true
	default:
		return false
	}
}

var semanticVersion = regexp.MustCompile(`(?:^|[^0-9])([0-9]+\.[0-9]+\.[0-9]+)(?:$|[^0-9])`)

func aiperfVersion(output string) (string, error) {
	match := semanticVersion.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) != 2 {
		return "", errors.New("AIPerf version output did not contain a semantic version")
	}
	return match[1], nil
}

func findRecordExports(directory, prefix string) ([]string, error) {
	var preferred, fallback []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == prefix+".jsonl" {
			preferred = append(preferred, path)
		} else if name == "profile_export.jsonl" {
			fallback = append(fallback, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover AIPerf records: %w", err)
	}
	result := preferred
	if len(result) == 0 {
		result = fallback
	}
	sort.Strings(result)
	if len(result) == 0 {
		return nil, errors.New("AIPerf produced no records export")
	}
	// Multi-run AIPerf may retain an aggregate export at the artifact root as
	// well as one export per run. The per-run files are the confidence samples.
	if len(result) > 1 {
		var nested []string
		for _, path := range result {
			if filepath.Clean(filepath.Dir(path)) != filepath.Clean(directory) {
				nested = append(nested, path)
			}
		}
		if len(nested) > 0 {
			return nested, nil
		}
	}
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
	return parseRecordFilesWithSLO([]string{path}, cfg)
}

func parseRecordFilesWithSLO(paths []string, cfg Config) (Result, error) {
	var ttft, tpot, latency, outputTokens []float64
	var result Result
	goodRequests := 0
	hasSLO := cfg.TTFTSLOMS > 0 || cfg.TPOTSLOMS > 0 || cfg.LatencySLOMS > 0
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return Result{}, fmt.Errorf("read AIPerf records: %w", err)
		}
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
			if row.Metadata.RequestStartNS > 0 && (earliest == 0 || row.Metadata.RequestStartNS < earliest) {
				earliest = row.Metadata.RequestStartNS
			}
			if row.Metadata.RequestEndNS > latest {
				latest = row.Metadata.RequestEndNS
			}
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
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return Result{}, scanErr
		}
		if closeErr != nil {
			return Result{}, closeErr
		}
		if latest > earliest {
			result.DurationSeconds += float64(latest-earliest) / float64(time.Second)
		}
	}
	if result.Requests == 0 {
		return Result{}, errors.New("AIPerf produced no profiling records")
	}
	result.TTFTP50MS, result.TTFTP95MS = percentiles(ttft)
	result.TPOTP50MS, result.TPOTP95MS = percentiles(tpot)
	result.LatencyP50MS, result.LatencyP95MS = percentiles(latency)
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

func profileConfidence(paths []string, cfg Config) map[string]ConfidenceInterval {
	values := map[string][]float64{}
	for _, path := range paths {
		result, err := parseRecordsWithSLO(path, cfg)
		if err != nil {
			return nil
		}
		appendValue := func(name string, value *float64) {
			if value != nil {
				values[name] = append(values[name], *value)
			}
		}
		appendValue("request_throughput", result.RequestThroughput)
		appendValue("output_token_throughput", result.OutputTokenThroughput)
		appendValue("ttft_p95_ms", result.TTFTP95MS)
		appendValue("tpot_p95_ms", result.TPOTP95MS)
		appendValue("latency_p95_ms", result.LatencyP95MS)
		appendValue("goodput", result.Goodput)
	}
	confidence := map[string]ConfidenceInterval{}
	for name, samples := range values {
		if len(samples) >= 2 {
			confidence[name] = confidence95(samples)
		}
	}
	if len(confidence) == 0 {
		return nil
	}
	return confidence
}

func confidence95(samples []float64) ConfidenceInterval {
	var mean float64
	for _, value := range samples {
		mean += value
	}
	mean /= float64(len(samples))
	var squared float64
	for _, value := range samples {
		difference := value - mean
		squared += difference * difference
	}
	standardDeviation := math.Sqrt(squared / float64(len(samples)-1))
	tCritical := []float64{0, 0, 12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262}[len(samples)]
	margin := tCritical * standardDeviation / math.Sqrt(float64(len(samples)))
	coefficientVariation := 0.0
	if mean != 0 {
		coefficientVariation = standardDeviation / math.Abs(mean)
	}
	return ConfidenceInterval{Samples: len(samples), Mean: mean, StandardDeviation: standardDeviation, CoefficientVariation: coefficientVariation, Lower95: mean - margin, Upper95: mean + margin}
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
