package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestOperationalMeasurementsAreImmutableTenantSafeAndFreshnessBounded(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "collector-" + time.Now().UTC().Format("150405.000000000")
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://collector.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
	input := []domain.OperationalMeasurement{{ReplicaID: "replica-1", Name: "gpu_utilization", Value: 73, Unit: "percent", EvidenceClass: "measured", Source: "dcgm_exporter", SampleCount: 2, ObservedAt: observed, ValidUntil: observed.Add(2 * time.Minute)}}
	created, err := s.RecordOperationalMeasurements(ctx, "global", deployment.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := s.RecordOperationalMeasurements(ctx, "global", name, input)
	if err != nil || len(retried) != 1 || retried[0].ID != created[0].ID || !retried[0].CreatedAt.Equal(created[0].CreatedAt) {
		t.Fatalf("idempotent retry created=%+v retried=%+v err=%v", created, retried, err)
	}
	conflict := append([]domain.OperationalMeasurement(nil), input...)
	conflict[0].Value = 74
	if _, err = s.RecordOperationalMeasurements(ctx, "global", name, conflict); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting immutable retry err=%v", err)
	}
	if _, err = s.RecordOperationalMeasurements(ctx, "other", name, input); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant write err=%v", err)
	}
	wrongUnit := append([]domain.OperationalMeasurement(nil), input...)
	wrongUnit[0].ObservedAt = wrongUnit[0].ObservedAt.Add(time.Second)
	wrongUnit[0].ValidUntil = wrongUnit[0].ValidUntil.Add(time.Second)
	wrongUnit[0].Unit = "ratio"
	if _, err = s.RecordOperationalMeasurements(ctx, "global", name, wrongUnit); err == nil {
		t.Fatal("ambiguous canonical GPU unit was accepted")
	}
	snapshot, err := s.EndpointMonitoring(ctx, "global", name, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	measurement := measurementByName(snapshot.Evidence.Measurements, "gpu_utilization")
	if measurement.Availability != "available" || measurement.Value == nil || *measurement.Value != 73 || measurement.Unit != "percent" || measurement.Source != "dcgm_exporter" || measurement.FreshUntil == nil {
		t.Fatalf("fresh collector evidence=%+v", measurement)
	}

	staleName := name + "-stale"
	staleTarget := target + "-stale"
	if _, err = s.AddTarget(ctx, domain.Target{Name: staleTarget, URL: "http://collector-stale.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyDeployment(ctx, domain.Deployment{Name: staleName, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{staleTarget}); err != nil {
		t.Fatal(err)
	}
	staleObserved := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Microsecond)
	if _, err = s.RecordOperationalMeasurements(ctx, "global", staleName, []domain.OperationalMeasurement{{Name: "gpu_memory", Value: 1024, Unit: "bytes", EvidenceClass: "measured", Source: "dcgm_exporter", SampleCount: 1, ObservedAt: staleObserved, ValidUntil: staleObserved.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	staleSnapshot, err := s.EndpointMonitoring(ctx, "global", staleName, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale := measurementByName(staleSnapshot.Evidence.Measurements, "gpu_memory")
	if stale.Availability != "stale" || stale.Value != nil || stale.Reason == "" {
		t.Fatalf("stale collector evidence=%+v", stale)
	}
}

func measurementByName(values []domain.MeasurementEvidence, name string) domain.MeasurementEvidence {
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	return domain.MeasurementEvidence{}
}
