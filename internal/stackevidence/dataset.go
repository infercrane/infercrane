// Package stackevidence holds InferCrane's reviewed dataset of serving-stack
// benchmark evidence and ranks the best-known stacks for a model and goal.
//
// Every data point is copied from a cited source at the exact model, engine,
// version, GPU, precision, parallelism and workload it was measured on, with
// the source's date and license. Nothing in the dataset is estimated or
// interpolated. The ranking chooses among measured operating points; the only
// computed number is cost per million output tokens, which combines a cited
// throughput with a current price observation and is labelled as such.
//
// The dataset is embedded and reviewed as a diff. Runtime code never fetches
// benchmark data.
package stackevidence

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = "infercrane.stack-evidence/v1"

// Source kinds. Each row in a ranking carries exactly one.
const (
	SourcePublicBenchmark      = "public_benchmark"
	SourceVendorRecipe         = "vendor_recipe"
	SourceMeasuredByInferCrane = "measured_by_infercrane"
)

// Precision labels used by the dataset.
var precisions = []string{"bf16", "fp16", "fp8", "fp4", "mxfp4", "nvfp4", "int8", "int4"}

// Engines the dataset may name. Deployment support is a separate question
// answered by the reviewed catalog profile a stack points to.
var engines = []string{"vllm", "sglang", "trt-llm", "dynamo-trt", "dynamo-sglang", "dynamo-vllm"}

//go:embed dataset.json
var embedded []byte

type Dataset struct {
	SchemaVersion string `json:"schema_version"`
	// Version changes whenever a source, family or stack changes.
	Version    string `json:"version"`
	ReviewedAt string `json:"reviewed_at"`
	// StaleAfterDays marks evidence older than this many days, counted from the
	// measurement date, as stale. Stale rows stay visible and rank after fresh ones.
	StaleAfterDays int      `json:"stale_after_days"`
	Sources        []Source `json:"sources"`
	Families       []Family `json:"families"`
	Stacks         []Stack  `json:"stacks"`
}

// Source is one cited publication or receipt.
type Source struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Label is the short attribution shown on every row, for example
	// "SemiAnalysis InferenceMAX".
	Label       string `json:"label"`
	Publisher   string `json:"publisher"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	License     string `json:"license"`
	LicenseURL  string `json:"license_url"`
	PublishedAt string `json:"published_at"`
	RetrievedAt string `json:"retrieved_at"`
	Notes       string `json:"notes,omitempty"`
}

// Family groups checkpoints that share one architecture, so evidence for one
// can be shown, clearly labelled, for another when no exact evidence exists.
type Family struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Basis states why the members are comparable, for example the shared
	// architecture, and what differs.
	Basis   string   `json:"basis"`
	Members []string `json:"members"`
}

// Stack is one serving configuration measured by one source on one workload.
type Stack struct {
	ID            string `json:"id"`
	Source        string `json:"source"`
	Model         string `json:"model"`
	ModelRevision string `json:"model_revision,omitempty"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	// Image is set only when the measured image is pinned by digest.
	Image string `json:"image,omitempty"`
	// ImageAsPublished is the image reference exactly as the source names it,
	// usually a mutable tag.
	ImageAsPublished string   `json:"image_as_published,omitempty"`
	GPU              string   `json:"gpu"`
	GPUVendor        string   `json:"gpu_vendor"`
	GPUCount         int      `json:"gpu_count"`
	Precision        string   `json:"precision"`
	Parallelism      string   `json:"parallelism"`
	Techniques       []string `json:"techniques"`
	Workload         Workload `json:"workload"`
	MeasuredAt       string   `json:"measured_at"`
	// SourceRef points at the exact file, table or field the points were read from.
	SourceRef   string     `json:"source_ref"`
	Deploy      *DeployRef `json:"deploy,omitempty"`
	Limitations []string   `json:"limitations"`
	Points      []Point    `json:"points"`
}

type Workload struct {
	ID           string `json:"id,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Dataset      string `json:"dataset"`
}

// DeployRef names the reviewed catalog profile that launches this exact stack.
type DeployRef struct {
	CatalogModel string `json:"catalog_model"`
	Profile      string `json:"profile"`
}

// Point is one measured operating point. Missing metrics stay nil.
type Point struct {
	Concurrency int `json:"concurrency"`
	// OutputTokensPerSecond is the aggregate output throughput of the whole
	// stack (all of its GPUs), as published.
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second,omitempty"`
	// OutputTokensPerSecondPerGPU is set only when the source publishes the
	// per-GPU figure itself.
	OutputTokensPerSecondPerGPU *float64 `json:"output_tokens_per_second_per_gpu,omitempty"`
	// PerUserOutputTokensPerSecond is one request's output speed, using the
	// statistic named in PerUserStatistic.
	PerUserOutputTokensPerSecond *float64    `json:"per_user_output_tokens_per_second,omitempty"`
	PerUserStatistic             string      `json:"per_user_statistic,omitempty"`
	TTFTMs                       Percentiles `json:"ttft_ms"`
	ITLMs                        Percentiles `json:"itl_ms"`
	Ref                          string      `json:"ref"`
}

type Percentiles struct {
	Mean *float64 `json:"mean,omitempty"`
	P50  *float64 `json:"p50,omitempty"`
	P90  *float64 `json:"p90,omitempty"`
	P95  *float64 `json:"p95,omitempty"`
	P99  *float64 `json:"p99,omitempty"`
}

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]{1,127}$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}$`)
	datePattern  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	statistics   = []string{"mean", "median"}
)

var (
	loadOnce sync.Once
	loaded   Dataset
	loadErr  error
)

// Default returns the embedded reviewed dataset.
func Default() (Dataset, error) {
	loadOnce.Do(func() { loaded, loadErr = Parse(embedded) })
	if loadErr != nil {
		return Dataset{}, loadErr
	}
	return loaded.clone(), nil
}

// MustDefault panics when the embedded dataset is invalid. Tests prove it is not.
func MustDefault() Dataset {
	dataset, err := Default()
	if err != nil {
		panic(err)
	}
	return dataset
}

// Parse decodes and validates a dataset document. Unknown fields are refused.
func Parse(data []byte) (Dataset, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var dataset Dataset
	if err := decoder.Decode(&dataset); err != nil {
		return Dataset{}, fmt.Errorf("decode stack evidence: %w", err)
	}
	if err := dataset.Validate(); err != nil {
		return Dataset{}, err
	}
	return dataset, nil
}

// Validate refuses anything that could become an unsourced or ambiguous number.
func (d Dataset) Validate() error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }
	if d.SchemaVersion != SchemaVersion {
		add("schema_version must be %s", SchemaVersion)
	}
	if strings.TrimSpace(d.Version) == "" {
		add("version is required")
	}
	if _, err := parseDate(d.ReviewedAt); err != nil {
		add("reviewed_at: %v", err)
	}
	if d.StaleAfterDays < 30 || d.StaleAfterDays > 730 {
		add("stale_after_days must be between 30 and 730")
	}
	sources := map[string]Source{}
	for _, source := range d.Sources {
		if !idPattern.MatchString(source.ID) {
			add("source %q: invalid id", source.ID)
		}
		if _, dup := sources[source.ID]; dup {
			add("source %q: duplicate id", source.ID)
		}
		sources[source.ID] = source
		if !slices.Contains([]string{SourcePublicBenchmark, SourceVendorRecipe, SourceMeasuredByInferCrane}, source.Kind) {
			add("source %q: unknown kind %q", source.ID, source.Kind)
		}
		for field, value := range map[string]string{"label": source.Label, "publisher": source.Publisher, "title": source.Title, "license": source.License} {
			if strings.TrimSpace(value) == "" {
				add("source %q: %s is required", source.ID, field)
			}
		}
		for field, value := range map[string]string{"url": source.URL, "license_url": source.LicenseURL} {
			if !strings.HasPrefix(value, "https://") {
				add("source %q: %s must be an https URL", source.ID, field)
			}
		}
		published, err := parseDate(source.PublishedAt)
		if err != nil {
			add("source %q: published_at: %v", source.ID, err)
		}
		retrieved, err := parseDate(source.RetrievedAt)
		if err != nil {
			add("source %q: retrieved_at: %v", source.ID, err)
		} else if retrieved.Before(published) {
			add("source %q: retrieved before it was published", source.ID)
		}
	}
	members := map[string]string{}
	for _, family := range d.Families {
		if !idPattern.MatchString(family.ID) || strings.TrimSpace(family.Label) == "" || strings.TrimSpace(family.Basis) == "" {
			add("family %q: id, label and basis are required", family.ID)
		}
		if len(family.Members) < 2 {
			add("family %q: needs at least two members", family.ID)
		}
		for _, member := range family.Members {
			if !repoPattern.MatchString(member) {
				add("family %q: member %q is not an organization/repository", family.ID, member)
			}
			key := strings.ToLower(member)
			if other, dup := members[key]; dup {
				add("model %q belongs to families %q and %q", member, other, family.ID)
			}
			members[key] = family.ID
		}
	}
	stacks := map[string]bool{}
	for _, stack := range d.Stacks {
		label := fmt.Sprintf("stack %q", stack.ID)
		if !idPattern.MatchString(stack.ID) {
			add("%s: invalid id", label)
		}
		if stacks[stack.ID] {
			add("%s: duplicate id", label)
		}
		stacks[stack.ID] = true
		source, ok := sources[stack.Source]
		if !ok {
			add("%s: unknown source %q", label, stack.Source)
		}
		if !repoPattern.MatchString(stack.Model) {
			add("%s: model must be an exact organization/repository", label)
		}
		if stack.ModelRevision != "" && !revPattern.MatchString(stack.ModelRevision) {
			add("%s: model_revision must be a 40-character commit", label)
		}
		if !slices.Contains(engines, stack.Engine) {
			add("%s: unknown engine %q", label, stack.Engine)
		}
		if strings.TrimSpace(stack.EngineVersion) == "" {
			add("%s: engine_version is required; write \"not stated\" when the source omits it", label)
		}
		if stack.Image != "" && !imagePattern.MatchString(stack.Image) {
			add("%s: image must be pinned by digest", label)
		}
		if len(stack.ImageAsPublished) > 256 {
			add("%s: image_as_published is too long", label)
		}
		if strings.TrimSpace(stack.GPU) == "" || !slices.Contains([]string{"nvidia", "amd"}, stack.GPUVendor) {
			add("%s: gpu and gpu_vendor (nvidia or amd) are required", label)
		}
		if stack.GPUCount < 1 || stack.GPUCount > 72 {
			add("%s: gpu_count must be between 1 and 72", label)
		}
		if !slices.Contains(precisions, stack.Precision) {
			add("%s: unknown precision %q", label, stack.Precision)
		}
		if strings.TrimSpace(stack.Parallelism) == "" {
			add("%s: parallelism is required", label)
		}
		if stack.Workload.InputTokens < 1 || stack.Workload.OutputTokens < 1 || strings.TrimSpace(stack.Workload.Dataset) == "" {
			add("%s: workload needs input_tokens, output_tokens and dataset", label)
		}
		measured, err := parseDate(stack.MeasuredAt)
		if err != nil {
			add("%s: measured_at: %v", label, err)
		} else if ok {
			if retrieved, rerr := parseDate(source.RetrievedAt); rerr == nil && measured.After(retrieved) {
				add("%s: measured after its source was retrieved", label)
			}
		}
		if strings.TrimSpace(stack.SourceRef) == "" {
			add("%s: source_ref is required", label)
		}
		if stack.Deploy != nil && (strings.TrimSpace(stack.Deploy.CatalogModel) == "" || strings.TrimSpace(stack.Deploy.Profile) == "") {
			add("%s: deploy needs catalog_model and profile", label)
		}
		if len(stack.Points) == 0 {
			add("%s: needs at least one measured point", label)
		}
		concurrency := map[int]bool{}
		for index, point := range stack.Points {
			pointLabel := fmt.Sprintf("%s point %d", label, index)
			if point.Concurrency < 1 {
				add("%s: concurrency must be positive", pointLabel)
			}
			if concurrency[point.Concurrency] {
				add("%s: duplicate concurrency %d", pointLabel, point.Concurrency)
			}
			concurrency[point.Concurrency] = true
			if point.OutputTokensPerSecond == nil && point.OutputTokensPerSecondPerGPU == nil {
				add("%s: needs output_tokens_per_second or output_tokens_per_second_per_gpu", pointLabel)
			}
			if point.PerUserOutputTokensPerSecond != nil && !slices.Contains(statistics, point.PerUserStatistic) {
				add("%s: per_user_statistic must be mean or median", pointLabel)
			}
			if strings.TrimSpace(point.Ref) == "" {
				add("%s: ref is required", pointLabel)
			}
			for name, value := range point.values() {
				if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0) {
					add("%s: %s must be a positive finite number", pointLabel, name)
				}
			}
		}
	}
	return errors.Join(problems...)
}

func (p Point) values() map[string]*float64 {
	return map[string]*float64{
		"output_tokens_per_second": p.OutputTokensPerSecond, "output_tokens_per_second_per_gpu": p.OutputTokensPerSecondPerGPU,
		"per_user_output_tokens_per_second": p.PerUserOutputTokensPerSecond,
		"ttft_ms.mean":                      p.TTFTMs.Mean, "ttft_ms.p50": p.TTFTMs.P50, "ttft_ms.p90": p.TTFTMs.P90, "ttft_ms.p95": p.TTFTMs.P95, "ttft_ms.p99": p.TTFTMs.P99,
		"itl_ms.mean": p.ITLMs.Mean, "itl_ms.p50": p.ITLMs.P50, "itl_ms.p90": p.ITLMs.P90, "itl_ms.p95": p.ITLMs.P95, "itl_ms.p99": p.ITLMs.P99,
	}
}

func parseDate(value string) (time.Time, error) {
	if !datePattern.MatchString(value) {
		return time.Time{}, fmt.Errorf("%q is not YYYY-MM-DD", value)
	}
	return time.Parse(time.DateOnly, value)
}

// Source returns a source by id.
func (d Dataset) Source(id string) (Source, bool) {
	for _, source := range d.Sources {
		if source.ID == id {
			return source, true
		}
	}
	return Source{}, false
}

// Stack returns a stack by id.
func (d Dataset) Stack(id string) (Stack, bool) {
	for _, stack := range d.Stacks {
		if stack.ID == id {
			return stack.clone(), true
		}
	}
	return Stack{}, false
}

// FamilyOf returns the family a model belongs to.
func (d Dataset) FamilyOf(model string) (Family, bool) {
	for _, family := range d.Families {
		for _, member := range family.Members {
			if strings.EqualFold(member, model) {
				return family, true
			}
		}
	}
	return Family{}, false
}

func (d Dataset) clone() Dataset {
	out := d
	out.Sources = slices.Clone(d.Sources)
	out.Families = make([]Family, len(d.Families))
	for index, family := range d.Families {
		family.Members = slices.Clone(family.Members)
		out.Families[index] = family
	}
	out.Stacks = make([]Stack, len(d.Stacks))
	for index, stack := range d.Stacks {
		out.Stacks[index] = stack.clone()
	}
	return out
}

func (s Stack) clone() Stack {
	s.Techniques = slices.Clone(s.Techniques)
	s.Limitations = slices.Clone(s.Limitations)
	s.Points = slices.Clone(s.Points)
	if s.Deploy != nil {
		deploy := *s.Deploy
		s.Deploy = &deploy
	}
	return s
}

// Throughput returns the stack's aggregate and per-GPU output throughput for a
// point. When the source published only one of them, the other is the exact
// product or quotient by the stack's GPU count, and derived reports which.
func (s Stack) Throughput(point Point) (total, perGPU float64, derived string) {
	switch {
	case point.OutputTokensPerSecond != nil && point.OutputTokensPerSecondPerGPU != nil:
		return *point.OutputTokensPerSecond, *point.OutputTokensPerSecondPerGPU, ""
	case point.OutputTokensPerSecond != nil:
		return *point.OutputTokensPerSecond, *point.OutputTokensPerSecond / float64(s.GPUCount), "output_tokens_per_second_per_gpu"
	case point.OutputTokensPerSecondPerGPU != nil:
		return *point.OutputTokensPerSecondPerGPU * float64(s.GPUCount), *point.OutputTokensPerSecondPerGPU, "output_tokens_per_second"
	default:
		return 0, 0, ""
	}
}
