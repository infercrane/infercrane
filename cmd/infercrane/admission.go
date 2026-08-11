package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
)

func admissionCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane admission get|set ENDPOINT [flags]")
	}
	action, endpoint := args[0], args[1]
	fs := flag.NewFlagSet("admission "+action, flag.ContinueOnError)
	maxConcurrency := fs.Int("max-concurrency", 32, "maximum concurrently executing requests")
	maxQueue := fs.Int("max-queue", 64, "maximum queued requests")
	queueTimeout := fs.Int("queue-timeout-ms", 5000, "maximum queue wait in milliseconds")
	maxRequestBytes := fs.Int("max-request-bytes", 16<<20, "maximum encoded request size")
	maxOutputTokens := fs.Int("max-output-tokens", 8192, "maximum requested output tokens")
	priorities := fs.String("priorities", "normal", "comma-separated allowed priorities: low,normal,high")
	retryBudget := fs.Int("retry-budget", 0, "bounded inference retry budget")
	disabled := fs.Bool("disabled", false, "store the policy without enforcing it")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || endpoint == "" {
		return errors.New("usage: infercrane admission get|set ENDPOINT [flags]")
	}
	path := "/api/v1/endpoints/" + url.PathEscape(endpoint) + "/admission"
	var response map[string]any
	switch action {
	case "get":
		if err := controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
			return err
		}
	case "set":
		allowed := make([]string, 0, 3)
		for _, priority := range strings.Split(*priorities, ",") {
			priority = strings.TrimSpace(priority)
			if priority != "" {
				allowed = append(allowed, priority)
			}
		}
		request := map[string]any{"max_concurrency": *maxConcurrency, "max_queue_depth": *maxQueue, "queue_timeout_ms": *queueTimeout, "max_request_bytes": *maxRequestBytes, "max_output_tokens": *maxOutputTokens, "allowed_priorities": allowed, "retry_budget": *retryBudget, "enabled": !*disabled}
		if err := controlJSON(ctx, cfg, http.MethodPut, path, "", request, &response); err != nil {
			return err
		}
	default:
		return errors.New("usage: infercrane admission get|set ENDPOINT [flags]")
	}
	if *output == "json" {
		return printJSON(response)
	}
	encoded, _ := json.Marshal(response["policy"])
	fmt.Printf("admission %s · endpoint %s · policy %s\n", action, endpoint, encoded)
	return nil
}
