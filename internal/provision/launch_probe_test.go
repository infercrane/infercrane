package provision

import (
	"context"
	"testing"
	"time"
)

func TestConfiguredLaunchProbeDoesNotClaimStockQuotaOrDeployability(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	evidence, err := (ConfiguredLaunchProbe{Provider: "lambda", Now: func() time.Time { return now }}).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "lambda", GPU: "H100", GPUCount: 8})
	if err != nil || evidence.ConnectionState != "configured" || evidence.AvailabilityState != "unknown" || evidence.QuotaState != "unknown" || evidence.Deployability != "unknown" || !evidence.ObservedAt.Equal(now) || len(evidence.Limitations) == 0 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}
