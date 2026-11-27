package resultstore

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/technique"
)

// benchFixture is a synthetic benchmark runner run (testdata/synthetic/generate.py),
// in the runner's format, trimmed to what the importer reads: long/16, whose bars
// pass, and short/16, whose copy was outside the noise. Every number is made up.
const benchFixture = "testdata/synthetic/bench-run"

func byName(records []Record) map[string]Record {
	out := map[string]Record{}
	for _, record := range records {
		out[record.Cell.Key+" "+record.Setup.Name] = record
	}
	return out
}

func TestImportRunKeepsTheRunnersRecordsAndJudgesEveryOne(t *testing.T) {
	records, err := ImportRun(benchFixture, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Four arms in each of two cells as the runner wrote them, and each cell's current setup.
	if len(records) != 10 {
		t.Fatalf("%d records, want 10", len(records))
	}
	got := byName(records)
	kv := got["long/16 example-8b-fp8-kv-cache"]
	if kv.Result.Technique != "fp8-kv-cache" || kv.Result.Measured.Change != 0.8703 || kv.Observed.Median != 1500 || kv.Knobs().KVCache != "fp8" || kv.Cell.Set != "synthetic-rung" {
		t.Fatalf("the runner's record: %+v %+v %+v", kv.Result.Measured, kv.Observed, kv.Knobs())
	}
	if kv.Model.ID != "example/dense-8b" || kv.Model.Architecture != ArchitectureDense || kv.Model.ParamsB != 8.2 || kv.Model.Checkpoint != "bf16" {
		t.Fatalf("model facts from the spec: %+v", kv.Model)
	}
	current := got["long/16 example-8b-engine-defaults"]
	if current.Result.Technique != TechniqueStartingSetup || current.Observed != (Observed{Median: 802, Min: 800, Max: 804, Runs: 2}) || current.Knobs() != (Knobs{Engine: "vllm", Speculation: "none", KVCache: "auto"}) {
		t.Fatalf("the current setup's record: %+v %+v", current.Observed, current.Knobs())
	}
	// The fixture's records use the runner's older bare metric; each takes its cell's limit.
	if kv.Metric() != "useful_output_tokens_per_second.ttft_3000ms" || got["short/16 example-8b-fp8-kv-cache"].Metric() != "useful_output_tokens_per_second.ttft_1000ms" || current.Metric() != kv.Metric() {
		t.Fatalf("metrics: %s, %s, %s", kv.Metric(), got["short/16 example-8b-fp8-kv-cache"].Metric(), current.Metric())
	}
	if copyArm := got["long/16 example-8b-copy"]; copyArm.Setup.Flags[0] != "--seed=1" {
		t.Fatalf("the copy's flags: %v", copyArm.Setup.Flags)
	}
	// A dry run on stand-in servers claims nothing, and says why. The baselines (the
	// current setup, the publisher's setup, the copy) are never said to lack a manifest.
	baselines := 0
	for _, record := range records {
		why := record.NotShownBecause()
		if record.Claim() != technique.ClaimNotShown {
			t.Fatalf("%s %s claims", record.Cell.Key, record.Setup.Name)
		}
		if record.Baseline() {
			baselines++
			if !slices.Equal(why, []string{ReasonBaseline}) || record.Judgement.Manifest != "" {
				t.Fatalf("baseline %s: %v", record.Setup.Name, why)
			}
			continue
		}
		if !slices.Contains(why, ReasonDryRun) || !slices.Contains(why, "no technique manifest") || slices.Contains(why, "failed dry-run") {
			t.Fatalf("%s %s: %v", record.Cell.Key, record.Setup.Name, why)
		}
		if strings.HasPrefix(record.Cell.Key, "short/16") && !slices.Contains(why, "failed bar.copy-inside-noise") {
			t.Fatalf("short/16's copy was outside the noise: %v", why)
		}
	}
	if baselines != 6 {
		t.Fatalf("%d baselines, want the current setup, the publisher and the copy in each of two cells", baselines)
	}
	// The records go through the store like any other.
	again, err := ImportRun(benchFixture, ImportOptions{})
	if err != nil || again[3].ID != records[3].ID {
		t.Fatal("importing a run twice made other IDs")
	}
}

// realRun is the fixture as a run on a real L4 would write it: not dry, and
// each record naming its GPU.
func realRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir(benchFixture, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(benchFixture, path)
		switch relative {
		case "rung.json":
			body = []byte(strings.Replace(string(body), `"dry": true`, `"dry": false`, 1))
		case "results.jsonl":
			body = []byte(strings.ReplaceAll(string(body), `"hardware": {"accelerator": "none (dry run, stand-in server)", "count": 0}`, `"hardware": {"accelerator": "NVIDIA L4", "count": 1}`))
		}
		target := filepath.Join(dir, relative)
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func manifestFile(t *testing.T, id string, evidence ...string) LoadedManifest {
	t.Helper()
	manifest := technique.Manifest{
		SchemaVersion: technique.SchemaV1, ID: id, Version: "1.0.0", Label: "FP8 KV cache", License: "Apache-2.0",
		Requires: technique.Requires{Engines: []technique.Engine{{Name: "vllm", Versions: []string{"0.34.0"}}}, Accelerators: []string{"L4"}},
		Relieves: []technique.Limit{technique.LimitKVCacheRoom}, Costs: []technique.Limit{technique.LimitDecodeSteps},
		Prediction: technique.Method{Metric: "useful_output_tokens_per_second", Basis: "cache-room"}, ChangesAnswers: true,
		Runtime:  technique.Runtime{Policy: technique.RuntimeStatic},
		Fallback: technique.Fallback{Serves: technique.FallbackEngineDefault, Reason: "the engine's own KV cache format"},
		Evidence: evidence,
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestImportRunShowsAClaimOnlyWithEveryPieceOfEvidenceItsManifestRequires(t *testing.T) {
	dir := realRun(t)
	cellRecord := manifestFile(t, "fp8-kv-cache", "cell-record")
	records, err := ImportRun(dir, ImportOptions{Manifests: []LoadedManifest{cellRecord}})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(records)
	// One machine, one run: everything else holds, and the claim still waits
	// for a fresh machine to measure it again.
	oneMachine := got["long/16 example-8b-fp8-kv-cache"]
	if oneMachine.Claim() != technique.ClaimNotShown || !slices.Equal(oneMachine.NotShownBecause(), []string{"unknown evidence." + technique.EvidenceFreshMachineConfirmation}) {
		t.Fatalf("a run on one machine: %s %v", oneMachine.Claim(), oneMachine.NotShownBecause())
	}
	shown := oneMachine
	shown.Result.Evidence = append(slices.Clone(shown.Result.Evidence), technique.EvidenceItem{Name: technique.EvidenceFreshMachineConfirmation, Digest: "sha256:" + strings.Repeat("e", 64)})
	if shown.Claim() != technique.ClaimShown || !strings.HasPrefix(shown.Judgement.Manifest, "fp8-kv-cache@1.0.0 sha256:") || !slices.Contains(shown.Result.RequiredEvidence, "cell-record") {
		t.Fatalf("every field, a gain beyond the noise, answers held, the manifest's evidence, a fresh machine's confirmation and the bars passed: %s %v %+v", shown.Claim(), shown.NotShownBecause(), shown.Judgement)
	}
	// The same technique in a cell whose copy was outside the noise claims nothing.
	if record := got["short/16 example-8b-fp8-kv-cache"]; record.Claim() != technique.ClaimNotShown || !slices.Contains(record.NotShownBecause(), "failed bar.copy-inside-noise") {
		t.Fatalf("short/16: %v", record.NotShownBecause())
	}
	// A technique without a manifest claims nothing, however good its numbers.
	if record := got["long/16 example-8b-fp8-weights"]; record.Claim() != technique.ClaimNotShown || !slices.Contains(record.NotShownBecause(), "no technique manifest") {
		t.Fatalf("fp8 weights without a manifest: %v", record.NotShownBecause())
	}
	// Baselines make no claim even with every field, a gain and a manifest for their ID.
	publisher := manifestFile(t, "baseline-publisher", "cell-record")
	records, err = ImportRun(dir, ImportOptions{Manifests: []LoadedManifest{cellRecord, publisher}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"long/16 example-8b-publisher", "long/16 example-8b-copy", "long/16 example-8b-engine-defaults"} {
		record := byName(records)[name]
		if record.Claim() != technique.ClaimNotShown || !slices.Equal(record.NotShownBecause(), []string{ReasonBaseline}) || record.Judgement.Manifest != "" {
			t.Fatalf("%s: %s %v %+v", name, record.Claim(), record.NotShownBecause(), record.Judgement)
		}
	}
	// The claim is the contract's own: for a judged record with no failed check it is
	// exactly Result.Claim, so a change to that rule reaches stored records.
	for _, record := range records {
		if !record.Baseline() && record.Judgement.Manifest != "" && len(record.Judgement.Failed) == 0 && record.Claim() != record.Result.Claim() {
			t.Fatalf("%s: the store's claim %s, the contract's %s", record.Setup.Name, record.Claim(), record.Result.Claim())
		}
	}
	// A manifest that requires evidence the record lacks withholds the claim and names it.
	strict := manifestFile(t, "fp8-kv-cache", "cell-record", "kernel-invocation-proof")
	records, err = ImportRun(dir, ImportOptions{Manifests: []LoadedManifest{strict}})
	if err != nil {
		t.Fatal(err)
	}
	if record := byName(records)["long/16 example-8b-fp8-kv-cache"]; record.Claim() != technique.ClaimNotShown || !slices.Equal(record.NotShownBecause(), []string{"unknown evidence.kernel-invocation-proof", "unknown evidence." + technique.EvidenceFreshMachineConfirmation}) {
		t.Fatalf("missing evidence: %v", record.NotShownBecause())
	}
	// The manifest judges only its own version.
	other := cellRecord
	other.Manifest.Version = "2.0.0"
	records, err = ImportRun(dir, ImportOptions{Manifests: []LoadedManifest{other}})
	if err != nil {
		t.Fatal(err)
	}
	if record := byName(records)["long/16 example-8b-fp8-kv-cache"]; record.Claim() != technique.ClaimNotShown {
		t.Fatal("a manifest of another version judged the record")
	}
}

func TestImportRunRefusesWhatIsNotARun(t *testing.T) {
	if _, err := ImportRun(t.TempDir(), ImportOptions{}); err == nil {
		t.Fatal("an empty directory imported")
	}
	dir := realRun(t)
	if err := os.WriteFile(filepath.Join(dir, "results.jsonl"), []byte(`{"schema_version": "infercrane.dev/technique-result/v1", "note": "free text"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportRun(dir, ImportOptions{}); err == nil {
		t.Fatal("a record with an unknown field imported")
	}
}

// The synthetic fixtures are exactly what their seeded generator writes.
func TestSyntheticFixturesMatchTheirGenerator(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	output, err := exec.Command(python, "-I", "testdata/synthetic/generate.py", "--check").CombinedOutput()
	if err != nil {
		t.Fatalf("testdata/synthetic is not what generate.py writes: %v\n%s", err, output)
	}
}

// A newer run names the limit in its metric itself; the importer keeps it.
func TestImportRunKeepsAMetricThatNamesItsLimit(t *testing.T) {
	dir := realRun(t)
	path := filepath.Join(dir, "results.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	named := strings.ReplaceAll(string(body), `"metric": "useful_output_tokens_per_second"`, `"metric": "useful_output_tokens_per_second.ttft_2500ms"`)
	if named == string(body) {
		t.Fatal("the fixture has no bare metric to rename")
	}
	if err = os.WriteFile(path, []byte(named), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := ImportRun(dir, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Metric() != "useful_output_tokens_per_second.ttft_2500ms" {
			t.Fatalf("%s %s: metric %s", record.Cell.Key, record.Setup.Name, record.Metric())
		}
	}
}

// A bar the run could not settle reads "unknown", never "failed"; a failed
// bar reads "failed", never "unknown". Either withholds the claim.
func TestImportRunKeepsUnknownBarsApartFromFailedOnes(t *testing.T) {
	dir := realRun(t)
	path := filepath.Join(dir, "rung.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rung map[string]any
	if err = json.Unmarshal(body, &rung); err != nil {
		t.Fatal(err)
	}
	bars := rung["cells"].(map[string]any)["example-8b/long/16"].(map[string]any)["bars"].(map[string]any)
	bars["repeats-steady"], bars["copy-inside-noise"] = "unknown", ""
	encoded, _ := json.Marshal(rung)
	if err = os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	cellRecord := manifestFile(t, "fp8-kv-cache", "cell-record")
	records, err := ImportRun(dir, ImportOptions{Manifests: []LoadedManifest{cellRecord}})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(records)
	unknown := got["long/16 example-8b-fp8-kv-cache"]
	// A run on one machine also lacks the fresh machine's confirmation.
	if unknown.Claim() != technique.ClaimNotShown || !slices.Equal(unknown.NotShownBecause(), []string{"unknown evidence." + technique.EvidenceFreshMachineConfirmation, "unknown bar.copy-inside-noise", "unknown bar.repeats-steady"}) ||
		len(unknown.Judgement.Failed) != 0 || unknown.Judgement.Dry {
		t.Fatalf("unsettled bars: %v %+v", unknown.NotShownBecause(), unknown.Judgement)
	}
	failed := got["short/16 example-8b-fp8-kv-cache"]
	if !slices.Contains(failed.NotShownBecause(), "failed bar.copy-inside-noise") || slices.Contains(failed.NotShownBecause(), "unknown bar.copy-inside-noise") || len(failed.Judgement.Unsettled) != 0 {
		t.Fatalf("a failed bar: %v %+v", failed.NotShownBecause(), failed.Judgement)
	}
	// A record cannot name one check both failed and unknown.
	bad := unknown
	bad.Judgement = &Judgement{Manifest: unknown.Judgement.Manifest, Failed: []string{"bar.repeats-steady"}, Unsettled: []string{"bar.repeats-steady"}}
	if _, err = bad.Seal(); err == nil {
		t.Fatal("a check both failed and unknown was sealed")
	}
}
