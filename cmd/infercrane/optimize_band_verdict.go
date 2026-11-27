package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/bandverdict"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

// optimizeBandVerdictCommand gives a verdict per load band from a screen
// summary. It reads one local file and calls nothing, so it runs against
// results from any server or harness.
func optimizeBandVerdictCommand(args []string) error {
	fs := newFlagSet("optimize band-verdict")
	filePath := fs.String("file", "", "band screen summary JSON (schemas/band-screen-v1.schema.json)")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*filePath) == "" {
		return errors.New("usage: infercrane optimize band-verdict --file SCREEN.json [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	body, err := os.ReadFile(*filePath)
	if err != nil {
		return fmt.Errorf("read band screen: %w", err)
	}
	screen, err := bandverdict.Decode(body)
	if err != nil {
		return err
	}
	verdict, err := bandverdict.Decide(screen)
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(verdict)
	}
	printBandVerdict(os.Stdout, verdict, screen.Current)
	fmt.Println("A screen's verdict is not a proof: finalists are measured again on fresh servers.")
	return nil
}

// optimizeBandsCommand reads finished campaigns, one per load, as a band
// study: each campaign's decision is its load band's verdict.
func optimizeBandsCommand(ctx context.Context, args []string) error {
	const usage = "usage: infercrane optimize bands RUN_ID RUN_ID [RUN_ID...] [--output text|json]"
	var ids []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ids, args = append(ids, args[0]), args[1:]
	}
	fs := newFlagSet("optimize bands")
	output := fs.String("output", "text", "text or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || len(ids) < 2 || (*output != "text" && *output != "json") {
		return errors.New(usage)
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	var study optimizationreport.BandStudy
	if err = controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/band-study?campaigns="+url.QueryEscape(strings.Join(ids, ",")), "", nil, &study); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(study)
	}
	fmt.Printf("Band study of %s, %d runs, one per load.\n", study.Model, len(study.Campaigns))
	printBandVerdict(os.Stdout, study.Verdict, study.Current)
	fmt.Println("\nSetups:")
	for _, key := range sortedKeys(study.Setups) {
		setup := study.Setups[key]
		fmt.Printf("  %s  %s · %s %s %s\n", key, setup.Label, setup.Engine, setup.EngineVersion, strings.Join(setup.Args, " "))
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func printBandVerdict(w io.Writer, verdict bandverdict.Verdict, current string) {
	fmt.Fprintf(w, "Verdict per load band, judged on %s against %s.\n", strings.ReplaceAll(verdict.Metric, "_", " "), current)
	for _, band := range verdict.Bands {
		copyText := "no copy"
		if band.CopyGain != nil {
			copyText = fmt.Sprintf("copy %+.1f%%", 100**band.CopyGain)
		}
		fmt.Fprintf(w, "\n%s: %s\n  current %.1f, repeats differ by %.1f%%, %s, noise %.1f%%\n", band.Key, band.Decision, band.Current, 100*band.Spread, copyText, 100*band.NoiseFloor)
		for _, setup := range band.Setups {
			change := "-"
			if setup.Gain != nil {
				change = fmt.Sprintf("%+.1f%%", 100**setup.Gain)
			}
			mark := ""
			switch {
			case setup.Ahead:
				mark = "  ahead"
			case setup.HeldBack != "":
				mark = "  held back: " + setup.HeldBack
			}
			fmt.Fprintf(w, "  %s %s%s\n", setup.ID, change, mark)
		}
		fmt.Fprintf(w, "  %s\n", band.Reason)
	}
	fmt.Fprintf(w, "\n%s: %s\n", verdict.Summary, verdict.Reason)
	if len(verdict.SwitchBands) > 0 {
		fmt.Fprintln(w, "Band table for a per-server switch:")
		for _, row := range verdict.SwitchBands {
			bound := "above"
			if row.UpTo > 0 {
				bound = fmt.Sprintf("up to %d", row.UpTo)
			}
			fmt.Fprintf(w, "  %s requests at once: %s\n", bound, row.Engine)
		}
	}
}
