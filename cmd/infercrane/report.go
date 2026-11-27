package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/proofreport"
)

// report proof prints the Proof Report of an engagement's finished runs:
// before and after per workload and load, the verified saving, the limits,
// and who signed it. It only reads the control API. report proof verify
// checks a saved report offline.

const (
	reportUsage            = "usage: infercrane report proof RUN [RUN...] [--title TEXT] [--prepared-for NAME] [--output text|json|markdown|html] [--html FILE] [--markdown FILE] | report proof verify FILE [--public-key KEY]"
	reportProofVerifyUsage = "usage: infercrane report proof verify FILE [--public-key KEY]"
)

func reportCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "proof" {
		return errors.New(reportUsage)
	}
	args = args[1:]
	if len(args) > 0 && args[0] == "verify" {
		return reportProofVerifyCommand(os.Stdout, args[1:])
	}
	var ids []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ids, args = append(ids, args[0]), args[1:]
	}
	fs := newFlagSet("report proof")
	title := fs.String("title", "", "the report's title, one line")
	preparedFor := fs.String("prepared-for", "", "who the report is prepared for, one line")
	output := fs.String("output", "text", "text, json, markdown, or html")
	htmlPath := fs.String("html", "", "also write the print-ready HTML page to FILE")
	markdownPath := fs.String("markdown", "", "also write the Markdown document to FILE")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || len(ids) == 0 {
		return errors.New(reportUsage)
	}
	switch *output {
	case "text", "json", "markdown", "html":
	default:
		return errors.New(reportUsage)
	}
	request, err := proofreport.Request{Title: *title, PreparedFor: *preparedFor, CampaignIDs: ids}.Check()
	if err != nil {
		return err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	raw, document, err := loadProofReport(ctx, cfg, request)
	if err != nil {
		return err
	}
	return writeProofReport(os.Stdout, os.Stderr, raw, document, *output, *htmlPath, *markdownPath)
}

// loadProofReport returns the response exactly as the control plane sent it,
// so a saved report keeps the bytes its signature covers, plus its decoded
// form for rendering.
func loadProofReport(ctx context.Context, cfg config.Config, request proofreport.Request) (json.RawMessage, proofreport.Document, error) {
	query := url.Values{"campaigns": {strings.Join(request.CampaignIDs, ",")}}
	if request.Title != "" {
		query.Set("title", request.Title)
	}
	if request.PreparedFor != "" {
		query.Set("prepared_for", request.PreparedFor)
	}
	var raw json.RawMessage
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/proof-report?"+query.Encode(), "", nil, &raw); err != nil {
		return nil, proofreport.Document{}, err
	}
	var document proofreport.Document
	if json.Unmarshal(raw, &document) != nil || document.Report.SchemaVersion != proofreport.SchemaVersion {
		return nil, proofreport.Document{}, errors.New("the control plane returned an unsupported proof report")
	}
	return raw, document, nil
}

func writeProofReport(stdout, stderr io.Writer, raw json.RawMessage, document proofreport.Document, output, htmlPath, markdownPath string) error {
	page, err := proofreport.HTML(document)
	if err != nil {
		return fmt.Errorf("render HTML report: %w", err)
	}
	markdown := proofreport.Markdown(document)
	for _, file := range []struct {
		path, kind string
		body       []byte
	}{{htmlPath, "HTML", page}, {markdownPath, "Markdown", markdown}} {
		if file.path == "" {
			continue
		}
		if err = os.WriteFile(file.path, file.body, 0o644); err != nil {
			return fmt.Errorf("write %s report: %w", file.kind, err)
		}
		fmt.Fprintf(stderr, "%s report written to %s\n", file.kind, file.path)
	}
	switch output {
	case "json":
		// Indenting changes only whitespace, which verification ignores.
		var indented bytes.Buffer
		if err = json.Indent(&indented, raw, "", "  "); err != nil {
			return fmt.Errorf("format report JSON: %w", err)
		}
		indented.WriteByte('\n')
		_, err = stdout.Write(indented.Bytes())
	case "markdown":
		_, err = stdout.Write(markdown)
	case "html":
		_, err = stdout.Write(page)
	default:
		_, err = io.WriteString(stdout, proofreport.Text(document))
	}
	return err
}

// reportProofVerifyCommand checks a report saved with --output json without
// the control plane: schema, digest, and Ed25519 signature. Any mismatch is
// an error, so the command exits non-zero.
func reportProofVerifyCommand(stdout io.Writer, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(reportProofVerifyUsage)
	}
	path := args[0]
	fs := newFlagSet("report proof verify")
	publicKey := fs.String("public-key", "", "base64 Ed25519 public key the report must be signed by")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(reportProofVerifyUsage)
	}
	var trusted ed25519.PublicKey
	if *publicKey != "" {
		key, err := passport.DecodePublicKey(*publicKey)
		if err != nil {
			return fmt.Errorf("--public-key: %w", err)
		}
		trusted = key
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	verified, err := proofreport.Verify(saved, trusted)
	if err != nil {
		return fmt.Errorf("report verification failed: %w", err)
	}
	trust := "matches --public-key"
	if !verified.Pinned {
		trust = "compare it with your operator's key, or pass --public-key"
	}
	_, err = fmt.Fprintf(stdout, "VERIFIED  %s\nRuns      %s\nKey       %s (%s)\n", verified.Digest, strings.Join(verified.Runs, ", "), verified.KeyID, trust)
	return err
}
