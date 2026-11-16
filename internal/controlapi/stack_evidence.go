package controlapi

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/stackevidence"
)

var bestKnownStackParameters = map[string]bool{
	"model": true, "goal": true, "input_tokens": true, "output_tokens": true, "gpus": true,
	"max_ttft_p95_ms": true, "min_per_user_output_tokens_per_second": true, "limit": true,
}

// bestKnownStacks ranks cited serving-stack evidence for one model and goal.
// It reads only the embedded reviewed dataset and the current price snapshot;
// it never probes capacity or launches anything.
func (a API) bestKnownStacks(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for key, values := range query {
		if !bestKnownStackParameters[key] || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "unsupported or repeated best-known stacks filter "+strconv.Quote(key))
			return
		}
	}
	request := stackevidence.Query{Model: strings.TrimSpace(query.Get("model")), Goal: query.Get("goal")}
	if request.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "model is required")
		return
	}
	if entry, ok := curatedrecipe.Get(request.Model); ok {
		request.Model = entry.Model
	}
	var err error
	if request.InputTokens, err = optionalInt(query.Get("input_tokens")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "input_tokens must be an integer")
		return
	}
	if request.OutputTokens, err = optionalInt(query.Get("output_tokens")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "output_tokens must be an integer")
		return
	}
	if request.Limit, err = optionalInt(query.Get("limit")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "limit must be an integer")
		return
	}
	if request.MaxTTFTP95Ms, err = optionalFloat(query.Get("max_ttft_p95_ms")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "max_ttft_p95_ms must be a number")
		return
	}
	if request.MinPerUserOutputTokensPerSecond, err = optionalFloat(query.Get("min_per_user_output_tokens_per_second")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "min_per_user_output_tokens_per_second must be a number")
		return
	}
	if gpus := strings.TrimSpace(query.Get("gpus")); gpus != "" {
		request.GPUs = strings.Split(gpus, ",")
	}
	dataset, err := stackevidence.Default()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "benchmark evidence could not be read")
		return
	}
	now := time.Now().UTC()
	var snapshot map[pricing.Request]pricing.Estimate
	if a.GPUPriceCatalog != nil {
		snapshot = a.GPUPriceCatalog.Snapshot()
	}
	result, err := dataset.Rank(request, stackPrices(snapshot, now), now)
	if errors.Is(err, stackevidence.ErrInvalidQuery) {
		writeError(w, http.StatusBadRequest, "invalid_query", strings.TrimPrefix(err.Error(), stackevidence.ErrInvalidQuery.Error()+": "))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "best-known stacks could not be ranked")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// stackPrices returns the cheapest current, complete, one-replica USD price
// for exactly the stack's GPU count, across every provider in the snapshot.
// Accelerator-only components, stale rows and other currencies never count.
func stackPrices(snapshot map[pricing.Request]pricing.Estimate, now time.Time) stackevidence.Prices {
	if len(snapshot) == 0 {
		return nil
	}
	return func(gpu string, gpuCount int) (stackevidence.PriceQuote, bool) {
		var best stackevidence.PriceQuote
		bestKey := ""
		found := false
		for request, estimate := range snapshot {
			if request.GPUCount != gpuCount || request.Replicas != 1 || !provideridentity.MatchesGPU(request.Cloud, request.GPU, gpu) {
				continue
			}
			if !estimate.ComparableTotal() || estimate.Stale(now) || !strings.EqualFold(estimate.Currency, "USD") || estimate.Hourly <= 0 || math.IsNaN(estimate.Hourly) || math.IsInf(estimate.Hourly, 0) {
				continue
			}
			key := request.Cloud + "\x00" + request.Region + "\x00" + estimate.Source
			if found && (estimate.Hourly > best.HourlyUSD || estimate.Hourly == best.HourlyUSD && key >= bestKey) {
				continue
			}
			best = stackevidence.PriceQuote{
				Provider: request.Cloud, Region: request.Region, HourlyUSD: estimate.Hourly, Source: estimate.Source,
				Authority: string(estimate.Authority), ObservedAt: estimate.ObservedAt, ValidUntil: estimate.ObservedAt.Add(estimate.StaleAfter),
			}
			bestKey, found = key, true
		}
		return best, found
	}
}

func optionalInt(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	return strconv.Atoi(strings.TrimSpace(value))
}

func optionalFloat(value string) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, errors.New("not a finite number")
	}
	return parsed, nil
}
