package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/modelimport"
)

// modelsImportCommand registers weights or a LoRA adapter that live in the
// customer's own S3 or GCS bucket. It hashes a local copy of the same files
// into a manifest; InferCrane never reads the bucket, and each worker checks
// every downloaded file against the manifest before serving.
func modelsImportCommand(ctx context.Context, cfg config.Config, args []string) error {
	uri, err := commandSubject(args, 0, "usage: infercrane models import s3://BUCKET/PREFIX --from DIR [--kind weights|adapter] [--base-model OWNER/NAME] [--output human|json]")
	if err != nil {
		return err
	}
	fs := newFlagSet("models import")
	from := fs.String("from", "", "local directory holding the same files as the bucket location")
	kind := fs.String("kind", modelimport.KindWeights, "weights or adapter")
	baseModel := fs.String("base-model", "", "for an adapter: the base model it will be served on")
	output := fs.String("output", "human", "human or json")
	if err = fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *from == "" {
		return errors.New("usage: infercrane models import s3://BUCKET/PREFIX --from DIR [--kind weights|adapter]")
	}
	if err = validateOutput(*output); err != nil {
		return err
	}
	if _, ok := modelimport.Source(modelimport.NormalizeURI(uri)); !ok {
		return errors.New("the location must be an s3:// or gs:// bucket path")
	}
	files, err := modelimport.FromDirectory(*from)
	if err != nil {
		return fmt.Errorf("hash %s: %w", *from, err)
	}
	request := map[string]any{"kind": *kind, "uri": uri, "files": files}
	configName, configField := "config.json", "config_base64"
	if *kind == modelimport.KindAdapter {
		configName, configField = "adapter_config.json", "adapter_config_base64"
		if *baseModel != "" {
			request["base_model"] = *baseModel
		}
	}
	if content, readErr := os.ReadFile(filepath.Join(*from, configName)); readErr == nil {
		request[configField] = base64.StdEncoding.EncodeToString(content)
	}
	var response struct {
		Import domain.ModelImport `json:"import"`
		Check  *modelcheck.Report `json:"check"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/model-imports", "", request, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Imported %s\nRevision  %s\n", response.Import.URI, response.Import.ManifestDigest)
	printImportChecks(response.Import.Checks)
	if response.Import.Kind == modelimport.KindAdapter {
		fmt.Printf("\nServe it with: infercrane deploy BASE_MODEL --adapter NAME=%s@%s ...\n", response.Import.URI, response.Import.ManifestDigest)
	} else {
		fmt.Printf("\nDeploy it with: infercrane deploy %s --model-revision %s --cloud kubernetes|aws ...\n", response.Import.URI, response.Import.ManifestDigest)
	}
	return nil
}

func printImportChecks(checks domain.ModelImportCheck) {
	for _, line := range checks.Verified {
		fmt.Println("  ✓ " + line)
	}
	for _, line := range checks.Unknown {
		fmt.Println("  ? " + line)
	}
	for _, line := range checks.Failed {
		fmt.Println("  ✗ " + line)
	}
}

// modelsAdapterCheckCommand pins a Hugging Face LoRA adapter to a commit and
// shows what its adapter_config.json says.
func modelsAdapterCheckCommand(ctx context.Context, cfg config.Config, args []string) error {
	repository, err := commandSubject(args, 0, "usage: infercrane models adapter-check OWNER/NAME [--revision REV] [--secret ID] [--base-model OWNER/NAME] [--output human|json]")
	if err != nil {
		return err
	}
	fs := newFlagSet("models adapter-check")
	revision := fs.String("revision", "", "branch, tag, or commit (default main)")
	secret := fs.String("secret", "", "saved Hugging Face token reference ID for a private adapter")
	baseModel := fs.String("base-model", "", "the base model the adapter will be served on")
	output := fs.String("output", "human", "human or json")
	if err = fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !modelcheck.ValidRepository(repository) {
		return errors.New("usage: infercrane models adapter-check OWNER/NAME [flags]")
	}
	if err = validateOutput(*output); err != nil {
		return err
	}
	query := url.Values{"repository": {repository}}
	for name, value := range map[string]string{"revision": *revision, "model_secret_reference_id": *secret, "base_model": *baseModel} {
		if value != "" {
			query.Set(name, value)
		}
	}
	var response struct {
		Repository string                  `json:"repository"`
		Revision   string                  `json:"revision"`
		Checks     domain.ModelImportCheck `json:"checks"`
	}
	if err = controlJSON(ctx, cfg, http.MethodGet, "/api/v1/catalog/hugging-face/adapter-check?"+query.Encode(), "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("%s@%s\n", response.Repository, response.Revision)
	printImportChecks(response.Checks)
	return nil
}

// parseAdapterFlag reads NAME=REPOSITORY@REVISION for --adapter.
func parseAdapterFlag(value string) (domain.ModelAdapter, error) {
	name, location, found := strings.Cut(value, "=")
	index := strings.LastIndex(location, "@")
	if !found || index <= 0 || index == len(location)-1 {
		return domain.ModelAdapter{}, errors.New("--adapter takes NAME=OWNER/REPO@COMMIT or NAME=s3://BUCKET/PREFIX@sha256:DIGEST")
	}
	return domain.ModelAdapter{Name: strings.TrimSpace(name), Repository: location[:index], Revision: location[index+1:]}, nil
}
