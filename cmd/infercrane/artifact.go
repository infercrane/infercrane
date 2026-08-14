package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

func artifactCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane artifact inspect|observe|prefetch ARTIFACT_ID [flags]")
	}
	action, artifactID := args[0], args[1]
	fs := flag.NewFlagSet("artifact "+action, flag.ContinueOnError)
	provider := fs.String("provider", "", "provider adapter")
	region := fs.String("region", "", "provider region")
	location := fs.String("location", "", "provider-native cache location")
	state := fs.String("state", "unknown", "present, prefetching, missing, or unknown")
	source := fs.String("source", "operator", "observation source")
	ttl := fs.Duration("ttl", 5*time.Minute, "observation validity")
	idempotencyKey := fs.String("idempotency-key", "", "stable safe-retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected artifact arguments")
	}
	path := "/api/v1/artifacts/" + url.PathEscape(artifactID)
	switch action {
	case "inspect":
		var response struct {
			Observations []map[string]any `json:"observations"`
			Prefetches   []map[string]any `json:"prefetches"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, path+"/cache", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("Artifact cache  %s\n\nOBSERVATIONS\n", artifactID)
		if len(response.Observations) == 0 {
			fmt.Println("  No provider cache observation has been recorded.")
		}
		for _, row := range response.Observations {
			freshness := "expired"
			if expires, err := time.Parse(time.RFC3339, fmt.Sprint(row["expires_at"])); err == nil && expires.After(time.Now()) {
				freshness = "fresh"
			}
			fmt.Printf("  %-12v %-12v %-20v %s · %s\n", row["provider"], row["state"], row["location"], row["source"], freshness)
		}
		fmt.Println("\nPREFETCH INTENTS")
		if len(response.Prefetches) == 0 {
			fmt.Println("  None.")
		}
		for _, row := range response.Prefetches {
			fmt.Printf("  %-12v %-20v %v\n", row["provider"], row["status"], row["location"])
		}
		fmt.Println("\nProvider cache evidence is observational. A requested prefetch is not proof that weights are present; wait for a fresh provider observation.")
		return nil
	case "observe":
		if *provider == "" || *location == "" || *source == "" || *ttl < time.Second || *ttl > 24*time.Hour {
			return errors.New("observe requires --provider, --location, --source, and --ttl between 1s and 24h")
		}
		var response map[string]any
		body := map[string]any{"provider": *provider, "region": *region, "location": *location, "state": *state, "source": *source, "evidence": map[string]any{}, "ttl_seconds": int(ttl.Seconds())}
		if err := controlJSON(ctx, cfg, http.MethodPost, path+"/cache-observations", "", body, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("Cache observation recorded\nArtifact  %s\nProvider  %s\nLocation  %s\nState     %s\nTTL       %s\n", artifactID, *provider, *location, *state, *ttl)
		return nil
	case "prefetch":
		if *provider == "" || *location == "" {
			return errors.New("prefetch requires --provider and --location")
		}
		if *idempotencyKey == "" {
			digest := sha256.Sum256([]byte(artifactID + "\x00" + *provider + "\x00" + *region + "\x00" + *location))
			*idempotencyKey = "artifact-prefetch-" + hex.EncodeToString(digest[:16])
		}
		var response struct {
			Prefetch struct {
				Status              string `json:"status"`
				ProviderOperationID string `json:"provider_operation_id"`
			} `json:"prefetch"`
			Created   bool   `json:"created"`
			Execution string `json:"execution"`
		}
		body := map[string]any{"provider": *provider, "region": *region, "location": *location, "idempotency_key": *idempotencyKey}
		if err := controlJSON(ctx, cfg, http.MethodPost, path+"/prefetches", *idempotencyKey, body, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("Artifact prefetch intent persisted\nArtifact   %s\nProvider   %s\nLocation   %s\nStatus     %s\nExecution  %s\n", artifactID, *provider, *location, response.Prefetch.Status, response.Execution)
		if response.Prefetch.ProviderOperationID != "" {
			fmt.Printf("Provider operation  %s\n", response.Prefetch.ProviderOperationID)
		}
		if response.Execution == "not_configured" {
			fmt.Println("\nNo provider cache adapter is configured here. The durable intent is waiting for an adapter; warming has not started.")
		} else {
			fmt.Println("\nThis is not cache-hit proof. Wait for a fresh provider observation before planning assumes locality.")
		}
		return nil
	default:
		return fmt.Errorf("unknown artifact action %q", action)
	}
}
