package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestBenchmarkOperationalMeasurementIsExactRevisionWeightedAndWindowBound(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "benchmark-evidence-" + time.Now().UTC().Format("150405.000000000")
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://benchmark-evidence.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := []domain.OperationalMeasurement{
		{ReplicaID: "replica-a", Name: "gpu_utilization", Value: 50, Unit: "percent", EvidenceClass: "measured", Source: "dcgm_exporter", SampleCount: 2, ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute)},
		{ReplicaID: "replica-b", Name: "gpu_utilization", Value: 70, Unit: "percent", EvidenceClass: "measured", Source: "dcgm_exporter", SampleCount: 6, ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute)},
		{ReplicaID: "provider", Name: "gpu_utilization", Value: 100, Unit: "percent", EvidenceClass: "provider_reported", Source: "provider", SampleCount: 100, ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute)},
	}
	if _, err = s.RecordOperationalMeasurements(ctx, "global", name, rows); err != nil {
		t.Fatal(err)
	}
	evidence, err := s.BenchmarkOperationalMeasurement(ctx, "global", deployment.ID, deployment.ActiveRevisionID, "gpu_utilization", now.Add(-30*time.Second), now.Add(30*time.Second))
	if err != nil || evidence.Value == nil || *evidence.Value != 65 || evidence.SampleCount != 8 || evidence.Source != "dcgm_exporter" || evidence.EvidenceClass != "measured" {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	if _, err = s.BenchmarkOperationalMeasurement(ctx, "global", deployment.ID, deployment.ActiveRevisionID, "gpu_utilization", now.Add(2*time.Minute), now.Add(3*time.Minute)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-overlapping evidence err=%v", err)
	}
	if _, err = s.BenchmarkOperationalMeasurement(ctx, "global", deployment.ID, "other-revision", "gpu_utilization", now.Add(-30*time.Second), now.Add(30*time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-revision evidence err=%v", err)
	}
}
