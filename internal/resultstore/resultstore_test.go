package resultstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/infercrane/infercrane/internal/technique"
)

// mapFile is a synthetic map (testdata/synthetic/generate.py): every number in it is made up.
const mapFile = "testdata/synthetic/map-hybrid.json"

var measuredAt = time.Date(2001, 1, 2, 3, 0, 0, 0, time.UTC)

func importMap(t *testing.T, options ImportOptions) []Record {
	t.Helper()
	body, err := os.ReadFile(mapFile)
	if err != nil {
		t.Fatal(err)
	}
	options.RecordedAt = measuredAt
	if options.Model.Architecture == "" {
		options.Model = Model{Architecture: ArchitectureHybrid, ParamsB: 27, Checkpoint: "fp8"}
	}
	records, err := Import(mapFile, body, options)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestImportGymFileMakesOneRecordPerSetupCellAndMetric(t *testing.T) {
	records := importMap(t, ImportOptions{})
	// 15 cells; every measured setup gives an all-output record and a useful-output one.
	measured := 0
	var file gymFile
	body, _ := os.ReadFile(mapFile)
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatal(err)
	}
	for _, cell := range file.Cells {
		for _, m := range cell.Measured {
			measured++
			if m.UsefulOutput != nil {
				measured++
			}
		}
	}
	if len(records) != measured || measured < 100 {
		t.Fatalf("got %d records for %d measured values", len(records), measured)
	}
	byKey := map[string]Record{}
	for _, record := range records {
		if record.Cell.Set != "map-hybrid" || record.Model.ID != "example/hybrid-27b-fp8" || record.Accelerator() != "H100" {
			t.Fatalf("record names the wrong measurement: %+v", record.Cell)
		}
		byKey[record.Cell.Key+"/"+record.Setup.Name+"/"+record.Metric()] = record
	}
	plain := byKey["short/128/plain/"+MetricOutput]
	one := byKey["short/128/one-draft/"+MetricOutput]
	three := byKey["short/128/three-drafts/"+MetricOutput]
	if plain.Result.Technique != TechniqueStartingSetup || plain.Result.Measured.Change != 0 || plain.Observed.Median <= 0 {
		t.Fatalf("baseline record: %+v %+v", plain.Result.Measured, plain.Observed)
	}
	if one.Result.Technique != "speculation" || one.Setup.Speculation != SpeculationMTP || one.Setup.Drafts != 1 || one.Result.Measured.Decision != technique.DecisionImproved {
		t.Fatalf("in the synthetic map one draft at 128 is ahead of the plain command: %+v %+v", one.Setup, one.Result.Measured)
	}
	if three.Result.Measured.Decision != technique.DecisionWorse || three.Result.Measured.Change > -0.05 {
		t.Fatalf("in the synthetic map three drafts at 128 are behind the plain command: %+v", three.Result.Measured)
	}
	kv := byKey["long/128/three-drafts-fp8-kv/"+MetricOutput]
	if kv.Result.Technique != "speculation-and-kv-cache-precision" || kv.Result.Precision.KVCache != "fp8" || kv.Result.Precision.Weights != "fp8" {
		t.Fatalf("FP8 KV cache on three drafts: %+v %+v", kv.Result.Technique, kv.Result.Precision)
	}
	reasoning := byKey["short/32/reasoning/three-drafts/"+MetricOutput]
	if reasoning.Kind() != "short-reasoning" {
		t.Fatalf("reasoning cell kind %q", reasoning.Kind())
	}
	// A planner-gym file carries no image digest, traffic shape, command, or answer check, so
	// nothing it holds makes a claim.
	for _, record := range records {
		if record.Result.Claim() != technique.ClaimNotShown {
			t.Fatalf("%s claims a gain without its five fields", record.ID)
		}
	}
	unknown := strings.Join(one.Result.Unknown(), " ")
	for _, field := range []string{"software.image_digest", "workload.shape_digest", "reproduction.command", "correctness.verdict"} {
		if !strings.Contains(unknown, field) {
			t.Errorf("unknown fields %q miss %s", unknown, field)
		}
	}
	useful := byKey["short/128/three-drafts/"+UsefulOutputMetric(1000)]
	if useful.Observed.Median != 0 || useful.Result.Measured.Decision != technique.DecisionWorse {
		t.Fatalf("three drafts at 128 serve nothing within a second: %+v %+v", useful.Observed, useful.Result.Measured)
	}
}

func TestImportIsIdempotentAndIDsFollowContent(t *testing.T) {
	first, second := importMap(t, ImportOptions{}), importMap(t, ImportOptions{Sharing: SharingNumbers})
	for index := range first {
		if first[index].ID != second[index].ID {
			t.Fatalf("sharing changed a record's ID: %s %s", first[index].ID, second[index].ID)
		}
	}
	other := importMap(t, ImportOptions{Commit: "0123abcd"})
	if other[0].ID == first[0].ID {
		t.Fatal("a record with another commit kept the same ID")
	}
	tampered := first[0]
	tampered.Observed.Median++
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "does not match its content") {
		t.Fatalf("a changed number kept its ID: %v", err)
	}
}

func TestValidateRefusesWhatIsNotNumbersAndNames(t *testing.T) {
	base := importMap(t, ImportOptions{})[0]
	cases := map[string]func(*Record){
		"long label":     func(r *Record) { r.Setup.Label = strings.Repeat("a", 201) },
		"path in source": func(r *Record) { r.Source.File = "/Users/someone/run.json" },
		"free text flag": func(r *Record) { r.Setup.Flags = []string{"Explain the following text"} },
		"bad sharing":    func(r *Record) { r.Sharing = "public" },
		"range":          func(r *Record) { r.Observed.Min = r.Observed.Median + 1 },
		"architecture":   func(r *Record) { r.Model.Architecture = "transformer" },
	}
	for name, change := range cases {
		record := base
		change(&record)
		sealed, err := record.Seal()
		if err == nil {
			t.Errorf("%s: sealed %s", name, sealed.ID)
		}
	}
}

func TestNumbersOnlyKeepsNumbersAndDropsNames(t *testing.T) {
	record := importMap(t, ImportOptions{Commit: "0123abcd", ImageDigest: "sha256:" + strings.Repeat("a", 64), Sharing: SharingNumbers})[5]
	shared := record.NumbersOnly()
	body, err := json.Marshal(shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{record.ID, record.Cell.ID, record.Cell.Set, record.Model.ID, record.Setup.Name, record.Source.File, record.Source.Digest, "0123abcd", strings.Repeat("a", 64)} {
		if strings.Contains(string(body), private) {
			t.Errorf("another tenant's view holds %q", private)
		}
	}
	if !shared.Shared() || shared.Knobs() != record.Knobs() || shared.Observed != record.Observed || shared.Load() != record.Load() || shared.Kind() != record.Kind() {
		t.Fatalf("numbers-only lost what the prior compares: %+v", shared)
	}
	if !strings.HasPrefix(shared.CellWords(), "a shared cell (H100, hybrid 27B model") {
		t.Fatalf("shared cell words %q", shared.CellWords())
	}
}

func TestLocalStoreIsOneTenantAndAddsOnce(t *testing.T) {
	ctx := context.Background()
	local := Local{Path: filepath.Join(t.TempDir(), "results.jsonl")}
	records := importMap(t, ImportOptions{})
	added, err := local.PutResultRecords(ctx, LocalTenant, records)
	if err != nil || added != len(records) {
		t.Fatalf("added %d of %d: %v", added, len(records), err)
	}
	if added, err = local.PutResultRecords(ctx, LocalTenant, records); err != nil || added != 0 {
		t.Fatalf("a second import added %d: %v", added, err)
	}
	if _, err = local.PutResultRecords(ctx, "tenant-b", records); err == nil {
		t.Fatal("a local store took another tenant's records")
	}
	listed, err := local.ListResultRecords(ctx, LocalTenant, Filter{Kind: "short", MinLoad: 64, Metric: MetricOutput})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range listed {
		if record.Kind() != "short" || record.Load() < 64 || record.Metric() != MetricOutput {
			t.Fatalf("filter let through %s %d %s", record.Kind(), record.Load(), record.Metric())
		}
	}
	if len(listed) != 10 { // short/64 (3 setups), short/96 (3), short/128 (4)
		t.Fatalf("listed %d records, want 10", len(listed))
	}
	if other, _ := local.ListResultRecords(ctx, "tenant-b", Filter{}); len(other) != 0 {
		t.Fatal("another tenant read the local store")
	}
	got, err := local.GetResultRecord(ctx, LocalTenant, records[3].ID)
	if err != nil || got.ID != records[3].ID {
		t.Fatalf("get: %v", err)
	}
	if _, err = local.GetResultRecord(ctx, LocalTenant, "rr_"+strings.Repeat("0", 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record: %v", err)
	}
	// The bundle form reads back as the same records.
	bundle, err := json.Marshal(NewBundle(records[:4]))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Import("bundle.json", bundle, ImportOptions{})
	if err != nil || len(again) != 4 || again[0].ID != records[0].ID {
		t.Fatalf("bundle import: %d %v", len(again), err)
	}
}

func TestPublishedSchemaTakesARecord(t *testing.T) {
	body, err := os.ReadFile("../../schemas/result-record-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed jsonschema.Schema
	if err = json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range importMap(t, ImportOptions{Commit: "0123abcd"})[:6] {
		var instance map[string]any
		encoded, _ := json.Marshal(record)
		if err = json.Unmarshal(encoded, &instance); err != nil {
			t.Fatal(err)
		}
		if err = resolved.Validate(instance); err != nil {
			t.Fatalf("the schema refused a sealed record: %v", err)
		}
		instance["observed"].(map[string]any)["note"] = "free text"
		if resolved.Validate(instance) == nil {
			t.Fatal("the schema took a field the Go type does not have")
		}
	}
	runRecords, err := ImportRun(benchFixture, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range runRecords {
		var instance map[string]any
		encoded, _ := json.Marshal(record)
		if err = json.Unmarshal(encoded, &instance); err != nil {
			t.Fatal(err)
		}
		if err = resolved.Validate(instance); err != nil {
			t.Fatalf("the schema refused a judged run record: %v", err)
		}
	}
	if !slices.Contains(schemaEnum(t, body, "sharing"), SharingNumbers) {
		t.Fatal("the schema does not name numbers sharing")
	}
}

func schemaEnum(t *testing.T, body []byte, property string) []string {
	t.Helper()
	var document struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	return document.Properties[property].Enum
}
