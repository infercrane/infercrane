package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/infercrane/infercrane/internal/config"
)

var modelAPIManifestEndpoints = map[string]string{
	"product":        "/api/v1/admin/model-api/products",
	"rate":           "/api/v1/admin/model-api/rates",
	"offer":          "/api/v1/admin/model-api/offers",
	"qualification":  "/api/v1/admin/model-api/qualifications",
	"target-binding": "/api/v1/admin/model-api/target-bindings",
	"plan":           "/api/v1/admin/model-api/plans",
	"publication":    "/api/v1/admin/model-api/publications",
	"entitlement":    "/api/v1/admin/model-api/entitlements",
}

func modelAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 || args[0] != "publish" {
		return errors.New("usage: infercrane model-api publish product|rate|offer|qualification|target-binding|plan|publication|entitlement --file MANIFEST.json")
	}
	contractType := args[1]
	endpoint, ok := modelAPIManifestEndpoints[contractType]
	if !ok {
		return fmt.Errorf("unsupported Model API contract type %q", contractType)
	}

	fs := flag.NewFlagSet("model-api publish "+contractType, flag.ContinueOnError)
	filePath := fs.String("file", "", "JSON manifest containing one immutable Model API contract")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *filePath == "" {
		return errors.New("--file is required and no additional positional arguments are accepted")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}

	manifest, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read Model API manifest: %w", err)
	}
	if !json.Valid(manifest) {
		return errors.New("Model API manifest must contain valid JSON")
	}
	var response map[string]any
	if err = controlJSON(ctx, cfg, http.MethodPost, endpoint, "", json.RawMessage(manifest), &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Published Model API %s\n", contractType)
	return nil
}
