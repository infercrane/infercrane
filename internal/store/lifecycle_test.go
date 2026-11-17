package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestOperationsOfKindIsTenantKindAndResourceScopedNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	for _, tenant := range []string{"lab-a", "lab-b"} {
		if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
			t.Fatal(err)
		}
	}
	const labKind, campaignKind = "optimization.accelerator-lab.execute", "optimization.campaign.execute"
	base := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	enqueue := func(tenant, kind, resourceType, resourceName, key string, created time.Time) string {
		t.Helper()
		operation, _, err := s.EnqueueOperation(ctx, domain.Operation{TenantID: tenant, Kind: kind, ResourceType: resourceType, ResourceName: resourceName, IdempotencyKey: key, RequestJSON: `{"campaign_id":"` + resourceName + `"}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ExecContext(ctx, `UPDATE operations SET created_at=? WHERE id=?`, created, operation.ID); err != nil {
			t.Fatal(err)
		}
		return operation.ID
	}
	oldest := enqueue("lab-a", labKind, "optimization_campaign", "campaign-1", "lab-1", base)
	middle := enqueue("lab-a", labKind, "optimization_campaign", "campaign-2", "lab-2", base.Add(time.Hour))
	newest := enqueue("lab-a", labKind, "optimization_campaign", "campaign-1", "lab-3", base.Add(2*time.Hour))
	enqueue("lab-a", campaignKind, "optimization_campaign", "campaign-1", "campaign-1", base.Add(3*time.Hour))
	enqueue("lab-a", labKind, "deployment", "campaign-1", "lab-deployment", base.Add(4*time.Hour))
	foreign := enqueue("lab-b", labKind, "optimization_campaign", "campaign-1", "lab-1", base.Add(5*time.Hour))

	ids := func(rows []domain.Operation) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.ID)
		}
		return out
	}
	all, err := s.OperationsOfKind(ctx, "lab-a", labKind, "optimization_campaign", "", 20)
	if err != nil || !slices.Equal(ids(all), []string{newest, middle, oldest}) {
		t.Fatalf("tenant lab-a runs=%v err=%v, want %v", ids(all), err, []string{newest, middle, oldest})
	}
	for _, row := range all {
		if row.TenantID != "lab-a" || row.Kind != labKind || row.ResultJSON != "{}" || row.RequestJSON == "" || row.CreatedAt.IsZero() {
			t.Fatalf("row=%+v", row)
		}
	}
	campaign, err := s.OperationsOfKind(ctx, "lab-a", labKind, "optimization_campaign", "campaign-1", 20)
	if err != nil || !slices.Equal(ids(campaign), []string{newest, oldest}) {
		t.Fatalf("campaign-1 runs=%v err=%v", ids(campaign), err)
	}
	limited, err := s.OperationsOfKind(ctx, "lab-a", labKind, "optimization_campaign", "", 1)
	if err != nil || !slices.Equal(ids(limited), []string{newest}) {
		t.Fatalf("limited runs=%v err=%v", ids(limited), err)
	}
	other, err := s.OperationsOfKind(ctx, "lab-b", labKind, "optimization_campaign", "", 20)
	if err != nil || !slices.Equal(ids(other), []string{foreign}) {
		t.Fatalf("tenant lab-b runs=%v err=%v", ids(other), err)
	}
	none, err := s.OperationsOfKind(ctx, "lab-a", labKind, "optimization_campaign", "campaign-9", 20)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("unknown campaign runs=%#v err=%v", none, err)
	}
}
