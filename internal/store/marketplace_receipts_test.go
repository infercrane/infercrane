package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/openrouterprovider"
)

func TestMarketplaceRequestReceiptsAreImmutableIdempotentAndChannelScoped(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	receipt := openrouterprovider.RequestReceipt{
		SchemaVersion: "infercrane.dev/marketplace-request-receipt/v1",
		RecordedAt:    time.Now().UTC(), RequestID: "req-shared", Channel: openrouterprovider.ChannelHuggingFace,
		Model: "qwen/qwen3.8-27b", UpstreamModel: "Qwen/Qwen3.8-27B-FP8",
		StatusCode: 200, Outcome: "completed", PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8, CostNanoUSD: 7100,
	}
	if err := s.RecordMarketplaceRequestReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMarketplaceRequestReceipt(ctx, receipt); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	conflict := receipt
	conflict.CostNanoUSD++
	if err := s.RecordMarketplaceRequestReceipt(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay error = %v, want conflict", err)
	}
	costs, err := s.MarketplaceRequestCosts(ctx, openrouterprovider.ChannelHuggingFace, []string{"req-shared", "req-shared", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[0].CostNanoUSD != 7100 {
		t.Fatalf("Hugging Face costs = %#v", costs)
	}
	costs, err = s.MarketplaceRequestCosts(ctx, openrouterprovider.ChannelOpenRouter, []string{"req-shared"})
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 0 {
		t.Fatalf("cross-channel costs leaked: %#v", costs)
	}
}
