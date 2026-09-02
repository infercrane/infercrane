package domain

import "time"

// HostedModelAPIUsageSnapshot is a bounded, tenant-scoped projection of
// funded hosted Model API supplier attempts. It intentionally excludes
// latency, error, and content fields because the hosted billing journal does
// not record those observations.
type HostedModelAPIUsageSnapshot struct {
	WindowStart   time.Time                   `json:"window_start"`
	WindowEnd     time.Time                   `json:"window_end"`
	BucketSeconds int                         `json:"bucket_seconds"`
	Currency      string                      `json:"currency"`
	Summary       HostedModelAPIUsageSummary  `json:"summary"`
	Models        []HostedModelAPIUsageModel  `json:"models"`
	Series        []HostedModelAPIUsageBucket `json:"series"`
	Evidence      HostedModelAPIUsageEvidence `json:"evidence"`
}

// HostedModelAPIUsageSummary reports the current settlement state of supplier
// attempts transmitted during the requested window. Token totals are null
// when no settled row contains token evidence.
type HostedModelAPIUsageSummary struct {
	TransmittedRequests           int64  `json:"transmitted_requests"`
	SettledRequests               int64  `json:"settled_requests"`
	InFlightRequests              int64  `json:"in_flight_requests"`
	PendingReconciliationRequests int64  `json:"pending_reconciliation_requests"`
	ConfirmedNoChargeRequests     int64  `json:"confirmed_no_charge_requests"`
	TokenUsageSamples             int64  `json:"token_usage_samples"`
	SettlementEntries             int64  `json:"settlement_entries"`
	InputTokens                   *int64 `json:"input_tokens"`
	OutputTokens                  *int64 `json:"output_tokens"`
	SettledSpendMicrousd          int64  `json:"settled_spend_microusd"`
}

type HostedModelAPIUsageModel struct {
	ProductID       string                     `json:"product_id"`
	LatestRequestAt time.Time                  `json:"latest_request_at"`
	Usage           HostedModelAPIUsageSummary `json:"usage"`
}

type HostedModelAPIUsageBucket struct {
	StartedAt time.Time                  `json:"started_at"`
	Usage     HostedModelAPIUsageSummary `json:"usage"`
}

type HostedModelAPIUsageEvidence struct {
	Source                 string     `json:"source"`
	RequestScope           string     `json:"request_scope"`
	LatestRequestAt        *time.Time `json:"latest_request_at"`
	TokenUsageComplete     bool       `json:"token_usage_complete"`
	ReconciliationComplete bool       `json:"reconciliation_complete"`
	ContentRecorded        bool       `json:"content_recorded"`
	Available              []string   `json:"available"`
	Unavailable            []string   `json:"unavailable"`
}
