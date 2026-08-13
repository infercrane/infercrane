package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualityevidence"
)

func evaluationCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane evaluation keygen|ingest|sign|verify|attach|list [arguments]")
	}
	switch args[0] {
	case "keygen":
		return evaluationKeygenCommand(args[1:])
	case "sign":
		return evaluationSignCommand(args[1:])
	case "ingest":
		return evaluationIngestCommand(ctx, args[1:])
	case "verify":
		return evaluationVerifyCommand(args[1:])
	case "attach", "list":
		cfg, err := config.LoadClient()
		if err != nil {
			return err
		}
		return evaluationAPICommand(ctx, cfg, args)
	default:
		return fmt.Errorf("unknown evaluation action %q", args[0])
	}
}

func evaluationIngestCommand(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane evaluation ingest DEPLOYMENT REVISION --result RESULT.json --key KEY --file EVIDENCE.json [--attach]")
	}
	deployment, revision := args[0], args[1]
	fs := flag.NewFlagSet("evaluation ingest", flag.ContinueOnError)
	resultPath := fs.String("result", "", "content-free evaluator result JSON")
	keyPath := fs.String("key", "", "Ed25519 evaluator private key")
	filePath := fs.String("file", "", "signed evidence destination")
	attach := fs.Bool("attach", false, "attach signed evidence through the control-plane API")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *resultPath == "" || *keyPath == "" || *filePath == "" {
		return errors.New("ingest requires --result, --key, and --file")
	}
	body, err := os.ReadFile(*resultPath)
	if err != nil {
		return fmt.Errorf("read evaluator result: %w", err)
	}
	result, err := qualityevidence.DecodeResult(body)
	if err != nil {
		return err
	}
	payload := result.Bind(deployment, revision)
	if err = payload.Validate(); err != nil {
		return err
	}
	privateKey, err := loadPassportSigningKey(*keyPath)
	if err != nil {
		return err
	}
	envelope, err := passport.Sign(payload, privateKey)
	if err != nil {
		return err
	}
	if err = writeQualityEnvelope(*filePath, envelope); err != nil {
		return err
	}

	if *attach {
		cfg, loadErr := config.LoadClient()
		if loadErr != nil {
			return fmt.Errorf("signed evidence was written to %s but could not load control-plane configuration for attachment: %w", *filePath, loadErr)
		}
		path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/quality-evidence"
		var response map[string]any
		if attachErr := controlJSON(ctx, cfg, http.MethodPost, path, envelope.Digest, envelope, &response); attachErr != nil {
			return fmt.Errorf("signed evidence was written to %s but attachment failed: %w", *filePath, attachErr)
		}
	}

	fmt.Printf("Evaluator result ingested\nDeployment  %s\nRevision    %s\nSuite       %s@%s\nEvaluator   %s@%s\nScore       %.4f\nPassed      %t\nDigest      %s\nFile        %s\nAttached    %t\n\nOnly aggregate evidence was signed; unknown fields and prompt/output content are rejected.\n", deployment, revision, result.Suite, result.SuiteVersion, result.Evaluator, result.EvaluatorVersion, result.Score, result.Passed, envelope.Digest, *filePath, *attach)
	return nil
}

func evaluationKeygenCommand(args []string) error {
	fs := flag.NewFlagSet("evaluation keygen", flag.ContinueOnError)
	path := fs.String("file", "quality-evidence.key", "private-key destination")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane evaluation keygen [--file PATH]")
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil {
		return err
	}
	_, privateKey, err := passport.GenerateKey()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("refusing to overwrite existing quality-evidence key %s", *path)
	}
	if err != nil {
		return err
	}
	if _, err = file.WriteString(passport.EncodePrivateKey(privateKey) + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	fmt.Printf("Quality-evidence signing key created\nFile  %s\nMode  0600\n\nThe authorized operator who attaches evidence is the trust boundary. Keep this evaluator key outside the control plane.\n", *path)
	return nil
}

func evaluationSignCommand(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane evaluation sign DEPLOYMENT REVISION [flags]")
	}
	deployment, revision := args[0], args[1]
	fs := flag.NewFlagSet("evaluation sign", flag.ContinueOnError)
	suite := fs.String("suite", "", "evaluation suite name")
	suiteVersion := fs.String("suite-version", "", "immutable suite version")
	evaluator := fs.String("evaluator", "", "evaluator name")
	evaluatorVersion := fs.String("evaluator-version", "", "evaluator version")
	scoreText := fs.String("score", "", "normalized aggregate score from 0 to 1")
	passed := fs.Bool("passed", false, "whether the external suite passed")
	samples := fs.Int("samples", 0, "evaluated sample count")
	artifactDigest := fs.String("artifact-digest", "", "digest of the private evaluation result artifact")
	evaluatedAtText := fs.String("evaluated-at", "", "RFC3339 evaluation time; defaults to now")
	keyPath := fs.String("key", "", "Ed25519 evaluator private key")
	filePath := fs.String("file", "", "signed evidence destination")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	passedExplicit := false
	fs.Visit(func(item *flag.Flag) { passedExplicit = passedExplicit || item.Name == "passed" })
	if fs.NArg() != 0 || *suite == "" || *suiteVersion == "" || *evaluator == "" || *evaluatorVersion == "" || *scoreText == "" || !passedExplicit || *samples < 1 || *artifactDigest == "" || *keyPath == "" || *filePath == "" {
		return errors.New("sign requires --suite, --suite-version, --evaluator, --evaluator-version, --score, explicit --passed=true|false, --samples, --artifact-digest, --key, and --file")
	}
	score, err := strconv.ParseFloat(*scoreText, 64)
	if err != nil {
		return fmt.Errorf("--score: %w", err)
	}
	evaluatedAt := time.Now().UTC()
	if *evaluatedAtText != "" {
		evaluatedAt, err = time.Parse(time.RFC3339, *evaluatedAtText)
		if err != nil {
			return fmt.Errorf("--evaluated-at: %w", err)
		}
	}
	payload := qualityevidence.Payload{Schema: qualityevidence.Schema, Deployment: deployment, RevisionID: revision, Suite: *suite, SuiteVersion: *suiteVersion, Evaluator: *evaluator, EvaluatorVersion: *evaluatorVersion, Score: score, Passed: *passed, SampleCount: *samples, ArtifactDigest: *artifactDigest, EvaluatedAt: evaluatedAt}
	if err = payload.Validate(); err != nil {
		return err
	}
	privateKey, err := loadPassportSigningKey(*keyPath)
	if err != nil {
		return err
	}
	envelope, err := passport.Sign(payload, privateKey)
	if err != nil {
		return err
	}
	if err = writeQualityEnvelope(*filePath, envelope); err != nil {
		return err
	}
	fmt.Printf("Signed quality evidence\nDeployment  %s\nRevision    %s\nSuite       %s@%s\nScore       %.4f\nPassed      %t\nDigest      %s\nKey         %s\nFile        %s\n\nNo prompt or output content was included.\n", deployment, revision, *suite, *suiteVersion, score, *passed, envelope.Digest, envelope.KeyID, *filePath)
	return nil
}

func writeQualityEnvelope(path string, envelope passport.Envelope) error {
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quality evidence: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("refusing to overwrite existing evidence file %s", path)
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

func evaluationVerifyCommand(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: infercrane evaluation verify FILE")
	}
	envelope, payload, err := readQualityEnvelope(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("Quality evidence verified\nDeployment  %s\nRevision    %s\nSuite       %s@%s\nEvaluator   %s@%s\nScore       %.4f\nPassed      %t\nDigest      %s\nKey         %s\n", payload.Deployment, payload.RevisionID, payload.Suite, payload.SuiteVersion, payload.Evaluator, payload.EvaluatorVersion, payload.Score, payload.Passed, envelope.Digest, envelope.KeyID)
	return nil
}

func evaluationAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane evaluation attach|list DEPLOYMENT [flags]")
	}
	action, deployment := args[0], args[1]
	fs := flag.NewFlagSet("evaluation "+action, flag.ContinueOnError)
	filePath := fs.String("file", "", "signed evidence file")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected evaluation arguments")
	}
	path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/quality-evidence"
	if action == "list" {
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Data)
		}
		if len(response.Data) == 0 {
			fmt.Println("No quality evidence attached.")
			return nil
		}
		for _, item := range response.Data {
			fmt.Printf("%s  revision %s  %s@%s  score %.4v  passed %v  key %s\n", item["evaluated_at"], shortValue(fmt.Sprint(item["revision_id"])), item["suite"], item["suite_version"], item["score"], item["passed"], item["key_id"])
		}
		return nil
	}
	if action != "attach" || *filePath == "" {
		return errors.New("usage: infercrane evaluation attach DEPLOYMENT --file SIGNED.json")
	}
	envelope, payload, err := readQualityEnvelope(*filePath)
	if err != nil {
		return err
	}
	if payload.Deployment != deployment {
		return errors.New("signed evidence deployment does not match command deployment")
	}
	var response map[string]any
	if err = controlJSON(ctx, cfg, http.MethodPost, path, envelope.Digest, envelope, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Quality evidence attached\nDeployment  %s\nRevision    %s\nSuite       %s@%s\nScore       %.4f\nPassed      %t\nDigest      %s\n\nRelease Guard applies the persisted deterministic policy; the external evaluator does not promote revisions.\n", deployment, payload.RevisionID, payload.Suite, payload.SuiteVersion, payload.Score, payload.Passed, envelope.Digest)
	return nil
}

func readQualityEnvelope(path string) (passport.Envelope, qualityevidence.Payload, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return passport.Envelope{}, qualityevidence.Payload{}, fmt.Errorf("read quality evidence: %w", err)
	}
	if len(body) > qualityevidence.MaxFileSize {
		return passport.Envelope{}, qualityevidence.Payload{}, errors.New("quality evidence file exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var envelope passport.Envelope
	if err = decoder.Decode(&envelope); err != nil {
		return envelope, qualityevidence.Payload{}, fmt.Errorf("decode quality evidence envelope: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return envelope, qualityevidence.Payload{}, errors.New("quality evidence envelope contains trailing JSON")
		}
		return envelope, qualityevidence.Payload{}, fmt.Errorf("decode trailing quality evidence envelope: %w", err)
	}
	payload, err := qualityevidence.Decode(envelope)
	return envelope, payload, err
}
