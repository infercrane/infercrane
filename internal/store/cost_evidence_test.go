package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestCostEvidenceIsImmutableRevisionBoundTenantSafeAndCurrencyExplicit(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "cost-" + time.Now().UTC().Format("150405.000000000")
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://cost.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	input := []domain.CostEvidence{{Source: "opencost/allocation", Scope: "deployment_hourly_rate/inference", Resource: "inference", Currency: "USD", BillingUnit: "hour", EvidenceClass: "measured", Amount: 1.25, WindowStart: now.Add(-time.Hour), WindowEnd: now, ObservedAt: now, ValidUntil: now.Add(time.Hour)}}
	created, err := s.RecordCostEvidence(ctx, "global", deployment.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := s.RecordCostEvidence(ctx, "global", name, input)
	if err != nil || len(retried) != 1 || retried[0].ID != created[0].ID || !retried[0].CreatedAt.Equal(created[0].CreatedAt) {
		t.Fatalf("retry created=%+v retried=%+v err=%v", created, retried, err)
	}
	changed := append([]domain.CostEvidence(nil), input...)
	changed[0].Amount = 9
	if _, err = s.RecordCostEvidence(ctx, "global", name, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("changed retry err=%v", err)
	}
	if _, err = s.RecordCostEvidence(ctx, "other", name, input); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant write err=%v", err)
	}
	invalid := append([]domain.CostEvidence(nil), input...)
	invalid[0].WindowStart = invalid[0].WindowStart.Add(-time.Second)
	invalid[0].Currency = ""
	if _, err = s.RecordCostEvidence(ctx, "global", name, invalid); err == nil {
		t.Fatal("implicit currency was accepted")
	}
	invalid = append([]domain.CostEvidence(nil), input...)
	invalid[0].EvidenceClass = "provider_reported"
	if _, err = s.RecordCostEvidence(ctx, "global", name, invalid); err == nil {
		t.Fatal("tenant import minted provider-reported authority")
	}
	rows, err := s.CostEvidenceForDeployment(ctx, "global", name, now.Add(-2*time.Hour), now.Add(time.Hour), 20)
	if err != nil || len(rows) != 1 || rows[0].RevisionID == "" || rows[0].Amount != 1.25 || rows[0].Currency != "USD" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
