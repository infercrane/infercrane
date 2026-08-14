package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

func sandboxCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane sandbox connect|list|rotate|revoke [arguments]")
	}
	switch args[0] {
	case "connect":
		return sandboxConnect(ctx, cfg, args[1:])
	case "list":
		return sandboxList(ctx, cfg, args[1:])
	case "rotate":
		return sandboxRotate(ctx, cfg, args[1:])
	case "revoke":
		return sandboxRevoke(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("unknown sandbox action %q", args[0])
	}
}

func sandboxConnect(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("sandbox connect", flag.ContinueOnError)
	provider := fs.String("provider", "", "external sandbox provider or adapter name")
	externalID := fs.String("external-id", "", "externally owned sandbox identity")
	externalRevision := fs.String("external-revision", "", "optional immutable sandbox template or revision")
	endpoint := fs.String("endpoint", "", "single InferCrane endpoint the credential may invoke")
	ttl := fs.Duration("ttl", 30*time.Minute, "credential lifetime between 1m and 24h")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *provider == "" || *externalID == "" || *endpoint == "" || *ttl < time.Minute || *ttl > 24*time.Hour {
		return errors.New("connect requires --provider, --external-id, --endpoint, and --ttl between 1m and 24h")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Reference         map[string]any `json:"reference"`
		Credential        string         `json:"credential"`
		CredentialOnce    bool           `json:"credential_once"`
		CacheSynchronized bool           `json:"credential_cache_synchronized"`
		ExternalMutated   bool           `json:"external_resource_mutated"`
	}
	body := map[string]any{"provider": *provider, "external_id": *externalID, "external_revision": *externalRevision, "endpoint": *endpoint, "ttl_seconds": int(ttl.Seconds()), "metadata": map[string]any{}}
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/sandboxes/references", "", body, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	readiness := "ready on this control-plane instance"
	if !response.CacheSynchronized {
		readiness = "issued; gateway credential cache is converging"
	}
	fmt.Printf("Sandbox access connected\nReference  %v\nProvider   %s\nExternal   %s\nEndpoint   %s\nExpires    %v\nAccess     %s\n\nCredential (shown once)\n%s\n\nInject this credential with the sandbox provider's secret mechanism. InferCrane did not create or mutate the external sandbox.\n", response.Reference["id"], *provider, *externalID, *endpoint, response.Reference["expires_at"], readiness, response.Credential)
	return nil
}

func sandboxList(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("sandbox list", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil {
		return errors.New("usage: infercrane sandbox list [--output human|json]")
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/sandboxes/references", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	if len(response.Data) == 0 {
		fmt.Println("No sandbox references. InferCrane does not discover or create external sandboxes automatically.")
		return nil
	}
	fmt.Println("SANDBOX REFERENCES")
	for _, row := range response.Data {
		fmt.Printf("%-24v %-12v %-24v %-24v %v\n", row["id"], strings.ToUpper(fmt.Sprint(row["status"])), row["provider"], row["endpoint"], row["expires_at"])
	}
	return nil
}

func sandboxRotate(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane sandbox rotate REFERENCE_ID [--output human|json]")
	}
	fs := flag.NewFlagSet("sandbox rotate", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil {
		return errors.New("usage: infercrane sandbox rotate REFERENCE_ID [--output human|json]")
	}
	var response struct {
		ReferenceID       string `json:"reference_id"`
		Credential        string `json:"credential"`
		CredentialOnce    bool   `json:"credential_once"`
		CacheSynchronized bool   `json:"credential_cache_synchronized"`
	}
	path := "/api/v1/sandboxes/references/" + url.PathEscape(args[0]) + "/credential/rotate"
	if err := controlJSON(ctx, cfg, http.MethodPost, path, "", nil, &response); err != nil {
		return err
	}
	response.ReferenceID = args[0]
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Sandbox credential rotated\nReference  %s\nCredential (shown once)\n%s\n", args[0], response.Credential)
	return nil
}

func sandboxRevoke(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane sandbox revoke REFERENCE_ID --yes")
	}
	fs := flag.NewFlagSet("sandbox revoke", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm InferCrane credential revocation")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !*yes || validateOutput(*output) != nil {
		return errors.New("sandbox revoke requires --yes; the external sandbox will not be deleted")
	}
	path := "/api/v1/sandboxes/references/" + url.PathEscape(args[0])
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodDelete, path, "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Sandbox access revoked\nReference  %s\nExternal sandbox  unchanged\n", args[0])
	return nil
}
