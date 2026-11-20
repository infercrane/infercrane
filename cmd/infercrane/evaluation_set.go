package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

type qualityEvalSetView struct {
	Digest      string `json:"digest"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	PromptCount int    `json:"prompt_count"`
	CreatedAt   string `json:"created_at"`
}

// evaluationSetCommand manages customer eval sets for the built-in answer
// check. An eval set is addressed by the SHA-256 digest of the file's exact
// bytes, so the digest printed here is the one a run names.
func evaluationSetCommand(ctx context.Context, args []string) error {
	usage := errors.New("usage: infercrane evaluation set upload FILE | inspect DIGEST | delete DIGEST | digest FILE | default | stay-optimized DEPLOYMENT DIGEST|default")
	if len(args) == 0 {
		return usage
	}
	action := args[0]
	fs := newFlagSet("evaluation set " + action)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if action == "default" {
		if fs.NArg() != 0 {
			return usage
		}
		// Print the built-in suite so it can be copied and adapted.
		_, err := os.Stdout.Write(qualitycheck.DefaultSuiteBytes())
		return err
	}
	if action == "stay-optimized" {
		if fs.NArg() != 2 {
			return usage
		}
		return stayOptimizedEvalSet(ctx, fs.Arg(0), fs.Arg(1), *output)
	}
	if fs.NArg() != 1 {
		return usage
	}
	argument := fs.Arg(0)
	if action == "digest" {
		set, err := readEvalSet(argument)
		if err != nil {
			return err
		}
		return printEvalSet(qualityEvalSetView{Digest: set.Digest, Name: set.Name, Version: set.Version, PromptCount: len(set.Prompts)}, *output, "Eval set is valid (not uploaded)")
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	switch action {
	case "upload":
		set, readErr := readEvalSet(argument)
		if readErr != nil {
			return readErr
		}
		body, _ := os.ReadFile(argument)
		var response struct {
			EvalSet qualityEvalSetView `json:"eval_set"`
			Created bool               `json:"created"`
		}
		if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/quality/eval-sets", "", rawRequestBody(body), &response); err != nil {
			return err
		}
		if response.EvalSet.Digest != set.Digest {
			return fmt.Errorf("control plane stored digest %s, but the file's digest is %s", response.EvalSet.Digest, set.Digest)
		}
		title := "Eval set uploaded"
		if !response.Created {
			title = "Eval set already uploaded"
		}
		if err = printEvalSet(response.EvalSet, *output, title); err != nil || *output == "json" {
			return err
		}
		fmt.Printf("\nUse it in an optimization run:\n  infercrane optimize create MODEL --provider PROVIDER --gpu GPU --eval-set %s\n", response.EvalSet.Digest)
		return nil
	case "inspect":
		var response struct {
			EvalSet qualityEvalSetView `json:"eval_set"`
		}
		if err = controlJSON(ctx, cfg, http.MethodGet, "/api/v1/quality/eval-sets/"+url.PathEscape(argument), "", nil, &response); err != nil {
			return err
		}
		return printEvalSet(response.EvalSet, *output, "Eval set")
	case "delete":
		if err = controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/quality/eval-sets/"+url.PathEscape(argument), "", nil, nil); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(map[string]any{"digest": argument, "deleted": true})
		}
		fmt.Printf("Eval set %s deleted. Runs that name it wait until it is uploaded again.\n", argument)
		return nil
	default:
		return usage
	}
}

// stayOptimizedEvalSet sets the eval set that runs Stay optimized proposes
// for a deployment ask in their answer check, or "default" for the built-in
// prompts. It changes only that field of the saved policy.
func stayOptimizedEvalSet(ctx context.Context, deployment, digest, output string) error {
	if digest != "default" && !qualitycheck.ValidDigest(digest) {
		return errors.New("name the eval set by its digest (sha256:...) as printed by: infercrane evaluation set upload FILE; or default for the built-in prompts")
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	if digest != "default" {
		// A run waits for a missing eval set, so catch a wrong digest now.
		if err = controlJSON(ctx, cfg, http.MethodGet, "/api/v1/quality/eval-sets/"+url.PathEscape(digest), "", nil, nil); err != nil {
			var controlErr *ControlError
			if errors.As(err, &controlErr) && controlErr.StatusCode == http.StatusNotFound {
				return fmt.Errorf("eval set %s is not uploaded; upload it first: infercrane evaluation set upload FILE", digest)
			}
			return err
		}
	}
	path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/continual-optimization/policy"
	var current struct {
		Policy map[string]any `json:"policy"`
	}
	if err = controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &current); err != nil {
		var controlErr *ControlError
		if errors.As(err, &controlErr) && controlErr.StatusCode == http.StatusNotFound {
			return fmt.Errorf("stay optimized is not set up for %s; turn it on from the endpoint page first", deployment)
		}
		return err
	}
	if len(current.Policy) == 0 {
		return fmt.Errorf("the saved Stay optimized policy for %s could not be read; save it again from the endpoint page first", deployment)
	}
	if digest == "default" {
		delete(current.Policy, "quality_eval_set")
	} else {
		current.Policy["quality_eval_set"] = digest
	}
	var saved struct {
		Policy map[string]any `json:"policy"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPut, path, "", current.Policy, &saved); err != nil {
		return err
	}
	evalSet, _ := saved.Policy["quality_eval_set"].(string)
	if output == "json" {
		return printJSON(map[string]any{"deployment": deployment, "quality_eval_set": evalSet})
	}
	if evalSet == "" {
		fmt.Printf("Stay optimized runs for %s check answers with the built-in prompts.\n", deployment)
		return nil
	}
	fmt.Printf("Stay optimized runs for %s check answers with eval set %s.\n", deployment, evalSet)
	return nil
}

func readEvalSet(path string) (qualitycheck.EvalSet, error) {
	info, err := os.Stat(path)
	if err != nil {
		return qualitycheck.EvalSet{}, fmt.Errorf("read eval set: %w", err)
	}
	if info.Size() > qualitycheck.MaxEvalSetBytes {
		return qualitycheck.EvalSet{}, fmt.Errorf("eval set exceeds %d KiB", qualitycheck.MaxEvalSetBytes>>10)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return qualitycheck.EvalSet{}, fmt.Errorf("read eval set: %w", err)
	}
	return qualitycheck.DecodeEvalSet(body)
}

func printEvalSet(set qualityEvalSetView, output, title string) error {
	if output == "json" {
		return printJSON(set)
	}
	fmt.Printf("%s\nDigest   %s\nName     %s\nVersion  %s\nPrompts  %d\n", title, set.Digest, set.Name, set.Version, set.PromptCount)
	return nil
}
