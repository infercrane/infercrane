package openrouterportfolio

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSnapshotJoinsDemandWithOpenWeightCatalogAndEndpointPairs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/models":
			fmt.Fprint(w, `{"data":[`+
				`{"id":"org/open","canonical_slug":"org/open-v1","hugging_face_id":"hf/open","context_length":1000,"architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.000001","completion":"0.000002"},"supported_parameters":["tools"]},`+
				`{"id":"org/open:batch","canonical_slug":"org/open-batch-v1","hugging_face_id":"hf/open","architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.000001","completion":"0.000002"}},`+
				`{"id":"org/closed","canonical_slug":"org/closed-v1","hugging_face_id":"","architecture":{"output_modalities":["text"]},"pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`)
		case "/api/v1/datasets/rankings-daily":
			if request.Header.Get("Authorization") != "Bearer secret" || request.URL.Query().Get("start_date") == "" {
				t.Fatalf("missing authenticated bounded rankings request: %+v", request)
			}
			fmt.Fprint(w, `{"data":[`+
				`{"date":"2026-09-21","model_permaslug":"org/open-v1","total_tokens":"100"},`+
				`{"date":"2026-09-22","model_permaslug":"org/open-v1","total_tokens":"300"},`+
				`{"date":"2026-09-22","model_permaslug":"org/open-batch-v1","total_tokens":"99999"},`+
				`{"date":"2026-09-22","model_permaslug":"org/closed-v1","total_tokens":"9999"}]}`)
		case "/api/v1/models/org/open/endpoints":
			fmt.Fprint(w, `{"data":{"endpoints":[`+
				`{"provider_name":"cheap-output","pricing":{"prompt":"0.0000002","completion":"0.000001"},"uptime_last_30m":99.5},`+
				`{"provider_name":"cheap-input","pricing":{"prompt":"0.00000001","completion":"0.000002"},"uptime_last_30m":100}`+
				`]}}`)
		case "/api/models/hf/open":
			fmt.Fprint(w, `{"safetensors":{"total":150323855360}}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	client := Client{BaseURL: server.URL, HuggingFaceBaseURL: server.URL, APIKey: "secret", Now: func() time.Time { return now }}
	markets, err := client.Snapshot(t.Context(), now.Add(-48*time.Hour), now.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(markets) != 1 {
		t.Fatalf("expected only open-weight model, got %+v", markets)
	}
	market := markets[0]
	if market.DailyTotalTokens != 200 || market.ProviderCount != 2 || len(market.CompetitorOffers) != 2 || market.BestUptimePercent != 100 {
		t.Fatalf("unexpected joined market: %+v", market)
	}
	if market.ParameterCount != 150323855360 || market.MinimumH10080GBCount != 2 {
		t.Fatalf("unexpected model footprint: %+v", market)
	}
	if math.Abs(market.FloorInputUSDPerMillion-0.2) > 1e-9 || market.FloorOutputUSDPerMillion != 1 {
		t.Fatalf("floor must remain one purchasable provider pair: %+v", market)
	}
	input, output := competitivePrice(market, 20)
	if math.Abs(input-0.01) > 1e-9 || output != 2 {
		t.Fatalf("workload-aware competitive pair was not selected: input=%v output=%v", input, output)
	}
}

func TestSnapshotDoesNotSendCredentialToInsecureRemoteHost(t *testing.T) {
	client := Client{BaseURL: "http://example.com", APIKey: "secret"}
	_, err := client.Snapshot(t.Context(), time.Now(), time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected insecure host rejection, got %v", err)
	}
}
