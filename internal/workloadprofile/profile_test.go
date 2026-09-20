package workloadprofile

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/performanceprofile"
)

func validDocument() Document {
	distribution := Distribution{Unit: "tokens", Samples: 100, Percentiles: Percentiles{P50: 10, P90: 20, P95: 30, P99: 40}}
	return Document{
		SchemaVersion:       SchemaVersion,
		EvidenceClass:       EvidenceClass,
		GeneratedAt:         "2026-09-20T12:00:00Z",
		Source:              Source{Name: "public trace", URL: "https://example.com/trace.parquet", License: "CC-BY-4.0", TraceRows: 1000},
		Sampling:            Sampling{Method: "deterministic", Columns: []string{"it", "ot"}, RowGroups: []int{0}, TotalRowGroups: 10, RowsInspected: 100, RemoteOnly: true},
		Distributions:       Distributions{InputTokens: distribution, OutputTokens: distribution, CachedTokens: distribution, TTFTMilliseconds: distribution, DurationMS: distribution, CacheHitFraction: Distribution{Unit: "ratio", Samples: 100, Percentiles: Percentiles{P50: 0, P90: .5, P95: .75, P99: 1}}},
		Profiles:            []BenchmarkShape{{Name: "interactive", Description: "median public shape", Objective: "latency", Requests: 32, Concurrency: 4, InputTokens: 10, OutputTokens: 10, Streaming: true}},
		MethodologyBoundary: "Public shape evidence is a screening input, not customer production evidence.",
	}
}

func TestDecodeValidatesDigestAndRejectsUnknownContent(t *testing.T) {
	document := validDocument()
	digest, err := Digest(document)
	if err != nil {
		t.Fatal(err)
	}
	document.Digest = digest
	encoded, _ := json.Marshal(document)
	decoded, err := Decode(encoded)
	if err != nil || decoded.Digest != digest {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	withContent := strings.Replace(string(encoded), `"schema_version":`, `"prompt":"secret","schema_version":`, 1)
	if _, err = Decode([]byte(withContent)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("content-bearing unknown field was accepted: %v", err)
	}
}

func TestValidateRejectsInvalidSamplingAndPercentiles(t *testing.T) {
	document := validDocument()
	document.Sampling.RemoteOnly = false
	if err := Validate(document); err == nil || !strings.Contains(err.Error(), "remote_only") {
		t.Fatalf("local sampling accepted: %v", err)
	}
	document = validDocument()
	document.Distributions.InputTokens.Percentiles.P95 = 1
	if err := Validate(document); err == nil || !strings.Contains(err.Error(), "monotonic") {
		t.Fatalf("invalid percentile ordering accepted: %v", err)
	}
}

func TestLookupConvertsToBenchmarkProfile(t *testing.T) {
	profile, err := Lookup(validDocument(), "INTERACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	converted := AsBenchmarkProfile(profile)
	if converted.Name != "interactive" || converted.Concurrency != 4 || converted.InputTokens != 10 {
		t.Fatalf("converted=%+v", converted)
	}
}

func TestPublicChutesPriorMatchesMeasuredArtifactDigest(t *testing.T) {
	document, err := PublicChutesPrior()
	if err != nil {
		t.Fatal(err)
	}
	if document.Digest != PublicPriorDigest || document.Sampling.RowsInspected != 150_000 || len(document.Profiles) != 3 {
		t.Fatalf("document=%+v", document)
	}
	for _, profile := range document.Profiles {
		if !performanceprofile.IsPublicPrior(profile.Name) {
			t.Fatalf("unknown embedded public prior %q", profile.Name)
		}
	}
}
