package resultstore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/technique"
)

// ImporterVersion names this importer in every record it makes.
const ImporterVersion = "resultstore-import/v1"

// Metrics the importer records. Useful output names its first-token limit.
const (
	MetricOutput       = "output_tokens_per_second"
	MetricUsefulOutput = "useful_output_tokens_per_second"
)

// UsefulOutputMetric names useful output under a first-token limit.
func UsefulOutputMetric(limitMS float64) string {
	return fmt.Sprintf("%s.ttft_%.0fms", MetricUsefulOutput, limitMS)
}

// Technique IDs for imported setups that no registered technique made. Their
// version is UnregisteredVersion, so no manifest's ClaimOf accepts them.
const (
	TechniqueStartingSetup = "starting-setup"
	UnregisteredVersion    = "0.0.0"
)

// maxImportBytes bounds one imported file.
const maxImportBytes = 32 << 20

// ImportOptions are facts a file does not carry. Anything left empty stays
// unknown in the records, and a record missing one of the five fields makes
// no claim.
type ImportOptions struct {
	// Model facts: merged with the file's model id, revision, and features.
	Model Model
	// Set names the measurement; the file's own set or its base name when
	// empty.
	Set string
	// Basis is how arms were placed (same-machine, separate-servers,
	// fresh-servers); the file's own when empty.
	Basis       string
	Commit      string
	ImageDigest string
	Driver      string
	RecordedAt  time.Time
	// Sharing is SharingPrivate unless the tenant chose SharingNumbers.
	Sharing string
	// Manifests judge a benchmark run's records (ImportRun): a record whose
	// technique and version have one here can make a claim.
	Manifests []LoadedManifest
}

// Import reads one file: a record bundle, JSON Lines of records, one record,
// or a planner gym cases file (infercrane.planner-gym/v1), and returns sealed
// records. Records read from a bundle keep their content; their sharing is
// set from the options, never from the file.
func Import(name string, body []byte, options ImportOptions) ([]Record, error) {
	if len(body) > maxImportBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", name, maxImportBytes)
	}
	if options.Sharing == "" {
		options.Sharing = SharingPrivate
	}
	if options.Sharing != SharingPrivate && options.Sharing != SharingNumbers {
		return nil, fmt.Errorf("sharing must be %q or %q", SharingPrivate, SharingNumbers)
	}
	var head struct {
		Schema        string `json:"schema"`
		SchemaVersion string `json:"schema_version"`
	}
	trimmed := bytes.TrimSpace(body)
	if err := json.Unmarshal(trimmed, &head); err != nil {
		// Not one JSON value: JSON Lines of records.
		return importLines(name, trimmed, options)
	}
	switch {
	case head.Schema == "infercrane.planner-gym/v1":
		return importGym(name, trimmed, options)
	case head.SchemaVersion == BundleSchemaV1:
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		var bundle struct {
			SchemaVersion string   `json:"schema_version"`
			Records       []Record `json:"records"`
		}
		if err := decoder.Decode(&bundle); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return reshare(name, bundle.Records, options.Sharing)
	case head.SchemaVersion == RecordSchemaV1:
		return importLines(name, trimmed, options)
	}
	return nil, fmt.Errorf("%s: not a result record, a record bundle (%s), or a planner gym cases file", name, BundleSchemaV1)
}

func importLines(name string, body []byte, options ImportOptions) ([]Record, error) {
	var records []Record
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), maxLocalLine)
	for line := 1; scanner.Scan(); line++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		record, err := decodeRecord(text)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", name, line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return reshare(name, records, options.Sharing)
}

func reshare(name string, records []Record, sharing string) ([]Record, error) {
	for index := range records {
		if err := records[index].Validate(); err != nil {
			return nil, fmt.Errorf("%s record %d: %w", name, index+1, err)
		}
		records[index].Sharing = sharing
	}
	return records, nil
}

// The planner gym's cases file (tools/planner-gym/build_cases.py), with the
// optional per-setup engine, speculation and image fields a converter can add
// for runs of several engines (testdata/synthetic/two-engines-hybrid.json).
type gymFile struct {
	Schema        string `json:"schema"`
	Title         string `json:"title"`
	Set           string `json:"set"`
	Basis         string `json:"basis"`
	RecordedAt    string `json:"recorded_at"`
	Commit        string `json:"commit"`
	Driver        string `json:"driver"`
	GPUCount      int    `json:"gpu_count"`
	CorrectMethod string `json:"correctness_method"`
	CorrectResult string `json:"correctness_verdict"`
	Model         struct {
		ID       string   `json:"id"`
		Revision string   `json:"revision"`
		Features []string `json:"features"`
	} `json:"model"`
	ModelFacts        *Model                  `json:"model_facts"`
	GPU               string                  `json:"gpu"`
	Engine            string                  `json:"engine"`
	EngineVersion     string                  `json:"engine_version"`
	FirstTokenLimitMS float64                 `json:"first_token_limit_ms"`
	Setups            map[string]gymSetup     `json:"setups"`
	Cells             []gymCell               `json:"cells"`
	CurrentSetups     []string                `json:"current_setups"`
	Baseline          string                  `json:"baseline"`
	ProductCases      []json.RawMessage       `json:"product_cases"`
	BuiltFrom         []struct{ File string } `json:"built_from"`
}

type gymSetup struct {
	Label         string   `json:"label"`
	Drafts        int      `json:"drafts"`
	KVCacheDtype  string   `json:"kv_cache_dtype"`
	Quantization  string   `json:"quantization"`
	Ngram         bool     `json:"ngram"`
	Speculation   string   `json:"speculation"`
	Engine        string   `json:"engine"`
	EngineVersion string   `json:"engine_version"`
	ImageDigest   string   `json:"image_digest"`
	Flags         []string `json:"flags"`
}

type gymRange struct {
	Median  float64 `json:"median"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Servers int     `json:"servers"`
}

type gymMeasured struct {
	Output       *gymRange          `json:"output"`
	UsefulOutput *gymRange          `json:"useful_output"`
	Window       map[string]float64 `json:"window"`
}

type gymCell struct {
	Key            string                 `json:"key"`
	Kind           string                 `json:"kind"`
	RequestsAtOnce int                    `json:"requests_at_once"`
	Reasoning      string                 `json:"reasoning"`
	Measured       map[string]gymMeasured `json:"measured"`
}

// importGym turns each measured setup of each cell into two records, one per
// metric (all output, and output within the file's first-token limit). The
// baseline of a cell is the file's baseline, else its first current setup,
// else "plain", when measured there.
func importGym(name string, body []byte, options ImportOptions) ([]Record, error) {
	var file gymFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if file.GPU == "" || file.Engine == "" || file.Model.ID == "" || len(file.Cells) == 0 {
		return nil, fmt.Errorf("%s: a planner gym file needs its model, GPU, engine, and cells", name)
	}
	digest := sha256.Sum256(body)
	source := Source{Kind: "planner-gym", File: filepath.Base(name), Digest: "sha256:" + hex.EncodeToString(digest[:]), Importer: ImporterVersion}
	set := firstNonEmpty(options.Set, file.Set, strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
	basis := firstNonEmpty(options.Basis, file.Basis)
	if basis == "" {
		basis = "separate-servers"
		if len(file.CurrentSetups) > 0 {
			basis = "same-machine"
		}
	}
	recordedAt := options.RecordedAt
	if recordedAt.IsZero() && file.RecordedAt != "" {
		parsed, err := time.Parse(time.RFC3339, file.RecordedAt)
		if err != nil {
			return nil, fmt.Errorf("%s: recorded_at: %w", name, err)
		}
		recordedAt = parsed
	}
	if recordedAt.IsZero() {
		return nil, fmt.Errorf("%s: say when it was measured (recorded_at)", name)
	}
	model := options.Model
	if file.ModelFacts != nil && model.Architecture == "" {
		model = *file.ModelFacts
	}
	model.ID, model.Revision = file.Model.ID, firstNonEmpty(file.Model.Revision, model.Revision)
	model.Features = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(model.Features), file.Model.Features...))))
	if model.Architecture == "" {
		model.Architecture = ArchitectureUnknown
	}
	gpuCount := max(file.GPUCount, 1)
	columns := []struct {
		metric string
		value  func(gymMeasured) *gymRange
	}{
		{MetricOutput, func(m gymMeasured) *gymRange { return m.Output }},
	}
	if file.FirstTokenLimitMS > 0 {
		columns = append(columns, struct {
			metric string
			value  func(gymMeasured) *gymRange
		}{UsefulOutputMetric(file.FirstTokenLimitMS), func(m gymMeasured) *gymRange { return m.UsefulOutput }})
	}
	var records []Record
	for _, cell := range file.Cells {
		baseline := gymBaseline(file, cell)
		if baseline == "" {
			continue
		}
		kind := cell.Kind
		if strings.EqualFold(cell.Reasoning, "on") {
			kind += "-reasoning"
		}
		names := make([]string, 0, len(cell.Measured))
		for setupName := range cell.Measured {
			names = append(names, setupName)
		}
		sort.Strings(names)
		for _, column := range columns {
			base := column.value(cell.Measured[baseline])
			for _, setupName := range names {
				measured := cell.Measured[setupName]
				value := column.value(measured)
				if value == nil {
					continue
				}
				setup, ok := file.Setups[setupName]
				if !ok {
					return nil, fmt.Errorf("%s: cell %s measured setup %q that the file does not describe", name, cell.Key, setupName)
				}
				record := gymRecord(file, model, setupName, setup, file.Setups[baseline], baseline, value, base)
				record.Cell = Cell{Set: set, Key: cell.Key}
				record.Source = source
				record.RecordedAt = recordedAt.UTC()
				record.Sharing = options.Sharing
				record.Result.Hardware = technique.Hardware{Accelerator: file.GPU, Count: gpuCount, Driver: firstNonEmpty(options.Driver, file.Driver)}
				record.Result.Workload = technique.Workload{Kind: kind, Load: cell.RequestsAtOnce, Requests: int(measured.Window["requests"])}
				record.Result.Baseline = technique.Baseline{Setup: baseline, Basis: basis}
				record.Result.Correctness = technique.Correctness{Method: firstNonEmpty(file.CorrectMethod, "none"), Verdict: firstNonEmpty(file.CorrectResult, technique.DecisionUnknown)}
				record.Result.Measured.Metric = column.metric
				image := firstNonEmpty(setup.ImageDigest, options.ImageDigest)
				record.Result.Software.ImageDigest = image
				record.Result.Reproduction = technique.Reproduction{Commit: firstNonEmpty(options.Commit, file.Commit), ImageDigest: image}
				sealed, err := record.Seal()
				if err != nil {
					return nil, fmt.Errorf("%s: cell %s setup %s: %w", name, cell.Key, setupName, err)
				}
				records = append(records, sealed)
			}
		}
	}
	return records, nil
}

func gymBaseline(file gymFile, cell gymCell) string {
	candidates := []string{file.Baseline}
	candidates = append(candidates, file.CurrentSetups...)
	candidates = append(candidates, "plain")
	for _, name := range candidates {
		if _, ok := cell.Measured[name]; name != "" && ok {
			return name
		}
	}
	return ""
}

func gymRecord(file gymFile, model Model, name string, setup, baselineSetup gymSetup, baseline string, value, base *gymRange) Record {
	knobs := gymKnobs(file, setup)
	baseKnobs := gymKnobs(file, baselineSetup)
	weights := firstNonEmpty(setup.Quantization, model.Checkpoint, "checkpoint")
	record := Record{
		Model:    model,
		Setup:    Setup{Name: name, Label: truncate(setup.Label, maxLabel), Speculation: knobs.Speculation, Drafts: knobs.Drafts, Quantization: setup.Quantization, Flags: setup.Flags},
		Observed: Observed{Median: value.Median, Min: value.Min, Max: value.Max, Runs: value.Servers},
		Result: technique.Result{
			SchemaVersion: technique.ResultSchemaV1, Technique: techniqueOf(knobs, baseKnobs, name == baseline), TechniqueVersion: UnregisteredVersion, Candidate: name,
			Software:  technique.Software{Engine: knobs.Engine, EngineVersion: firstNonEmpty(setup.EngineVersion, file.EngineVersion)},
			Precision: technique.Precision{Weights: weights, KVCache: knobs.KVCache},
		},
	}
	record.Result.Measured.Change, record.Result.Measured.NoiseFloor, record.Result.Measured.Decision = compare(value, base)
	return record
}

func gymKnobs(file gymFile, setup gymSetup) Knobs {
	speculation := normalSpeculation(setup.Speculation)
	switch {
	case setup.Speculation != "":
	case setup.Ngram:
		speculation = SpeculationNgram
	case setup.Drafts > 0:
		speculation = SpeculationMTP
	}
	return Knobs{Engine: firstNonEmpty(setup.Engine, file.Engine), Speculation: speculation, Drafts: setup.Drafts,
		KVCache: setup.KVCacheDtype, Quantization: setup.Quantization, Flags: strings.Join(sortedFlags(setup.Flags), " ")}.Normal()
}

// techniqueOf names what a setup changes from its cell's baseline.
func techniqueOf(knobs, base Knobs, isBaseline bool) string {
	if isBaseline || knobs == base {
		return TechniqueStartingSetup
	}
	var parts []string
	if knobs.Engine != base.Engine {
		parts = append(parts, "engine")
	}
	if knobs.Speculation != base.Speculation || knobs.Drafts != base.Drafts {
		parts = append(parts, "speculation")
	}
	if knobs.KVCache != base.KVCache {
		parts = append(parts, "kv-cache-precision")
	}
	if knobs.Quantization != base.Quantization {
		parts = append(parts, "weight-precision")
	}
	if knobs.Flags != base.Flags {
		parts = append(parts, "engine-flags")
	}
	return strings.Join(parts, "-and-")
}

// compare states a setup's median against its baseline's: the relative
// change, the noise it had to clear (the larger of the two setups' measured
// ranges, relative to the baseline), and the decision. Without a measured
// range or a baseline above zero the decision is unknown.
func compare(value, base *gymRange) (float64, float64, string) {
	if base == nil || base.Median <= 0 {
		return 0, 0, technique.DecisionUnknown
	}
	change := round6(value.Median/base.Median - 1)
	noise := round6(math.Max(value.Max-value.Min, base.Max-base.Min) / base.Median)
	switch {
	case noise <= 0:
		return change, 0, technique.DecisionUnknown
	case change > noise:
		return change, noise, technique.DecisionImproved
	case change < -noise:
		return change, noise, technique.DecisionWorse
	}
	return change, noise, technique.DecisionSame
}

func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Bundle is a file of records.
type Bundle struct {
	SchemaVersion string   `json:"schema_version"`
	Records       []Record `json:"records"`
}

// NewBundle wraps records for export.
func NewBundle(records []Record) Bundle {
	return Bundle{SchemaVersion: BundleSchemaV1, Records: records}
}
