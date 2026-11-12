// Command checkpoint-catalog-sync refreshes the reviewed checkpoint catalog in
// two explicit steps: `fetch --network` records a Hugging Face snapshot, and
// `plan` turns a recorded snapshot into a proposed catalog and a reviewable
// diff without any network access.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
	"github.com/infercrane/infercrane/internal/checkpointsync"
)

const (
	defaultPolicy  = "internal/checkpointcatalog/policy.json"
	defaultCatalog = "internal/checkpointcatalog/catalog.json"
)

var reviewDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type pins []string

func (values *pins) String() string     { return strings.Join(*values, ",") }
func (values *pins) Set(v string) error { *values = append(*values, v); return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "checkpoint-catalog-sync:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: checkpoint-catalog-sync fetch --network --out DIR | plan --snapshot DIR [--write --reviewed-at YYYY-MM-DD]")
	}
	switch args[0] {
	case "fetch":
		return fetch(ctx, args[1:], stderr)
	case "plan":
		return plan(args[1:], stdout)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func fetch(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("fetch", flag.ContinueOnError)
	network := flags.Bool("network", false, "allow requests to the Hugging Face API (required)")
	out := flags.String("out", "", "empty directory for the recorded snapshot")
	endpoint := flags.String("endpoint", checkpointsync.DefaultEndpoint, "Hugging Face endpoint")
	policyPath := flags.String("policy", defaultPolicy, "reviewed policy")
	catalogPath := flags.String("catalog", defaultCatalog, "committed catalog whose pins are re-observed")
	noPrevious := flags.Bool("no-previous", false, "start without a committed catalog (first sync)")
	var extra pins
	flags.Var(&extra, "pin", "repository@revision to observe besides the catalog (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*network {
		return errors.New("fetch reads the public Hugging Face API; pass --network to allow it")
	}
	if *out == "" {
		return errors.New("fetch requires --out")
	}
	policy, digest, err := loadPolicy(*policyPath)
	if err != nil {
		return err
	}
	previous, err := loadPrevious(*catalogPath, *noPrevious)
	if err != nil {
		return err
	}
	fetcher := checkpointsync.Fetcher{Endpoint: *endpoint, Policy: policy, PolicyDigest: digest, Previous: previous, Pins: extra, Log: stderr}
	if _, err = fetcher.Fetch(ctx, *out); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "recorded snapshot in %s; review it with: go run ./tools/checkpoint-catalog-sync plan --snapshot %s\n", *out, *out)
	return nil
}

func plan(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	snapshotDir := flags.String("snapshot", "", "recorded snapshot directory")
	policyPath := flags.String("policy", defaultPolicy, "reviewed policy")
	catalogPath := flags.String("catalog", defaultCatalog, "committed catalog to diff against")
	noPrevious := flags.Bool("no-previous", false, "diff against an empty catalog (first sync)")
	out := flags.String("out", "", "directory for the proposed catalog and diff (default <snapshot>/plan)")
	write := flags.Bool("write", false, "replace --catalog with the proposed catalog after review")
	reviewedAt := flags.String("reviewed-at", "", "review date written into the catalog (required with --write)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *snapshotDir == "" {
		return errors.New("plan requires --snapshot")
	}
	snapshot, err := checkpointsync.OpenSnapshot(*snapshotDir)
	if err != nil {
		return err
	}
	if *write && !reviewDate.MatchString(*reviewedAt) {
		return errors.New("--write requires --reviewed-at YYYY-MM-DD")
	}
	date := *reviewedAt
	if date == "" {
		date = snapshot.Manifest.FetchedAt[:10]
	}
	policy, digest, err := loadPolicy(*policyPath)
	if err != nil {
		return err
	}
	previous, err := loadPrevious(*catalogPath, *noPrevious)
	if err != nil {
		return err
	}
	result, err := checkpointsync.Plan(snapshot, policy, digest, previous, date)
	if err != nil {
		return err
	}
	formatted, err := checkpointsync.FormatCatalog(result.Catalog)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(*snapshotDir, checkpointsync.PlanDir)
	}
	if err = os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	diff := checkpointsync.RenderDiff(result)
	if err = os.WriteFile(filepath.Join(*out, "catalog.json"), formatted, 0o644); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*out, "catalog-diff.md"), diff, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%d entries: %d added, %d revised, %d changed, %d removed, %d unchanged\nproposed catalog: %s\nreviewable diff: %s\n",
		len(result.Catalog.Entries), len(result.Diff.Added), len(result.Diff.Revised), len(result.Diff.Changed), len(result.Diff.Removed), result.Diff.Unchanged,
		filepath.Join(*out, "catalog.json"), filepath.Join(*out, "catalog-diff.md"))
	if *write {
		if err = os.WriteFile(*catalogPath, formatted, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", *catalogPath)
	}
	return nil
}

func loadPolicy(path string) (checkpointcatalog.Policy, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return checkpointcatalog.Policy{}, "", err
	}
	return checkpointcatalog.DecodePolicy(raw)
}

func loadPrevious(path string, empty bool) (checkpointcatalog.Catalog, error) {
	if empty {
		return checkpointcatalog.Catalog{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return checkpointcatalog.Catalog{}, err
	}
	return checkpointcatalog.Decode(raw)
}
