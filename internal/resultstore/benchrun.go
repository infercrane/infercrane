package resultstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/technique"
)

// BenchmarkRunSchemaV1 is the benchmark runner's run file (rung.json,
// tools/benchmark on feat/benchmark-fanout).
const BenchmarkRunSchemaV1 = "infercrane.dev/benchmark-run/v1"

// LoadedManifest is a technique manifest and the digest of the file it was
// read from, so a judged record names exactly what judged it.
type LoadedManifest struct {
	Manifest technique.Manifest
	Digest   string
}

// LoadManifest reads a technique manifest (infercrane.dev/technique/v1) or a
// static technique file (infercrane.dev/technique-static/v1).
func LoadManifest(body []byte) (LoadedManifest, error) {
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return LoadedManifest{}, err
	}
	if head.SchemaVersion == technique.StaticSchemaV1 {
		loaded, err := technique.LoadStatic(body)
		if err != nil {
			return LoadedManifest{}, err
		}
		return LoadedManifest{Manifest: loaded.Manifest(), Digest: digest}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var manifest technique.Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return LoadedManifest{}, fmt.Errorf("decode technique manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return LoadedManifest{}, err
	}
	return LoadedManifest{Manifest: manifest, Digest: digest}, nil
}

// The parts of the runner's files the importer reads.
type benchRung struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	Dry    bool   `json:"dry"`
	Commit string `json:"commit"`
	Spec   struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	} `json:"spec"`
	Cells map[string]struct {
		State string            `json:"state"`
		Bars  map[string]string `json:"bars"`
	} `json:"cells"`
	Summaries []struct {
		Cell              string  `json:"cell"`
		FirstTokenLimitMS float64 `json:"first_token_limit_ms"`
		Current           string  `json:"current"`
		Arms              map[string]struct {
			UsefulOutput *float64 `json:"useful_output"`
			Error        *string  `json:"error"`
		} `json:"arms"`
		CurrentUseful struct {
			Runs []float64 `json:"runs"`
			Mean *float64  `json:"mean"`
		} `json:"current_useful_output"`
		NoiseFloor *float64 `json:"noise_floor"`
	} `json:"summaries"`
}

type benchScreen struct {
	Benchmark struct {
		Cell struct {
			ID string `json:"id"`
		} `json:"cell"`
		Model struct {
			ID        string `json:"id"`
			Revision  string `json:"revision"`
			Name      string `json:"name"`
			Precision struct {
				Weights string `json:"weights"`
			} `json:"precision"`
		} `json:"model"`
		Engine struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Image   string `json:"image"`
		} `json:"engine"`
		Arms []struct {
			SetupID string   `json:"setup_id"`
			Kind    string   `json:"kind"`
			Label   string   `json:"label"`
			Args    []string `json:"args"`
		} `json:"arms"`
	} `json:"benchmark"`
}

type benchPlan struct {
	Spec struct {
		Models []struct {
			Name    string `json:"name"`
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Weights struct {
				Bytes float64 `json:"bytes"`
			} `json:"weights"`
			Precision struct {
				Weights string `json:"weights"`
			} `json:"precision"`
		} `json:"models"`
	} `json:"spec"`
}

// ImportRun reads one benchmark runner run directory: results.jsonl (one
// technique result per measured arm and cell, kept as written, except that a
// bare useful-output metric gets its cell's first-token limit from rung.json,
// as newer runs write it themselves), rung.json
// (each cell's numbers and bars), plan.json (the spec's model facts),
// run.json (when it ended) and cells/*/screen.json (each arm's flags). It adds
// one record per cell for the current setup, which the runner compares every
// arm with and writes no result for.
//
// Each record is judged (Record.Judgement). A baseline arm (the runner's
// kind "baseline": the publisher's setup, the copy) and the current setup are
// marked baselines: no manifest, no claim. Any other arm is judged against the
// manifest of its technique and version when options.Manifests has one: its
// evidence list is copied into the result's required evidence, and the
// result's own rule must then decide as Manifest.ClaimOf does. Every record
// carries the run's checks apart: failed ones, ones the run could not settle
// (its "unknown"), and whether it was dry (benchRung.checks). A technique
// without a manifest makes no claim.
func ImportRun(dir string, options ImportOptions) ([]Record, error) {
	read := func(name string) ([]byte, error) {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && len(body) > maxImportBytes {
			return nil, fmt.Errorf("%s: larger than %d bytes", name, maxImportBytes)
		}
		return body, err
	}
	rungBody, err := read("rung.json")
	if err != nil {
		return nil, err
	}
	var rung benchRung
	if err = json.Unmarshal(rungBody, &rung); err != nil || rung.Schema != BenchmarkRunSchemaV1 {
		return nil, fmt.Errorf("%s: not a benchmark run (%s): %v", dir, BenchmarkRunSchemaV1, err)
	}
	resultsBody, err := read("results.jsonl")
	if err != nil {
		return nil, err
	}
	var plan benchPlan
	if body, err := read("plan.json"); err == nil {
		_ = json.Unmarshal(body, &plan)
	}
	recordedAt := options.RecordedAt
	if recordedAt.IsZero() {
		var run struct {
			EndedAt time.Time `json:"ended_at"`
		}
		if body, err := read("run.json"); err == nil && json.Unmarshal(body, &run) == nil {
			recordedAt = run.EndedAt
		}
	}
	if recordedAt.IsZero() {
		return nil, fmt.Errorf("%s: run.json says no end time; pass --recorded-at", dir)
	}
	if options.Sharing == "" {
		options.Sharing = SharingPrivate
	}
	screens, err := readScreens(filepath.Join(dir, "cells"))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(resultsBody)
	source := Source{Kind: "benchmark-run", File: "results.jsonl", Digest: "sha256:" + hex.EncodeToString(sum[:]), Importer: ImporterVersion}
	set := firstNonEmpty(options.Set, rung.RunID)
	manifests := map[string]LoadedManifest{}
	for _, loaded := range options.Manifests {
		manifests[loaded.Manifest.ID+"@"+loaded.Manifest.Version] = loaded
	}

	// The runner's own records, kept as written, in file order.
	var results []technique.Result
	decoder := json.NewDecoder(bytes.NewReader(resultsBody))
	decoder.DisallowUnknownFields()
	for decoder.More() {
		var result technique.Result
		if err = decoder.Decode(&result); err != nil {
			return nil, fmt.Errorf("%s/results.jsonl record %d: %w", dir, len(results)+1, err)
		}
		if err = result.Validate(); err != nil {
			return nil, fmt.Errorf("%s/results.jsonl record %d: %w", dir, len(results)+1, err)
		}
		results = append(results, result)
	}

	var records []Record
	seenCurrent := map[string]bool{}
	for index, result := range results {
		cellID := cellOfCommand(result.Reproduction.Command, result.Workload.Kind, result.Workload.Load, screens)
		screen, ok := screens[cellID]
		if !ok {
			return nil, fmt.Errorf("%s/results.jsonl record %d: no cells/*/screen.json for %s/%d", dir, index+1, result.Workload.Kind, result.Workload.Load)
		}
		summary := rung.summary(cellID)
		if summary == nil {
			return nil, fmt.Errorf("%s/rung.json: no summary for cell %s", dir, cellID)
		}
		arm, ok := summary.Arms[result.Candidate]
		if !ok || arm.UsefulOutput == nil {
			return nil, fmt.Errorf("%s/rung.json: cell %s has no number for %s", dir, cellID, result.Candidate)
		}
		// Runs before the runner named the first-token limit in the metric
		// counted useful output under each cell's own limit (1 s for short
		// chat, 3 s for long prompts). Name it, as the runner now does, so
		// useful output under different limits never shares one metric.
		if result.Measured.Metric == MetricUsefulOutput && summary.FirstTokenLimitMS > 0 {
			result.Measured.Metric = UsefulOutputMetric(summary.FirstTokenLimitMS)
		}
		model := benchModel(screen, plan, options.Model)
		engine := screen.Benchmark.Engine
		setup := SetupFromArgs(result.Candidate, screen.armLabel(result.Candidate), engine.Name, screen.armArgs(result.Candidate))
		failed, unsettled, dry := rung.checks(cellID)
		judgement := &Judgement{Failed: failed, Unsettled: unsettled, Dry: dry, Baseline: screen.armKind(result.Candidate) == "baseline"}
		if loaded, ok := manifests[result.Technique+"@"+result.TechniqueVersion]; ok && !judgement.Baseline {
			judgement.Manifest = fmt.Sprintf("%s@%s %s", loaded.Manifest.ID, loaded.Manifest.Version, loaded.Digest)
			judged := loaded.Manifest.ClaimOf(result)
			for _, name := range loaded.Manifest.Evidence {
				if !slices.Contains(result.RequiredEvidence, name) {
					result.RequiredEvidence = append(result.RequiredEvidence, name)
				}
			}
			// The copied evidence must make the result's own rule say what the
			// manifest's says, so reading the record later decides the same.
			if result.Claim() != judged {
				return nil, fmt.Errorf("%s/results.jsonl record %d: the manifest judges %q and the result with its evidence %q", dir, index+1, judged, result.Claim())
			}
		}
		record := Record{Model: model, Setup: setup, Cell: Cell{Set: set, Key: cellKey(cellID)},
			Observed: Observed{Median: *arm.UsefulOutput, Min: *arm.UsefulOutput, Max: *arm.UsefulOutput, Runs: 1},
			Result:   result, Source: source, RecordedAt: recordedAt.UTC(), Sharing: options.Sharing, Judgement: judgement}
		sealed, err := record.Seal()
		if err != nil {
			return nil, fmt.Errorf("%s/results.jsonl record %d: %w", dir, index+1, err)
		}
		records = append(records, sealed)

		// The current setup's own record, once per cell, from the runner's
		// two runs of it (first and last).
		if seenCurrent[cellID] || summary.CurrentUseful.Mean == nil || len(summary.CurrentUseful.Runs) == 0 {
			continue
		}
		seenCurrent[cellID] = true
		runs := summary.CurrentUseful.Runs
		low, high := slices.Min(runs), slices.Max(runs)
		mean := *summary.CurrentUseful.Mean
		current := technique.Result{SchemaVersion: technique.ResultSchemaV1, Technique: TechniqueStartingSetup, TechniqueVersion: UnregisteredVersion, Candidate: summary.Current,
			Hardware: result.Hardware, Software: result.Software, Workload: result.Workload, Baseline: result.Baseline, Correctness: technique.Correctness{Method: result.Correctness.Method, Verdict: "baseline"},
			Precision:    technique.Precision{Weights: model.Checkpoint, KVCache: result.Precision.KVCache},
			Measured:     technique.Measured{Metric: result.Measured.Metric, Change: 0, NoiseFloor: result.Measured.NoiseFloor, Decision: technique.DecisionSame},
			Reproduction: result.Reproduction}
		if model.Checkpoint == "" {
			current.Precision.Weights = result.Precision.Weights
		}
		current.Workload.Requests = 0
		base := Record{Model: model, Setup: SetupFromArgs(summary.Current, screen.armLabel(summary.Current), engine.Name, screen.armArgs(summary.Current)),
			Cell: Cell{Set: set, Key: cellKey(cellID)}, Observed: Observed{Median: mean, Min: math64(low, mean), Max: math64high(high, mean), Runs: len(runs)},
			Result: current, Source: source, RecordedAt: recordedAt.UTC(), Sharing: options.Sharing, Judgement: &Judgement{Failed: failed, Unsettled: unsettled, Dry: dry, Baseline: true}}
		sealed, err = base.Seal()
		if err != nil {
			return nil, fmt.Errorf("%s: current setup of %s: %w", dir, cellID, err)
		}
		records = append(records, sealed)
	}
	return records, nil
}

// math64 and math64high keep the current setup's range around its mean
// (the runner's mean of two runs lies between them; rounding can move it).
func math64(low, mean float64) float64 {
	if low > mean {
		return mean
	}
	return low
}

func math64high(high, mean float64) float64 {
	if high < mean {
		return mean
	}
	return high
}

func readScreens(dir string) (map[string]benchScreen, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the run's cells: %w", err)
	}
	screens := map[string]benchScreen{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name(), "screen.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var screen benchScreen
		if err = json.Unmarshal(body, &screen); err != nil {
			return nil, fmt.Errorf("%s/screen.json: %w", entry.Name(), err)
		}
		if id := screen.Benchmark.Cell.ID; id != "" {
			screens[id] = screen
		}
	}
	return screens, nil
}

// cellOfCommand finds a record's cell: the runner's command names it with
// --cells; otherwise the one screen of that kind and load.
func cellOfCommand(command, kind string, load int, screens map[string]benchScreen) string {
	fields := strings.Fields(command)
	for index, field := range fields {
		if field == "--cells" && index+1 < len(fields) {
			if _, ok := screens[fields[index+1]]; ok {
				return fields[index+1]
			}
		}
	}
	suffix := fmt.Sprintf("/%s/%d", kind, load)
	var found []string
	for id := range screens {
		if strings.HasSuffix(id, suffix) {
			found = append(found, id)
		}
	}
	sort.Strings(found)
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// cellKey is the cell's traffic kind and load ("long/16" of
// "example-8b/long/16").
func cellKey(cellID string) string {
	if _, rest, ok := strings.Cut(cellID, "/"); ok {
		return rest
	}
	return cellID
}

func (r benchRung) summary(cellID string) *struct {
	Cell              string  `json:"cell"`
	FirstTokenLimitMS float64 `json:"first_token_limit_ms"`
	Current           string  `json:"current"`
	Arms              map[string]struct {
		UsefulOutput *float64 `json:"useful_output"`
		Error        *string  `json:"error"`
	} `json:"arms"`
	CurrentUseful struct {
		Runs []float64 `json:"runs"`
		Mean *float64  `json:"mean"`
	} `json:"current_useful_output"`
	NoiseFloor *float64 `json:"noise_floor"`
} {
	for index := range r.Summaries {
		if r.Summaries[index].Cell == cellID {
			return &r.Summaries[index]
		}
	}
	return nil
}

// checks reads a cell's checks from the run apart: the ones that failed (a
// bar the runner marked "fail", a cell that failed), the ones it could not
// settle (a bar it marked "unknown" or left blank, a cell that did not
// finish for another reason), and whether the run was dry. An unknown is
// never reported as a failure, nor a failure as an unknown.
func (r benchRung) checks(cellID string) (failed, unsettled []string, dry bool) {
	cell, ok := r.Cells[cellID]
	if !ok {
		return nil, []string{"cell.not-in-run"}, r.Dry
	}
	switch cell.State {
	case "done":
	case "failed":
		failed = append(failed, "cell.state")
	default:
		unsettled = append(unsettled, "cell."+firstNonEmpty(cell.State, "state"))
	}
	bars := make([]string, 0, len(cell.Bars))
	for bar := range cell.Bars {
		bars = append(bars, bar)
	}
	sort.Strings(bars)
	for _, bar := range bars {
		switch cell.Bars[bar] {
		case "pass":
		case "fail":
			failed = append(failed, "bar."+bar)
		default:
			unsettled = append(unsettled, "bar."+bar)
		}
	}
	return failed, unsettled, r.Dry
}

func (s benchScreen) armArgs(setupID string) []string {
	for _, arm := range s.Benchmark.Arms {
		if arm.SetupID == setupID {
			return arm.Args
		}
	}
	return nil
}

func (s benchScreen) armKind(setupID string) string {
	for _, arm := range s.Benchmark.Arms {
		if arm.SetupID == setupID {
			return arm.Kind
		}
	}
	return ""
}

func (s benchScreen) armLabel(setupID string) string {
	for _, arm := range s.Benchmark.Arms {
		if arm.SetupID == setupID {
			return arm.Label
		}
	}
	return ""
}

// benchModel is the model's facts: what the options give, else the run's
// spec (architecture from its kind, parameters from the weights' bytes at the
// checkpoint's precision), else unknown.
func benchModel(screen benchScreen, plan benchPlan, given Model) Model {
	model := given
	model.ID = firstNonEmpty(screen.Benchmark.Model.ID, model.ID)
	model.Revision = firstNonEmpty(screen.Benchmark.Model.Revision, model.Revision)
	model.Checkpoint = firstNonEmpty(model.Checkpoint, strings.ToLower(screen.Benchmark.Model.Precision.Weights))
	for _, spec := range plan.Spec.Models {
		if spec.Name != screen.Benchmark.Model.Name && spec.ID != model.ID {
			continue
		}
		if model.Architecture == "" {
			kind := strings.ToLower(spec.Kind)
			for _, architecture := range []string{ArchitectureDense, ArchitectureMoE, ArchitectureHybrid} {
				if strings.Contains(kind, architecture) {
					model.Architecture = architecture
				}
			}
		}
		if model.ParamsB == 0 && spec.Weights.Bytes > 0 {
			switch model.Checkpoint {
			case "bf16", "fp16":
				model.ParamsB = roundTenth(spec.Weights.Bytes / 2 / 1e9)
			case "fp8":
				model.ParamsB = roundTenth(spec.Weights.Bytes / 1e9)
			}
		}
	}
	if model.Architecture == "" {
		model.Architecture = ArchitectureUnknown
	}
	return model
}

func roundTenth(value float64) float64 { return float64(int64(value*10+0.5)) / 10 }
