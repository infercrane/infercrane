package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

const migrateUsage = "usage: infercrane migrate capture ENDPOINT [--binding NAME] | migrate price ENDPOINT BINDING --authority list_price|contract --input-usd N --output-usd N [--cached-input-usd N] --valid-until DATE | migrate verdicts RUN | migrate canary ENDPOINT --verdict ID | migrate primary ENDPOINT | migrate rollback ENDPOINT"

// migrateCommand runs the migration Proof from a closed API: capture its
// traffic, record its price, read the verdicts a migration run signed, and
// switch the endpoint in stages with the closed API as fallback.
func migrateCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New(migrateUsage)
	}
	action, resource := args[0], args[1]
	fs := newFlagSet("migrate " + action)
	binding := fs.String("binding", "", "the closed API's binding (capture); default: every binding of the endpoint")
	window := fs.Duration("window", 0, "capture window (default 24h) or Release Guard window for primary (default 1h)")
	maxRequests := fs.Int("max-requests", 1000, "maximum captured requests")
	authority := fs.String("authority", "", "list_price (the supplier's published price) or contract (your own price)")
	inputUSD := fs.String("input-usd", "", "USD per 1M input tokens")
	cachedUSD := fs.String("cached-input-usd", "", "USD per 1M cached input tokens (default: the input price)")
	outputUSD := fs.String("output-usd", "", "USD per 1M output tokens")
	validUntil := fs.String("valid-until", "", "date or RFC 3339 time after which the price is no longer used")
	source := fs.String("source", "", "where the price comes from, such as the supplier's pricing page")
	verdict := fs.String("verdict", "", "the signed PASS migration verdict a canary rests on")
	percent := fs.Float64("percent", 5, "share of traffic the canary sends to your deployment, at most 5")
	output := fs.String("output", "human", "human or json")
	flagArgs := args[2:]
	bindingArg := ""
	if action == "price" {
		if len(args) < 3 || strings.HasPrefix(args[2], "-") {
			return errors.New(migrateUsage)
		}
		bindingArg, flagArgs = args[2], args[3:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(migrateUsage)
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	endpoint := "/api/v1/endpoints/" + url.PathEscape(resource)
	switch action {
	case "capture":
		if *window == 0 {
			*window = 24 * time.Hour
		}
		var response struct {
			Replay map[string]any `json:"replay"`
		}
		body := map[string]any{"window_seconds": int(window.Seconds()), "max_requests": *maxRequests}
		if *binding != "" {
			body["binding"] = *binding
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, endpoint+"/replays", "", body, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Replay)
		}
		fmt.Printf("Captured %v requests from %s (prompts and answers are not stored)\nWorkload fingerprint  sha256:%v\nUse it with: infercrane optimize create MODEL --migrate-from %s/%s --workload-fingerprint sha256:%v --eval-set DIGEST ...\n", response.Replay["request_count"], resource, response.Replay["shape_digest"], resource, emptyAs(*binding, "BINDING"), response.Replay["shape_digest"])
		return nil
	case "price":
		card, err := rateCardBody(*authority, *inputUSD, *cachedUSD, *outputUSD, *validUntil, *source)
		if err != nil {
			return err
		}
		var response struct {
			RateCard map[string]any `json:"rate_card"`
		}
		if err = controlJSON(ctx, cfg, http.MethodPut, endpoint+"/bindings/"+url.PathEscape(bindingArg)+"/rate-card", "", card, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.RateCard)
		}
		fmt.Printf("Recorded the %s rate card for %s/%s, valid until %v\n", strings.ReplaceAll(*authority, "_", " "), resource, bindingArg, response.RateCard["valid_until"])
		return nil
	case "verdicts":
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns/"+url.PathEscape(resource)+"/migration-verdicts", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Data)
		}
		if len(response.Data) == 0 {
			fmt.Println("No signed migration verdicts yet.")
			return nil
		}
		for _, item := range response.Data {
			fmt.Printf("%v  %v  setup %v  current API %v  setup %v\n", item["id"], item["verdict"], item["candidate_id"], scoreText(item["reference_score"]), scoreText(item["candidate_score"]))
		}
		return nil
	case "canary", "primary", "rollback":
		body := map[string]any{"step": action}
		if action == "canary" {
			if *verdict == "" {
				return errors.New("a canary needs --verdict: the signed PASS migration verdict it rests on (see: infercrane migrate verdicts RUN)")
			}
			if *percent <= 0 || *percent > 5 {
				return errors.New("--percent must be greater than 0 and at most 5")
			}
			body["verdict_id"], body["canary_basis_points"] = *verdict, int(math.Round(*percent*100))
		}
		if *window > 0 {
			body["window_seconds"] = int(window.Seconds())
		}
		var response struct {
			Migration struct {
				Step       string         `json:"step"`
				Activated  bool           `json:"activated"`
				Routing    string         `json:"routing"`
				Evaluation map[string]any `json:"evaluation"`
			} `json:"migration"`
		}
		err := controlJSON(ctx, cfg, http.MethodPost, endpoint+"/migration", "", body, &response)
		var apiErr *ControlError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict && json.Unmarshal([]byte(apiErr.Message), &response) == nil && response.Migration.Step != "" {
			// Release Guard decided against the step; its decision is the answer.
			err = nil
		}
		if err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Migration)
		}
		if !response.Migration.Activated {
			return fmt.Errorf("Release Guard decided %v for the %s step; nothing changed. Reasons: %v", response.Migration.Evaluation["decision"], action, response.Migration.Evaluation["reasons"])
		}
		fmt.Printf("%s: %s is live (routes %s)\n", resource, migrationStepWords[action], response.Migration.Routing)
		return nil
	}
	return errors.New(migrateUsage)
}

var migrationStepWords = map[string]string{
	"canary":   "the canary",
	"primary":  "your deployment first, the closed API as fallback",
	"rollback": "the closed API alone",
}

func rateCardBody(authority, input, cached, output, validUntil, source string) (map[string]any, error) {
	if authority != "list_price" && authority != "contract" {
		return nil, errors.New("--authority must be list_price or contract")
	}
	micro := func(name, value string, required bool) (*int64, error) {
		if value == "" {
			if required {
				return nil, fmt.Errorf("--%s is required", name)
			}
			return nil, nil
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, fmt.Errorf("--%s must be a nonnegative USD amount", name)
		}
		result := int64(math.Round(parsed * 1_000_000))
		return &result, nil
	}
	inputMicro, err := micro("input-usd", input, true)
	if err != nil {
		return nil, err
	}
	outputMicro, err := micro("output-usd", output, true)
	if err != nil {
		return nil, err
	}
	cachedMicro, err := micro("cached-input-usd", cached, false)
	if err != nil {
		return nil, err
	}
	until, err := time.Parse(time.RFC3339, validUntil)
	if err != nil {
		if until, err = time.Parse("2006-01-02", validUntil); err != nil {
			return nil, errors.New("--valid-until must be a date (2026-12-31) or an RFC 3339 time")
		}
	}
	body := map[string]any{"authority": authority, "currency": "USD", "input_microusd_per_million": inputMicro, "output_microusd_per_million": outputMicro, "valid_until": until.UTC().Format(time.RFC3339), "source": source}
	if cachedMicro != nil {
		body["cached_input_microusd_per_million"] = cachedMicro
	}
	return body, nil
}

func scoreText(value any) string {
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', 3, 64)
	}
	return "unknown"
}
