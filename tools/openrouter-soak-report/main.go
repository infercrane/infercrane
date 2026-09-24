package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type probeReceipt struct {
	SchemaVersion    string  `json:"schema_version"`
	StartedAtUnixMS  int64   `json:"started_at_unix_ms"`
	Endpoint         string  `json:"endpoint"`
	Model            string  `json:"model"`
	Status           int     `json:"status"`
	DurationMS       float64 `json:"duration_ms"`
	TTFTMS           float64 `json:"ttft_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	SawDone          bool    `json:"saw_done"`
	SawFinishReason  bool    `json:"saw_finish_reason"`
	Passed           bool    `json:"passed"`
}

type report struct {
	SchemaVersion       string    `json:"schema_version"`
	CreatedAt           time.Time `json:"created_at"`
	RequestedSince      time.Time `json:"requested_since"`
	WindowStartedAt     time.Time `json:"window_started_at"`
	WindowEndedAt       time.Time `json:"window_ended_at"`
	ObservedDurationSec float64   `json:"observed_duration_seconds"`
	Samples             int       `json:"samples"`
	Passed              int       `json:"passed"`
	Failed              int       `json:"failed"`
	FailureRate         float64   `json:"failure_rate"`
	TTFTP50MS           float64   `json:"ttft_p50_ms"`
	TTFTP95MS           float64   `json:"ttft_p95_ms"`
	TTFTP99MS           float64   `json:"ttft_p99_ms"`
	MaxGapSec           float64   `json:"max_probe_gap_seconds"`
	RequiredDurationSec float64   `json:"required_duration_seconds"`
	RequiredSamples     int       `json:"required_samples"`
	MaxFailureRate      float64   `json:"max_failure_rate"`
	MaxTTFTP95MS        float64   `json:"max_ttft_p95_ms"`
	MaxAllowedGapSec    float64   `json:"max_allowed_probe_gap_seconds"`
	Eligible            bool      `json:"eligible"`
	Reasons             []string  `json:"reasons,omitempty"`
}

func main() {
	directory := flag.String("dir", "", "directory containing content-free soak receipts")
	since := flag.String("since", "", "RFC3339 start of the post-release soak window")
	minimumDuration := flag.Duration("minimum-duration", 24*time.Hour, "minimum uninterrupted observation duration")
	minimumSamples := flag.Int("minimum-samples", 1380, "minimum probes in the observation window")
	maxFailureRate := flag.Float64("max-failure-rate", 0.001, "maximum failed-probe ratio")
	maxTTFTP95 := flag.Duration("max-ttft-p95", 1500*time.Millisecond, "maximum p95 streaming TTFT")
	maxGap := flag.Duration("max-gap", 3*time.Minute, "maximum allowed gap between probes")
	output := flag.String("output", "", "optional owner-only JSON report path")
	flag.Parse()

	if *directory == "" || *since == "" || *minimumDuration <= 0 || *minimumSamples < 1 || *maxFailureRate < 0 || *maxFailureRate > 1 || *maxTTFTP95 <= 0 || *maxGap <= 0 {
		fatal(errors.New("valid --dir, --since, and positive release thresholds are required"))
	}
	windowStart, err := time.Parse(time.RFC3339, *since)
	if err != nil {
		fatal(fmt.Errorf("parse --since: %w", err))
	}
	receipts, err := loadReceipts(*directory, windowStart.UTC())
	if err != nil {
		fatal(err)
	}
	result := summarize(receipts, windowStart.UTC(), time.Now().UTC(), *minimumDuration, *minimumSamples, *maxFailureRate, *maxTTFTP95, *maxGap)
	body, _ := json.MarshalIndent(result, "", "  ")
	body = append(body, '\n')
	if *output != "" {
		if err = os.MkdirAll(filepath.Dir(*output), 0o700); err != nil {
			fatal(err)
		}
		if err = os.WriteFile(*output, body, 0o600); err != nil {
			fatal(err)
		}
	}
	_, _ = os.Stdout.Write(body)
	if !result.Eligible {
		os.Exit(1)
	}
}

func loadReceipts(directory string, since time.Time) ([]probeReceipt, error) {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("--dir must be a readable directory")
	}
	var receipts []probeReceipt
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var receipt probeReceipt
		if decodeErr := json.Unmarshal(body, &receipt); decodeErr != nil {
			return fmt.Errorf("decode %s: %w", path, decodeErr)
		}
		observedAt := time.UnixMilli(receipt.StartedAtUnixMS).UTC()
		if receipt.SchemaVersion != "infercrane.dev/openrouter-provider-soak/v1" || receipt.StartedAtUnixMS <= 0 {
			return fmt.Errorf("invalid soak receipt %s", path)
		}
		if !observedAt.Before(since) {
			receipts = append(receipts, receipt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].StartedAtUnixMS < receipts[j].StartedAtUnixMS })
	return receipts, nil
}

func summarize(receipts []probeReceipt, requestedSince, evaluatedAt time.Time, minimumDuration time.Duration, minimumSamples int, maxFailureRate float64, maxTTFTP95, maxGap time.Duration) report {
	result := report{
		SchemaVersion: "infercrane.dev/openrouter-provider-soak-report/v1", CreatedAt: evaluatedAt, RequestedSince: requestedSince,
		RequiredDurationSec: minimumDuration.Seconds(), RequiredSamples: minimumSamples, MaxFailureRate: maxFailureRate,
		MaxTTFTP95MS: float64(maxTTFTP95.Microseconds()) / 1000, MaxAllowedGapSec: maxGap.Seconds(),
	}
	if len(receipts) == 0 {
		result.Reasons = []string{"no probes in the requested release window"}
		return result
	}
	result.Samples = len(receipts)
	result.WindowStartedAt = time.UnixMilli(receipts[0].StartedAtUnixMS).UTC()
	result.WindowEndedAt = time.UnixMilli(receipts[len(receipts)-1].StartedAtUnixMS).UTC()
	result.ObservedDurationSec = result.WindowEndedAt.Sub(result.WindowStartedAt).Seconds()
	result.MaxGapSec = math.Max(0, result.WindowStartedAt.Sub(requestedSince).Seconds())
	result.MaxGapSec = math.Max(result.MaxGapSec, evaluatedAt.Sub(result.WindowEndedAt).Seconds())
	var ttft []float64
	for index, receipt := range receipts {
		if receipt.Passed && receipt.Status == 200 && receipt.SawDone && receipt.SawFinishReason && receipt.PromptTokens > 0 && receipt.CompletionTokens > 0 {
			result.Passed++
			ttft = append(ttft, receipt.TTFTMS)
		} else {
			result.Failed++
		}
		if index > 0 {
			gap := float64(receipt.StartedAtUnixMS-receipts[index-1].StartedAtUnixMS) / 1000
			result.MaxGapSec = math.Max(result.MaxGapSec, gap)
		}
	}
	result.FailureRate = float64(result.Failed) / float64(result.Samples)
	sort.Float64s(ttft)
	result.TTFTP50MS = percentile(ttft, 0.50)
	result.TTFTP95MS = percentile(ttft, 0.95)
	result.TTFTP99MS = percentile(ttft, 0.99)
	if result.ObservedDurationSec < minimumDuration.Seconds() {
		result.Reasons = append(result.Reasons, "observation window is shorter than the required soak")
	}
	if result.Samples < minimumSamples {
		result.Reasons = append(result.Reasons, "probe count is below the release threshold")
	}
	if result.FailureRate > maxFailureRate {
		result.Reasons = append(result.Reasons, "failure rate exceeds the release threshold")
	}
	if len(ttft) == 0 || result.TTFTP95MS > float64(maxTTFTP95.Microseconds())/1000 {
		result.Reasons = append(result.Reasons, "p95 TTFT exceeds the release threshold")
	}
	if result.MaxGapSec > maxGap.Seconds() {
		result.Reasons = append(result.Reasons, "probe continuity gap exceeds the release threshold")
	}
	result.Eligible = len(result.Reasons) == 0
	return result
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "openrouter soak report:", err)
	os.Exit(1)
}
