// Package workloadprofile defines content-free workload evidence that can be
// converted into reproducible InferCrane benchmark inputs.
package workloadprofile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/performanceprofile"
)

const (
	SchemaVersion     = "infercrane.public-workload-profile/v1"
	EvidenceClass     = "public-trace-derived-screening-input"
	PublicPriorDigest = "sha256:fc88149b14f24e73c49b841df7e32db2be2a2e3e10c7cfbd834b1617310d5c7e"
)

type Source struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	License    string `json:"license"`
	Citation   string `json:"citation,omitempty"`
	TraceRows  int64  `json:"trace_rows"`
	TraceBytes int64  `json:"trace_bytes,omitempty"`
}

type Sampling struct {
	Method         string   `json:"method"`
	Columns        []string `json:"columns"`
	RowGroups      []int    `json:"row_groups"`
	TotalRowGroups int      `json:"total_row_groups"`
	RowsInspected  int      `json:"rows_inspected"`
	RemoteOnly     bool     `json:"remote_only"`
}

type Percentiles struct {
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type Distribution struct {
	Unit        string      `json:"unit"`
	Samples     int         `json:"samples"`
	Percentiles Percentiles `json:"percentiles"`
}

type Distributions struct {
	InputTokens      Distribution `json:"input_tokens"`
	OutputTokens     Distribution `json:"output_tokens"`
	CachedTokens     Distribution `json:"cached_tokens"`
	TTFTMilliseconds Distribution `json:"ttft_ms"`
	DurationMS       Distribution `json:"duration_ms"`
	CacheHitFraction Distribution `json:"cache_hit_fraction"`
}

// BenchmarkShape is a bounded, reproducible screening lane. Concurrency and
// request count are controlled experiment inputs, not estimates of fleet-wide
// production capacity.
type BenchmarkShape struct {
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	Objective      string  `json:"objective"`
	Requests       int     `json:"requests"`
	Concurrency    int     `json:"concurrency"`
	InputTokens    int     `json:"input_tokens"`
	OutputTokens   int     `json:"output_tokens"`
	Streaming      bool    `json:"streaming"`
	ArrivalPattern string  `json:"arrival_pattern,omitempty"`
	RequestRate    float64 `json:"request_rate,omitempty"`
	Clipped        bool    `json:"clipped_to_context_window"`
}

type Document struct {
	SchemaVersion       string           `json:"schema_version"`
	EvidenceClass       string           `json:"evidence_class"`
	GeneratedAt         string           `json:"generated_at"`
	Source              Source           `json:"source"`
	Sampling            Sampling         `json:"sampling"`
	Distributions       Distributions    `json:"distributions"`
	Profiles            []BenchmarkShape `json:"profiles"`
	MethodologyBoundary string           `json:"methodology_boundary"`
	Digest              string           `json:"digest,omitempty"`
}

// PublicChutesPrior returns the immutable, product-visible day-zero workload
// prior. It is screening input only and must be replaced by an observed replay
// before a candidate can become production-qualified.
func PublicChutesPrior() (Document, error) {
	distribution := func(unit string, samples int, p50, p90, p95, p99 float64) Distribution {
		return Distribution{Unit: unit, Samples: samples, Percentiles: Percentiles{P50: p50, P90: p90, P95: p95, P99: p99}}
	}
	profiles := make([]BenchmarkShape, 0, len(performanceprofile.PublicPriorNames()))
	for _, name := range performanceprofile.PublicPriorNames() {
		profile, err := performanceprofile.Get(name)
		if err != nil {
			return Document{}, err
		}
		shape := BenchmarkShape{
			Name: profile.Name, Description: profile.Description, Objective: profile.Objective,
			Requests: profile.Requests, Concurrency: profile.Concurrency,
			InputTokens: profile.InputTokens, OutputTokens: profile.OutputTokens,
			Streaming: profile.Streaming,
		}
		shape.Clipped = name == "public-long-prefill"
		profiles = append(profiles, shape)
	}
	document := Document{
		SchemaVersion: SchemaVersion,
		EvidenceClass: EvidenceClass,
		GeneratedAt:   "2026-09-20T07:18:56Z",
		Source: Source{
			Name:       "A Year in LLM Serving (Chutes)",
			URL:        "https://harvardsys-datasets.s3.us-east-1.amazonaws.com/2026_chutes_anonymized/chutes_trace.parquet",
			License:    "CC-BY-4.0",
			Citation:   "Nixon et al., A Year in LLM Serving: Workload Evolution, Caching and Load-Balancing (2026)",
			TraceRows:  6_122_413_756,
			TraceBytes: 91_044_147_663,
		},
		Sampling: Sampling{
			Method:    "deterministic 10%-to-90% row-group coverage with bounded leading batches",
			Columns:   []string{"started_at", "completed_at", "it", "ot", "ct", "ttft"},
			RowGroups: []int{611, 3056, 5502}, TotalRowGroups: 6114, RowsInspected: 150_000, RemoteOnly: true,
		},
		Distributions: Distributions{
			InputTokens:      distribution("tokens", 116_721, 3919, 15339, 24832, 64428),
			OutputTokens:     distribution("tokens", 116_721, 295, 915, 1377, 3403),
			CachedTokens:     distribution("tokens", 20_790, 64, 4060, 7998, 36736),
			TTFTMilliseconds: distribution("milliseconds", 77_565, 989.0000224113464, 5192.999839782715, 8505.000114440918, 21295.000076293945),
			DurationMS:       distribution("milliseconds", 147_802, 6505.866, 31081.427, 49102.949, 108171.524),
			CacheHitFraction: distribution("ratio", 20_790, 0.018447348193697154, 0.9944567627494457, 0.9997684496047105, 1),
		},
		Profiles:            profiles,
		MethodologyBoundary: "The public fleet sample selects representative token shapes only. It does not represent a customer's model mix, absolute arrival rate, semantic quality, or production SLO. Controlled concurrency lanes must be qualified on the exact model, runtime, GPU, and customer replay before promotion.",
	}
	digest, err := Digest(document)
	if err != nil {
		return Document{}, err
	}
	if digest != PublicPriorDigest {
		return Document{}, fmt.Errorf("embedded public workload prior digest changed: got %s want %s", digest, PublicPriorDigest)
	}
	document.Digest = digest
	if err = Validate(document); err != nil {
		return Document{}, err
	}
	return document, nil
}

func Decode(data []byte) (Document, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode workload profile: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Document{}, err
	}
	if err := Validate(document); err != nil {
		return Document{}, err
	}
	return document, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("workload profile contains multiple JSON values")
		}
		return fmt.Errorf("decode workload profile trailer: %w", err)
	}
	return nil
}

func Validate(document Document) error {
	if document.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	if document.EvidenceClass != EvidenceClass {
		return fmt.Errorf("evidence_class must be %q", EvidenceClass)
	}
	if _, err := time.Parse(time.RFC3339, document.GeneratedAt); err != nil {
		return errors.New("generated_at must be an RFC3339 timestamp")
	}
	if strings.TrimSpace(document.Source.Name) == "" || document.Source.TraceRows < 1 {
		return errors.New("source name and positive trace_rows are required")
	}
	parsedURL, err := url.Parse(document.Source.URL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return errors.New("source URL must be an absolute HTTPS URL")
	}
	if strings.TrimSpace(document.Source.License) == "" {
		return errors.New("source license is required")
	}
	if document.Sampling.Method == "" || len(document.Sampling.Columns) == 0 || len(document.Sampling.RowGroups) == 0 {
		return errors.New("sampling method, columns, and row groups are required")
	}
	if document.Sampling.TotalRowGroups < len(document.Sampling.RowGroups) || document.Sampling.RowsInspected < 1 {
		return errors.New("sampling row group and inspected row counts are invalid")
	}
	if !document.Sampling.RemoteOnly {
		return errors.New("public workload profiles must attest remote_only sampling")
	}
	for name, distribution := range map[string]Distribution{
		"input_tokens":       document.Distributions.InputTokens,
		"output_tokens":      document.Distributions.OutputTokens,
		"cached_tokens":      document.Distributions.CachedTokens,
		"ttft_ms":            document.Distributions.TTFTMilliseconds,
		"duration_ms":        document.Distributions.DurationMS,
		"cache_hit_fraction": document.Distributions.CacheHitFraction,
	} {
		if err = validateDistribution(name, distribution); err != nil {
			return err
		}
	}
	if len(document.Profiles) == 0 {
		return errors.New("at least one benchmark profile is required")
	}
	seen := make(map[string]struct{}, len(document.Profiles))
	for _, profile := range document.Profiles {
		if err = validateProfile(profile); err != nil {
			return err
		}
		key := strings.ToLower(profile.Name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate workload profile %q", profile.Name)
		}
		seen[key] = struct{}{}
	}
	if strings.TrimSpace(document.MethodologyBoundary) == "" {
		return errors.New("methodology_boundary is required")
	}
	digest, err := Digest(document)
	if err != nil {
		return err
	}
	if document.Digest != "" && document.Digest != digest {
		return fmt.Errorf("workload profile digest mismatch: got %q want %q", document.Digest, digest)
	}
	return nil
}

func validateDistribution(name string, distribution Distribution) error {
	if distribution.Unit == "" || distribution.Samples < 1 {
		return fmt.Errorf("distribution %s requires a unit and positive sample count", name)
	}
	values := []float64{distribution.Percentiles.P50, distribution.Percentiles.P90, distribution.Percentiles.P95, distribution.Percentiles.P99}
	for index, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("distribution %s contains an invalid percentile", name)
		}
		if index > 0 && value < values[index-1] {
			return fmt.Errorf("distribution %s percentiles must be monotonic", name)
		}
	}
	return nil
}

func validateProfile(profile BenchmarkShape) error {
	if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Description) == "" || strings.TrimSpace(profile.Objective) == "" {
		return errors.New("profile name, description, and objective are required")
	}
	if profile.Requests < 1 || profile.Concurrency < 1 || profile.Requests < profile.Concurrency {
		return fmt.Errorf("profile %q requires positive requests and concurrency with requests >= concurrency", profile.Name)
	}
	if profile.InputTokens < 1 || profile.OutputTokens < 1 || profile.InputTokens > 2_000_000 || profile.OutputTokens > 2_000_000 {
		return fmt.Errorf("profile %q token counts are outside safe bounds", profile.Name)
	}
	pattern := strings.ToLower(strings.TrimSpace(profile.ArrivalPattern))
	if pattern != "" && pattern != "constant" && pattern != "poisson" && pattern != "gamma" {
		return fmt.Errorf("profile %q arrival_pattern must be constant, poisson, or gamma", profile.Name)
	}
	if math.IsNaN(profile.RequestRate) || math.IsInf(profile.RequestRate, 0) || profile.RequestRate < 0 || (pattern == "") != (profile.RequestRate == 0) {
		return fmt.Errorf("profile %q must supply arrival_pattern and positive request_rate together", profile.Name)
	}
	return nil
}

func Digest(document Document) (string, error) {
	document.Digest = ""
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode workload profile digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func Lookup(document Document, name string) (BenchmarkShape, error) {
	for _, profile := range document.Profiles {
		if strings.EqualFold(profile.Name, strings.TrimSpace(name)) {
			return profile, nil
		}
	}
	names := make([]string, 0, len(document.Profiles))
	for _, profile := range document.Profiles {
		names = append(names, profile.Name)
	}
	sort.Strings(names)
	return BenchmarkShape{}, fmt.Errorf("unknown workload profile %q (choose %s)", name, strings.Join(names, ", "))
}

func AsBenchmarkProfile(profile BenchmarkShape) performanceprofile.Profile {
	return performanceprofile.Profile{
		Name: profile.Name, Description: profile.Description, Objective: profile.Objective,
		Requests: profile.Requests, Concurrency: profile.Concurrency,
		InputTokens: profile.InputTokens, OutputTokens: profile.OutputTokens,
		Streaming: profile.Streaming,
	}
}
