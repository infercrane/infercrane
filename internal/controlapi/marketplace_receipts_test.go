package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/openrouterprovider"
)

type fakeMarketplaceReceiptStore struct {
	*fakeStore
	receipt openrouterprovider.RequestReceipt
	costs   []openrouterprovider.RequestCost
	channel string
	ids     []string
}

func (f *fakeMarketplaceReceiptStore) RecordMarketplaceRequestReceipt(_ context.Context, receipt openrouterprovider.RequestReceipt) error {
	f.receipt = receipt
	return nil
}

func (f *fakeMarketplaceReceiptStore) MarketplaceRequestCosts(_ context.Context, channel string, requestIDs []string) ([]openrouterprovider.RequestCost, error) {
	f.channel = channel
	f.ids = append([]string(nil), requestIDs...)
	return append([]openrouterprovider.RequestCost(nil), f.costs...), nil
}

func TestMarketplaceReceiptControlAPI(t *testing.T) {
	store := &fakeMarketplaceReceiptStore{fakeStore: &fakeStore{}, costs: []openrouterprovider.RequestCost{{RequestID: "req-1", CostNanoUSD: 7100}}}
	handler := (API{Store: store, APIKey: "secret", ModelAPIOperatorTenantID: "global"}).Handler()
	receipt := openrouterprovider.RequestReceipt{
		SchemaVersion: "infercrane.dev/marketplace-request-receipt/v1", RecordedAt: time.Now().UTC(),
		RequestID: "req-1", Channel: openrouterprovider.ChannelHuggingFace,
		Model: "qwen/qwen3.8-27b", UpstreamModel: "Qwen/Qwen3.8-27B-FP8",
		StatusCode: 200, Outcome: "completed", PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8, CostNanoUSD: 7100,
	}
	body, _ := json.Marshal(map[string]any{"receipt": receipt})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/marketplace/receipts", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.receipt.RequestID != "req-1" {
		t.Fatalf("record status=%d body=%s receipt=%#v", response.Code, response.Body, store.receipt)
	}

	body, _ = json.Marshal(map[string]any{"channel": openrouterprovider.ChannelHuggingFace, "request_ids": []string{"req-1", "missing"}})
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/marketplace/billing/requests", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.channel != openrouterprovider.ChannelHuggingFace || len(store.ids) != 2 {
		t.Fatalf("lookup status=%d body=%s channel=%q ids=%#v", response.Code, response.Body, store.channel, store.ids)
	}
	var result struct {
		Requests []openrouterprovider.RequestCost `json:"requests"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Requests) != 1 || result.Requests[0].CostNanoUSD != 7100 {
		t.Fatalf("lookup result = %#v", result.Requests)
	}
}
