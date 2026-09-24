package openrouterprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPReceiptLedgerRecordsAndLooksUpScopedCosts(t *testing.T) {
	var recordAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ledger-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case receiptIngestPath:
			if recordAttempts.Add(1) == 1 {
				http.Error(w, "retry", http.StatusServiceUnavailable)
				return
			}
			var body struct {
				Receipt RequestReceipt `json:"receipt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Receipt.SchemaVersion != receiptSchemaVersion || body.Receipt.RecordedAt.IsZero() || body.Receipt.Channel != ChannelHuggingFace {
				t.Fatalf("unprepared receipt: %#v", body.Receipt)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"recorded":true}`))
		case receiptLookupPath:
			var body struct {
				Channel    string   `json:"channel"`
				RequestIDs []string `json:"request_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Channel != ChannelHuggingFace || len(body.RequestIDs) != 1 || body.RequestIDs[0] != "req-1" {
				t.Fatalf("unexpected lookup: %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"requests":[{"requestId":"req-1","costNanoUsd":7100}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ledger, err := NewHTTPReceiptLedger(server.URL, "ledger-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	receipt := RequestReceipt{RequestID: "req-1", Channel: ChannelHuggingFace, Model: "model", UpstreamModel: "upstream", StatusCode: 200, Outcome: "completed", PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8, CostNanoUSD: 7100}
	if err = ledger.Record(receipt); err != nil {
		t.Fatal(err)
	}
	if recordAttempts.Load() != 2 {
		t.Fatalf("record attempts = %d, want 2", recordAttempts.Load())
	}
	costs, err := ledger.Lookup(ChannelHuggingFace, []string{"req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[0].RequestID != "req-1" || costs[0].CostNanoUSD != 7100 {
		t.Fatalf("costs = %#v", costs)
	}
}

func TestHTTPReceiptLedgerRejectsNonTLSRemoteURL(t *testing.T) {
	if _, err := NewHTTPReceiptLedger("http://marketplace.example.com", "secret", nil); err == nil {
		t.Fatal("expected non-loopback plaintext URL to fail")
	}
}

func TestJSONLReceiptLedgerReloadsAndIsolatesChannels(t *testing.T) {
	path := t.TempDir() + "/receipts.ndjson"
	recorder, err := NewJSONLReceiptRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	base := RequestReceipt{RecordedAt: time.Now().UTC(), RequestID: "same-id", Model: "model", UpstreamModel: "upstream", StatusCode: 200, Outcome: "completed", PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
	hf := base
	hf.Channel, hf.CostNanoUSD = ChannelHuggingFace, 200
	openRouter := base
	openRouter.Channel, openRouter.CostNanoUSD = ChannelOpenRouter, 300
	if err = recorder.Record(hf); err != nil {
		t.Fatal(err)
	}
	if err = recorder.Record(openRouter); err != nil {
		t.Fatal(err)
	}
	if err = recorder.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewJSONLReceiptRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	costs, err := reloaded.Lookup(ChannelHuggingFace, []string{"same-id", "same-id", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[0].CostNanoUSD != 200 {
		t.Fatalf("Hugging Face costs = %#v", costs)
	}
	costs, err = reloaded.Lookup(ChannelOpenRouter, []string{"same-id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[0].CostNanoUSD != 300 {
		t.Fatalf("OpenRouter costs = %#v", costs)
	}
}
