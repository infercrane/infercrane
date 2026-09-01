package finops

import (
	"strings"
	"testing"
	"time"
)

func TestParseOpenCostAllocationRequiresExplicitExactSelection(t *testing.T) {
	now := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)
	body := []byte(`{"code":200,"status":"success","data":[{"other":{"name":"other","totalCost":9,"window":{"start":"2026-08-19T19:00:00Z","end":"2026-08-19T20:00:00Z"}},"inference":{"name":"inference","totalCost":1.25,"window":{"start":"2026-08-19T19:00:00Z","end":"2026-08-19T20:00:00Z"}}}]}`)
	rows, err := ParseOpenCostAllocation(body, OpenCostOptions{Allocations: []string{"inference"}, Currency: "USD", Source: "opencost/allocation", ObservedAt: now, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Amount != 1.25 || rows[0].Resource != "inference" || rows[0].Currency != "USD" || rows[0].EvidenceClass != "measured" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	if _, err = ParseOpenCostAllocation(body, OpenCostOptions{Allocations: []string{"missing"}, Currency: "USD", Source: "opencost/allocation", ObservedAt: now, TTL: time.Hour}); err == nil {
		t.Fatal("expected exact missing allocation to fail closed")
	}
}

func TestParseOpenCostAllocationRejectsUnsafeOrAmbiguousResponses(t *testing.T) {
	now := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)
	options := OpenCostOptions{Allocations: []string{"inference"}, Currency: "USD", Source: "opencost/allocation", ObservedAt: now, TTL: time.Hour}
	cases := [][]byte{
		[]byte(`{"code":500,"data":[]}`),
		[]byte(`{"code":200,"data":[]}`),
		[]byte(`{"code":200,"data":[{},{}]}`),
		[]byte(`{"code":200,"data":[{"inference":{"totalCost":-1,"window":{"start":"2026-08-19T19:00:00Z","end":"2026-08-19T20:00:00Z"}}}]}`),
		append([]byte(`{"code":200,"data":[{}]}`), []byte(` {}`)...),
		[]byte(strings.Repeat("x", maxOpenCostPayload+1)),
	}
	for index, body := range cases {
		if _, err := ParseOpenCostAllocation(body, options); err == nil {
			t.Fatalf("case %d unexpectedly passed", index)
		}
	}
}
