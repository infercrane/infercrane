package optimizationreport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeStore struct {
	mu        sync.Mutex
	evidence  Evidence
	owner     string
	tenants   []string
	err       error
	replayErr error
}

func (f *fakeStore) record(tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenant)
}

func (f *fakeStore) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	f.record(tenant)
	if tenant != f.owner || id != f.evidence.Campaign.ID {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.evidence.Campaign, nil
}

func (f *fakeStore) BenchmarksForDeployment(_ context.Context, tenant, name string, _ int) ([]domain.BenchmarkResult, error) {
	f.record(tenant)
	if f.err != nil {
		return nil, f.err
	}
	rows := []domain.BenchmarkResult{{ID: "unrelated", DeploymentName: name}}
	for _, row := range f.evidence.Benchmarks {
		if row.DeploymentName == name {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (f *fakeStore) QualityEvidenceForDeployment(_ context.Context, tenant, name string, _ int) ([]domain.QualityEvidence, error) {
	f.record(tenant)
	rows := []domain.QualityEvidence{}
	for _, row := range f.evidence.Quality {
		if strings.HasSuffix(name, strings.TrimPrefix(row.RevisionID, "rev-")) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (f *fakeStore) ReleaseGuardEvaluations(_ context.Context, tenant, _ string, _ int) ([]domain.ReleaseGuardEvaluation, error) {
	f.record(tenant)
	rows := []domain.ReleaseGuardEvaluation{}
	for _, row := range f.evidence.ReleaseGuard {
		rows = append(rows, row)
	}
	return rows, nil
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
