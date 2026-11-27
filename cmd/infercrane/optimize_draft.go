package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/technique"
)

const optimizeDraftUsage = `usage:
  infercrane optimize draft job --file JOB.json
      the training Job for your own Kubernetes cluster (kubectl apply -f -)
  infercrane optimize draft record --report report.json --repository OWNER/NAME --revision COMMIT --engine vllm --engine-version VERSION --accelerator GPU
      the draft record a plan names in speculative_drafts, pinned where it was qualified
  infercrane optimize draft args --record DRAFT.json [--engine vllm]
      the engine flags that serve the draft, for any vLLM server
  infercrane optimize draft candidates --record DRAFT.json --setup SETUP.json
      the trained-draft technique's candidates for a starting setup (technique contract)

Every action runs on this machine: no control plane, no sign-in, and nothing is sent anywhere.`

// optimizeDraftCommand is the standalone side of drafts trained on your
// traffic: it renders and checks files, and never reads a prompt.
func optimizeDraftCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(optimizeDraftUsage)
	}
	fs := newFlagSet("optimize draft " + args[0])
	file := fs.String("file", "", "training job JSON (speculator.TrainingJob)")
	reportPath := fs.String("report", "", "report.json the training job wrote")
	repository := fs.String("repository", "", "where you put the draft: Hugging Face OWNER/NAME, or s3:// or gs://")
	revision := fs.String("revision", "", "its 40-character commit, or the report's artifact_digest for a bucket")
	engine := fs.String("engine", "vllm", "engine the draft was qualified on, or serves on")
	engineVersion := fs.String("engine-version", "", "exact engine version the draft was qualified on")
	accelerator := fs.String("accelerator", "", "GPU the draft was qualified on")
	recordPath := fs.String("record", "", "draft record JSON (from optimize draft record)")
	setupPath := fs.String("setup", "", "starting setup JSON (technique.Setup)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(optimizeDraftUsage)
	}
	switch args[0] {
	case "job":
		var job speculator.TrainingJob
		if err := readStrictJSON(*file, &job); err != nil {
			return err
		}
		manifest, err := job.WithDefaults().KubernetesJob()
		if err != nil {
			return err
		}
		return writeJSON(out, manifest)
	case "record":
		if *reportPath == "" || *repository == "" || *revision == "" || *engineVersion == "" || *accelerator == "" {
			return errors.New(optimizeDraftUsage)
		}
		raw, err := os.ReadFile(*reportPath)
		if err != nil {
			return fmt.Errorf("read report: %w", err)
		}
		report, err := speculator.DecodeReport(raw)
		if err != nil {
			return err
		}
		draft, err := report.Draft(*repository, *revision, &speculator.Pin{Engine: *engine, EngineVersion: *engineVersion, Accelerator: *accelerator})
		if err != nil {
			return err
		}
		return writeJSON(out, draft)
	case "args":
		draft, err := readDraft(*recordPath)
		if err != nil {
			return err
		}
		flags, err := speculator.RuntimeArgs(*engine, draft)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, strings.Join(shellQuote(flags), " "))
		return err
	case "candidates":
		draft, err := readDraft(*recordPath)
		if err != nil {
			return err
		}
		var setup technique.Setup
		if err := readStrictJSON(*setupPath, &setup); err != nil {
			return err
		}
		registry, err := technique.NewRegistry(speculator.Technique{Drafts: []speculator.Draft{draft}})
		if err != nil {
			return err
		}
		candidates, skipped, err := registry.Candidates(ctx, setup)
		if err != nil {
			return err
		}
		return writeJSON(out, map[string]any{"manifest": speculator.Technique{}.Manifest(), "candidates": candidates, "skipped": skipped})
	}
	return errors.New(optimizeDraftUsage)
}

func readDraft(path string) (speculator.Draft, error) {
	var draft speculator.Draft
	if err := readStrictJSON(path, &draft); err != nil {
		return speculator.Draft{}, err
	}
	draft = draft.Normalize()
	return draft, draft.Validate()
}

func readStrictJSON(path string, into any) error {
	if strings.TrimSpace(path) == "" {
		return errors.New(optimizeDraftUsage)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("%s is larger than 1 MiB", path)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// shellQuote quotes each flag for a POSIX shell, so the line can be pasted
// after `vllm serve MODEL`.
func shellQuote(values []string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		if strings.ContainsAny(value, ` "'{}$\`) {
			out[index] = "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
			continue
		}
		out[index] = value
	}
	return out
}
