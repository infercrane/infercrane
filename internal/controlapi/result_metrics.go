package controlapi

import (
	"context"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/resultmetrics"
)

type resultMetricsStore interface {
	ResultMetrics(context.Context, string) (resultmetrics.Snapshot, error)
}

// resultMetrics serves the caller's workspace results as Prometheus series,
// or as JSON when the client asks for application/json (the SDKs do). It
// reads persisted records on the control path only; the inference gateway
// never calls it. The gateway's unauthenticated /metrics stays free of
// workspace names: these series name deployments and endpoints, so they need
// a read credential and cover one workspace.
func (a API) resultMetrics(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(resultMetricsStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "result metrics are not configured")
		return
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "metrics accepts no query parameters")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	snapshot, err := store.ResultMetrics(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "workspace results could not be read")
		return
	}
	if reports, ok := a.Store.(optimizationreport.Store); ok {
		a.ResultReports.apply(r.Context(), reports, actor.TenantID, snapshot.Campaigns)
	}
	if acceptsJSON(r.Header.Get("Accept")) {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = resultmetrics.WritePrometheus(w, snapshot)
}

// acceptsJSON is true when the Accept header names application/json.
// Prometheus asks for the text or OpenMetrics formats and never for JSON, so
// a scrape gets the text format; so does a client that sends no preference.
func acceptsJSON(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err == nil && mediaType == "application/json" {
			return true
		}
	}
	return false
}

const (
	// resultReportReadsPerScrape bounds how many optimization run reports
	// one scrape assembles. Runs whose report is not read yet have no
	// outcome or cost series until a later scrape reads them.
	resultReportReadsPerScrape = 4
	resultReportCacheEntries   = 4096
)

// ResultReports remembers the outcome and costs of optimization run reports
// the metrics route has read, keyed by run and last change, so a scrape does
// not reassemble a report that has not changed. A nil ResultReports reads no
// reports: outcome and cost series are then absent, never zero.
type ResultReports struct {
	mu      sync.Mutex
	entries map[string]resultReport
}

type resultReport struct {
	outcome          *string
	baseline, chosen *float64
}

func (c *ResultReports) apply(ctx context.Context, store optimizationreport.Store, tenant string, campaigns []resultmetrics.Campaign) {
	if c == nil {
		return
	}
	reads := 0
	for i := range campaigns {
		campaign := &campaigns[i]
		key := tenant + "\x00" + campaign.ID + "\x00" + campaign.UpdatedAt.UTC().Format(time.RFC3339Nano)
		summary, found := c.get(key)
		if !found {
			if reads >= resultReportReadsPerScrape {
				continue
			}
			reads++
			report, err := optimizationreport.Load(ctx, store, tenant, campaign.ID)
			if err != nil {
				// Unknown stays unknown; a later scrape tries again.
				continue
			}
			summary = summarizeReport(report)
			c.put(key, summary)
		}
		campaign.Outcome, campaign.BaselineCostPerMillionOutputTokensUSD, campaign.ChosenCostPerMillionOutputTokensUSD = summary.outcome, summary.baseline, summary.chosen
	}
}

func (c *ResultReports) get(key string) (resultReport, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	summary, found := c.entries[key]
	return summary, found
}

func (c *ResultReports) put(key string, summary resultReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= resultReportCacheEntries {
		c.entries = make(map[string]resultReport)
	}
	c.entries[key] = summary
}

// summarizeReport keeps the report's outcome and its cost per million output
// tokens for the baseline and the chosen setup. When the run kept the
// baseline, the chosen setup is the baseline and the report gives both the
// same value.
func summarizeReport(report optimizationreport.Report) resultReport {
	var summary resultReport
	if resultmetrics.Listed(resultmetrics.CampaignOutcomes, report.Outcome.Kind) {
		outcome := report.Outcome.Kind
		summary.outcome = &outcome
	}
	for _, metric := range report.Metrics {
		if metric.Key == optimizationreport.MetricCostPerMillion {
			summary.baseline, summary.chosen = copyFloat(metric.Baseline), copyFloat(metric.Winner)
		}
	}
	return summary
}

func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
