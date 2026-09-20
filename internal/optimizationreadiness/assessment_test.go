package optimizationreadiness

import (
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestAssessmentRequiresFreshRepresentativeEvidence(t *testing.T) {
	snapshot := fixtureSnapshot()
	snapshot.Evidence.Fresh = false
	assessment := Assess(snapshot, Policy{})
	if assessment.State != "collecting" || assessment.PrimaryBottleneck != "unknown" || assessment.KernelGate.Eligible || assessment.ProviderMutation {
		t.Fatalf("stale evidence crossed readiness boundary: %+v", assessment)
	}
	snapshot.Evidence.Fresh = true
	snapshot.Summary.Requests, snapshot.Evidence.SampleCount = 5, 5
	assessment = Assess(snapshot, Policy{})
	if assessment.State != "collecting" || assessment.Confidence != "insufficient" {
		t.Fatalf("small sample crossed readiness boundary: %+v", assessment)
	}
}

func TestAssessmentDiagnosesServingStageWithoutClaimingKernel(t *testing.T) {
	tests := []struct {
		name, expected string
		mutate         func(*domain.EndpointMonitoringSnapshot)
	}{
		{"reliability", "reliability", func(s *domain.EndpointMonitoringSnapshot) { value := .05; s.Summary.ErrorRate = &value }},
		{"queue", "queueing-and-admission", func(s *domain.EndpointMonitoringSnapshot) { value := 250.0; s.Summary.P95QueueMS = &value }},
		{"prefill", "prefill-and-first-token", func(s *domain.EndpointMonitoringSnapshot) { value := 500.0; s.Summary.P95TTFTMS = &value }},
		{"decode", "decode-and-generation", func(s *domain.EndpointMonitoringSnapshot) { value := 700.0; s.Summary.P95GenerationMS = &value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := fixtureSnapshot()
			test.mutate(&snapshot)
			assessment := Assess(snapshot, Policy{})
			if assessment.PrimaryBottleneck != test.expected || assessment.KernelGate.Eligible || assessment.ContentRecorded {
				t.Fatalf("assessment=%+v", assessment)
			}
		})
	}
}

func fixtureSnapshot() domain.EndpointMonitoringSnapshot {
	now := time.Now().UTC()
	zero, latency := 0.0, 1000.0
	return domain.EndpointMonitoringSnapshot{
		Endpoint: "coder-production", WindowStart: now.Add(-time.Hour), WindowEnd: now, BucketSeconds: 60,
		Summary:  domain.MonitoringSummary{Requests: 100, ErrorRate: &zero, P95LatencyMS: &latency},
		Evidence: domain.MonitoringEvidence{Source: "infercrane_gateway_request_records", SampleCount: 100, Fresh: true, ContentRecorded: false},
	}
}
