package store

import (
	"context"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

func TestEngineMeasurementsReachMonitoringAndStayUnavailableWhenAbsent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	name := "engine-" + suffix
	var targets []string
	for _, replica := range []string{"a", "b"} {
		target := name + "-" + replica
		if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://engine-" + replica + ".invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 2}, targets)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := s.ResolveEndpointForTenant(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}

	observed := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Microsecond)
	row := func(replica, measurement string, value float64, samples int) domain.OperationalMeasurement {
		unit := ""
		for _, item := range metrics.EngineMeasurementCatalog {
			if item.Name == measurement {
				unit = item.Unit
			}
		}
		return domain.OperationalMeasurement{ReplicaID: replica, Name: measurement, Value: value, Unit: unit, EvidenceClass: "measured", Source: "vllm_metrics", SampleCount: samples, ObservedAt: observed, ValidUntil: observed.Add(90 * time.Second)}
	}
	for _, batch := range [][]domain.OperationalMeasurement{
		{
			row("replica-a", metrics.MeasurementRequestsRunning, 2, 1), row("replica-a", metrics.MeasurementKVCacheUsage, 93, 1),
			row("replica-a", metrics.MeasurementPrefixCacheHitRate, .1, 20000), row("replica-a", metrics.MeasurementPreemptions, 5, 1),
			row("replica-a", metrics.MeasurementITLP95, 90, 5000), row("replica-a", metrics.MeasurementTPOTP95, 100, 100),
		},
		{
			row("replica-b", metrics.MeasurementRequestsRunning, 3, 1), row("replica-b", metrics.MeasurementKVCacheUsage, 40, 1),
			row("replica-b", metrics.MeasurementPrefixCacheHitRate, .6, 30000), row("replica-b", metrics.MeasurementPreemptions, 1, 1),
			row("replica-b", metrics.MeasurementITLP95, 60, 4000),
		},
	} {
		if _, err = s.RecordOperationalMeasurements(ctx, "global", deployment.ID, batch); err != nil {
			t.Fatal(err)
		}
	}

	hash := func(value string) string { return value }
	records := []domain.InferenceRecord{
		{OperationName: "chat", SharedPrefixHash: hash("system-a")},
		{OperationName: "chat", SharedPrefixHash: hash("system-a")},
		{OperationName: "chat", SharedPrefixHash: hash("system-a")},
		{OperationName: "chat", SharedPrefixHash: hash("system-b")},
		{OperationName: "chat"},
		// Unknown prefix shapes stay out of the denominator.
		{OperationName: "embeddings"},
		{OperationName: "chat", ComputeMode: "imported"},
	}
	for index, record := range records {
		record.RequestID = "engine-" + suffix + "-" + string(rune('a'+index))
		record.TenantID, record.DeploymentID, record.RevisionID = "global", deployment.ID, deployment.ActiveRevisionID
		record.LogicalModelID, record.EnvironmentID, record.EndpointID = endpoint.LogicalModel.ID, endpoint.Environment.ID, endpoint.Endpoint.ID
		record.ServingPlanID, record.BindingID = endpoint.ActivePlan.ID, endpoint.Bindings[0].ID
		record.Provider, record.Runtime, record.StatusCode, record.LatencyMS = "existing", "vllm", 200, 400
		record.StartedAt = time.Now().UTC().Add(-time.Duration(index+1) * time.Second)
		if err = s.RecordRequest(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := s.EndpointMonitoring(ctx, "global", name, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]domain.MeasurementEvidence{}
	for _, measurement := range snapshot.Evidence.Measurements {
		byName[measurement.Name] = measurement
	}
	for measurement, want := range map[string]float64{
		metrics.MeasurementRequestsRunning:    5,  // summed across replicas
		metrics.MeasurementKVCacheUsage:       93, // worst replica
		metrics.MeasurementPreemptions:        6,
		metrics.MeasurementPrefixCacheHitRate: .4, // (0.1*20000 + 0.6*30000) / 50000
		metrics.MeasurementITLP95:             90,
		metrics.MeasurementTPOTP95:            100,
		"shared_prefix_request_ratio":         .6, // 3 of 5 known-shape chat requests
	} {
		item := byName[measurement]
		if item.Availability != "available" || item.Value == nil || *item.Value < want-1e-9 || *item.Value > want+1e-9 {
			t.Fatalf("%s=%+v want %v", measurement, item, want)
		}
	}
	if item := byName[metrics.MeasurementPrefixCacheHitRate]; item.SampleCount != 50000 || item.Source != "vllm_metrics" || item.Unit != "ratio" {
		t.Fatalf("weighted ratio envelope=%+v", item)
	}
	if !contains(snapshot.Evidence.Available, metrics.MeasurementTPOTP95) || contains(snapshot.Evidence.Unavailable, metrics.MeasurementTPOTP95) || !contains(snapshot.Evidence.Available, "shared_prefix_request_ratio") || !contains(snapshot.Evidence.Unavailable, metrics.MeasurementDecodeTimeP95) {
		t.Fatalf("available=%v unavailable=%v", snapshot.Evidence.Available, snapshot.Evidence.Unavailable)
	}
	missing := byName[metrics.MeasurementDecodeTimeP95]
	if missing.Availability != "not_observed" || missing.Value != nil || missing.Reason != engineNotReportedReason {
		t.Fatalf("unreported engine signal=%+v", missing)
	}

	// A deployment without an engine scrape keeps every engine signal
	// explicitly unavailable, never zero.
	otherName := name + "-quiet"
	otherTarget := otherName + "-target"
	if _, err = s.AddTarget(ctx, domain.Target{Name: otherTarget, URL: "http://engine-quiet.invalid", Provider: "existing", Runtime: "llamacpp", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyDeployment(ctx, domain.Deployment{Name: otherName, Model: "org/model", Runtime: "llamacpp", MinReplicas: 1, MaxReplicas: 1}, []string{otherTarget}); err != nil {
		t.Fatal(err)
	}
	quiet, err := s.EndpointMonitoring(ctx, "global", otherName, time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range quiet.Evidence.Measurements {
		if !isEngineMeasurement(item.Name) {
			continue
		}
		if item.Availability != "not_observed" || item.Value != nil || item.Reason != engineNotConnectedReason {
			t.Fatalf("absent engine evidence=%+v", item)
		}
	}
	if prefix := measurementByName(quiet.Evidence.Measurements, "shared_prefix_request_ratio"); prefix.Availability != "not_observed" || prefix.Value != nil {
		t.Fatalf("shared prefix without requests=%+v", prefix)
	}

	for _, invalid := range []domain.OperationalMeasurement{
		{Name: metrics.MeasurementITLP95, Value: 12, Unit: "seconds"},
		{Name: metrics.MeasurementPrefixCacheHitRate, Value: 1.5, Unit: "ratio"},
		{Name: metrics.MeasurementKVCacheUsage, Value: 120, Unit: "percent"},
		{Name: metrics.MeasurementPreemptions, Value: -1, Unit: "count"},
	} {
		invalid.EvidenceClass, invalid.Source, invalid.SampleCount = "measured", "vllm_metrics", 1
		invalid.ObservedAt, invalid.ValidUntil = observed.Add(time.Second), observed.Add(time.Minute)
		if _, err = s.RecordOperationalMeasurements(ctx, "global", name, []domain.OperationalMeasurement{invalid}); err == nil {
			t.Fatalf("invalid engine measurement accepted: %+v", invalid)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
