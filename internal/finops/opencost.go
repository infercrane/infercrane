package finops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const maxOpenCostPayload = 8 << 20

type OpenCostOptions struct {
	Allocations []string
	Currency    string
	Source      string
	ObservedAt  time.Time
	TTL         time.Duration
}

type openCostWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type openCostAllocation struct {
	Name      string         `json:"name"`
	TotalCost *float64       `json:"totalCost"`
	Window    openCostWindow `json:"window"`
}

// ParseOpenCostAllocation imports only explicitly selected allocation keys
// from a single-window /allocation response. Unknown upstream fields are
// ignored for compatibility, while envelope, selection, cost, and time
// semantics fail closed.
func ParseOpenCostAllocation(body []byte, options OpenCostOptions) ([]domain.CostEvidence, error) {
	if len(body) == 0 || len(body) > maxOpenCostPayload {
		return nil, errors.New("OpenCost response must contain at most 8 MiB")
	}
	if len(options.Allocations) == 0 || len(options.Allocations) > 128 {
		return nil, errors.New("1..128 exact OpenCost allocation keys are required")
	}
	if options.Currency == "" || options.Source == "" || options.ObservedAt.IsZero() || options.TTL <= 0 || options.TTL > 24*time.Hour {
		return nil, errors.New("OpenCost import requires explicit currency, source, observation time, and TTL")
	}
	if len(options.Currency) != 3 || options.Currency != strings.ToUpper(options.Currency) || options.Currency[0] < 'A' || options.Currency[0] > 'Z' || options.Currency[1] < 'A' || options.Currency[1] > 'Z' || options.Currency[2] < 'A' || options.Currency[2] > 'Z' {
		return nil, errors.New("OpenCost currency must be an explicit three-letter uppercase code")
	}
	var envelope struct {
		Code   int                             `json:"code"`
		Status string                          `json:"status"`
		Data   []map[string]openCostAllocation `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode OpenCost allocation response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("OpenCost response contains trailing JSON")
	}
	if envelope.Code != 200 || (envelope.Status != "" && envelope.Status != "success") || len(envelope.Data) != 1 {
		return nil, errors.New("OpenCost must return one successful allocation window")
	}
	requested := append([]string(nil), options.Allocations...)
	sort.Strings(requested)
	for index, name := range requested {
		if strings.TrimSpace(name) == "" || len(name) > 255 || (index > 0 && name == requested[index-1]) {
			return nil, errors.New("OpenCost allocation keys must be unique bounded exact names")
		}
	}
	rows := make([]domain.CostEvidence, 0, len(requested))
	for _, key := range requested {
		allocation, ok := envelope.Data[0][key]
		if !ok {
			return nil, fmt.Errorf("OpenCost allocation %q was not returned", key)
		}
		if allocation.TotalCost == nil || math.IsNaN(*allocation.TotalCost) || math.IsInf(*allocation.TotalCost, 0) || *allocation.TotalCost < 0 {
			return nil, fmt.Errorf("OpenCost allocation %q has no finite non-negative totalCost", key)
		}
		if allocation.Window.Start.IsZero() || !allocation.Window.End.After(allocation.Window.Start) || allocation.Window.End.After(options.ObservedAt.Add(time.Minute)) {
			return nil, fmt.Errorf("OpenCost allocation %q has an invalid or incomplete window", key)
		}
		hours := allocation.Window.End.Sub(allocation.Window.Start).Hours()
		if hours <= 0 || math.IsInf(hours, 0) || math.IsNaN(hours) {
			return nil, fmt.Errorf("OpenCost allocation %q has an invalid duration", key)
		}
		rows = append(rows, domain.CostEvidence{
			Source:        options.Source,
			Scope:         "deployment_hourly_rate/" + key,
			Resource:      key,
			Currency:      options.Currency,
			BillingUnit:   "hour",
			EvidenceClass: "measured",
			Amount:        *allocation.TotalCost / hours,
			WindowStart:   allocation.Window.Start.UTC(),
			WindowEnd:     allocation.Window.End.UTC(),
			ObservedAt:    options.ObservedAt.UTC(),
			ValidUntil:    options.ObservedAt.UTC().Add(options.TTL),
		})
	}
	return rows, nil
}
