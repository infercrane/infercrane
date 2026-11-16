package stackevidence

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
)

const ResultSchemaVersion = "infercrane.best-known-stacks/v1"

// Goals.
const (
	GoalLowestLatency     = "lowest_latency"
	GoalLowestCost        = "lowest_cost"
	GoalHighestThroughput = "highest_throughput"
)

// Goals lists the supported goals in display order.
var Goals = []string{GoalLowestLatency, GoalLowestCost, GoalHighestThroughput}

// Match states for the model and the workload shape.
const (
	MatchExact   = "exact"
	MatchFamily  = "family"
	MatchClosest = "closest"
	MatchNone    = "none"
)

// Target states for a ranked row.
const (
	TargetMet      = "met"
	TargetUnknown  = "unknown"
	TargetNotGiven = "no_target"
	targetMissed   = "missed"
)

// Deploy states for a ranked row.
const (
	DeployReady       = "ready"
	DeployNotReviewed = "not_reviewed"
)

const (
	DefaultInputTokens  = 1024
	DefaultOutputTokens = 1024
	DefaultLimit        = 10
	MaxLimit            = 50
	MaxGPUs             = 16
)

const evidencePolicy = "Every throughput and latency number is cited from its source at the exact model, engine, GPU, precision and workload it was measured on. Cost per 1M output tokens is an estimate: the cited throughput at the chosen operating point times a current price for the same GPUs. None of it is measured on your traffic until an optimization run compares setups side by side."

var ErrInvalidQuery = errors.New("invalid best-known stacks query")

// Query asks for the best-known stacks for one model and goal.
type Query struct {
	// Model is an exact organization/repository.
	Model        string
	Goal         string
	InputTokens  int
	OutputTokens int
	// GPUs limits the ranking to these accelerators; empty means any.
	GPUs []string
	// MaxTTFTP95Ms and MinPerUserOutputTokensPerSecond are the latency target.
	// Zero means no target.
	MaxTTFTP95Ms                    float64
	MinPerUserOutputTokensPerSecond float64
	Limit                           int
}

// PriceQuote is a current, complete hourly price for exactly GPUCount GPUs.
type PriceQuote struct {
	Provider   string
	Region     string
	HourlyUSD  float64
	Source     string
	Authority  string
	ObservedAt time.Time
	ValidUntil time.Time
}

// Prices returns the cheapest current complete price for gpuCount GPUs of one
// type, or false when none is known. A nil Prices leaves every cost unknown.
type Prices func(gpu string, gpuCount int) (PriceQuote, bool)

type Result struct {
	SchemaVersion  string           `json:"schema_version"`
	DatasetVersion string           `json:"dataset_version"`
	Model          ModelMatch       `json:"model"`
	Goal           string           `json:"goal"`
	RankedBy       string           `json:"ranked_by"`
	Workload       ComparedWorkload `json:"workload"`
	Target         Target           `json:"target"`
	Stacks         []RankedStack    `json:"stacks"`
	Total          int              `json:"total"`
	// DefaultStack is the highest-ranked stack that has a reviewed launch
	// configuration, which Deploy uses unless another is chosen.
	DefaultStack   string          `json:"default_stack,omitempty"`
	Excluded       []Exclusion     `json:"excluded"`
	OtherWorkloads []WorkloadShape `json:"other_workloads"`
	EvidencePolicy string          `json:"evidence_policy"`
}

type ModelMatch struct {
	Requested string     `json:"requested"`
	Match     string     `json:"match"`
	Family    *FamilyRef `json:"family,omitempty"`
}

type FamilyRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Basis string `json:"basis"`
}

type WorkloadShape struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Stacks       int `json:"stacks,omitempty"`
}

type ComparedWorkload struct {
	InputTokens  int           `json:"input_tokens,omitempty"`
	OutputTokens int           `json:"output_tokens,omitempty"`
	Requested    WorkloadShape `json:"requested"`
	Match        string        `json:"match"`
}

type Target struct {
	MaxTTFTP95Ms                    *float64 `json:"max_ttft_p95_ms,omitempty"`
	MinPerUserOutputTokensPerSecond *float64 `json:"min_per_user_output_tokens_per_second,omitempty"`
}

type Exclusion struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

type RankedStack struct {
	Rank          int      `json:"rank"`
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Model         string   `json:"model"`
	ModelMatch    string   `json:"model_match"`
	Engine        string   `json:"engine"`
	EngineVersion string   `json:"engine_version"`
	GPU           string   `json:"gpu"`
	GPUVendor     string   `json:"gpu_vendor"`
	GPUCount      int      `json:"gpu_count"`
	Precision     string   `json:"precision"`
	Parallelism   string   `json:"parallelism"`
	Techniques    []string `json:"techniques"`
	Workload      Workload `json:"workload"`
	MeasuredAt    string   `json:"measured_at"`
	Stale         bool     `json:"stale"`
	// RankBasis names the metric that placed this row.
	RankBasis      string         `json:"rank_basis"`
	OperatingPoint OperatingPoint `json:"operating_point"`
	Metrics        Metrics        `json:"metrics"`
	Cost           Cost           `json:"cost"`
	Target         string         `json:"target"`
	Source         SourceRef      `json:"source"`
	Deploy         DeployState    `json:"deploy"`
	Limitations    []string       `json:"limitations"`
}

type OperatingPoint struct {
	Concurrency int    `json:"concurrency"`
	Ref         string `json:"ref"`
}

type Metrics struct {
	OutputTokensPerSecond        float64  `json:"output_tokens_per_second"`
	OutputTokensPerSecondPerGPU  float64  `json:"output_tokens_per_second_per_gpu"`
	PerUserOutputTokensPerSecond *float64 `json:"per_user_output_tokens_per_second"`
	PerUserStatistic             string   `json:"per_user_statistic,omitempty"`
	TTFTP50Ms                    *float64 `json:"ttft_p50_ms"`
	TTFTP95Ms                    *float64 `json:"ttft_p95_ms"`
	TTFTP99Ms                    *float64 `json:"ttft_p99_ms"`
	// TTFTP95AtMostMs is the published p99 when the p95 was not published:
	// the p95 is at most this. Null when the p95 itself is published.
	TTFTP95AtMostMs *float64 `json:"ttft_p95_at_most_ms"`
	ITLP95Ms        *float64 `json:"itl_p95_ms"`
	// Derived names a throughput the source did not publish itself: it is the
	// published figure multiplied or divided by the GPU count.
	Derived []string `json:"derived"`
}

type Cost struct {
	State                     string     `json:"state"`
	PerMillionOutputTokensUSD *float64   `json:"per_million_output_tokens_usd"`
	HourlyUSD                 *float64   `json:"hourly_usd,omitempty"`
	Provider                  string     `json:"provider,omitempty"`
	Region                    string     `json:"region,omitempty"`
	PriceSource               string     `json:"price_source,omitempty"`
	PriceAuthority            string     `json:"price_authority,omitempty"`
	PriceObservedAt           *time.Time `json:"price_observed_at,omitempty"`
	Basis                     string     `json:"basis"`
}

// Cost states.
const (
	CostEstimated = "estimated"
	CostUnknown   = "unknown"
)

type SourceRef struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Display     string `json:"display"`
	Publisher   string `json:"publisher"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	License     string `json:"license"`
	LicenseURL  string `json:"license_url"`
	PublishedAt string `json:"published_at"`
	Ref         string `json:"ref"`
}

type DeployState struct {
	State        string `json:"state"`
	CatalogModel string `json:"catalog_model,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// NormalizeQuery applies defaults and bounds.
func NormalizeQuery(query Query) (Query, error) {
	query.Model = strings.TrimSpace(query.Model)
	query.Goal = strings.TrimSpace(query.Goal)
	if !repoPattern.MatchString(query.Model) {
		return Query{}, fmt.Errorf("%w: model must be an exact organization/repository or a reviewed catalog name", ErrInvalidQuery)
	}
	if query.Goal == "" {
		query.Goal = GoalLowestCost
	}
	if !slices.Contains(Goals, query.Goal) {
		return Query{}, fmt.Errorf("%w: goal must be one of %s", ErrInvalidQuery, strings.Join(Goals, ", "))
	}
	if query.InputTokens == 0 {
		query.InputTokens = DefaultInputTokens
	}
	if query.OutputTokens == 0 {
		query.OutputTokens = DefaultOutputTokens
	}
	if query.InputTokens < 1 || query.InputTokens > 1_048_576 || query.OutputTokens < 1 || query.OutputTokens > 262_144 {
		return Query{}, fmt.Errorf("%w: input_tokens must be 1 to 1048576 and output_tokens 1 to 262144", ErrInvalidQuery)
	}
	if len(query.GPUs) > MaxGPUs {
		return Query{}, fmt.Errorf("%w: at most %d GPUs", ErrInvalidQuery, MaxGPUs)
	}
	gpus := make([]string, 0, len(query.GPUs))
	for _, gpu := range query.GPUs {
		gpu = strings.TrimSpace(gpu)
		if gpu == "" || len(gpu) > 64 {
			return Query{}, fmt.Errorf("%w: each GPU must be 1 to 64 characters", ErrInvalidQuery)
		}
		if !slices.ContainsFunc(gpus, func(seen string) bool { return strings.EqualFold(seen, gpu) }) {
			gpus = append(gpus, gpu)
		}
	}
	query.GPUs = gpus
	if query.MaxTTFTP95Ms < 0 || query.MaxTTFTP95Ms > 600_000 || math.IsNaN(query.MaxTTFTP95Ms) {
		return Query{}, fmt.Errorf("%w: max_ttft_p95_ms must be 0 to 600000", ErrInvalidQuery)
	}
	if query.MinPerUserOutputTokensPerSecond < 0 || query.MinPerUserOutputTokensPerSecond > 10_000 || math.IsNaN(query.MinPerUserOutputTokensPerSecond) {
		return Query{}, fmt.Errorf("%w: min_per_user_output_tokens_per_second must be 0 to 10000", ErrInvalidQuery)
	}
	if query.Limit == 0 {
		query.Limit = DefaultLimit
	}
	if query.Limit < 1 || query.Limit > MaxLimit {
		return Query{}, fmt.Errorf("%w: limit must be 1 to %d", ErrInvalidQuery, MaxLimit)
	}
	return query, nil
}

// Rank returns the best-known stacks for a query. It never invents a number:
// it picks one measured operating point per stack, and the only computed value
// is the cost estimate, which stays unknown without a current price.
func (d Dataset) Rank(query Query, prices Prices, now time.Time) (Result, error) {
	query, err := NormalizeQuery(query)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		SchemaVersion: ResultSchemaVersion, DatasetVersion: d.Version,
		Model:          ModelMatch{Requested: query.Model, Match: MatchNone},
		Goal:           query.Goal,
		RankedBy:       rankedBy(query),
		Workload:       ComparedWorkload{Requested: WorkloadShape{InputTokens: query.InputTokens, OutputTokens: query.OutputTokens}, Match: MatchNone},
		Target:         target(query),
		Stacks:         []RankedStack{},
		Excluded:       []Exclusion{},
		OtherWorkloads: []WorkloadShape{},
		EvidencePolicy: evidencePolicy,
	}
	candidates := d.stacksFor(query.Model, MatchExact)
	if len(candidates) > 0 {
		result.Model.Match = MatchExact
	} else if family, ok := d.FamilyOf(query.Model); ok {
		candidates = d.stacksFor(query.Model, MatchFamily)
		if len(candidates) > 0 {
			result.Model.Match = MatchFamily
			result.Model.Family = &FamilyRef{ID: family.ID, Label: family.Label, Basis: family.Basis}
		}
	}
	if len(candidates) == 0 {
		return result, nil
	}

	available := make([]Stack, 0, len(candidates))
	for _, stack := range candidates {
		if len(query.GPUs) > 0 && !slices.ContainsFunc(query.GPUs, func(gpu string) bool { return sameGPU(gpu, stack.GPU) }) {
			result.Excluded = append(result.Excluded, Exclusion{ID: stack.ID, Label: stack.Label(), Reason: "Runs on " + stack.GPU + ", which is not in the GPUs you can use."})
			continue
		}
		available = append(available, stack)
	}
	shapes := shapesOf(available)
	if len(shapes) == 0 {
		return result, nil
	}
	chosen := closestShape(shapes, query.InputTokens, query.OutputTokens)
	result.Workload.InputTokens, result.Workload.OutputTokens = chosen.InputTokens, chosen.OutputTokens
	result.Workload.Match = MatchClosest
	if chosen.InputTokens == query.InputTokens && chosen.OutputTokens == query.OutputTokens {
		result.Workload.Match = MatchExact
	}
	for _, shape := range shapes {
		if shape.InputTokens != chosen.InputTokens || shape.OutputTokens != chosen.OutputTokens {
			result.OtherWorkloads = append(result.OtherWorkloads, shape)
		}
	}

	ranked := make([]RankedStack, 0, len(available))
	for _, stack := range available {
		if stack.Workload.InputTokens != chosen.InputTokens || stack.Workload.OutputTokens != chosen.OutputTokens {
			continue
		}
		row, reason := d.rankRow(stack, query, result.Model.Match, prices, now)
		if reason != "" {
			result.Excluded = append(result.Excluded, Exclusion{ID: stack.ID, Label: stack.Label(), Reason: reason})
			continue
		}
		ranked = append(ranked, row)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return less(query.Goal, ranked[i], ranked[j]) })
	for index := range ranked {
		ranked[index].Rank = index + 1
		ranked[index].RankBasis = rankBasis(query.Goal, ranked[index])
		if result.DefaultStack == "" && ranked[index].Deploy.State == DeployReady {
			result.DefaultStack = ranked[index].ID
		}
	}
	result.Total = len(ranked)
	if len(ranked) > query.Limit {
		ranked = ranked[:query.Limit]
	}
	result.Stacks = ranked
	return result, nil
}

func (d Dataset) stacksFor(model, match string) []Stack {
	out := []Stack{}
	family, inFamily := d.FamilyOf(model)
	for _, stack := range d.Stacks {
		switch match {
		case MatchExact:
			if strings.EqualFold(stack.Model, model) {
				out = append(out, stack.clone())
			}
		case MatchFamily:
			if inFamily && slices.ContainsFunc(family.Members, func(member string) bool { return strings.EqualFold(member, stack.Model) }) {
				out = append(out, stack.clone())
			}
		}
	}
	return out
}

func shapesOf(stacks []Stack) []WorkloadShape {
	counts := map[[2]int]int{}
	for _, stack := range stacks {
		counts[[2]int{stack.Workload.InputTokens, stack.Workload.OutputTokens}]++
	}
	shapes := make([]WorkloadShape, 0, len(counts))
	for key, count := range counts {
		shapes = append(shapes, WorkloadShape{InputTokens: key[0], OutputTokens: key[1], Stacks: count})
	}
	sort.Slice(shapes, func(i, j int) bool {
		if shapes[i].InputTokens != shapes[j].InputTokens {
			return shapes[i].InputTokens < shapes[j].InputTokens
		}
		return shapes[i].OutputTokens < shapes[j].OutputTokens
	})
	return shapes
}

// closestShape compares shapes by the sum of absolute log ratios of input and
// output length, so 1k→2k counts the same as 8k→16k. Ties prefer the shape
// with more stacks, then the shorter one.
func closestShape(shapes []WorkloadShape, input, output int) WorkloadShape {
	distance := func(shape WorkloadShape) float64 {
		return math.Abs(math.Log(float64(shape.InputTokens)/float64(input))) + math.Abs(math.Log(float64(shape.OutputTokens)/float64(output)))
	}
	best := shapes[0]
	for _, shape := range shapes[1:] {
		a, b := distance(shape), distance(best)
		switch {
		case a < b-1e-12:
			best = shape
		case math.Abs(a-b) <= 1e-12 && shape.Stacks > best.Stacks:
			best = shape
		}
	}
	return best
}

func (d Dataset) rankRow(stack Stack, query Query, match string, prices Prices, now time.Time) (RankedStack, string) {
	point, targetState, reason := choosePoint(stack, query)
	if reason != "" {
		return RankedStack{}, reason
	}
	total, perGPU, derived := stack.Throughput(point)
	source, _ := d.Source(stack.Source)
	measured, _ := parseDate(stack.MeasuredAt)
	row := RankedStack{
		ID: stack.ID, Label: stack.Label(), Model: stack.Model, ModelMatch: match,
		Engine: stack.Engine, EngineVersion: stack.EngineVersion, GPU: stack.GPU, GPUVendor: stack.GPUVendor, GPUCount: stack.GPUCount,
		Precision: stack.Precision, Parallelism: stack.Parallelism, Techniques: nonNil(stack.Techniques), Workload: stack.Workload,
		MeasuredAt: stack.MeasuredAt, Stale: now.Sub(measured) > time.Duration(d.StaleAfterDays)*24*time.Hour,
		OperatingPoint: OperatingPoint{Concurrency: point.Concurrency, Ref: point.Ref},
		Metrics: Metrics{
			OutputTokensPerSecond: round(total, 2), OutputTokensPerSecondPerGPU: round(perGPU, 2),
			PerUserOutputTokensPerSecond: point.PerUserOutputTokensPerSecond, PerUserStatistic: point.PerUserStatistic,
			TTFTP50Ms: point.TTFTMs.P50, TTFTP95Ms: point.TTFTMs.P95, TTFTP99Ms: point.TTFTMs.P99, ITLP95Ms: point.ITLMs.P95,
			Derived: []string{},
		},
		Target:      targetState,
		Source:      sourceRef(source, stack),
		Deploy:      deployState(stack),
		Limitations: nonNil(stack.Limitations),
	}
	if derived != "" {
		row.Metrics.Derived = append(row.Metrics.Derived, derived)
	}
	if point.TTFTMs.P95 == nil {
		row.Metrics.TTFTP95AtMostMs = point.TTFTMs.P99
	}
	row.Cost = cost(stack, total, point.Concurrency, prices)
	return row, ""
}

// choosePoint picks the measured operating point that serves the goal while
// meeting the target. Points that miss the target are never chosen; points
// whose target metric was not published are chosen only when no point is
// known to meet it, and the row then says the target is unknown.
func choosePoint(stack Stack, query Query) (Point, string, string) {
	hasTarget := query.MaxTTFTP95Ms > 0 || query.MinPerUserOutputTokensPerSecond > 0
	met, unknown := []Point{}, []Point{}
	for _, point := range stack.Points {
		switch meets(point, query) {
		case TargetMet:
			met = append(met, point)
		case TargetUnknown:
			unknown = append(unknown, point)
		}
	}
	state := TargetNotGiven
	pool := met
	if hasTarget {
		state = TargetMet
		if len(pool) == 0 {
			pool, state = unknown, TargetUnknown
		}
	}
	if len(pool) == 0 {
		return Point{}, "", "Misses the latency target at every measured point."
	}
	best := pool[0]
	for _, point := range pool[1:] {
		if betterPoint(stack, query.Goal, point, best) {
			best = point
		}
	}
	return best, state, ""
}

func meets(point Point, query Query) string {
	state := TargetMet
	if query.MaxTTFTP95Ms > 0 {
		// Percentiles bound the p95 when it was not published: it is at least
		// the p90 and p50, and at most the p99. Only a bound decides; the mean
		// says nothing about the p95.
		lower, upper := ttftP95Bounds(point.TTFTMs)
		switch {
		case lower != nil && *lower > query.MaxTTFTP95Ms:
			return targetMissed
		case upper != nil && *upper <= query.MaxTTFTP95Ms:
		default:
			state = TargetUnknown
		}
	}
	if query.MinPerUserOutputTokensPerSecond > 0 {
		switch {
		case point.PerUserOutputTokensPerSecond == nil:
			state = TargetUnknown
		case *point.PerUserOutputTokensPerSecond < query.MinPerUserOutputTokensPerSecond:
			return targetMissed
		}
	}
	return state
}

// ttftP95Bounds returns the tightest published bounds on first-token p95.
// When the p95 itself is published both bounds are that value.
func ttftP95Bounds(ttft Percentiles) (lower, upper *float64) {
	if ttft.P95 != nil {
		return ttft.P95, ttft.P95
	}
	if ttft.P90 != nil {
		lower = ttft.P90
	} else if ttft.P50 != nil {
		lower = ttft.P50
	}
	return lower, ttft.P99
}

func betterPoint(stack Stack, goal string, candidate, current Point) bool {
	if goal == GoalLowestLatency {
		if candidate.TTFTMs.P95 != nil || current.TTFTMs.P95 != nil {
			switch {
			case candidate.TTFTMs.P95 == nil:
				return false
			case current.TTFTMs.P95 == nil:
				return true
			case *candidate.TTFTMs.P95 != *current.TTFTMs.P95:
				return *candidate.TTFTMs.P95 < *current.TTFTMs.P95
			}
		} else if candidate.PerUserOutputTokensPerSecond != nil && current.PerUserOutputTokensPerSecond != nil && *candidate.PerUserOutputTokensPerSecond != *current.PerUserOutputTokensPerSecond {
			return *candidate.PerUserOutputTokensPerSecond > *current.PerUserOutputTokensPerSecond
		}
		return candidate.Concurrency < current.Concurrency
	}
	_, a, _ := stack.Throughput(candidate)
	_, b, _ := stack.Throughput(current)
	if a != b {
		return a > b
	}
	return candidate.Concurrency < current.Concurrency
}

func cost(stack Stack, total float64, concurrency int, prices Prices) Cost {
	gpus := fmt.Sprintf("%d × %s", stack.GPUCount, stack.GPU)
	if prices == nil {
		return Cost{State: CostUnknown, Basis: "No GPU prices are configured, so cost stays unknown."}
	}
	quote, ok := prices(stack.GPU, stack.GPUCount)
	if !ok || quote.HourlyUSD <= 0 || math.IsNaN(quote.HourlyUSD) || math.IsInf(quote.HourlyUSD, 0) || total <= 0 {
		return Cost{State: CostUnknown, Basis: "No current complete price for " + gpus + ", so cost stays unknown."}
	}
	perMillion := round(quote.HourlyUSD/(total*3600)*1_000_000, 4)
	hourly := quote.HourlyUSD
	observed := quote.ObservedAt.UTC()
	return Cost{
		State: CostEstimated, PerMillionOutputTokensUSD: &perMillion, HourlyUSD: &hourly,
		Provider: quote.Provider, Region: quote.Region, PriceSource: quote.Source, PriceAuthority: quote.Authority, PriceObservedAt: &observed,
		Basis: fmt.Sprintf("Estimate: %s output tokens/s cited at concurrency %d, at $%.2f/h for %s from %s.", formatNumber(total), concurrency, hourly, gpus, quote.Provider),
	}
}

func less(goal string, a, b RankedStack) bool {
	if tier(a) != tier(b) {
		return tier(a) < tier(b)
	}
	if a.Stale != b.Stale {
		return !a.Stale
	}
	if order := compareGoal(goal, a, b); order != 0 {
		return order < 0
	}
	if kindOrder(a.Source.Kind) != kindOrder(b.Source.Kind) {
		return kindOrder(a.Source.Kind) < kindOrder(b.Source.Kind)
	}
	return a.ID < b.ID
}

func tier(row RankedStack) int {
	if row.Target == TargetUnknown {
		return 1
	}
	return 0
}

// compareGoal orders known values before unknown ones and then by the goal.
func compareGoal(goal string, a, b RankedStack) int {
	switch goal {
	case GoalLowestCost:
		if order := compareOptional(a.Cost.PerMillionOutputTokensUSD, b.Cost.PerMillionOutputTokensUSD, true); order != 0 {
			return order
		}
	case GoalLowestLatency:
		if order := compareOptional(a.Metrics.TTFTP95Ms, b.Metrics.TTFTP95Ms, true); order != 0 {
			return order
		}
		if order := compareOptional(a.Metrics.PerUserOutputTokensPerSecond, b.Metrics.PerUserOutputTokensPerSecond, false); order != 0 {
			return order
		}
	}
	switch {
	case a.Metrics.OutputTokensPerSecondPerGPU > b.Metrics.OutputTokensPerSecondPerGPU:
		return -1
	case a.Metrics.OutputTokensPerSecondPerGPU < b.Metrics.OutputTokensPerSecondPerGPU:
		return 1
	}
	return 0
}

func compareOptional(a, b *float64, ascending bool) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a == *b:
		return 0
	case (*a < *b) == ascending:
		return -1
	default:
		return 1
	}
}

func rankBasis(goal string, row RankedStack) string {
	switch {
	case goal == GoalLowestCost && row.Cost.PerMillionOutputTokensUSD != nil:
		return "cost_per_million_output_tokens"
	case goal == GoalLowestLatency && row.Metrics.TTFTP95Ms != nil:
		return "ttft_p95"
	case goal == GoalLowestLatency && row.Metrics.PerUserOutputTokensPerSecond != nil:
		return "per_user_output_tokens_per_second"
	default:
		return "output_tokens_per_second_per_gpu"
	}
}

func kindOrder(kind string) int {
	switch kind {
	case SourceMeasuredByInferCrane:
		return 0
	case SourceVendorRecipe:
		return 1
	default:
		return 2
	}
}

func rankedBy(query Query) string {
	targetText := ""
	if query.MaxTTFTP95Ms > 0 {
		targetText += fmt.Sprintf(" with first-token p95 at most %s ms", formatNumber(query.MaxTTFTP95Ms))
	}
	if query.MinPerUserOutputTokensPerSecond > 0 {
		if targetText != "" {
			targetText += " and"
		} else {
			targetText += " with"
		}
		targetText += fmt.Sprintf(" at least %s output tokens/s per user", formatNumber(query.MinPerUserOutputTokensPerSecond))
	}
	switch query.Goal {
	case GoalLowestLatency:
		return "Lowest first-token p95" + targetText + "; per-user speed where p95 was not published."
	case GoalHighestThroughput:
		return "Most output tokens per second per GPU" + targetText + "."
	default:
		return "Lowest estimated cost per 1M output tokens" + targetText + "; throughput per GPU where no price is known."
	}
}

func target(query Query) Target {
	var out Target
	if query.MaxTTFTP95Ms > 0 {
		value := query.MaxTTFTP95Ms
		out.MaxTTFTP95Ms = &value
	}
	if query.MinPerUserOutputTokensPerSecond > 0 {
		value := query.MinPerUserOutputTokensPerSecond
		out.MinPerUserOutputTokensPerSecond = &value
	}
	return out
}

// SourceDisplay is the one-line source label every row carries.
func SourceDisplay(source Source, measuredAt string) string {
	switch source.Kind {
	case SourcePublicBenchmark:
		return "Public benchmark · " + source.Label + " · " + measuredAt
	case SourceVendorRecipe:
		return "Vendor recipe · " + source.Label + " · " + measuredAt
	case SourceMeasuredByInferCrane:
		return "Measured by InferCrane · " + measuredAt
	}
	return source.Label + " · " + measuredAt
}

func sourceRef(source Source, stack Stack) SourceRef {
	return SourceRef{
		ID: source.ID, Kind: source.Kind, Label: source.Label, Display: SourceDisplay(source, stack.MeasuredAt), Publisher: source.Publisher, Title: source.Title,
		URL: source.URL, License: source.License, LicenseURL: source.LicenseURL, PublishedAt: source.PublishedAt, Ref: stack.SourceRef,
	}
}

// deployState reports whether a reviewed catalog profile launches the stack.
// The dataset test proves every reference matches its profile exactly.
func deployState(stack Stack) DeployState {
	if stack.Deploy == nil {
		return DeployState{State: DeployNotReviewed, Reason: "No reviewed launch configuration for this exact stack yet."}
	}
	entry, ok := curatedrecipe.Get(stack.Deploy.CatalogModel)
	if !ok || !slices.ContainsFunc(entry.Profiles, func(profile curatedrecipe.ServingProfile) bool { return profile.Name == stack.Deploy.Profile }) {
		return DeployState{State: DeployNotReviewed, Reason: "The reviewed launch configuration for this stack is no longer in the catalog."}
	}
	return DeployState{State: DeployReady, CatalogModel: stack.Deploy.CatalogModel, Profile: stack.Deploy.Profile}
}

var engineLabels = map[string]string{"vllm": "vLLM", "sglang": "SGLang", "trt-llm": "TensorRT-LLM", "dynamo-trt": "Dynamo + TensorRT-LLM", "dynamo-sglang": "Dynamo + SGLang", "dynamo-vllm": "Dynamo + vLLM"}

// Label names the stack for people: engine and version, GPUs, precision.
func (stack Stack) Label() string {
	engine := engineLabels[stack.Engine]
	if stack.EngineVersion != "" && stack.EngineVersion != "not stated" {
		engine += " " + stack.EngineVersion
	}
	return fmt.Sprintf("%s · %d × %s · %s", engine, stack.GPUCount, stack.GPU, strings.ToUpper(stack.Precision))
}

// sameGPU compares accelerator names without vendor prefixes or spacing.
func sameGPU(a, b string) bool {
	normalize := func(value string) string {
		value = strings.ToUpper(strings.TrimSpace(value))
		for _, prefix := range []string{"NVIDIA ", "AMD ", "INSTINCT "} {
			value = strings.TrimPrefix(value, prefix)
		}
		return strings.NewReplacer(" ", "", "-", "", "_", "").Replace(value)
	}
	return normalize(a) == normalize(b)
}

func round(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

func formatNumber(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%.0f", value)
	}
	return fmt.Sprintf("%.1f", value)
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return slices.Clone(values)
}
