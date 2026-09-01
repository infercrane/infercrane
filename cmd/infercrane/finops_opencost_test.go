package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestFinOpsCollectOpenCostUsesExactAllocationAndCreatesReport(t *testing.T) {
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/allocation" || r.URL.Query().Get("window") != "1h" || r.URL.Query().Get("aggregate") != "controller" {
			t.Fatalf("OpenCost request=%s %s query=%v", r.Method, r.URL.Path, r.URL.Query())
		}
		_, _ = io.WriteString(w, `{"code":200,"status":"success","data":[{"coder":{"name":"coder","totalCost":1.25,"window":{"start":"2026-08-19T19:00:00Z","end":"2026-08-19T20:00:00Z"}},"other":{"name":"other","totalCost":99,"window":{"start":"2026-08-19T19:00:00Z","end":"2026-08-19T20:00:00Z"}}}]}`)
	}))
	defer exporter.Close()

	requests := 0
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("auth=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deployments/coder-production/cost-evidence":
			var body struct {
				Currency      string `json:"currency"`
				EvidenceClass string `json:"evidence_class"`
				Allocations   []struct {
					Resource string  `json:"resource"`
					Amount   float64 `json:"amount"`
				} `json:"allocations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Currency != "USD" || body.EvidenceClass != "measured" || len(body.Allocations) != 1 || body.Allocations[0].Resource != "coder" || body.Allocations[0].Amount != 1.25 {
				t.Fatalf("body=%+v err=%v", body, err)
			}
			_, _ = io.WriteString(w, `{"data":[{"resource":"coder","amount":1.25,"currency":"USD","billing_unit":"hour","source":"opencost/allocation"}],"content_recorded":false,"currency_converted":false}`)
		case "/api/v1/deployments/coder-production/finops/reports":
			_, _ = io.WriteString(w, `{"report":{"id":"report-1","known_cost":1.25,"currency":"USD"}}`)
		default:
			t.Fatalf("unexpected control path %s", r.URL.Path)
		}
	}))
	defer control.Close()

	output, err := captureStdout(t, func() error {
		return finOpsCommand(context.Background(), config.Config{ControlURL: control.URL, APIKey: "secret"}, []string{"collect", "opencost", "coder-production", "--url", exporter.URL + "/allocation", "--window", "1h", "--allocation", "coder", "--currency", "USD"})
	})
	if err != nil || requests != 2 || !strings.Contains(output, "1.2500 USD/hour") || !strings.Contains(output, "no currency conversion") {
		t.Fatalf("requests=%d output=%q err=%v", requests, output, err)
	}
}

func TestFinOpsCollectOpenCostRejectsImplicitClusterCostAndCredentials(t *testing.T) {
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	if err := finOpsCommand(context.Background(), cfg, []string{"collect", "opencost", "prod", "--currency", "USD"}); err == nil || !strings.Contains(err.Error(), "cluster-wide") {
		t.Fatalf("implicit allocation err=%v", err)
	}
	if err := finOpsCommand(context.Background(), cfg, []string{"collect", "opencost", "prod", "--allocation", "prod", "--currency", "USD", "--url", "http://user:secret@example.test/allocation"}); err == nil {
		t.Fatal("credential-bearing OpenCost URL was accepted")
	}
}
