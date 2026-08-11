package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"

	"github.com/infercrane/infercrane/internal/config"
)

func alertCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane alert list|configure|evaluate ENDPOINT [flags]")
	}
	action, endpoint := args[0], args[1]
	fs := flag.NewFlagSet("alert "+action, flag.ContinueOnError)
	name := fs.String("name", "operations", "alert policy name")
	webhook := fs.String("webhook", "", "HTTPS webhook URL")
	secretReference := fs.String("secret-reference", "", "signing secret reference ID")
	minimumSeverity := fs.String("minimum-severity", "warning", "info, warning, or critical")
	maxAttempts := fs.Int("max-attempts", 3, "bounded delivery attempts")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || endpoint == "" {
		return errors.New("usage: infercrane alert list|configure|evaluate ENDPOINT [flags]")
	}
	path := "/api/v1/endpoints/" + url.PathEscape(endpoint) + "/alerts"
	var response map[string]any
	switch action {
	case "list":
		if err := controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
			return err
		}
	case "configure":
		if *webhook == "" || *secretReference == "" {
			return errors.New("alert configure requires --webhook and --secret-reference")
		}
		request := map[string]any{"name": *name, "webhook_url": *webhook, "secret_reference_id": *secretReference, "minimum_severity": *minimumSeverity, "enabled": true, "max_attempts": *maxAttempts}
		if err := controlJSON(ctx, cfg, http.MethodPost, path, "", request, &response); err != nil {
			return err
		}
	case "evaluate":
		if err := controlJSON(ctx, cfg, http.MethodPost, path+"/evaluate", "", map[string]any{}, &response); err != nil {
			return err
		}
	default:
		return errors.New("usage: infercrane alert list|configure|evaluate ENDPOINT [flags]")
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("alert %s completed for %s\n", action, endpoint)
	return nil
}
