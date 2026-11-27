package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/resultstore"
)

func resultRecordsForStore(t *testing.T, sharing string) []resultstore.Record {
	t.Helper()
	body, err := os.ReadFile("../resultstore/testdata/synthetic/map-hybrid.json")
	if err != nil {
		t.Fatal(err)
	}
	records, err := resultstore.Import("map-hybrid.json", body, resultstore.ImportOptions{
		Model: resultstore.Model{Architecture: resultstore.ArchitectureHybrid, ParamsB: 27, Checkpoint: "fp8"}, Sharing: sharing,
		Set: "store-test-" + time.Now().UTC().Format("150405.000000000"), RecordedAt: time.Date(2001, 1, 2, 3, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return records[:12]
}

func TestResultRecordsAreTenantScopedImmutableAndSharedOnlyAsNumbers(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	tenantA, tenantB := "results-a-"+suffix, "results-b-"+suffix
	private := resultRecordsForStore(t, resultstore.SharingPrivate)
	added, err := s.PutResultRecords(ctx, tenantA, private)
	if err != nil || added != len(private) {
		t.Fatalf("added %d of %d: %v", added, len(private), err)
	}
	if added, err = s.PutResultRecords(ctx, tenantA, private); err != nil || added != 0 {
		t.Fatalf("the same records again added %d: %v", added, err)
	}
	listed, err := s.ListResultRecords(ctx, tenantA, resultstore.Filter{Kind: private[0].Kind(), Metric: private[0].Metric()})
	if err != nil || len(listed) == 0 {
		t.Fatalf("list: %d %v", len(listed), err)
	}
	for _, record := range listed {
		if record.Kind() != private[0].Kind() || record.Metric() != private[0].Metric() || record.Validate() != nil {
			t.Fatalf("filter or round trip: %+v", record)
		}
	}
	if other, err := s.ListResultRecords(ctx, tenantB, resultstore.Filter{}); err != nil || len(other) != 0 {
		t.Fatalf("tenant B read %d of tenant A's records: %v", len(other), err)
	}
	if _, err = s.GetResultRecord(ctx, tenantB, private[0].ID); !errors.Is(err, resultstore.ErrNotFound) {
		t.Fatalf("tenant B got tenant A's record: %v", err)
	}
	got, err := s.GetResultRecord(ctx, tenantA, private[0].ID)
	if err != nil || got.ID != private[0].ID || got.Sharing != resultstore.SharingPrivate {
		t.Fatalf("get: %+v %v", got, err)
	}
	// Private records are never shared.
	shared, err := s.SharedResultRecords(ctx, tenantB, resultstore.Filter{Limit: resultstore.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range shared {
		if strings.Contains(record.CellWords(), "store-test-") {
			t.Fatal("a private record was shared")
		}
	}
	// Importing again with numbers sharing states the tenant's new choice.
	numbers := resultRecordsForStore(t, resultstore.SharingNumbers)
	if _, err = s.PutResultRecords(ctx, tenantA, numbers); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutResultRecords(ctx, tenantA, private); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetResultRecord(ctx, tenantA, private[0].ID); again.Sharing != resultstore.SharingPrivate {
		t.Fatalf("a later private import did not take sharing back: %s", again.Sharing)
	}
	if _, err = s.PutResultRecords(ctx, tenantA, numbers); err != nil {
		t.Fatal(err)
	}
	shared, err = s.SharedResultRecords(ctx, tenantB, resultstore.Filter{Accelerator: "H100", Limit: resultstore.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, record := range shared {
		if !record.Shared() || record.ID != "" || record.Model.ID != "" || record.Setup.Name != "" || record.Source.File != "" {
			t.Fatalf("a shared record kept a name: %+v", record)
		}
		for _, mine := range numbers {
			if record.Observed == mine.Observed && record.Knobs() == mine.Knobs() && record.Load() == mine.Load() {
				found++
				break
			}
		}
	}
	if found < len(numbers) {
		t.Fatalf("tenant B's prior saw %d of %d shared records", found, len(numbers))
	}
	// A tenant's own records never come back as shared: what it reads is exactly the
	// other tenants' numbers rows.
	own, err := s.SharedResultRecords(ctx, tenantA, resultstore.Filter{Limit: resultstore.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	var others int
	if err = s.QueryRowContext(ctx, `SELECT count(*) FROM result_records WHERE sharing='numbers' AND tenant_id<>?`, tenantA).Scan(&others); err != nil {
		t.Fatal(err)
	}
	if len(own) != min(others, resultstore.MaxListLimit) {
		t.Fatalf("tenant A's prior read %d shared records; other tenants share %d", len(own), others)
	}
	// The record itself cannot change.
	if _, err = s.ExecContext(ctx, `UPDATE result_records SET record=jsonb_set(record,'{observed,median}','1') WHERE tenant_id=? AND id=?`, tenantA, private[0].ID); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a stored number changed: %v", err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE result_records SET sharing='numbers' WHERE tenant_id=? AND id=?`, tenantA, private[0].ID); err == nil {
		t.Fatal("sharing changed in the column alone")
	}
}
