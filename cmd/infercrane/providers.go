package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/infercrane/infercrane/internal/config"
)

type providerConnectionView struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Adapter             string `json:"adapter"`
	Target              string `json:"target"`
	SecretReferenceID   string `json:"secret_reference_id"`
	SecretReferenceName string `json:"secret_reference"`
}

func providerCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("provider requires connect, list, or delete")
	}
	action := args[0]
	fs := flag.NewFlagSet("provider "+action, flag.ContinueOnError)
	adapter := fs.String("adapter", "openrouter", "openrouter or openai-compatible-external")
	providerURL := fs.String("url", "", "provider OpenAI-compatible base URL")
	model := fs.String("model", "", "provider model identifier")
	secretReference := fs.String("secret-reference", "", "existing InferCrane secret reference ID")
	fromEnv := fs.String("from-env", "", "control-plane environment variable containing the provider credential")
	output := fs.String("output", "human", "human or json")
	name := ""
	flagArgs := args[1:]
	if action == "connect" || action == "delete" {
		if len(args) > 1 {
			name, flagArgs = args[1], args[2:]
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	switch action {
	case "list":
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane provider list [--output human|json]")
		}
		var response struct {
			Data []providerConnectionView `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/provider-connections", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Data)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tADAPTER\tMODEL TARGET\tCREDENTIAL")
		for _, item := range response.Data {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Name, item.Adapter, item.Target, item.SecretReferenceName)
		}
		return w.Flush()
	case "connect":
		if name == "" || fs.NArg() != 0 || *model == "" || (*secretReference == "") == (*fromEnv == "") {
			return errors.New("usage: infercrane provider connect NAME --model MODEL (--from-env VARIABLE | --secret-reference ID) [--adapter openrouter|openai-compatible-external --url URL]")
		}
		if *adapter != "openrouter" && *adapter != "openai-compatible-external" {
			return errors.New("--adapter must be openrouter or openai-compatible-external")
		}
		if *providerURL == "" && *adapter == "openrouter" {
			*providerURL = "https://openrouter.ai/api/v1"
		}
		if *providerURL == "" {
			return errors.New("--url is required for an OpenAI-compatible provider")
		}
		resolvedSecretReference := *secretReference
		if *fromEnv != "" {
			var secretResponse struct {
				Secret struct {
					ID string `json:"id"`
				} `json:"secret"`
			}
			secretRequest := map[string]string{"name": "provider-" + name, "resolver": "env", "reference": *fromEnv}
			if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/secrets", "", secretRequest, &secretResponse); err != nil {
				return fmt.Errorf("register provider credential reference: %w", err)
			}
			if secretResponse.Secret.ID == "" {
				return errors.New("register provider credential reference: control plane returned an empty reference ID")
			}
			resolvedSecretReference = secretResponse.Secret.ID
		}
		targetName := "provider-" + name
		var targetResponse map[string]any
		targetRequest := map[string]string{"name": targetName, "url": *providerURL, "provider": *adapter, "runtime": "openai-compatible-api", "upstream_model": *model}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/targets", "", targetRequest, &targetResponse); err != nil {
			return fmt.Errorf("register provider target: %w", err)
		}
		var response map[string]any
		request := map[string]string{"name": name, "adapter": *adapter, "target": targetName, "secret_reference_id": resolvedSecretReference}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/provider-connections", "", request, &response); err != nil {
			return fmt.Errorf("create provider connection: %w", err)
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("provider %s connected · credentials remain in secret reference %s\n", name, resolvedSecretReference)
		fmt.Printf("No traffic or spend is enabled. Bind it with:\n  infercrane endpoint bind ENDPOINT --name %s --connection %s --request-limit LIMIT --cost-limit-usd BUDGET --max-request-cost-usd MAX --acknowledge-external-data --enable-external\n", name, name)
		return nil
	case "delete":
		if name == "" || fs.NArg() != 0 {
			return errors.New("usage: infercrane provider delete NAME")
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/provider-connections/"+url.PathEscape(name), "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("provider connection %s deleted · immutable endpoint bindings were not changed\n", name)
		return nil
	default:
		return errors.New("provider requires connect, list, or delete")
	}
}
