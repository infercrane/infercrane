package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/intentplan"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/spec"
	"gopkg.in/yaml.v3"
)

// modelsCheckCommand asks the control plane to check an open Hugging Face
// model that is not in the reviewed catalog, and shows the setup it would
// deploy. With --spec it writes that setup as a DeploymentSpec for
// `infercrane deploy FILE`.
func modelsCheckCommand(ctx context.Context, cfg config.Config, args []string) error {
	repository, err := commandSubject(args, 0, "usage: infercrane models check OWNER/NAME [--revision REV] [--secret ID] [--context-tokens N] [--concurrency N] [--spec FILE] [--output human|json]")
	if err != nil {
		return err
	}
	fs := newFlagSet("models check")
	revision := fs.String("revision", "", "branch, tag, or commit to check (default main)")
	secret := fs.String("secret", "", "saved Hugging Face token reference ID for gated or private models")
	contextTokens := fs.Int("context-tokens", 0, "tokens per request the GPU is sized for (default 8192)")
	concurrency := fs.Int("concurrency", 0, "concurrent requests the GPU is sized for (default 8)")
	specPath := fs.String("spec", "", "write the proposed setup as a new DeploymentSpec file")
	output := fs.String("output", "human", "human or json")
	if err = fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !modelcheck.ValidRepository(repository) {
		return errors.New("usage: infercrane models check OWNER/NAME [flags]; the model must be an exact Hugging Face repository")
	}
	if err = validateOutput(*output); err != nil {
		return err
	}
	request := intentplan.Request{Intent: "deploy", Model: repository, ModelRevision: *revision, ModelSecretReferenceID: *secret, ContextTokens: *contextTokens, Concurrency: *concurrency}
	var envelope intentplan.Envelope
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/planning/intents", "", request, &envelope); err != nil {
		return err
	}
	if *specPath != "" {
		if envelope.DeploymentDraft == nil {
			return errors.New("the check did not produce a deployable setup; see `infercrane models check " + repository + "` for what is missing")
		}
		if err = writeDraftSpec(*specPath, *envelope.DeploymentDraft); err != nil {
			return err
		}
	}
	if *output == "json" {
		return printJSON(envelope)
	}
	printModelCheck(envelope)
	if *specPath != "" {
		fmt.Printf("\nDeploymentSpec: %s\nNext: infercrane deploy %s --wait\n", *specPath, *specPath)
	} else if envelope.DeploymentDraft != nil {
		fmt.Printf("\nNext: infercrane models check %s --spec %s.yaml, then infercrane deploy %s.yaml --wait\n", repository, envelope.DeploymentDraft.Name, envelope.DeploymentDraft.Name)
	}
	return nil
}

func printModelCheck(envelope intentplan.Envelope) {
	plan := envelope.Plan
	check := plan.ModelCheck
	if plan.Model != nil && plan.Model.Review == intentplan.ReviewReviewed {
		fmt.Printf("%s is a reviewed model (%s@%s).\nInspect it with: infercrane models inspect %s\n", plan.Model.Repository, plan.Model.Repository, shortID(plan.Model.Revision), plan.Model.Name)
		return
	}
	if check == nil {
		for _, missing := range plan.Missing {
			fmt.Printf("%s\n  %s\n  %s\n", missing.Prompt, missing.Reason, missing.Remediation)
		}
		return
	}
	fmt.Printf("%s @ %s · Not reviewed: checked automatically\n", check.Repository, shortID(check.Revision))
	fmt.Printf("Verdict  %s · %s\n\n", check.Verdict, check.Reason)
	marks := map[string]string{modelcheck.Pass: "✓", modelcheck.Fail: "✗", modelcheck.Unknown: "?", modelcheck.Info: "i"}
	for _, item := range check.Checks {
		fmt.Printf("  %s %s\n    %s\n", marks[item.State], item.Label, item.Detail)
	}
	fmt.Printf("\nSized for %d requests at %d tokens. Source: %s\n", check.Memory.Concurrency, check.Memory.ContextTokens, check.SourceURL)
	if plan.Configuration != nil {
		configuration := plan.Configuration
		fmt.Printf("\nProposed setup\n  %s %s · %d × %s · %s\n  Args: %s\n", configuration.Runtime, configuration.RuntimeVersion, configuration.GPUCount, configuration.GPU, valueOr(configuration.Provider, "provider not chosen"), strings.Join(configuration.RuntimeArgs, " "))
	}
	for _, missing := range plan.Missing {
		fmt.Printf("\nNeeds: %s\n  %s\n", missing.Prompt, missing.Remediation)
		for _, option := range missing.Options {
			fmt.Printf("  - %s (%s)\n", option.Label, option.Value)
		}
	}
	fmt.Println("\nThroughput, latency, answer quality, and cost are unknown until measured.")
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func writeDraftSpec(path string, draft intentplan.DeploymentDraft) error {
	var document spec.Deployment
	document.APIVersion, document.Kind, document.Name, document.Endpoint = "infercrane.dev/v1", "Deployment", draft.Name, draft.EndpointName
	document.Model.ID, document.Model.Revision = draft.Model, draft.ModelRevision
	document.Runtime.Engine, document.Runtime.Version, document.Runtime.Args = draft.Runtime, draft.RuntimeVersion, append([]string{}, draft.RuntimeArgs...)
	document.Compute.Mode = draft.ComputeMode
	document.Resources.GPU, document.Resources.GPUCount = draft.GPU, draft.GPUCount
	document.Provider.Cloud, document.Provider.Adapter, document.Provider.Region = draft.Cloud, draft.ProviderAdapter, draft.Region
	document.Scaling.MinReplicas, document.Scaling.MaxReplicas = draft.MinReplicas, draft.MaxReplicas
	document.Routing.Strategy = "round-robin"
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode DeploymentSpec: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("write DeploymentSpec (an existing file is never overwritten): %w", err)
	}
	if _, err = file.Write(encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write DeploymentSpec: %w", err)
	}
	if err = file.Close(); err != nil {
		return err
	}
	if _, err = spec.Load(path); err != nil {
		return fmt.Errorf("the written DeploymentSpec does not load: %w", err)
	}
	return nil
}

// pinUncatalogedModel asks the control plane to check a model outside the
// reviewed catalog when no commit was given, so `optimize` can propose for
// it. It pins the checked commit and bounds the baseline's context to the
// length the GPU was sized for. Without a configured control plane, or when
// it cannot answer, the request is left as it was and the proposal fails
// closed on the missing commit, exactly as before. Only a model the check
// found unsupported is refused here.
func pinUncatalogedModel(ctx context.Context, request *optimizer.Request) error {
	identity := strings.TrimSpace(request.ModelIdentity)
	if request.ModelRevision != "" || strings.Contains(identity, "@") || !modelcheck.ValidRepository(identity) {
		return nil
	}
	for _, entry := range curatedrecipe.All() {
		if strings.EqualFold(entry.Model, identity) || entry.Name == identity {
			return nil
		}
	}
	cfg, err := config.LoadClient()
	if err != nil || strings.TrimSpace(cfg.APIKey) == "" {
		return nil
	}
	var response struct {
		Check modelcheck.Report `json:"check"`
	}
	path := "/api/v1/catalog/hugging-face/check?" + url.Values{"repository": {identity}}.Encode()
	if err = controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
		fmt.Fprintf(os.Stderr, "Could not check %s on the control plane (%v); pass --model-revision COMMIT to propose for it.\n", identity, err)
		return nil
	}
	if response.Check.Verdict == modelcheck.Unsupported {
		return fmt.Errorf("%s cannot run on InferCrane's runtimes: %s", identity, response.Check.Reason)
	}
	request.ModelRevision = response.Check.Revision
	if request.MaxModelLen == 0 && response.Check.Task == modelcheck.TaskChat {
		request.MaxModelLen = response.Check.Memory.ContextTokens
	}
	fmt.Fprintf(os.Stderr, "Not reviewed: checked automatically. Pinned %s to %s (%s).\n", identity, response.Check.Revision, strconv.Quote(response.Check.Verdict))
	return nil
}
