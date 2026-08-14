package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/trainingartifact"
)

func trainingCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane training keygen|sign|verify|attach|list [arguments]")
	}
	switch args[0] {
	case "keygen":
		return trainingKeygen(args[1:])
	case "sign":
		return trainingSign(args[1:])
	case "verify":
		return trainingVerify(args[1:])
	case "attach":
		return trainingAttach(ctx, cfg, args[1:])
	case "list":
		return trainingList(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("unknown training action %q", args[0])
	}
}

func trainingKeygen(args []string) error {
	fs := flag.NewFlagSet("training keygen", flag.ContinueOnError)
	path := fs.String("file", "training-handoff.key", "private-key destination")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil {
		return errors.New("usage: infercrane training keygen [--file PATH]")
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil {
		return err
	}
	_, key, err := passport.GenerateKey()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("refusing to overwrite existing training handoff key %s", *path)
	}
	if err != nil {
		return err
	}
	if _, err = file.WriteString(passport.EncodePrivateKey(key) + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(map[string]any{"file": *path, "mode": "0600", "private_key_exposed": false})
	}
	fmt.Printf("Training-handoff signing key created\nFile  %s\nMode  0600\n", *path)
	return nil
}

func trainingSign(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane training sign DEPLOYMENT REVISION [flags]")
	}
	deployment, revision := args[0], args[1]
	fs := flag.NewFlagSet("training sign", flag.ContinueOnError)
	provider := fs.String("provider", "", "external training system")
	runID := fs.String("run", "", "external immutable run identity")
	repository := fs.String("repository", "", "checkpoint repository identity or approved URI without credentials")
	immutableRevision := fs.String("immutable-revision", "", "immutable checkpoint or registry version")
	digest := fs.String("digest", "", "checkpoint sha256 digest")
	baseModel := fs.String("base-model", "", "immutable base model identity")
	method := fs.String("method", "", "training method such as lora or full")
	framework := fs.String("framework", "", "training framework")
	frameworkVersion := fs.String("framework-version", "", "training framework version")
	datasetFingerprint := fs.String("dataset-fingerprint", "", "content-free dataset fingerprint")
	producedAtText := fs.String("produced-at", "", "RFC3339 production time; defaults to now")
	keyPath := fs.String("key", "", "Ed25519 training-system private key")
	filePath := fs.String("file", "", "signed handoff destination")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil || *provider == "" || *runID == "" || *repository == "" || *immutableRevision == "" || *digest == "" || *keyPath == "" || *filePath == "" {
		return errors.New("sign requires --provider, --run, --repository, --immutable-revision, --digest, --key, and --file")
	}
	producedAt := time.Now().UTC()
	var err error
	if *producedAtText != "" {
		producedAt, err = time.Parse(time.RFC3339, *producedAtText)
		if err != nil {
			return fmt.Errorf("--produced-at: %w", err)
		}
	}
	payload := trainingartifact.Payload{Schema: trainingartifact.Schema, Deployment: deployment, RevisionID: revision, Provider: *provider, ExternalRunID: *runID, Repository: *repository, ImmutableRevision: *immutableRevision, ArtifactDigest: *digest, BaseModelIdentity: *baseModel, Method: *method, Framework: *framework, FrameworkVersion: *frameworkVersion, DatasetFingerprint: *datasetFingerprint, ProducedAt: producedAt}
	if err = payload.Validate(); err != nil {
		return err
	}
	key, err := loadTrainingKey(*keyPath)
	if err != nil {
		return err
	}
	envelope, err := passport.Sign(payload, key)
	if err != nil {
		return err
	}
	if err = writeTrainingEnvelope(*filePath, envelope); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(map[string]any{"deployment": deployment, "revision_id": revision, "provider": *provider, "external_run_id": *runID, "repository": *repository, "immutable_revision": *immutableRevision, "artifact_digest": *digest, "evidence_digest": envelope.Digest, "file": *filePath, "content_recorded": false})
	}
	fmt.Printf("Signed training artifact handoff\nDeployment  %s\nRevision    %s\nProvider    %s\nRun         %s\nArtifact    %s@%s\nDigest      %s\nEvidence    %s\nFile        %s\n\nNo dataset, prompt, output, checkpoint bytes, or credentials were included.\n", deployment, revision, *provider, *runID, *repository, *immutableRevision, *digest, envelope.Digest, *filePath)
	return nil
}

func trainingVerify(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane training verify FILE [--output human|json]")
	}
	fs := flag.NewFlagSet("training verify", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil {
		return errors.New("usage: infercrane training verify FILE [--output human|json]")
	}
	envelope, payload, err := readTrainingEnvelope(args[0])
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(map[string]any{"verified": true, "deployment": payload.Deployment, "revision_id": payload.RevisionID, "provider": payload.Provider, "external_run_id": payload.ExternalRunID, "repository": payload.Repository, "immutable_revision": payload.ImmutableRevision, "artifact_digest": payload.ArtifactDigest, "evidence_digest": envelope.Digest, "key_id": envelope.KeyID, "content_recorded": false})
	}
	fmt.Printf("Training artifact handoff verified\nDeployment  %s\nRevision    %s\nProvider    %s\nRun         %s\nArtifact    %s@%s\nDigest      %s\nKey         %s\n", payload.Deployment, payload.RevisionID, payload.Provider, payload.ExternalRunID, payload.Repository, payload.ImmutableRevision, payload.ArtifactDigest, envelope.KeyID)
	return nil
}

func trainingAttach(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane training attach DEPLOYMENT FILE [--output human|json]")
	}
	fs := flag.NewFlagSet("training attach", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil {
		return errors.New("usage: infercrane training attach DEPLOYMENT FILE [--output human|json]")
	}
	envelope, payload, err := readTrainingEnvelope(args[1])
	if err != nil {
		return err
	}
	if payload.Deployment != args[0] {
		return errors.New("signed handoff deployment does not match the command argument")
	}
	var response map[string]any
	path := "/api/v1/deployments/" + url.PathEscape(args[0]) + "/training-artifacts"
	if err = controlJSON(ctx, cfg, http.MethodPost, path, envelope.Digest, envelope, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Training artifact attached\nDeployment  %s\nRevision    %s\nArtifact    %s@%s\nEvidence    %s\n\nThe external training system remains the execution owner. Qualify this revision with benchmark, Replay, quality evidence, and Release Guard before promotion.\n", payload.Deployment, payload.RevisionID, payload.Repository, payload.ImmutableRevision, envelope.Digest)
	return nil
}

func trainingList(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane training list DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("training list", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || validateOutput(*output) != nil {
		return errors.New("usage: infercrane training list DEPLOYMENT [--output human|json]")
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	path := "/api/v1/deployments/" + url.PathEscape(args[0]) + "/training-artifacts"
	if err := controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	if len(response.Data) == 0 {
		fmt.Println("No signed training artifact handoffs.")
		return nil
	}
	fmt.Println("TRAINING ARTIFACTS")
	for _, row := range response.Data {
		fmt.Printf("%-24v %-14v %-20v %v\n", row["revision_id"], row["provider"], row["external_run_id"], row["artifact_digest"])
	}
	return nil
}

func loadTrainingKey(path string) (ed25519.PrivateKey, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read training handoff key: %w", err)
	}
	return passport.DecodePrivateKey(strings.TrimSpace(string(body)))
}

func writeTrainingEnvelope(path string, envelope passport.Envelope) error {
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("refusing to overwrite existing training handoff %s", path)
	}
	if err != nil {
		return err
	}
	if _, err = file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readTrainingEnvelope(path string) (passport.Envelope, trainingartifact.Payload, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return passport.Envelope{}, trainingartifact.Payload{}, fmt.Errorf("read training handoff: %w", err)
	}
	if len(body) > 128<<10 {
		return passport.Envelope{}, trainingartifact.Payload{}, errors.New("training handoff exceeds 128 KiB")
	}
	var envelope passport.Envelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&envelope); err != nil {
		return passport.Envelope{}, trainingartifact.Payload{}, fmt.Errorf("decode training handoff: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return passport.Envelope{}, trainingartifact.Payload{}, errors.New("training handoff contains trailing JSON")
		}
		return passport.Envelope{}, trainingartifact.Payload{}, fmt.Errorf("decode trailing training handoff: %w", err)
	}
	payload, err := trainingartifact.Decode(envelope)
	return envelope, payload, err
}
