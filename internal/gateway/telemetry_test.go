package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelemetryExportsPrometheusHistogram(t *testing.T) {
	telemetry := &Telemetry{}
	handler := telemetry.Observe(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	recorder := httptest.NewRecorder()
	telemetry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, expected := range []string{"infercrane_gateway_request_duration_seconds_bucket", "infercrane_gateway_request_duration_seconds_count 1", "infercrane_gateway_requests_total 1"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
}
