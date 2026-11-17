package optimizationreport

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

// fakeStore serves evidence by ID only, as the PostgreSQL store does. It keeps
// an unrelated row of every kind so a report that read more than its
// candidates recorded would show it.
type fakeStore struct {
	mu        sync.Mutex
	evidence  Evidence
	owner     string
	tenants   []string
	requested map[string][]string
	err       error
	replayErr error
}

func (f *fakeStore) record(tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenant)
}

func (f *fakeStore) ask(tenant, kind string, ids []string) {
	f.record(tenant)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requested == nil {
		f.requested = map[string][]string{}
	}
	for _, id := range ids {
		if id != "" {
			f.requested[kind] = append(f.requested[kind], id)
		}
	}
}

// byID returns the rows with the given IDs when the tenant owns them.
func byID[T any](rows map[string]T, unrelated T, owned bool, ids []string) map[string]T {
	out := map[string]T{}
	if !owned {
		return out
	}
	all := map[string]T{"unrelated": unrelated}
	for id, row := range rows {
		all[id] = row
	}
	for _, id := range ids {
		if row, found := all[id]; found {
			out[id] = row
		}
	}
	return out
}

func (f *fakeStore) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	f.record(tenant)
	if tenant != f.owner || id != f.evidence.Campaign.ID {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.evidence.Campaign, nil
}

func (f *fakeStore) BenchmarksByIDs(_ context.Context, tenant string, ids []string) (map[string]domain.BenchmarkResult, error) {
	f.ask(tenant, "benchmark", ids)
	if f.err != nil {
		return nil, f.err
	}
	return byID(f.evidence.Benchmarks, domain.BenchmarkResult{ID: "unrelated"}, tenant == f.owner, ids), nil
}

func (f *fakeStore) QualityEvidenceByIDs(_ context.Context, tenant string, ids []string) (map[string]domain.QualityEvidence, error) {
	f.ask(tenant, "quality", ids)
	return byID(f.evidence.Quality, domain.QualityEvidence{ID: "unrelated"}, tenant == f.owner, ids), nil
}

func (f *fakeStore) ReleaseGuardEvaluationsByIDs(_ context.Context, tenant string, ids []string) (map[string]domain.ReleaseGuardEvaluation, error) {
	f.ask(tenant, "release_guard", ids)
	return byID(f.evidence.ReleaseGuard, domain.ReleaseGuardEvaluation{ID: "unrelated"}, tenant == f.owner, ids), nil
}

func (f *fakeStore) ReplayTraceByDigest(_ context.Context, tenant, digest string) (domain.ReplayTrace, error) {
	f.record(tenant)
	if f.replayErr != nil {
		return domain.ReplayTrace{}, f.replayErr
	}
	if f.evidence.Replay == nil || digest != f.evidence.Replay.ShapeDigest {
		return domain.ReplayTrace{}, domain.ErrNotFound
	}
	return *f.evidence.Replay, nil
}

func TestLoadReadsOnlyTheTenantsReferencedEvidence(t *testing.T) {
	expected := winnerFixture().evidence()
	store := &fakeStore{evidence: expected, owner: "tenant-a"}
	report, err := Load(t.Context(), store, "tenant-a", "campaign-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := Assemble(expected); report.ReportDigest != want.ReportDigest {
		t.Fatalf("loaded report differs from the assembled evidence:\n%s\n%s", Text(report), Text(want))
	}
	for _, tenant := range store.tenants {
		if tenant != "tenant-a" {
			t.Fatalf("store was queried for tenant %q", tenant)
		}
	}

	other := &fakeStore{evidence: expected, owner: "tenant-a"}
	if _, err = Load(t.Context(), other, "tenant-b", "campaign-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant's campaign was readable: err=%v", err)
	}
	if len(other.tenants) != 1 {
		t.Fatalf("evidence was read before the campaign was authorized: %v", other.tenants)
	}
}

// TestLoadReadsEvidenceByTheRecordedIDs proves the report asks for exactly
// the records its candidates recorded, by ID, so evidence of a candidate whose
// deployment cleanup deleted, or whose name now belongs to another deployment,
// is still found. A recorded ID with no record stays unknown.
func TestLoadReadsEvidenceByTheRecordedIDs(t *testing.T) {
	guarded := winnerFixture()
	guarded.arms[1].guardID = "guard-1"
	guarded.guard = map[string]domain.ReleaseGuardEvaluation{"guard-1": {ID: "guard-1", Decision: "ACCEPT", MetricsJSON: `{}`}}
	expected := guarded.evidence()
	store := &fakeStore{evidence: expected, owner: "tenant-a"}
	report, err := Load(t.Context(), store, "tenant-a", "campaign-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := Assemble(expected); report.ReportDigest != want.ReportDigest {
		t.Fatalf("loaded report differs from the assembled evidence:\n%s\n%s", Text(report), Text(want))
	}
	want := map[string][]string{}
	for _, run := range expected.Campaign.Candidates {
		for kind, id := range map[string]string{"benchmark": run.BenchmarkID, "quality": run.QualityEvidenceID, "release_guard": run.ReleaseGuardEvaluationID} {
			if id != "" {
				want[kind] = append(want[kind], id)
			}
		}
	}
	for _, kind := range []string{"benchmark", "quality", "release_guard"} {
		got, wanted := append([]string(nil), store.requested[kind]...), append([]string(nil), want[kind]...)
		slices.Sort(got)
		slices.Sort(wanted)
		if !slices.Equal(got, wanted) {
			t.Fatalf("%s IDs requested=%v, want the recorded %v", kind, got, wanted)
		}
	}

	missing := winnerFixture().evidence()
	delete(missing.Benchmarks, "bench-winner")
	report, err = Load(t.Context(), &fakeStore{evidence: missing, owner: "tenant-a"}, "tenant-a", "campaign-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report.Candidates {
		if result.ID == "winner" && (result.OutputTokensPerSecond != nil || result.Reason != "its benchmark record could not be found") {
			t.Fatalf("a missing benchmark record was not kept unknown: %+v", result)
		}
	}
	if metric := metricByKey(t, report, MetricTTFTP95); metric.Winner != nil || metric.Decision != DecisionUnknown {
		t.Fatalf("a missing benchmark record produced a winner value: %+v", metric)
	}
}

func TestLoadFailsOnStoreErrorsButNotOnMissingEvidence(t *testing.T) {
	broken := &fakeStore{evidence: winnerFixture().evidence(), owner: "tenant-a", err: errors.New("connection reset")}
	if _, err := Load(t.Context(), broken, "tenant-a", "campaign-1"); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a store failure was not reported: %v", err)
	}
	replayDown := &fakeStore{evidence: winnerFixture().evidence(), owner: "tenant-a", replayErr: errors.New("timeout")}
	if _, err := Load(t.Context(), replayDown, "tenant-a", "campaign-1"); err == nil {
		t.Fatal("a replay lookup failure was reported as missing evidence")
	}

	evidence := winnerFixture().evidence()
	evidence.Replay = nil
	report, err := Load(t.Context(), &fakeStore{evidence: evidence, owner: "tenant-a"}, "tenant-a", "campaign-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Monthly.Basis != BasisUnknown || report.Monthly.Reason != "the replayed traffic record was not found" {
		t.Fatalf("monthly = %+v", report.Monthly)
	}
}
