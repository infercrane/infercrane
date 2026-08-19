package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/finops"
)

func finOpsCollectCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 || args[0] != "opencost" || strings.HasPrefix(args[1], "-") {
		return errors.New("usage: infercrane finops collect opencost DEPLOYMENT --allocation NAME --currency CODE [flags]")
	}
	deployment := args[1]
	fs := flag.NewFlagSet("finops collect opencost", flag.ContinueOnError)
	collectorURL := fs.String("url", "http://127.0.0.1:9003/allocation", "OpenCost allocation API URL")
	file := fs.String("file", "", "saved OpenCost allocation response")
	windowText := fs.String("window", "24h", "OpenCost allocation window")
	aggregate := fs.String("aggregate", "controller", "OpenCost allocation aggregation")
	allocationText := fs.String("allocation", "", "comma-separated exact allocation keys")
	currency := fs.String("currency", "", "explicit three-letter currency")
	source := fs.String("source", "opencost/allocation", "cost evidence source")
	ttl := fs.Duration("ttl", time.Hour, "evidence freshness TTL")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || deployment == "" {
		return errors.New("usage: infercrane finops collect opencost DEPLOYMENT --allocation NAME --currency CODE [flags]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	window, err := time.ParseDuration(*windowText)
	if err != nil || window <= 0 || window > 366*24*time.Hour {
		return errors.New("--window must be a positive duration no longer than 366 days")
	}
	allocations := splitExactValues(*allocationText)
	if len(allocations) == 0 {
		return errors.New("--allocation requires at least one exact OpenCost allocation key; cluster-wide cost is never imported implicitly")
	}
	payload, err := readOpenCostPayload(ctx, *collectorURL, *file, *windowText, *aggregate)
	if err != nil {
		return err
	}
	observedAt := time.Now().UTC()
	rows, err := finops.ParseOpenCostAllocation(payload, finops.OpenCostOptions{Allocations: allocations, Currency: *currency, Source: *source, ObservedAt: observedAt, TTL: *ttl})
	if err != nil {
		return err
	}
	values := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		values = append(values, map[string]any{"scope": row.Scope, "resource": row.Resource, "billing_unit": row.BillingUnit, "amount": row.Amount, "window_start": row.WindowStart, "window_end": row.WindowEnd})
	}
	body := map[string]any{"source": *source, "currency": *currency, "evidence_class": "provider_reported", "observed_at": observedAt, "valid_until": observedAt.Add(*ttl), "allocations": values}
	var ingested struct {
		Data              []domain.CostEvidence `json:"data"`
		ContentRecorded   bool                  `json:"content_recorded"`
		CurrencyConverted bool                  `json:"currency_converted"`
	}
	path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/cost-evidence"
	if err = controlJSON(ctx, cfg, http.MethodPost, path, "", body, &ingested); err != nil {
		return err
	}
	var report struct {
		Report map[string]any `json:"report"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(deployment)+"/finops/reports", "", map[string]any{"window_seconds": int(window.Seconds())}, &report); err != nil {
		return fmt.Errorf("cost evidence was recorded but report creation failed: %w", err)
	}
	if *output == "json" {
		return printJSON(map[string]any{"evidence": ingested.Data, "report": report.Report, "content_recorded": false, "currency_converted": false})
	}
	fmt.Printf("Cost evidence recorded for %s\n\n", deployment)
	for _, row := range ingested.Data {
		fmt.Printf("  %-28s %10.4f %s/%s · %s\n", row.Resource, row.Amount, row.Currency, row.BillingUnit, row.Source)
	}
	fmt.Printf("\nFinOps report %s · known hourly rate %s %s · no currency conversion\n", benchmarkValue(report.Report["id"]), benchmarkValue(report.Report["known_cost"]), benchmarkValue(report.Report["currency"]))
	return nil
}

func splitExactValues(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func readOpenCostPayload(ctx context.Context, rawURL, file, window, aggregate string) ([]byte, error) {
	if file != "" {
		info, err := os.Stat(file)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxCollectorPayload {
			return nil, errors.New("OpenCost response exceeds 8 MiB")
		}
		return os.ReadFile(file)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("--url must be an HTTP(S) URL without embedded credentials")
	}
	query := parsed.Query()
	query.Set("window", window)
	query.Set("aggregate", aggregate)
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read OpenCost API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenCost API returned HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxCollectorPayload+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxCollectorPayload {
		return nil, errors.New("OpenCost response exceeds 8 MiB")
	}
	return payload, nil
}
