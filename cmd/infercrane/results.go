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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/resultprior"
	"github.com/infercrane/infercrane/internal/resultstore"
)

const resultsUsage = `usage: infercrane results import FILE|RUN_DIR... [--store FILE] [--share numbers] [--technique MANIFEST.json]... [model facts]
       infercrane results list [--store FILE] [filters] [--output text|json]
       infercrane results show RECORD_ID [--store FILE]
       infercrane results export [--store FILE] [filters] > records.json
       infercrane results rank --query QUERY.json [--store FILE] [--output text|json]
With --store the measured results live in that file (no control plane); without it they are your organization's on the control plane.`

// resultsCommand keeps measured cells as result records and ranks setups
// from them (internal/resultstore, internal/resultprior).
func resultsCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(resultsUsage)
	}
	switch args[0] {
	case "import":
		return resultsImport(ctx, args[1:])
	case "list", "export":
		return resultsList(ctx, args[0], args[1:])
	case "show":
		return resultsShow(ctx, args[1:])
	case "rank":
		return resultsRank(ctx, args[1:])
	}
	return errors.New(resultsUsage)
}

// resultsBackend is a local store file or the control plane.
type resultsBackend struct {
	local *resultstore.Local
	cfg   config.Config
}

func openResults(store string) (resultsBackend, error) {
	if store != "" {
		return resultsBackend{local: &resultstore.Local{Path: store}}, nil
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return resultsBackend{}, fmt.Errorf("%w (or pass --store FILE to keep measured results in a local file)", err)
	}
	return resultsBackend{cfg: cfg}, nil
}

func (b resultsBackend) put(ctx context.Context, records []resultstore.Record, sharing string) (int, error) {
	if b.local != nil {
		return b.local.PutResultRecords(ctx, resultstore.LocalTenant, records)
	}
	added := 0
	for start := 0; start < len(records); start += resultstore.MaxPutRecords {
		chunk := records[start:min(start+resultstore.MaxPutRecords, len(records))]
		var receipt struct {
			Added int `json:"added"`
		}
		body := map[string]any{"schema_version": resultstore.BundleSchemaV1, "sharing": sharing, "records": chunk}
		if err := controlJSONWithTimeout(ctx, b.cfg, http.MethodPost, "/api/v1/optimization/results", "", body, &receipt, 2*time.Minute); err != nil {
			return added, err
		}
		added += receipt.Added
	}
	return added, nil
}

func (b resultsBackend) list(ctx context.Context, filter resultstore.Filter) ([]resultstore.Record, error) {
	if b.local != nil {
		return b.local.ListResultRecords(ctx, resultstore.LocalTenant, filter)
	}
	query := url.Values{}
	for name, value := range map[string]string{"model": filter.Model, "accelerator": filter.Accelerator, "engine": filter.Engine, "kind": filter.Kind, "metric": filter.Metric,
		"technique": filter.Technique, "cell": filter.Cell} {
		if value != "" {
			query.Set(name, value)
		}
	}
	for name, value := range map[string]int{"min_load": filter.MinLoad, "max_load": filter.MaxLoad, "limit": filter.Limit} {
		if value > 0 {
			query.Set(name, strconv.Itoa(value))
		}
	}
	var response struct {
		Data []resultstore.Record `json:"data"`
	}
	path := "/api/v1/optimization/results"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	err := controlJSON(ctx, b.cfg, http.MethodGet, path, "", nil, &response)
	return response.Data, err
}

func resultsImport(ctx context.Context, args []string) error {
	var files []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		files, args = append(files, args[0]), args[1:]
	}
	fs := newFlagSet("results import")
	store := fs.String("store", "", "keep the measured results in this JSON Lines file instead of the control plane")
	share := fs.String("share", resultstore.SharingPrivate, "private, or numbers: rankings for other organizations may use these results' numbers, without names")
	architecture := fs.String("architecture", "", "the model's architecture when the file does not say: dense, moe, or hybrid")
	params := fs.Float64("params-b", 0, "the model's parameters in billions when the file does not say")
	active := fs.Float64("active-params-b", 0, "a mixture of experts' parameters read per token, in billions")
	checkpoint := fs.String("checkpoint", "", "the precision of the published weights, for example bf16 or fp8")
	set := fs.String("set", "", "the measurement's name (default: the file's own, or its name)")
	recorded := fs.String("recorded-at", "", "when it was measured, RFC 3339, when the file does not say")
	commit := fs.String("commit", "", "the commit that ran it")
	image := fs.String("image-digest", "", "the serving image's sha256 digest when the file does not name it")
	basis := fs.String("basis", "", "how the setups were placed: same-machine, separate-servers, or fresh-servers")
	var techniques stringList
	fs.Var(&techniques, "technique", "a technique manifest or static technique file that judges a benchmark run's results of that technique and version (repeatable); without one a result makes no claim")
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return errors.New(resultsUsage)
		}
		args = fs.Args()
		for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			files, args = append(files, args[0]), args[1:]
		}
	}
	if len(files) == 0 {
		return errors.New(resultsUsage)
	}
	options := resultstore.ImportOptions{Model: resultstore.Model{Architecture: *architecture, ParamsB: *params, ActiveParamsB: *active, Checkpoint: *checkpoint},
		Set: *set, Commit: *commit, ImageDigest: *image, Basis: *basis, Sharing: *share}
	if *recorded != "" {
		at, err := time.Parse(time.RFC3339, *recorded)
		if err != nil {
			return fmt.Errorf("--recorded-at: %w", err)
		}
		options.RecordedAt = at
	}
	for _, path := range techniques {
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		loaded, err := resultstore.LoadManifest(body)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		options.Manifests = append(options.Manifests, loaded)
	}
	backend, err := openResults(*store)
	if err != nil {
		return err
	}
	var records []resultstore.Record
	for _, file := range files {
		var imported []resultstore.Record
		if info, err := os.Stat(file); err == nil && info.IsDir() {
			// A benchmark runner's run directory.
			imported, err = resultstore.ImportRun(file, options)
			if err != nil {
				return err
			}
		} else {
			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if imported, err = resultstore.Import(file, body, options); err != nil {
				return err
			}
		}
		printClaims(os.Stdout, file, imported)
		records = append(records, imported...)
	}
	added, err := backend.put(ctx, records, options.Sharing)
	if err != nil {
		return err
	}
	where := "your organization's measured results"
	if backend.local != nil {
		where = backend.local.Path
	}
	fmt.Printf("Added %d of %d measured results to %s (sharing: %s).\n", added, len(records), where, options.Sharing)
	return nil
}

// printClaims says how many results make a claim and, for the rest, how
// often each reason withholds it.
func printClaims(w io.Writer, file string, records []resultstore.Record) {
	claims, reasons := 0, map[string]int{}
	for _, record := range records {
		why := record.NotShownBecause()
		if len(why) == 0 {
			claims++
		}
		for _, reason := range why {
			reasons[reason]++
		}
	}
	fmt.Fprintf(w, "%s: %d measured results, %d make a claim\n", file, len(records), claims)
	for _, reason := range sortedKeys(reasons) {
		fmt.Fprintf(w, "  %d not shown: %s\n", reasons[reason], reason)
	}
}

func resultFilterFlags(fs *flag.FlagSet) *resultstore.Filter {
	filter := &resultstore.Filter{}
	fs.StringVar(&filter.Model, "model", "", "model ID")
	fs.StringVar(&filter.Accelerator, "gpu", "", "GPU, for example H100")
	fs.StringVar(&filter.Engine, "engine", "", "engine, for example vllm")
	fs.StringVar(&filter.Kind, "kind", "", "traffic kind, for example short")
	fs.StringVar(&filter.Metric, "metric", "", "metric, for example output_tokens_per_second")
	fs.StringVar(&filter.Technique, "technique", "", "technique ID")
	fs.StringVar(&filter.Cell, "cell", "", "the ID of one measurement of one traffic kind at one load")
	fs.IntVar(&filter.MinLoad, "min-load", 0, "fewest requests at once")
	fs.IntVar(&filter.MaxLoad, "max-load", 0, "most requests at once")
	fs.IntVar(&filter.Limit, "limit", 0, "at most this many results, newest first (default 200, at most 2000)")
	return filter
}

func resultsList(ctx context.Context, action string, args []string) error {
	fs := newFlagSet("results " + action)
	store := fs.String("store", "", "read the measured results in this JSON Lines file instead of the control plane")
	output := fs.String("output", "text", "text or json")
	filter := resultFilterFlags(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*output != "text" && *output != "json") {
		return errors.New(resultsUsage)
	}
	backend, err := openResults(*store)
	if err != nil {
		return err
	}
	records, err := backend.list(ctx, *filter)
	if err != nil {
		return err
	}
	if action == "export" {
		return printJSON(resultstore.NewBundle(records))
	}
	if *output == "json" {
		return printJSON(map[string]any{"data": records})
	}
	printResultRecords(os.Stdout, records)
	return nil
}

func printResultRecords(w io.Writer, records []resultstore.Record) {
	if len(records) == 0 {
		fmt.Fprintln(w, "No measured results.")
		return
	}
	table := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(table, "CELL\tGPU\tSETUP\tMETRIC\tMEDIAN\tCHANGE\tDECISION\tCLAIM\tID")
	for _, record := range records {
		change := "-"
		if record.Result.Measured.Decision != "unknown" || record.Result.Measured.Change != 0 {
			change = fmt.Sprintf("%+.1f%%", 100*record.Result.Measured.Change)
		}
		fmt.Fprintf(table, "%s %s\t%s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%s\n", record.Cell.Set, record.Cell.Key, record.Accelerator(), record.Setup.Name,
			record.Metric(), record.Observed.Median, change, record.Result.Measured.Decision, record.Claim(), record.ID)
	}
	table.Flush()
}

func resultsShow(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(resultsUsage)
	}
	id := args[0]
	fs := newFlagSet("results show")
	store := fs.String("store", "", "read the measured results in this JSON Lines file instead of the control plane")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(resultsUsage)
	}
	backend, err := openResults(*store)
	if err != nil {
		return err
	}
	var record resultstore.Record
	if backend.local != nil {
		record, err = backend.local.GetResultRecord(ctx, resultstore.LocalTenant, id)
	} else {
		err = controlJSON(ctx, backend.cfg, http.MethodGet, "/api/v1/optimization/results/"+url.PathEscape(id), "", nil, &record)
	}
	if err != nil {
		return err
	}
	if err = printJSON(record); err != nil {
		return err
	}
	if unknown := record.Result.Unknown(); len(unknown) > 0 {
		fmt.Fprintf(os.Stderr, "Claim: not shown. Unknown: %s.\n", strings.Join(unknown, ", "))
	}
	return nil
}

func resultsRank(ctx context.Context, args []string) error {
	fs := newFlagSet("results rank")
	store := fs.String("store", "", "rank from the measured results in this JSON Lines file instead of the control plane")
	queryFile := fs.String("query", "", "the traffic, load, GPU, model and setups to rank, as JSON (docs/features/results-store.mdx)")
	output := fs.String("output", "text", "text or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *queryFile == "" || (*output != "text" && *output != "json") {
		return errors.New(resultsUsage)
	}
	body, err := os.ReadFile(*queryFile)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var query resultprior.Query
	if err = decoder.Decode(&query); err != nil {
		return fmt.Errorf("%s: %w", *queryFile, err)
	}
	backend, err := openResults(*store)
	if err != nil {
		return err
	}
	var ranking resultprior.Ranking
	if backend.local != nil {
		ranking, err = resultprior.StorePrior{Store: backend.local}.Rank(ctx, resultstore.LocalTenant, query)
	} else {
		err = controlJSON(ctx, backend.cfg, http.MethodPost, "/api/v1/optimization/results/rank", "", query, &ranking)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(ranking)
	}
	fmt.Printf("Ranked from %d measured results in %d measurements (%s), on %s, against the current setup.\n", ranking.Records, ranking.Cells, ranking.Algorithm, strings.ReplaceAll(ranking.Metric, "_", " "))
	for index, ranked := range ranking.Ranked {
		fmt.Printf("%d. %s (%s): %s\n", index+1, ranked.Name, ranked.Setup, ranked.Reason)
	}
	fmt.Println("A ranking orders what to measure. It is not a result: only measuring the setups on your traffic decides.")
	return nil
}
