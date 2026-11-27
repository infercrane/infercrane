// Package resultstore keeps every measured cell as a technique result record,
// so the planner can learn which setups won in cells like the one it is asked
// about. A record is one setup's measured outcome in one cell (one kind of
// traffic at one load on one GPU), carrying the technique contract's result
// (internal/technique: the five fields, reproduction, required evidence, and
// Claim) plus what the store needs to compare records: the model's
// content-free facts, the setup's knobs, the cell it was measured in, and the
// measured numbers themselves.
//
// The store holds numbers and shapes only, never prompts, answers, or request
// text. Every record belongs to one tenant and is read inside that tenant.
// Another tenant's record is used only by the prior, only when its owner set
// Sharing to SharingNumbers, and only as NumbersOnly reduces it; it is never
// returned by a read. docs/features/results-store.mdx states the rule.
package resultstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/technique"
)

// RecordSchemaV1 is one stored record.
const RecordSchemaV1 = "infercrane.dev/result-record/v1"

// BundleSchemaV1 is a file of records, as `infercrane results export` writes
// it and `infercrane results import` reads it.
const BundleSchemaV1 = "infercrane.dev/result-records/v1"

// Sharing says who may use a record's numbers.
const (
	// SharingPrivate is the default: the record is read and used by its own
	// tenant only.
	SharingPrivate = "private"
	// SharingNumbers lets the prior use the record's numbers, reduced by
	// NumbersOnly, when it ranks setups for other tenants. The record itself
	// is never returned to them.
	SharingNumbers = "numbers"
)

// Model architectures.
const (
	ArchitectureDense   = "dense"
	ArchitectureMoE     = "moe"
	ArchitectureHybrid  = "hybrid"
	ArchitectureUnknown = "unknown"
)

// Speculation methods a setup can serve with.
const (
	SpeculationNone    = "none"
	SpeculationMTP     = "mtp"
	SpeculationNgram   = "ngram"
	SpeculationDraft   = "draft-model"
	SpeculationEagle   = "eagle"
	SpeculationUnknown = "unknown"
)

// Bounds on what one record may hold. They keep a record to numbers and short
// names: anything longer is refused rather than stored.
const (
	maxName    = 128
	maxLabel   = 200
	maxFlags   = 32
	maxFlag    = 256
	maxFeature = 16
)

var (
	idPattern     = regexp.MustCompile(`^rr_[a-f0-9]{32}$`)
	cellPattern   = regexp.MustCompile(`^cell_[a-f0-9]{24}$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/+-]{0,127}$`)
)

// Record is one setup's measured outcome in one cell.
type Record struct {
	SchemaVersion string `json:"schema_version"`
	// ID is derived from the record's content (ContentID), so importing the
	// same record twice stores it once.
	ID string `json:"id"`
	// Sharing is SharingPrivate unless the tenant chose SharingNumbers.
	Sharing  string   `json:"sharing"`
	Model    Model    `json:"model"`
	Setup    Setup    `json:"setup"`
	Cell     Cell     `json:"cell"`
	Observed Observed `json:"observed"`
	// Result is the technique contract's record. Its Measured.Change is
	// against Result.Baseline.Setup in the same cell; Observed holds the
	// number itself.
	Result     technique.Result `json:"result"`
	Source     Source           `json:"source"`
	RecordedAt time.Time        `json:"recorded_at"`
	// Judgement is how the importer judged the record's claim beyond the
	// result's own fields: the technique manifest it was judged against and
	// the run's checks that did not pass. Without it a record makes no
	// claim (Claim).
	Judgement *Judgement `json:"judgement,omitempty"`
}

// Judgement is what a claim was judged against when the record was made.
type Judgement struct {
	// Manifest is the technique manifest the record's required evidence was
	// copied from: "id@version sha256:<digest of the manifest file>". Empty
	// when no manifest was given; the record then makes no claim.
	Manifest string `json:"manifest,omitempty"`
	// Failed names the run's checks that failed for this record, for
	// example "bar.copy-inside-noise". Unsettled names the ones the run could
	// not decide (the runner's "unknown"). The two are never mixed: an
	// unknown is not a failure, and a failure is not an unknown. Dry is a run
	// on stand-in servers. Any of them withholds the claim.
	Failed    []string `json:"failed,omitempty"`
	Unsettled []string `json:"unsettled,omitempty"`
	Dry       bool     `json:"dry,omitempty"`
	// Baseline marks a setup measured to compare with: the current setup,
	// the publisher's documented setup, a copy of the current setup. A
	// baseline is not a technique: it needs no manifest and makes no claim.
	Baseline bool `json:"baseline,omitempty"`
}

// ReasonBaseline is the one reason a baseline makes no claim.
const ReasonBaseline = "baseline: measured to compare with, makes no claim"

// ReasonDryRun is why a dry run's record makes no claim.
const ReasonDryRun = "dry run: stand-in servers, makes no claim"

// Baseline reports whether the record is a baseline: marked so when it was
// judged, or the starting setup of a planner-gym import.
func (r Record) Baseline() bool {
	return r.Judgement != nil && r.Judgement.Baseline || r.Result.Technique == TechniqueStartingSetup
}

// Claim decides with the technique contract's own rule, never a copy of it:
// Result.Claim (internal/technique) on the result, whose required evidence
// holds the manifest's evidence list as Manifest.ClaimOf adds it. The store
// only adds conditions on top: a baseline makes no claim, a record not
// judged against its technique's manifest makes none, and neither does one
// whose run failed a check. A change to the contract's rule reaches every
// stored record when it is read.
func (r Record) Claim() string {
	if r.Baseline() || r.Judgement == nil || r.Judgement.Manifest == "" || len(r.Judgement.Failed) > 0 || len(r.Judgement.Unsettled) > 0 || r.Judgement.Dry {
		return technique.ClaimNotShown
	}
	return r.Result.Claim()
}

// NotShownBecause explains a "not shown" claim; it decides nothing (Claim
// does). A baseline gives one reason. Otherwise: the fields and evidence the
// contract finds missing (Result.Unknown), no manifest, a dry run, the run's
// failed checks ("failed bar.X") and its unsettled ones ("unknown bar.X"),
// and, when the contract's rule withholds the claim for another reason, what
// the result measured.
func (r Record) NotShownBecause() []string {
	if r.Claim() == technique.ClaimShown {
		return nil
	}
	if r.Baseline() {
		return []string{ReasonBaseline}
	}
	var out []string
	unknown := r.Result.Unknown()
	for _, field := range unknown {
		out = append(out, "unknown "+field)
	}
	if r.Judgement == nil || r.Judgement.Manifest == "" {
		out = append(out, "no technique manifest")
	}
	if r.Judgement != nil {
		if r.Judgement.Dry {
			out = append(out, ReasonDryRun)
		}
		for _, failed := range r.Judgement.Failed {
			out = append(out, "failed "+failed)
		}
		for _, unsettled := range r.Judgement.Unsettled {
			out = append(out, "unknown "+unsettled)
		}
	}
	if len(unknown) == 0 && r.Result.Claim() != technique.ClaimShown {
		measured := r.Result.Measured
		out = append(out, fmt.Sprintf("no gain shown: %s, change %+.1f%% against noise %.1f%%, answers %s", measured.Decision, 100*measured.Change, 100*measured.NoiseFloor,
			firstNonEmpty(r.Result.Correctness.Verdict, "unknown")))
	}
	return out
}

// Model is what the prior compares models by. None of it is content.
type Model struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
	// Architecture is dense, moe, hybrid (attention with a recurrent state),
	// or unknown.
	Architecture string `json:"architecture"`
	// ParamsB is the parameter count in billions; ActiveParamsB the share one
	// token reads, for a mixture of experts. 0 is unknown.
	ParamsB       float64 `json:"params_b,omitempty"`
	ActiveParamsB float64 `json:"active_params_b,omitempty"`
	// Checkpoint is the precision of the published weights, for example bf16
	// or fp8; empty is unknown.
	Checkpoint string `json:"checkpoint,omitempty"`
	// Features are the planner's model features, for example native_mtp.
	Features []string `json:"features,omitempty"`
}

// Setup is the candidate as knobs the prior can match across models and
// GPUs. The engine and KV cache format are in Result.Software and
// Result.Precision.
type Setup struct {
	// Name is the run's own name for the setup, for example three-drafts.
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Speculation is none, mtp, ngram, draft-model, eagle, or unknown.
	Speculation string `json:"speculation"`
	// Drafts is the number of draft tokens; 0 without speculation or when the
	// method sets it itself.
	Drafts int `json:"drafts,omitempty"`
	// Quantization is the weight quantization the engine applies on load;
	// empty serves the checkpoint's own weights.
	Quantization string `json:"quantization,omitempty"`
	// KVCache is the KV cache format the setup asks the engine for ("auto"
	// for the engine's own); empty takes Result.Precision.KVCache.
	KVCache string `json:"kv_cache,omitempty"`
	// Flags are any other engine flags the setup sets, as "--name" or
	// "--name=value", sorted. Two setups with different flags are different
	// setups.
	Flags []string `json:"flags,omitempty"`
}

// Cell is where the record was measured. Records of one cell were measured
// in one run, against one baseline, and compare with each other.
type Cell struct {
	// ID is derived from the set, the key, the GPU, and the model (CellID).
	ID string `json:"id"`
	// Set names the measurement, for example map-hybrid.
	Set string `json:"set"`
	// Key is the traffic kind and load, for example short/32.
	Key string `json:"key"`
}

// Observed is the measured number on Result.Measured.Metric: the median over
// the runs or servers, and their range.
type Observed struct {
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	// Runs is how many servers or runs the range covers; 0 is unknown.
	Runs int `json:"runs"`
}

// Source is where an imported record came from.
type Source struct {
	// Kind is planner-gym, record, or campaign.
	Kind string `json:"kind"`
	// File is the source file's base name, never a path.
	File string `json:"file,omitempty"`
	// Digest is the source file's sha256.
	Digest   string `json:"digest,omitempty"`
	Importer string `json:"importer"`
}

// Knobs is a setup as the prior matches it: two records with equal Knobs
// served the same way, whatever their runs called them.
type Knobs struct {
	Engine       string `json:"engine"`
	Speculation  string `json:"speculation,omitempty"`
	Drafts       int    `json:"drafts,omitempty"`
	KVCache      string `json:"kv_cache,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	// Flags are the setup's other engine flags, sorted, joined by spaces.
	Flags string `json:"flags,omitempty"`
}

// Knobs returns the record's setup as the prior matches it.
func (r Record) Knobs() Knobs {
	kv := r.Setup.KVCache
	if kv == "" {
		kv = r.Result.Precision.KVCache
	}
	return Knobs{Engine: r.Result.Software.Engine, Speculation: r.Setup.Speculation, Drafts: r.Setup.Drafts, KVCache: kv,
		Quantization: r.Setup.Quantization, Flags: strings.Join(r.Setup.Flags, " ")}.Normal()
}

// Normal returns knobs in the one form the store compares: lowercase names,
// "none" for no speculation, "auto" for the engine's own KV cache format, and
// fp8_e4m3 written as fp8 (the format both engines mean by fp8).
func (k Knobs) Normal() Knobs {
	k.Engine = strings.ToLower(strings.TrimSpace(k.Engine))
	k.Speculation = normalSpeculation(k.Speculation)
	k.KVCache = normalKV(k.KVCache)
	k.Quantization = strings.ToLower(strings.TrimSpace(k.Quantization))
	if k.Speculation == SpeculationNone {
		k.Drafts = 0
	}
	return k
}

// Words says the knobs in a few words, for tables and explanations.
func (k Knobs) Words() string {
	parts := []string{k.Engine}
	switch k.Speculation {
	case SpeculationNone, "":
	case SpeculationMTP, SpeculationDraft, SpeculationEagle:
		parts = append(parts, fmt.Sprintf("%s %d drafts", k.Speculation, k.Drafts))
	default:
		parts = append(parts, k.Speculation)
	}
	if k.KVCache != "auto" {
		parts = append(parts, k.KVCache+" KV cache")
	}
	if k.Quantization != "" {
		parts = append(parts, k.Quantization+" weights")
	}
	if k.Flags != "" {
		parts = append(parts, k.Flags)
	}
	if len(parts) == 1 {
		parts = append(parts, "defaults")
	}
	return strings.Join(parts, ", ")
}

func normalSpeculation(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return SpeculationNone
	}
	return value
}

func normalKV(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "":
		return "auto"
	case "fp8_e4m3":
		return "fp8"
	}
	return value
}

// Kind is the traffic kind of the record's cell.
func (r Record) Kind() string { return r.Result.Workload.Kind }

// Load is requests at once in the record's cell.
func (r Record) Load() int { return r.Result.Workload.Load }

// Metric is what Observed counts.
func (r Record) Metric() string { return r.Result.Measured.Metric }

// Accelerator is the GPU the record was measured on.
func (r Record) Accelerator() string { return r.Result.Hardware.Accelerator }

// CellID derives a cell's ID from what makes it one measurement.
func CellID(set, key, accelerator, model string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{set, key, accelerator, model}, "\x00")))
	return "cell_" + hex.EncodeToString(sum[:12])
}

// ContentID derives a record's ID from its content: everything but the ID,
// the sharing choice, and the time it was recorded. The same measurement
// imported twice has one ID.
func (r Record) ContentID() (string, error) {
	r.ID, r.Sharing, r.RecordedAt = "", "", time.Time{}
	body, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "rr_" + hex.EncodeToString(sum[:16]), nil
}

// Seal fills the derived fields (schema, cell ID, record ID, sharing) and
// validates the record.
func (r Record) Seal() (Record, error) {
	r.SchemaVersion = RecordSchemaV1
	if r.Sharing == "" {
		r.Sharing = SharingPrivate
	}
	r.Cell.ID = CellID(r.Cell.Set, r.Cell.Key, r.Result.Hardware.Accelerator, r.Model.ID)
	r.Setup.Flags = sortedFlags(r.Setup.Flags)
	slices.Sort(r.Model.Features)
	id, err := r.ContentID()
	if err != nil {
		return Record{}, err
	}
	r.ID = id
	return r, r.Validate()
}

func sortedFlags(flags []string) []string {
	if len(flags) == 0 {
		return nil
	}
	out := slices.Clone(flags)
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate checks a record's shape and bounds. A valid record can still make
// no claim: Result.Claim says whether it shows a gain.
func (r Record) Validate() error {
	if r.SchemaVersion != RecordSchemaV1 {
		return fmt.Errorf("result record schema must be %q", RecordSchemaV1)
	}
	if !idPattern.MatchString(r.ID) {
		return errors.New("result record id must be rr_ and 32 lowercase hex digits")
	}
	if want, err := r.ContentID(); err != nil || want != r.ID {
		return fmt.Errorf("result record %s: id does not match its content", r.ID)
	}
	if r.Sharing != SharingPrivate && r.Sharing != SharingNumbers {
		return fmt.Errorf("result record sharing must be %q or %q", SharingPrivate, SharingNumbers)
	}
	if err := r.Result.Validate(); err != nil {
		return fmt.Errorf("result record %s: %w", r.ID, err)
	}
	if !namePattern.MatchString(r.Model.ID) || len(r.Model.Revision) > maxName {
		return errors.New("result record model needs an id of at most 128 characters without spaces")
	}
	switch r.Model.Architecture {
	case ArchitectureDense, ArchitectureMoE, ArchitectureHybrid, ArchitectureUnknown:
	default:
		return fmt.Errorf("result record model architecture %q is not dense, moe, hybrid, or unknown", r.Model.Architecture)
	}
	if !finite(r.Model.ParamsB, r.Model.ActiveParamsB) || r.Model.ParamsB < 0 || r.Model.ActiveParamsB < 0 || len(r.Model.Checkpoint) > 32 || len(r.Model.Features) > maxFeature {
		return errors.New("result record model facts are out of bounds")
	}
	for _, feature := range r.Model.Features {
		if !namePattern.MatchString(feature) {
			return fmt.Errorf("result record model feature %q is not a short name", feature)
		}
	}
	if !namePattern.MatchString(r.Setup.Name) || len(r.Setup.Label) > maxLabel || len(r.Setup.Quantization) > 32 || r.Setup.Drafts < 0 || r.Setup.Drafts > 64 {
		return errors.New("result record setup needs a short name, a label of at most 200 characters, and 0 to 64 drafts")
	}
	switch normalSpeculation(r.Setup.Speculation) {
	case SpeculationNone, SpeculationMTP, SpeculationNgram, SpeculationDraft, SpeculationEagle, SpeculationUnknown:
	default:
		return fmt.Errorf("result record speculation %q is not none, mtp, ngram, draft-model, eagle, or unknown", r.Setup.Speculation)
	}
	if len(r.Setup.Flags) > maxFlags {
		return fmt.Errorf("result record setup sets more than %d other flags", maxFlags)
	}
	for _, flag := range r.Setup.Flags {
		if !strings.HasPrefix(flag, "--") || len(flag) > maxFlag || strings.ContainsAny(flag, "\n\r\t") {
			return fmt.Errorf("result record flag %q is not one engine flag of at most %d characters", flag, maxFlag)
		}
	}
	if !cellPattern.MatchString(r.Cell.ID) || r.Cell.ID != CellID(r.Cell.Set, r.Cell.Key, r.Result.Hardware.Accelerator, r.Model.ID) {
		return errors.New("result record cell id does not match its set, key, GPU, and model")
	}
	if !namePattern.MatchString(r.Cell.Set) || !namePattern.MatchString(r.Cell.Key) {
		return errors.New("result record cell needs a set and a key of at most 128 characters without spaces")
	}
	o := r.Observed
	if !finite(o.Median, o.Min, o.Max) || o.Min < 0 || o.Min > o.Median || o.Median > o.Max || o.Runs < 0 {
		return errors.New("result record observed numbers must be finite, not negative, and min <= median <= max")
	}
	if r.Source.Kind == "" || len(r.Source.Kind) > 32 || len(r.Source.File) > maxName || strings.ContainsAny(r.Source.File, `/\`) || len(r.Source.Importer) > 64 {
		return errors.New("result record source names its kind, a file name without a path, and the importer")
	}
	if r.Source.Digest != "" && !digestPattern.MatchString(r.Source.Digest) {
		return errors.New("result record source digest must be sha256:<64 hex>")
	}
	if r.RecordedAt.IsZero() {
		return errors.New("result record needs the time it was recorded")
	}
	if len(r.Setup.KVCache) > 32 {
		return errors.New("result record setup KV cache format is longer than 32 characters")
	}
	if j := r.Judgement; j != nil {
		if j.Baseline && j.Manifest != "" {
			return errors.New("result record judgement gives a baseline a manifest; a baseline makes no claim")
		}
		if len(j.Manifest) > 200 || len(j.Failed) > maxFlags || len(j.Unsettled) > maxFlags {
			return errors.New("result record judgement is out of bounds")
		}
		for _, check := range append(slices.Clone(j.Failed), j.Unsettled...) {
			if !namePattern.MatchString(check) {
				return fmt.Errorf("result record judgement names %q, not a short name", check)
			}
		}
		for _, check := range j.Failed {
			if slices.Contains(j.Unsettled, check) {
				return fmt.Errorf("result record judgement names %q both failed and unknown", check)
			}
		}
	}
	return nil
}

func finite(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

// NumbersOnly is a record reduced to what another tenant's prior may use:
// the model's facts without its name, the GPU, the engine and its version,
// the traffic kind and load, the setup's knobs, the measured numbers and the
// decision. Names, labels, commands, digests, sources, evidence, and the
// record's and cell's IDs are dropped; the cell is kept as an opaque
// grouping so records of one measurement still compare with each other.
func (r Record) NumbersOnly() Record {
	cell := sha256.Sum256([]byte("shared\x00" + r.Cell.ID))
	out := Record{
		SchemaVersion: r.SchemaVersion,
		Sharing:       SharingNumbers,
		Model: Model{Architecture: r.Model.Architecture, ParamsB: r.Model.ParamsB, ActiveParamsB: r.Model.ActiveParamsB,
			Checkpoint: r.Model.Checkpoint, Features: slices.Clone(r.Model.Features)},
		Setup:    Setup{Speculation: r.Setup.Speculation, Drafts: r.Setup.Drafts, Quantization: r.Setup.Quantization, KVCache: r.Setup.KVCache, Flags: slices.Clone(r.Setup.Flags)},
		Cell:     Cell{ID: "shared_" + hex.EncodeToString(cell[:12])},
		Observed: r.Observed,
		Result: technique.Result{
			SchemaVersion: r.Result.SchemaVersion,
			Hardware:      technique.Hardware{Accelerator: r.Result.Hardware.Accelerator, Count: r.Result.Hardware.Count},
			Software:      technique.Software{Engine: r.Result.Software.Engine, EngineVersion: r.Result.Software.EngineVersion},
			Workload:      technique.Workload{Kind: r.Result.Workload.Kind, Load: r.Result.Workload.Load},
			Precision:     r.Result.Precision,
			Measured:      r.Result.Measured,
		},
	}
	return out
}

// Shared reports whether a record is another tenant's numbers.
func (r Record) Shared() bool { return strings.HasPrefix(r.Cell.ID, "shared_") }

// CellWords names a record's cell for a person: its set and key, or, for
// another tenant's numbers, only what NumbersOnly keeps.
func (r Record) CellWords() string {
	if r.Shared() {
		return fmt.Sprintf("a shared cell (%s, %s %s model, %s/%s)", r.Accelerator(), r.Model.Architecture, paramsWords(r.Model.ParamsB), r.Kind(), strconv.Itoa(r.Load()))
	}
	return r.Cell.Set + " " + r.Cell.Key
}

func paramsWords(params float64) string {
	if params <= 0 {
		return "unknown-size"
	}
	return strconv.FormatFloat(params, 'f', -1, 64) + "B"
}
