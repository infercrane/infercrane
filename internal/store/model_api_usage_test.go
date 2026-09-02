package store

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestHostedModelAPIUsageFilterAlwaysScopesTenantAndPublicProduct(t *testing.T) {
	start, end := time.Now().Add(-time.Hour), time.Now()
	query, args := hostedModelAPIUsageFilter("tenant-a", "glm-5.2", start, end)
	if !strings.Contains(query, "r.customer_tenant_id=?") || !strings.Contains(query, "r.product_id=?") {
		t.Fatalf("usage query is not tenant and product scoped: %s", query)
	}
	if len(args) != 4 || args[0] != "tenant-a" || args[3] != "glm-5.2" {
		t.Fatalf("unexpected filter args: %#v", args)
	}
	if !strings.Contains(hostedModelAPIUsageAggregates, "COUNT(l.id)") || !strings.Contains(hostedModelAPIUsageAggregates, "SUM(-l.amount_microusd)") {
		t.Fatalf("settled spend must come from settlement ledger evidence: %s", hostedModelAPIUsageAggregates)
	}
}

func TestHostedModelAPIUsageValidationIsBounded(t *testing.T) {
	for _, test := range []struct {
		name            string
		tenant, product string
		window, bucket  time.Duration
	}{
		{"missing tenant", "", "", time.Hour, time.Minute},
		{"untrimmed product", "tenant", " model", time.Hour, time.Minute},
		{"long window", "tenant", "", 31 * 24 * time.Hour, time.Hour},
		{"too many buckets", "tenant", "", 24 * time.Hour, time.Minute},
	} {
		if err := validateHostedModelAPIUsageQuery(test.tenant, test.product, test.window, test.bucket); err == nil {
			t.Fatalf("%s unexpectedly passed validation", test.name)
		}
	}
	if err := validateHostedModelAPIUsageQuery("tenant", "glm-5.2", 24*time.Hour, 15*time.Minute); err != nil {
		t.Fatalf("valid query failed: %v", err)
	}
}

func TestMergeHostedModelAPIUsagePreservesUnknownTokens(t *testing.T) {
	var total domain.HostedModelAPIUsageSummary
	mergeHostedModelAPIUsage(&total, domain.HostedModelAPIUsageSummary{TransmittedRequests: 1, PendingReconciliationRequests: 1})
	if total.InputTokens != nil || total.OutputTokens != nil {
		t.Fatalf("unknown token totals became zero: %#v", total)
	}
	input, output := int64(100), int64(25)
	mergeHostedModelAPIUsage(&total, domain.HostedModelAPIUsageSummary{TransmittedRequests: 1, SettledRequests: 1, TokenUsageSamples: 1, SettlementEntries: 1, InputTokens: &input, OutputTokens: &output, SettledSpendMicrousd: 99})
	if total.InputTokens == nil || *total.InputTokens != 100 || total.OutputTokens == nil || *total.OutputTokens != 25 || total.SettledSpendMicrousd != 99 {
		t.Fatalf("settled evidence did not merge: %#v", total)
	}
}
