package apicontract

import "testing"

func TestHostedModelAPIUsageContractIsBoundedAndDoesNotClaimPerformance(t *testing.T) {
	doc, err := Document()
	if err != nil {
		t.Fatal(err)
	}
	operation := doc["paths"].(map[string]any)["/api/v1/model-api-usage"].(map[string]any)["get"].(map[string]any)
	parameters, ok := operation["parameters"].([]map[string]any)
	if !ok {
		t.Fatalf("hosted usage query parameters missing: %#v", operation["parameters"])
	}
	found := map[string]bool{}
	for _, parameter := range parameters {
		found[parameter["name"].(string)] = true
	}
	for _, name := range []string{"window_seconds", "bucket_seconds", "model"} {
		if !found[name] {
			t.Fatalf("hosted usage query parameter %q missing", name)
		}
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	summary := schemas["HostedModelAPIUsageSummary"].(map[string]any)["properties"].(map[string]any)
	for _, unavailable := range []string{"errors", "error_rate", "latency", "p95_latency_ms", "ttft", "p95_ttft_ms"} {
		if summary[unavailable] != nil {
			t.Fatalf("hosted billing evidence must not claim %q", unavailable)
		}
	}
	for _, required := range []string{"transmitted_requests", "pending_reconciliation_requests", "input_tokens", "settled_spend_microusd"} {
		if summary[required] == nil {
			t.Fatalf("hosted usage schema missing %q", required)
		}
	}
}
