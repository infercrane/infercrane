package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/openrouterprovider"
)

func (s *Store) RecordMarketplaceRequestReceipt(ctx context.Context, receipt openrouterprovider.RequestReceipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	digestReceipt := receipt
	// The observation time is operational metadata, not part of the immutable
	// settlement identity. A retried delivery may legitimately assign it again.
	digestReceipt.RecordedAt = time.Time{}
	encoded, err := json.Marshal(digestReceipt)
	if err != nil {
		return fmt.Errorf("encode marketplace receipt: %w", err)
	}
	digest := sha256.Sum256(encoded)
	digestHex := hex.EncodeToString(digest[:])
	var returned string
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO marketplace_request_receipts(
			channel,request_id,schema_version,model,upstream_model,stream,status_code,outcome,
			duration_ms,time_to_first_token_ms,prompt_tokens,completion_tokens,total_tokens,
			cost_nano_usd,finish_reason,receipt_digest,recorded_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT(channel,request_id) DO UPDATE SET request_id=EXCLUDED.request_id
		WHERE marketplace_request_receipts.receipt_digest=EXCLUDED.receipt_digest
		RETURNING receipt_digest`,
		receipt.Channel, receipt.RequestID, receipt.SchemaVersion, receipt.Model, receipt.UpstreamModel,
		receipt.Stream, receipt.StatusCode, receipt.Outcome, receipt.DurationMS, receipt.TimeToFirstTokenMS,
		receipt.PromptTokens, receipt.CompletionTokens, receipt.TotalTokens, receipt.CostNanoUSD,
		receipt.FinishReason, digestHex, receipt.RecordedAt.UTC(),
	).Scan(&returned)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("marketplace receipt %s/%s conflicts with an existing immutable receipt: %w", receipt.Channel, receipt.RequestID, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("record marketplace receipt: %w", err)
	}
	return nil
}

func (s *Store) MarketplaceRequestCosts(ctx context.Context, channel string, requestIDs []string) ([]openrouterprovider.RequestCost, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" || len(requestIDs) == 0 || len(requestIDs) > 10_000 {
		return nil, errors.New("marketplace channel and 1..10000 request IDs are required")
	}
	seen := make(map[string]struct{}, len(requestIDs))
	ordered := make([]string, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		requestID = strings.TrimSpace(requestID)
		if requestID == "" || len(requestID) > 256 {
			return nil, errors.New("marketplace request ID is invalid")
		}
		if _, duplicate := seen[requestID]; duplicate {
			continue
		}
		seen[requestID] = struct{}{}
		ordered = append(ordered, requestID)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT request_id,cost_nano_usd
		FROM marketplace_request_receipts
		WHERE channel=$1 AND request_id=ANY($2) AND status_code BETWEEN 200 AND 399 AND outcome='completed'`, channel, ordered)
	if err != nil {
		return nil, fmt.Errorf("query marketplace receipt costs: %w", err)
	}
	defer rows.Close()
	byID := make(map[string]openrouterprovider.RequestCost, len(ordered))
	for rows.Next() {
		var cost openrouterprovider.RequestCost
		if err = rows.Scan(&cost.RequestID, &cost.CostNanoUSD); err != nil {
			return nil, err
		}
		byID[cost.RequestID] = cost
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result := make([]openrouterprovider.RequestCost, 0, len(byID))
	for _, requestID := range ordered {
		if cost, ok := byID[requestID]; ok {
			result = append(result, cost)
		}
	}
	return result, nil
}
