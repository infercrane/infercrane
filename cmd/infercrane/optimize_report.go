package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

// optimize report prints the outcome of one campaign: the current setup
// against the winning setup on cost, speed, and answer quality, then the
// measurement that proves it. It only reads the control API.

const optimizeReportUsage = "usage: infercrane optimize report CAMPAIGN [--output text|json] [--html FILE]"

func optimizeReportCommand(ctx context.Context, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(optimizeReportUsage)
	}
	id := args[0]
	fs := flag.NewFlagSet("optimize report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	output := fs.String("output", "text", "text or json")
	htmlPath := fs.String("html", "", "also write a self-contained HTML page to FILE")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || (*output != "text" && *output != "json") {
		return errors.New(optimizeReportUsage)
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	raw, report, err := loadOptimizationReport(ctx, cfg, id)
	if err != nil {
		return err
	}
	if err = writeOptimizationReport(os.Stdout, os.Stderr, raw, report, *output, *htmlPath); err != nil {
		return err
	}
	if *output == "text" {
		printResourceLink(cfg, "run", id)
	}
	return nil
}

// loadOptimizationReport returns the report exactly as the control plane sent
// it, plus its decoded form for rendering.
func loadOptimizationReport(ctx context.Context, cfg config.Config, id string) (json.RawMessage, optimizationreport.Report, error) {
	var response struct {
		Report json.RawMessage `json:"report"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns/"+url.PathEscape(id)+"/report", "", nil, &response); err != nil {
		return nil, optimizationreport.Report{}, err
	}
	var report optimizationreport.Report
	if len(response.Report) == 0 || json.Unmarshal(response.Report, &report) != nil || report.SchemaVersion != optimizationreport.SchemaVersion {
		return nil, optimizationreport.Report{}, errors.New("the control plane returned an unsupported optimization report")
	}
	return response.Report, report, nil
}

func writeOptimizationReport(stdout, stderr io.Writer, raw json.RawMessage, report optimizationreport.Report, output, htmlPath string) error {
	if htmlPath != "" {
		page, err := optimizationreport.HTML(report)
		if err != nil {
			return fmt.Errorf("render HTML report: %w", err)
		}
		if err = os.WriteFile(htmlPath, page, 0o644); err != nil {
			return fmt.Errorf("write HTML report: %w", err)
		}
		fmt.Fprintf(stderr, "HTML report written to %s\n", htmlPath)
	}
	if output == "json" {
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			return fmt.Errorf("format report JSON: %w", err)
		}
		indented.WriteByte('\n')
		_, err := stdout.Write(indented.Bytes())
		return err
	}
	_, err := io.WriteString(stdout, optimizationreport.Text(report))
	return err
}
