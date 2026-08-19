// Package finops builds cost reports without fabricating missing prices or savings.
package finops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

type CostEvidence struct {
	ID            string     `json:"id"`
	Scope         string     `json:"scope"`
	Resource      string     `json:"resource"`
	Source        string     `json:"source"`
	Currency      string     `json:"currency"`
	BillingUnit   string     `json:"billing_unit"`
	EvidenceClass string     `json:"evidence_class"`
	Amount        float64    `json:"amount"`
	ObservedAt    time.Time  `json:"observed_at"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
}
type Report struct {
	Status      string         `json:"status"`
	Currency    string         `json:"currency,omitempty"`
	KnownCost   *float64       `json:"known_cost,omitempty"`
	Avoidable   *float64       `json:"estimated_avoidable_cost,omitempty"`
	Evidence    []CostEvidence `json:"evidence"`
	Missing     []string       `json:"missing"`
	Disclosures []string       `json:"disclosures"`
	InputDigest string         `json:"input_digest"`
}

func Evaluate(now time.Time, evidence []CostEvidence) Report {
	rows := append([]CostEvidence(nil), evidence...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	validRows := make([]CostEvidence, 0, len(rows))
	currencies := map[string]struct{}{}
	for _, row := range rows {
		if row.ID == "" || row.Source == "" || row.Currency == "" || row.Amount < 0 || row.ObservedAt.IsZero() || row.ObservedAt.After(now) || (row.ValidUntil != nil && !row.ValidUntil.After(now)) {
			continue
		}
		validRows = append(validRows, row)
		currencies[row.Currency] = struct{}{}
	}
	encoded, _ := json.Marshal(rows)
	digest := sha256.Sum256(encoded)
	report := Report{Status: "unavailable", Evidence: []CostEvidence{}, InputDigest: hex.EncodeToString(digest[:]), Disclosures: []string{"no savings estimate without measured utilization and a qualified alternative"}}
	if len(currencies) > 1 {
		report.Missing = []string{"single_currency_cost_evidence"}
		report.Disclosures = append(report.Disclosures, "mixed currencies are never converted or silently discarded")
		return report
	}
	latest := map[string]CostEvidence{}
	currency := ""
	for _, row := range validRows {
		currency = row.Currency
		if current, ok := latest[row.Scope]; !ok || row.ObservedAt.After(current.ObservedAt) {
			latest[row.Scope] = row
		}
	}
	valid := make([]CostEvidence, 0, len(latest))
	for _, row := range latest {
		valid = append(valid, row)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].ID < valid[j].ID })
	report.Evidence = valid
	if len(valid) == 0 {
		report.Missing = []string{"sourced_current_cost"}
		return report
	}
	total := 0.0
	for _, row := range valid {
		total += row.Amount
	}
	report.Status = "measured"
	report.Currency = currency
	report.KnownCost = &total
	return report
}
