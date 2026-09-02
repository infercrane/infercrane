package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const hostedModelAPIUsageAggregates = `COUNT(*),COUNT(*) FILTER(WHERE r.state='settled'),COUNT(*) FILTER(WHERE r.state IN ('transmitted','response_started')),COUNT(*) FILTER(WHERE r.state='pending_reconciliation'),COUNT(*) FILTER(WHERE r.state='released'),COUNT(*) FILTER(WHERE r.input_tokens IS NOT NULL AND r.output_tokens IS NOT NULL),COUNT(l.id),COALESCE(SUM(r.input_tokens),0),COALESCE(SUM(r.output_tokens),0),COALESCE(SUM(-l.amount_microusd),0)`

// HostedModelAPIUsage returns billing-journal evidence for supplier attempts
// transmitted in the requested window. Supplier identities and private route
// data never cross this customer-facing boundary.
func (s *Store) HostedModelAPIUsage(ctx context.Context, tenant, product string, window, bucket time.Duration) (domain.HostedModelAPIUsageSnapshot, error) {
	if err := validateHostedModelAPIUsageQuery(tenant, product, window, bucket); err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}
	end := time.Now().UTC()
	start := end.Add(-window)
	out := domain.HostedModelAPIUsageSnapshot{
		WindowStart: start, WindowEnd: end, BucketSeconds: int(bucket.Seconds()), Currency: "USD",
		Models: []domain.HostedModelAPIUsageModel{}, Series: []domain.HostedModelAPIUsageBucket{},
		Evidence: domain.HostedModelAPIUsageEvidence{
			Source: "model_api_usage_reservations+model_api_usage_ledger", RequestScope: "funded_supplier_attempts",
			ContentRecorded: false, Available: []string{"requests", "reconciliation_state", "settled_spend"},
			Unavailable: []string{"errors", "latency", "ttft"},
		},
	}
	filter, args := hostedModelAPIUsageFilter(tenant, product, start, end)
	rows, err := s.QueryContext(ctx, `SELECT r.product_id,`+hostedModelAPIUsageAggregates+`,MAX(r.transmitted_at) FROM model_api_usage_reservations r LEFT JOIN model_api_usage_ledger l ON l.customer_tenant_id=r.customer_tenant_id AND l.reservation_id=r.id AND l.kind='settlement' `+filter+` GROUP BY r.product_id ORDER BY r.product_id`, args...)
	if err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}
	for rows.Next() {
		var item domain.HostedModelAPIUsageModel
		var inputTokens, outputTokens int64
		if err = rows.Scan(&item.ProductID, &item.Usage.TransmittedRequests, &item.Usage.SettledRequests, &item.Usage.InFlightRequests, &item.Usage.PendingReconciliationRequests, &item.Usage.ConfirmedNoChargeRequests, &item.Usage.TokenUsageSamples, &item.Usage.SettlementEntries, &inputTokens, &outputTokens, &item.Usage.SettledSpendMicrousd, &item.LatestRequestAt); err != nil {
			rows.Close()
			return domain.HostedModelAPIUsageSnapshot{}, err
		}
		item.LatestRequestAt = item.LatestRequestAt.UTC()
		setHostedModelAPITokenTotals(&item.Usage, inputTokens, outputTokens)
		mergeHostedModelAPIUsage(&out.Summary, item.Usage)
		if out.Evidence.LatestRequestAt == nil || item.LatestRequestAt.After(*out.Evidence.LatestRequestAt) {
			latest := item.LatestRequestAt
			out.Evidence.LatestRequestAt = &latest
		}
		out.Models = append(out.Models, item)
	}
	if err = rows.Close(); err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}
	if err = rows.Err(); err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}

	seriesArgs := append([]any{int(bucket.Seconds())}, args...)
	rows, err = s.QueryContext(ctx, `SELECT date_bin((? * INTERVAL '1 second'),r.transmitted_at,TIMESTAMPTZ '1970-01-01'),`+hostedModelAPIUsageAggregates+` FROM model_api_usage_reservations r LEFT JOIN model_api_usage_ledger l ON l.customer_tenant_id=r.customer_tenant_id AND l.reservation_id=r.id AND l.kind='settlement' `+filter+` GROUP BY 1 ORDER BY 1`, seriesArgs...)
	if err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}
	series := make(map[int64]domain.HostedModelAPIUsageBucket)
	for rows.Next() {
		var item domain.HostedModelAPIUsageBucket
		var inputTokens, outputTokens int64
		if err = rows.Scan(&item.StartedAt, &item.Usage.TransmittedRequests, &item.Usage.SettledRequests, &item.Usage.InFlightRequests, &item.Usage.PendingReconciliationRequests, &item.Usage.ConfirmedNoChargeRequests, &item.Usage.TokenUsageSamples, &item.Usage.SettlementEntries, &inputTokens, &outputTokens, &item.Usage.SettledSpendMicrousd); err != nil {
			rows.Close()
			return domain.HostedModelAPIUsageSnapshot{}, err
		}
		item.StartedAt = item.StartedAt.UTC()
		setHostedModelAPITokenTotals(&item.Usage, inputTokens, outputTokens)
		series[item.StartedAt.Unix()] = item
	}
	if err = rows.Close(); err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}
	if err = rows.Err(); err != nil {
		return domain.HostedModelAPIUsageSnapshot{}, err
	}
	seconds := int64(bucket.Seconds())
	first := time.Unix((start.Unix()/seconds)*seconds, 0).UTC()
	last := time.Unix((end.Unix()/seconds)*seconds, 0).UTC()
	for stamp := first; !stamp.After(last); stamp = stamp.Add(bucket) {
		item, ok := series[stamp.Unix()]
		if !ok {
			item.StartedAt = stamp
		}
		out.Series = append(out.Series, item)
	}
	out.Evidence.TokenUsageComplete = out.Summary.TransmittedRequests == out.Summary.TokenUsageSamples+out.Summary.ConfirmedNoChargeRequests
	out.Evidence.ReconciliationComplete = out.Evidence.TokenUsageComplete && out.Summary.SettledRequests == out.Summary.SettlementEntries
	if out.Summary.TokenUsageSamples > 0 {
		out.Evidence.Available = append(out.Evidence.Available, "tokens")
	} else {
		out.Evidence.Unavailable = append(out.Evidence.Unavailable, "tokens")
	}
	return out, nil
}

func validateHostedModelAPIUsageQuery(tenant, product string, window, bucket time.Duration) error {
	if strings.TrimSpace(tenant) == "" {
		return errors.New("tenant is required")
	}
	if product != strings.TrimSpace(product) || len(product) > 256 {
		return errors.New("model filter must be trimmed and at most 256 characters")
	}
	if window < time.Minute || window > 30*24*time.Hour || bucket < time.Minute || bucket > 24*time.Hour || bucket > window || (window+bucket-1)/bucket+1 > 500 {
		return errors.New("hosted Model API usage requires a 1 minute..30 day window, a 1 minute..24 hour bucket, and at most 500 buckets")
	}
	return nil
}

func hostedModelAPIUsageFilter(tenant, product string, start, end time.Time) (string, []any) {
	query := `WHERE r.customer_tenant_id=? AND r.transmitted_at IS NOT NULL AND r.transmitted_at>=? AND r.transmitted_at<=?`
	args := []any{tenant, start, end}
	if product != "" {
		query += ` AND r.product_id=?`
		args = append(args, product)
	}
	return query, args
}

func setHostedModelAPITokenTotals(summary *domain.HostedModelAPIUsageSummary, input, output int64) {
	if summary.TokenUsageSamples == 0 {
		return
	}
	summary.InputTokens, summary.OutputTokens = &input, &output
}

func mergeHostedModelAPIUsage(total *domain.HostedModelAPIUsageSummary, item domain.HostedModelAPIUsageSummary) {
	total.TransmittedRequests += item.TransmittedRequests
	total.SettledRequests += item.SettledRequests
	total.InFlightRequests += item.InFlightRequests
	total.PendingReconciliationRequests += item.PendingReconciliationRequests
	total.ConfirmedNoChargeRequests += item.ConfirmedNoChargeRequests
	total.TokenUsageSamples += item.TokenUsageSamples
	total.SettlementEntries += item.SettlementEntries
	total.SettledSpendMicrousd += item.SettledSpendMicrousd
	if item.InputTokens != nil && item.OutputTokens != nil {
		if total.InputTokens == nil {
			input, output := int64(0), int64(0)
			total.InputTokens, total.OutputTokens = &input, &output
		}
		*total.InputTokens += *item.InputTokens
		*total.OutputTokens += *item.OutputTokens
	}
}
