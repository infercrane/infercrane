package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/accounting"
	"github.com/infercrane/infercrane/internal/artifact"
	"github.com/infercrane/infercrane/internal/authn"
	"github.com/infercrane/infercrane/internal/autoscale"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gateway"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/planning"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/reconcile"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
	runtimeadapter "github.com/infercrane/infercrane/internal/runtime"
	"github.com/infercrane/infercrane/internal/spec"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/workflows"
)

var version = "0.2.0-rc.1"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		writeCLIError(os.Stderr, os.Args[1:], err)
		os.Exit(2)
	}
}

func writeCLIError(w io.Writer, args []string, err error) {
	jsonOutput := false
	for index, arg := range args {
		if arg == "--output=json" || (arg == "--output" && index+1 < len(args) && args[index+1] == "json") {
			jsonOutput = true
			break
		}
	}
	if !jsonOutput {
		fmt.Fprintln(w, "Error:", err)
		return
	}
	detail := map[string]any{"code": "client_error", "category": "client", "message": err.Error(), "retryable": false, "remediation": "Correct the command or inspect current durable state before retrying."}
	var controlErr *ControlError
	if errors.As(err, &controlErr) {
		detail = map[string]any{"code": controlErr.Code, "category": controlErr.Category, "message": controlErr.Message, "retryable": controlErr.Retryable, "remediation": controlErr.Remediation, "status": controlErr.StatusCode}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": detail})
}
func run(ctx context.Context, args []string) error {
	for len(args) > 0 {
		switch args[0] {
		case "--no-color":
			if err := os.Setenv("NO_COLOR", "1"); err != nil {
				return err
			}
			args = args[1:]
		case "--context":
			if len(args) < 2 || strings.HasPrefix(args[1], "-") {
				return errors.New("--context requires a context name")
			}
			if err := os.Setenv("INFERCRANE_CONTEXT", args[1]); err != nil {
				return err
			}
			args = args[2:]
		default:
			goto execute
		}
	}
execute:
	root := newRootCommand(ctx)
	root.SetArgs(args)
	return root.Execute()
}

func runLegacy(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("a command is required")
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "init":
		return initCommand(args[1:])
	case "context":
		return contextCommand(args[1:])
	}
	switch args[0] {
	case "target", "deploy", "apply", "plan", "doctor", "ui", "deployments", "route", "status", "events", "logs", "request", "explain", "rollout", "delete", "inspect", "operation", "orphans", "integrations", "context", "auth", "tenant", "principal", "benchmark", "serve":
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	if args[0] == "serve" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		s, err := store.Open(ctx, cfg.DatabaseURL, store.Options{MaxOpenConns: cfg.DatabaseMaxOpen, MaxIdleConns: cfg.DatabaseMaxIdle})
		if err != nil {
			return err
		}
		defer s.Close()
		return serve(ctx, cfg, s)
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	switch args[0] {
	case "plan":
		return planCommand(ctx, cfg, args[1:])
	case "doctor":
		return doctorCommand(ctx, cfg, args[1:])
	case "ui":
		return uiCommand(ctx, cfg, args[1:])
	case "deploy":
		return deployAPICommand(ctx, cfg, "deploy", args[1:])
	case "apply":
		return deployAPICommand(ctx, cfg, "apply", args[1:])
	case "delete":
		return deleteAPICommand(ctx, cfg, args[1:])
	case "deployments":
		return listDeployments(ctx, cfg, args[1:])
	case "status":
		return statusCommand(ctx, cfg, args[1:])
	case "events":
		return eventsCommand(ctx, cfg, args[1:])
	case "logs":
		return logsCommand(ctx, cfg, args[1:])
	case "auth":
		return authCommand(ctx, cfg, args[1:])
	case "request":
		return requestCommand(ctx, cfg, args[1:])
	case "inspect":
		return inspectCommand(ctx, cfg, args[1:])
	case "explain":
		return explainCommand(ctx, cfg, args[1:])
	case "rollout":
		return rolloutCommand(ctx, cfg, args[1:])
	case "operation":
		return operationCommand(ctx, cfg, args[1:])
	case "target":
		return targetAPICommand(ctx, cfg, args[1:])
	case "orphans":
		return orphanAPICommand(ctx, cfg, args[1:])
	case "integrations":
		return integrationsCommand(ctx, cfg, args[1:])
	case "route":
		return routeAPICommand(ctx, cfg, args[1:])
	case "tenant":
		return tenantAPICommand(ctx, cfg, args[1:])
	case "principal":
		return principalAPICommand(ctx, cfg, args[1:])
	case "benchmark":
		return benchmarkCommand(ctx, cfg, args[1:])
	}
	return fmt.Errorf("%s has not yet been migrated to the control-plane API", args[0])
}
func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	controlURL := fs.String("url", "http://127.0.0.1:8080", "control-plane URL")
	apiKey := fs.String("api-key", "", "existing control-plane credential (prefer INFERCRANE_API_KEY to avoid shell history)")
	contextName := fs.String("context", "default", "context name")
	skipCheck := fs.Bool("skip-check", false, "store configuration without validating the control plane")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *output != "human" && *output != "json" {
		return errors.New("--output must be human or json")
	}
	credential := *apiKey
	if credential == "" {
		credential = os.Getenv("INFERCRANE_API_KEY")
	}
	if credential == "" {
		return errors.New("an existing control-plane credential is required; pass --api-key or set INFERCRANE_API_KEY")
	}
	if !*skipCheck {
		var identity struct {
			Principal map[string]any `json:"principal"`
		}
		if err := controlJSON(context.Background(), config.Config{ControlURL: *controlURL, APIKey: credential}, http.MethodGet, "/api/v1/whoami", "", nil, &identity); err != nil {
			return fmt.Errorf("validate control-plane connection: %w (use --skip-check only when intentionally configuring an offline control plane)", err)
		}
	}
	path, err := config.InitializeClientContext(*contextName, *controlURL, credential, true)
	if err != nil {
		return err
	}
	result := map[string]any{"config_path": path, "context": *contextName, "control_url": *controlURL, "credential_stored": true}
	switch *output {
	case "json":
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
	case "human":
		fmt.Printf("InferCrane configured\nContext        %s\nControl plane  %s\nConfig         %s\nCredential     existing credential stored with mode 0600\n", *contextName, *controlURL, path)
	}
	return nil
}

func planCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane plan MODEL [--name NAME] [--targets TARGETS | --cloud CLOUD --gpu GPU]")
	}
	model := args[0]
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	name := fs.String("name", "", "deployment name")
	targets := fs.String("targets", "", "comma-separated existing targets")
	cloud := fs.String("cloud", "", "provider cloud")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	computeMode := fs.String("compute", "elastic", "compute mode: elastic or serverless")
	minReplicas := fs.Int("min", 1, "minimum replicas")
	maxReplicas := fs.Int("max", 1, "maximum replicas")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	minExplicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "min" {
			minExplicit = true
		}
	})
	if *computeMode == "serverless" && !minExplicit {
		*minReplicas = 0
	}
	in := planning.Input{Name: *name, Model: model, ComputeMode: *computeMode, Cloud: *cloud, GPU: *gpu, Region: *region, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas}
	if ext := filepath.Ext(model); ext == ".yaml" || ext == ".yml" {
		file, err := spec.Load(model)
		if err != nil {
			return err
		}
		if *name != "" || *targets != "" || *cloud != "" || *gpu != "" || *region != "" {
			return errors.New("deployment YAML cannot be combined with deployment flags")
		}
		in = planning.Input{Name: file.Name, Model: file.Model.ID, ComputeMode: file.Compute.Mode, Cloud: file.Provider.Cloud,
			GPU: file.Resources.GPU, Region: file.Provider.Region, Runtime: file.Runtime.Engine,
			RuntimeArgs: file.Runtime.Args, Routing: file.Routing.Strategy,
			MinReplicas: file.Scaling.MinReplicas, MaxReplicas: file.Scaling.MaxReplicas}
	} else if *targets != "" {
		if *computeMode == "serverless" {
			return errors.New("--compute serverless cannot be combined with --targets")
		}
		in.Targets = splitTargets(*targets)
	}
	if len(in.Targets) == 0 && in.Cloud == "" && in.GPU == "" {
		in.Cloud, in.GPU = support.DefaultCloud, support.DefaultGPU
	}
	p, err := planning.Build(in)
	if err != nil {
		return err
	}
	current, lookupErr := fetchDeployment(ctx, cfg, p.Name)
	if lookupErr == nil {
		activeNumber := 0
		activeSpec := domain.DeploymentRevisionSpec{}
		for _, revision := range current.Revisions {
			if revision.ID == current.Deployment.ActiveRevisionID {
				activeNumber = revision.Number
				if len(revision.Spec) > 0 {
					if err := json.Unmarshal(revision.Spec, &activeSpec); err != nil {
						return fmt.Errorf("decode active revision for plan: %w", err)
					}
				}
				break
			}
		}
		p = planning.Compare(p, planning.Current{Model: current.Deployment.Model, Runtime: current.Deployment.Runtime, Routing: current.Deployment.RoutingStrategy, ComputeMode: activeSpec.ComputeMode, Cloud: activeSpec.Cloud, GPU: activeSpec.GPU, Region: activeSpec.Region, MinReplicas: current.Deployment.MinReplicas, MaxReplicas: current.Deployment.MaxReplicas, ActiveRevision: current.Deployment.ActiveRevisionID, ActiveRevisionNumber: activeNumber})
	} else {
		var controlErr *ControlError
		if !errors.As(lookupErr, &controlErr) || controlErr.Code != "not_found" {
			return fmt.Errorf("read current deployment for plan: %w", lookupErr)
		}
	}
	switch *output {
	case "json":
		encoded, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
	case "human":
		fmt.Printf("Deployment plan: %s\nModel:           %s\nMode:            %s\nRuntime:         %s\nRouting:         %s\n\n", p.Name, p.Model, p.Mode, p.Runtime, p.Routing)
		for _, change := range p.Changes {
			fmt.Printf("%-15s %s -> %s\n", change.Field, change.Before, change.After)
		}
		if len(p.Changes) > 0 {
			fmt.Println()
		}
		for _, action := range p.Actions {
			symbol := "+"
			if action.Kind == "drain" || action.Kind == "terminate" {
				symbol = "-"
			} else if action.Kind == "noop" {
				symbol = "="
			}
			fmt.Printf("%s %s\n", symbol, action.Summary)
		}
		for _, warning := range p.Warnings {
			fmt.Printf("\nWarning: %s\n", warning)
		}
		fmt.Printf("\nCost: %s — %s\n", p.Cost.Status, p.Cost.Reason)
	default:
		return errors.New("--output must be human or json")
	}
	return nil
}

func doctorCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	cloud := fs.Bool("cloud", false, "also validate SkyPilot cloud credentials")
	serverless := fs.Bool("serverless", false, "also validate RunPod Serverless credentials and template")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	query := url.Values{}
	query.Set("cloud", fmt.Sprint(*cloud))
	query.Set("serverless", fmt.Sprint(*serverless))
	var report doctor.Report
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/doctor?"+query.Encode(), "", nil, &report); err != nil {
		return err
	}
	switch *output {
	case "json":
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
	case "human":
		for _, check := range report.Checks {
			fmt.Printf("%-5s %-20s %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Message)
			if check.Remediation != "" {
				fmt.Printf("      Fix: %s\n", check.Remediation)
			}
		}
		if len(report.Capabilities) > 0 {
			fmt.Println("\nProvider capabilities")
			for _, capability := range report.Capabilities {
				fmt.Printf("%-10s %-24s %-11s %s\n", capability.Adapter, capability.Name, strings.ToUpper(capability.State), capability.Detail)
			}
		}
	default:
		return errors.New("--output must be human or json")
	}
	return report.Err()
}

func splitTargets(raw string) []string {
	var names []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			names = append(names, item)
		}
	}
	return names
}

func validateOutput(output string) error {
	if output != "human" && output != "json" {
		return errors.New("--output must be human or json")
	}
	return nil
}

func benchmarkCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane benchmark DEPLOYMENT [flags]")
	}
	deployment := args[0]
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	requests := fs.Int("requests", 100, "request count")
	concurrency := fs.Int("concurrency", 10, "concurrent clients")
	randomSeed := fs.Int64("random-seed", 17, "deterministic AIPerf dataset seed")
	revision := fs.String("revision", "active", "revision to benchmark: active, candidate, or revision ID")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane benchmark DEPLOYMENT [flags]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Benchmark map[string]any `json:"benchmark"`
	}
	request := map[string]any{"requests": *requests, "concurrency": *concurrency, "random_seed": *randomSeed, "revision": *revision}
	if *revision != "active" {
		fmt.Fprintln(os.Stderr, "Notice: selected-revision validation sends an explicit AIPerf workload directly to revision capacity and may incur provider inference cost; it does not duplicate user traffic.")
	}
	// AIPerf runs can legitimately exceed the ordinary control request timeout.
	if err := controlJSONWithTimeout(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(deployment)+"/benchmarks", "", request, &response, 35*time.Minute); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response.Benchmark, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	fmt.Printf("Benchmark     %s\nRevision      %s\nModel         %s\nRuntime       %s %s\nRuntime args  %s\nGPU           %s x %s\nProvider      %s\nRegion        %s\nCompute mode  %s\nWorkload      %s\nTTFT p50      %s ms\nTTFT p95      %s ms\nTPOT p95      %s ms\nOutput tok/s  %s\nGoodput       %s\nGPU util      %s\nErrors        %s\nCost          %s\n\nReproduce:\n  %s\n", benchmarkValue(response.Benchmark["id"]), benchmarkValue(response.Benchmark["revision_id"]), benchmarkValue(response.Benchmark["model_identity"]), benchmarkValue(response.Benchmark["runtime"]), benchmarkValue(response.Benchmark["runtime_version"]), benchmarkValue(response.Benchmark["runtime_configuration"]), benchmarkValue(response.Benchmark["gpu"]), benchmarkValue(response.Benchmark["gpu_count"]), benchmarkValue(response.Benchmark["provider"]), benchmarkValue(response.Benchmark["region"]), benchmarkValue(response.Benchmark["compute_mode"]), benchmarkValue(response.Benchmark["workload"]), benchmarkValue(response.Benchmark["ttft_p50_ms"]), benchmarkValue(response.Benchmark["ttft_p95_ms"]), benchmarkValue(response.Benchmark["tpot_p95_ms"]), benchmarkValue(response.Benchmark["output_token_throughput"]), benchmarkValue(response.Benchmark["goodput"]), benchmarkValue(response.Benchmark["gpu_utilization"]), benchmarkValue(response.Benchmark["failed"]), benchmarkValue(response.Benchmark["cost_metadata"]), benchmarkValue(response.Benchmark["reproduction_command"]))
	return nil
}

func benchmarkValue(value any) string {
	if value == nil {
		return "unavailable"
	}
	if text, ok := value.(string); ok && text == "" {
		return "unavailable"
	}
	if encoded, err := json.Marshal(value); err == nil {
		if _, structured := value.(map[string]any); structured {
			return string(encoded)
		}
		if _, structured := value.([]any); structured {
			return string(encoded)
		}
	}
	return fmt.Sprint(value)
}
func targetAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("target requires add or list")
	}
	if args[0] == "list" {
		var response struct {
			Data []targetView `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/targets", "", nil, &response); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tURL\tRUNTIME\tHEALTH")
		for _, target := range response.Data {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", target.Name, target.URL, target.Runtime, target.Health)
		}
		return w.Flush()
	}
	if args[0] != "add" || len(args) < 2 {
		return errors.New("usage: infercrane target add NAME --url URL")
	}
	fs := flag.NewFlagSet("target add", flag.ContinueOnError)
	targetURL := fs.String("url", "", "target URL")
	runtimeName := fs.String("runtime", "vllm", "runtime")
	upstream := fs.String("upstream-model", "", "upstream model")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *targetURL == "" {
		return errors.New("--url is required")
	}
	request := map[string]string{"name": args[1], "url": *targetURL, "runtime": *runtimeName, "upstream_model": *upstream}
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/targets", "", request, nil); err != nil {
		return err
	}
	fmt.Printf("target %s registered\n", args[1])
	return nil
}
func deployAPICommand(ctx context.Context, cfg config.Config, operationKind string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane deploy MODEL [--cloud CLOUD --gpu GPU | --targets TARGETS]")
	}
	model := args[0]
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	name := fs.String("name", "", "deployment name")
	targets := fs.String("targets", "", "comma-separated targets")
	cloud := fs.String("cloud", "", "provider cloud")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	computeMode := fs.String("compute", "elastic", "compute mode: elastic or serverless")
	minReplicas := fs.Int("min", 1, "minimum replicas")
	maxReplicas := fs.Int("max", 1, "maximum replicas")
	wait := fs.Bool("wait", false, "wait for the operation to finish")
	waitTimeout := fs.Duration("wait-timeout", 0, "stop waiting after this duration; the durable operation continues")
	idempotencyKey := fs.String("idempotency-key", "", "safe retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *output != "human" && *output != "json" {
		return errors.New("--output must be human or json")
	}
	if *waitTimeout < 0 || (*waitTimeout > 0 && !*wait) {
		return errors.New("--wait-timeout must be non-negative and requires --wait")
	}
	minExplicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "min" {
			minExplicit = true
		}
	})
	strategy := "round-robin"
	runtimeArgs := []string{}
	runtimeEngine := "vllm"
	modelRevision := ""
	runtimeVersion := ""
	if ext := filepath.Ext(model); ext == ".yaml" || ext == ".yml" {
		file, err := spec.Load(model)
		if err != nil {
			return err
		}
		if *name != "" || *targets != "" || *cloud != "" || *gpu != "" || *region != "" {
			return errors.New("deployment YAML cannot be combined with deployment flags")
		}
		*name = file.Name
		model = file.Model.ID
		modelRevision = file.Model.Revision
		runtimeVersion = file.Runtime.Version
		runtimeEngine = file.Runtime.Engine
		*computeMode = file.Compute.Mode
		*cloud = file.Provider.Cloud
		*gpu = file.Resources.GPU
		*region = file.Provider.Region
		strategy = file.Routing.Strategy
		runtimeArgs = file.Runtime.Args
		*minReplicas, *maxReplicas = file.Scaling.MinReplicas, file.Scaling.MaxReplicas
	}
	if *name == "" {
		*name = planning.DefaultName(model)
	}
	if runtimeVersion == "" {
		runtimeVersion = support.DefaultRuntimeVersion
	}
	if *targets == "" && *cloud == "" && *gpu == "" {
		*cloud, *gpu = support.DefaultCloud, support.DefaultGPU
	}
	if *computeMode == "serverless" && !minExplicit {
		*minReplicas = 0
	}
	if *computeMode != "elastic" && *computeMode != "serverless" {
		return errors.New("--compute must be elastic or serverless")
	}
	if (*computeMode == "elastic" && *minReplicas < 1) || (*computeMode == "serverless" && *minReplicas != 0) || *maxReplicas < 1 || *maxReplicas < *minReplicas {
		return errors.New("elastic requires 1 <= min <= max; serverless requires min=0 and max>=1")
	}
	if *idempotencyKey == "" {
		*idempotencyKey = fmt.Sprintf("cli-%s-%d", *name, time.Now().UnixNano())
	}
	path := "/api/v1/deployments"
	var request any
	if *targets != "" {
		if *computeMode != "elastic" {
			return errors.New("--compute serverless cannot be combined with --targets")
		}
		if *cloud != "" || *gpu != "" {
			return errors.New("use either --targets or --cloud/--gpu")
		}
		path = "/api/v1/deployments/apply"
		request = workflows.ApplyExistingRequest{Name: *name, Model: model, Targets: splitTargets(*targets), RoutingStrategy: strategy, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas, AutoscalingEnabled: *maxReplicas > *minReplicas}
	} else if *cloud != "" && *gpu != "" {
		request = workflows.CloudRequest{Name: *name, Model: model, ModelRevision: modelRevision, Runtime: runtimeEngine, RuntimeVersion: runtimeVersion, ComputeMode: *computeMode, Cloud: *cloud, GPU: *gpu, Region: *region, RuntimeArgs: runtimeArgs, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas}
	} else {
		return errors.New("provide --targets or both --cloud and --gpu")
	}
	var response struct {
		Operation domain.Operation `json:"operation"`
	}
	if err := controlJSON(ctx, cfg, http.MethodPost, path, *idempotencyKey, request, &response); err != nil {
		return fmt.Errorf("%w; safe retry key: %s", err, *idempotencyKey)
	}
	if *wait {
		if *output == "human" {
			fmt.Printf("Deployment  %s\nOperation   %s\nStatus      %s\n\nYou can close this terminal safely. Resume with:\n  infercrane operation watch %s\n\n", *name, response.Operation.ID, terminalStatus(response.Operation.Status), response.Operation.ID)
		}
		operation, err := waitForOperationWithin(ctx, *waitTimeout, cfg, response.Operation.ID, true)
		if err != nil {
			return err
		}
		response.Operation = operation
	}
	logicalEndpoint := strings.TrimRight(cfg.ControlURL, "/") + "/v1"
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{"deployment": *name, "endpoint": logicalEndpoint, "model": model, "runtime": runtimeEngine, "provider": *cloud, "compute_mode": *computeMode, "idempotency_key": *idempotencyKey, "operation": response.Operation}, "", "  ")
		fmt.Println(string(encoded))
	} else if *output == "human" {
		fmt.Printf("Deployment  %s\nEndpoint    %s\nModel       %s\nRuntime     %s\nProvider    %s\nCompute     %s\nOperation   %s\nStatus      %s\nRetry key   %s\n", *name, logicalEndpoint, model, runtimeEngine, displayValue(*cloud, "existing targets"), *computeMode, response.Operation.ID, terminalStatus(response.Operation.Status), *idempotencyKey)
		if response.Operation.Status == "succeeded" {
			fmt.Printf("\nNext\n  infercrane request %s --message \"Hello\"\n  infercrane status %s\n", *name, *name)
		} else {
			fmt.Printf("\nFollow progress\n  infercrane operation watch %s\n  infercrane status %s --watch\n  infercrane events %s\n", response.Operation.ID, *name, *name)
		}
	}
	_ = operationKind // deploy/apply share API semantics; retained for command UX.
	return nil
}

func displayValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

type deploymentSummary struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Model               string `json:"model"`
	Runtime             string `json:"runtime"`
	RoutingStrategy     string `json:"routing_strategy"`
	DesiredState        string `json:"desired_state"`
	ObservedState       string `json:"observed_state"`
	ActiveRevisionID    string `json:"active_revision_id"`
	CandidateRevisionID string `json:"candidate_revision_id"`
	MinReplicas         int    `json:"min_replicas"`
	MaxReplicas         int    `json:"max_replicas"`
	AutoscalingEnabled  bool   `json:"autoscaling_enabled"`
}
type targetView struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	URL                string          `json:"url"`
	Provider           string          `json:"provider"`
	Runtime            string          `json:"runtime"`
	UpstreamModel      string          `json:"upstream_model"`
	Health             string          `json:"health"`
	ProviderResourceID string          `json:"provider_resource_id"`
	ProviderDetails    json.RawMessage `json:"provider_details"`
}
type replicaView struct {
	ID                 string          `json:"id"`
	RevisionID         string          `json:"revision_id"`
	ExternalKey        string          `json:"external_key"`
	LifecycleState     string          `json:"lifecycle_state"`
	Provider           string          `json:"provider"`
	ProviderRequestID  string          `json:"provider_request_id"`
	ProviderResourceID string          `json:"provider_resource_id"`
	Endpoint           string          `json:"endpoint"`
	Health             string          `json:"health"`
	Ordinal            int             `json:"ordinal"`
	ProviderDetails    json.RawMessage `json:"provider_details"`
}
type revisionView struct {
	ID               string          `json:"id"`
	Status           string          `json:"status"`
	SourceRevisionID string          `json:"source_revision_id"`
	Reason           string          `json:"reason"`
	Number           int             `json:"number"`
	Spec             json.RawMessage `json:"spec"`
}
type artifactView struct {
	ID                   string          `json:"id"`
	RevisionID           string          `json:"revision_id"`
	Source               string          `json:"source"`
	Repository           string          `json:"repository"`
	RequestedRevision    string          `json:"requested_revision"`
	ImmutableRevision    string          `json:"immutable_revision"`
	ModelIdentity        string          `json:"model_identity"`
	ApproximateSizeBytes *int64          `json:"approximate_size_bytes"`
	CacheState           string          `json:"cache_state"`
	RuntimeCompatibility json.RawMessage `json:"runtime_compatibility"`
	ResolvedAt           time.Time       `json:"resolved_at"`
}
type deploymentView struct {
	Deployment              deploymentSummary         `json:"deployment"`
	Targets                 []targetView              `json:"targets"`
	Replicas                []replicaView             `json:"replicas"`
	Revisions               []revisionView            `json:"revisions"`
	ModelArtifacts          []artifactView            `json:"model_artifacts"`
	RequestStats            domain.RequestStats       `json:"request_stats"`
	ColdStartStats          domain.ColdStartStats     `json:"cold_start_stats"`
	ReleaseGuardPolicy      domain.ReleaseGuardPolicy `json:"release_guard_policy"`
	ReleaseGuardEvaluations []releaseGuardView        `json:"release_guard_evaluations"`
	ActiveOperation         *domain.Operation         `json:"active_operation,omitempty"`
	LifecycleStatus         lifecycleStatusView       `json:"lifecycle_status"`
}

type lifecycleStatusView struct {
	ServingState          string `json:"serving_state"`
	ConvergenceState      string `json:"convergence_state"`
	CandidateState        string `json:"candidate_state"`
	ReadyReplicas         int    `json:"ready_replicas"`
	DesiredReplicas       int    `json:"desired_replicas"`
	ProvisioningReplicas  int    `json:"provisioning_replicas"`
	DrainingReplicas      int    `json:"draining_replicas"`
	UnhealthyTargets      int    `json:"unhealthy_targets"`
	BlockingOperationID   string `json:"blocking_operation_id"`
	BlockingOperationKind string `json:"blocking_operation_kind"`
}

type releaseGuardView struct {
	ID                  string          `json:"id"`
	ActiveRevisionID    string          `json:"active_revision_id"`
	CandidateRevisionID string          `json:"candidate_revision_id"`
	Decision            string          `json:"decision"`
	Reasons             json.RawMessage `json:"reasons"`
	Metrics             json.RawMessage `json:"metrics"`
	Policy              json.RawMessage `json:"policy"`
	CreatedAt           time.Time       `json:"created_at"`
}

func listDeployments(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("deployments", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Data []deploymentSummary `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMODEL\tRUNTIME\tROUTING\tSTATUS")
	for _, r := range response.Data {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Model, r.Runtime, r.RoutingStrategy, r.ObservedState)
	}
	return w.Flush()
}
func routeAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane route DEPLOYMENT --strategy STRATEGY")
	}
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	strategy := fs.String("strategy", "", "routing strategy")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *strategy == "" {
		return errors.New("--strategy is required")
	}
	if err := controlJSON(ctx, cfg, http.MethodPut, "/api/v1/deployments/"+url.PathEscape(args[0])+"/route", "", map[string]string{"strategy": *strategy}, nil); err != nil {
		return err
	}
	fmt.Printf("%s routing set to %s\n", args[0], *strategy)
	return nil
}
func statusCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane status DEPLOYMENT [--watch] [--output human|json]")
	}
	name := args[0]
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	watch := fs.Bool("watch", false, "refresh until interrupted")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	for {
		view, err := fetchDeployment(ctx, cfg, name)
		if err != nil {
			return err
		}
		if *output == "json" {
			encoded, _ := json.Marshal(view)
			fmt.Println(string(encoded))
		} else if *output == "human" {
			healthy := 0
			capacity := len(view.Replicas)
			if capacity > 0 {
				for _, replica := range view.Replicas {
					if replica.Health == "healthy" {
						healthy++
					}
				}
			} else {
				capacity = len(view.Targets)
				for _, target := range view.Targets {
					if target.Health == "healthy" {
						healthy++
					}
				}
			}
			d := view.Deployment
			lifecycle := view.LifecycleStatus
			if lifecycle.ServingState == "" { // Compatibility with older control planes.
				lifecycle.ServingState, lifecycle.ConvergenceState = d.ObservedState, "unknown"
				lifecycle.ReadyReplicas, lifecycle.DesiredReplicas = healthy, d.MinReplicas
			}
			fmt.Printf("%s  %s · %s\nModel        %s\nRuntime      %s\nServing      %s\nConvergence  %s\nReady        %d/%d\nCapacity     %d\nProvisioning %d\nDraining     %d\nCandidate    %s\nRouting      %s\nRevision     %s\nRequests/s   %.2f\nError rate   %.1f%%\n", d.Name, terminalStatus(lifecycle.ServingState), terminalStatus(lifecycle.ConvergenceState), d.Model, d.Runtime, lifecycle.ServingState, lifecycle.ConvergenceState, lifecycle.ReadyReplicas, lifecycle.DesiredReplicas, capacity, lifecycle.ProvisioningReplicas, lifecycle.DrainingReplicas, lifecycle.CandidateState, d.RoutingStrategy, d.ActiveRevisionID, view.RequestStats.RequestsPerSecond, view.RequestStats.ErrorRate*100)
			if lifecycle.BlockingOperationID != "" {
				fmt.Printf("Operation    %s (%s)\n", lifecycle.BlockingOperationID, lifecycle.BlockingOperationKind)
			}
		} else {
			return errors.New("--output must be human or json")
		}
		if !*watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func fetchDeployment(ctx context.Context, cfg config.Config, name string) (deploymentView, error) {
	var view deploymentView
	err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(name), "", nil, &view)
	return view, err
}
func deleteAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane delete DEPLOYMENT [--keep-resources]")
	}
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	planOnly := fs.Bool("plan", false, "show deletion actions without mutating")
	yes := fs.Bool("yes", false, "confirm destructive deletion")
	wait := fs.Bool("wait", false, "wait for provider cleanup")
	waitTimeout := fs.Duration("wait-timeout", 0, "stop waiting after this duration; provider cleanup continues")
	idempotencyKey := fs.String("idempotency-key", "", "safe retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *output != "human" && *output != "json" {
		return errors.New("--output must be human or json")
	}
	if *waitTimeout < 0 || (*waitTimeout > 0 && !*wait) {
		return errors.New("--wait-timeout must be non-negative and requires --wait")
	}
	if *planOnly {
		actions := []string{"withdraw deployment from new routing", "delete every provider resource and verify inventory absence"}
		if *output == "json" {
			encoded, _ := json.MarshalIndent(map[string]any{"deployment": args[0], "actions": actions}, "", "  ")
			fmt.Println(string(encoded))
		} else {
			fmt.Printf("Deletion plan: %s\n", args[0])
			for _, action := range actions {
				fmt.Printf("- %s\n", action)
			}
		}
		return nil
	}
	if !*yes {
		return errors.New("deletion requires --yes; run with --plan first")
	}
	if *idempotencyKey == "" {
		*idempotencyKey = fmt.Sprintf("cli-delete-%s-%d", args[0], time.Now().UnixNano())
	}
	var response struct {
		Operation domain.Operation `json:"operation"`
	}
	if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/deployments/"+url.PathEscape(args[0]), *idempotencyKey, nil, &response); err != nil {
		return fmt.Errorf("%w; safe retry key: %s", err, *idempotencyKey)
	}
	if *wait {
		if *output == "human" {
			fmt.Printf("Deployment  %s\nOperation   %s\nStatus      %s\n\nYou can close this terminal safely. Resume with:\n  infercrane operation watch %s\n\n", args[0], response.Operation.ID, terminalStatus(response.Operation.Status), response.Operation.ID)
		}
		operation, err := waitForOperationWithin(ctx, *waitTimeout, cfg, response.Operation.ID, true)
		if err != nil {
			return err
		}
		response.Operation = operation
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{"deployment": args[0], "idempotency_key": *idempotencyKey, "operation": response.Operation}, "", "  ")
		fmt.Println(string(encoded))
	} else if *output == "human" {
		fmt.Printf("Deployment  %s\nOperation   %s\nStatus      %s\nRetry key   %s\n", args[0], response.Operation.ID, terminalStatus(response.Operation.Status), *idempotencyKey)
	}
	return nil
}

func controlJSON(ctx context.Context, cfg config.Config, method, path, idempotencyKey string, requestBody, responseBody any) error {
	return controlJSONWithTimeout(ctx, cfg, method, path, idempotencyKey, requestBody, responseBody, 30*time.Second)
}

func controlJSONWithTimeout(ctx context.Context, cfg config.Config, method, path, idempotencyKey string, requestBody, responseBody any, timeout time.Duration) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode control-plane request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.ControlURL, "/")+path, body)
	if err != nil {
		return fmt.Errorf("create control-plane request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("control plane %s is unreachable: %w", cfg.ControlURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		apiErr := &ControlError{StatusCode: response.StatusCode, Code: "http_error", Message: strings.TrimSpace(string(data))}
		var envelope struct {
			Error struct {
				Code        string `json:"code"`
				Category    string `json:"category"`
				Message     string `json:"message"`
				Retryable   bool   `json:"retryable"`
				Remediation string `json:"remediation"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error.Code != "" {
			apiErr.Code, apiErr.Category, apiErr.Message = envelope.Error.Code, envelope.Error.Category, envelope.Error.Message
			apiErr.Retryable, apiErr.Remediation = envelope.Error.Retryable, envelope.Error.Remediation
		}
		return apiErr
	}
	if responseBody == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(responseBody); err != nil {
		return fmt.Errorf("decode control-plane response: %w", err)
	}
	return nil
}

type ControlError struct {
	StatusCode  int
	Code        string
	Category    string
	Message     string
	Retryable   bool
	Remediation string
}

func (e *ControlError) Error() string {
	detail := fmt.Sprintf("control plane %s", e.Code)
	if e.Category != "" {
		detail += " [" + e.Category + "]"
	}
	detail += ": " + e.Message
	if e.Remediation != "" {
		detail += "; next: " + e.Remediation
	}
	if e.Retryable {
		detail += " (retryable)"
	}
	return detail
}

func waitForOperation(ctx context.Context, cfg config.Config, id string, printProgress bool) (domain.Operation, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastRendered *domain.Operation
	var lastPrinted time.Time
	for {
		var operation domain.Operation
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/operations/"+url.PathEscape(id), "", nil, &operation); err != nil {
			if ctx.Err() != nil {
				return operation, watcherStoppedError(ctx.Err(), id)
			}
			return domain.Operation{}, err
		}
		now := time.Now()
		if printProgress && shouldRenderOperationProgress(lastRendered, operation, lastPrinted, now) {
			fmt.Fprintln(os.Stderr, renderOperationProgress(operation, now))
			copy := operation
			lastRendered = &copy
			lastPrinted = now
		}
		switch operation.Status {
		case "succeeded":
			return operation, nil
		case "failed":
			code := operation.ErrorCode
			if code == "" {
				code = "operation_failed"
			}
			return operation, fmt.Errorf("operation %s failed [%s] after %d/%d attempts: %s; inspect with: infercrane operation %s", id, code, operation.Attempt, operation.MaxAttempts, operation.Message, id)
		case "cancelled":
			return operation, fmt.Errorf("operation %s cancelled: %s", id, operation.Message)
		}
		select {
		case <-ctx.Done():
			return operation, watcherStoppedError(ctx.Err(), id)
		case <-ticker.C:
		}
	}
}

func watcherStoppedError(cause error, id string) error {
	return fmt.Errorf("%w: watcher stopped; operation %s continues safely in the control plane (resume: infercrane operation watch %s; cancel: infercrane operation cancel %s)", cause, id, id, id)
}

func waitForOperationWithin(parent context.Context, timeout time.Duration, cfg config.Config, id string, printProgress bool) (domain.Operation, error) {
	if timeout <= 0 {
		return waitForOperation(parent, cfg, id, printProgress)
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return waitForOperation(ctx, cfg, id, printProgress)
}

func operationCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane operation ID | operation watch ID | operation cancel ID")
	}
	if args[0] == "cancel" {
		if len(args) != 2 {
			return errors.New("usage: infercrane operation cancel ID")
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/operations/"+url.PathEscape(args[1])+"/cancel", "", nil, nil); err != nil {
			return err
		}
		fmt.Printf("cancellation requested for operation %s\n", args[1])
		return nil
	}
	watch := false
	operationID := args[0]
	flagArgs := args[1:]
	if args[0] == "watch" {
		if len(args) < 2 {
			return errors.New("usage: infercrane operation watch ID [--output human|json] [--wait-timeout DURATION]")
		}
		watch, operationID, flagArgs = true, args[1], args[2:]
	}
	fs := flag.NewFlagSet("operation", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	waitTimeout := fs.Duration("wait-timeout", 0, "stop watching locally after this duration without cancelling the operation")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *waitTimeout < 0 || (*waitTimeout > 0 && !watch) {
		return errors.New("--wait-timeout must be non-negative and requires operation watch")
	}
	var op domain.Operation
	if watch {
		var err error
		op, err = waitForOperationWithin(ctx, *waitTimeout, cfg, operationID, true)
		if err != nil {
			return err
		}
	} else if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/operations/"+url.PathEscape(operationID), "", nil, &op); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(op, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	elapsed := "unknown"
	if !op.CreatedAt.IsZero() {
		end := time.Now()
		if op.CompletedAt != nil {
			end = *op.CompletedAt
		}
		elapsed = end.Sub(op.CreatedAt).Round(time.Second).String()
	}
	updated := "unknown"
	if !op.UpdatedAt.IsZero() {
		updated = op.UpdatedAt.Format(time.RFC3339)
	}
	next := "not scheduled"
	if op.NextAttemptAt != nil {
		next = op.NextAttemptAt.Format(time.RFC3339)
	}
	fmt.Printf("%s  %s\nPhase      %s\nKind       %s\nResource   %s/%s\nProgress   %d%%\nChecks     %d/%d\nElapsed    %s\nUpdated    %s\nNext check %s\nMessage    %s\nRetryable  %t\nCancel     %t\n", op.ID, strings.ToUpper(op.Status), operationPhase(op), op.Kind, op.ResourceType, op.ResourceName, op.Progress, op.Attempt, op.MaxAttempts, elapsed, updated, next, op.Message, op.Retryable, op.CancelRequested)
	return nil
}

func contextCommand(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		settings, err := config.ClientConfiguration()
		if err != nil {
			return err
		}
		if len(settings.Contexts) == 0 {
			return errors.New("no contexts configured; run infercrane init")
		}
		names := make([]string, 0, len(settings.Contexts))
		for name := range settings.Contexts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			marker := " "
			if name == settings.Current {
				marker = "*"
			}
			fmt.Printf("%s %-16s %s\n", marker, name, settings.Contexts[name].URL)
		}
		return nil
	}
	if args[0] == "use" {
		if len(args) != 2 {
			return errors.New("usage: infercrane context use NAME")
		}
		if err := config.SelectClientContext(args[1]); err != nil {
			return err
		}
		fmt.Printf("Current context  %s\n", args[1])
		return nil
	}
	if args[0] == "show" {
		settings, err := config.ClientConfiguration()
		if err != nil {
			return err
		}
		selected, ok := settings.Contexts[settings.Current]
		if !ok {
			return errors.New("no current context; run infercrane init")
		}
		fmt.Printf("Context        %s\nControl plane  %s\nCredential     configured\n", settings.Current, selected.URL)
		return nil
	}
	return errors.New("usage: infercrane context list | context show | context use NAME")
}

func authCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "status" {
		return errors.New("usage: infercrane auth status [--output human|json]")
	}
	fs := flag.NewFlagSet("auth status", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Principal struct {
			ID, TenantID, Name, Role string
		} `json:"principal"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/whoami", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Printf("Authenticated  yes\nPrincipal      %s\nRole           %s\nTenant         %s\nControl plane  %s\n", response.Principal.Name, response.Principal.Role, response.Principal.TenantID, cfg.ControlURL)
	return nil
}

func orphanAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("orphans", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Data []struct {
			Name               string    `json:"name"`
			Provider           string    `json:"provider"`
			ProviderResourceID string    `json:"provider_resource_id"`
			CreatedAt          time.Time `json:"created_at"`
		} `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/orphans", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tRESOURCE\tCREATED")
	for _, item := range response.Data {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Name, item.Provider, item.ProviderResourceID, item.CreatedAt.Format(time.RFC3339))
	}
	return w.Flush()
}

func integrationsCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("integrations", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Data integration.Snapshot `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/integrations", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	qualified := func(entries []integration.Qualification) string {
		states := make([]string, 0, len(entries))
		for _, entry := range entries {
			states = append(states, string(entry.State))
		}
		return strings.Join(states, ",")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Provider contract\t%s\nRuntime contract\t%s\n\n", response.Data.ProviderContract, response.Data.RuntimeContract)
	fmt.Fprintln(w, "TYPE\tADAPTER\tOWNER\tMODES/PROTOCOL\tQUALIFICATION")
	for _, provider := range response.Data.Providers {
		modes := make([]string, len(provider.Modes))
		for index, mode := range provider.Modes {
			modes[index] = string(mode)
		}
		fmt.Fprintf(w, "provider\t%s\t%s\t%s\t%s\n", provider.Adapter, provider.Cloud, strings.Join(modes, ","), qualified(provider.Qualification))
	}
	for _, runtime := range response.Data.Runtimes {
		fmt.Fprintf(w, "runtime\t%s\t%s\t%s\t%s\n", runtime.Runtime, runtime.EngineVersion, runtime.Protocol, qualified(runtime.Qualification))
	}
	return w.Flush()
}

func tenantAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 || args[0] != "create" {
		return errors.New("usage: infercrane tenant create ID [--name NAME]")
	}
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	name := fs.String("name", args[1], "tenant display name")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/tenants", "", map[string]string{"id": args[1], "name": *name}, &response); err != nil {
		return err
	}
	fmt.Printf("tenant %s created\n", args[1])
	return nil
}

func principalAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane principal create NAME --role ROLE | principal rotate ID | principal revoke ID")
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New("usage: infercrane principal create NAME --role ROLE")
		}
		fs := flag.NewFlagSet("principal create", flag.ContinueOnError)
		role := fs.String("role", "", "principal role")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *role == "" {
			return errors.New("--role is required")
		}
		var response struct {
			Principal struct {
				ID, Name, Role, TenantID string
			} `json:"principal"`
			Credential string `json:"credential"`
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/principals", "", map[string]string{"name": args[1], "role": *role}, &response); err != nil {
			return err
		}
		fmt.Printf("principal %s created with role %s\nCredential  %s\n", response.Principal.ID, response.Principal.Role, response.Credential)
		return nil
	case "rotate":
		if len(args) != 2 {
			return errors.New("usage: infercrane principal rotate ID")
		}
		var response struct {
			Credential string `json:"credential"`
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/principals/"+url.PathEscape(args[1])+"/rotate", "", nil, &response); err != nil {
			return err
		}
		fmt.Printf("principal %s credential rotated\nCredential  %s\n", args[1], response.Credential)
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: infercrane principal revoke ID")
		}
		if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/principals/"+url.PathEscape(args[1]), "", nil, nil); err != nil {
			return err
		}
		fmt.Printf("principal %s revoked\n", args[1])
		return nil
	default:
		return errors.New("usage: infercrane principal create NAME --role ROLE | principal rotate ID | principal revoke ID")
	}
}

func inspectCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane inspect DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	view, err := fetchDeployment(ctx, cfg, args[0])
	if err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(view, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	fmt.Printf("%s\nModel      %s\nRuntime    %s\nActive     %s\nCandidate  %s\n", view.Deployment.Name, view.Deployment.Model, view.Deployment.Runtime, view.Deployment.ActiveRevisionID, emptyAs(view.Deployment.CandidateRevisionID, "none"))
	for _, modelArtifact := range view.ModelArtifacts {
		if modelArtifact.RevisionID == view.Deployment.ActiveRevisionID {
			fmt.Printf("Artifact   %s\nCache      %s\n", modelArtifact.ModelIdentity, modelArtifact.CacheState)
		}
	}
	for _, replica := range view.Replicas {
		fmt.Printf("\nReplica    %s\nProvider   %s\nRequest    %s\nResource   %s\nEndpoint   %s\nState      %s/%s\n", replica.ID, replica.Provider, emptyAs(replica.ProviderRequestID, "none"), emptyAs(replica.ProviderResourceID, "none"), emptyAs(replica.Endpoint, "pending"), replica.LifecycleState, replica.Health)
		if len(replica.ProviderDetails) > 0 && string(replica.ProviderDetails) != "{}" {
			fmt.Printf("Details    %s\n", replica.ProviderDetails)
		}
	}
	return nil
}

type cliEvent struct {
	Type      string          `json:"type"`
	Summary   string          `json:"summary"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

func deploymentEvents(ctx context.Context, cfg config.Config, name string) ([]cliEvent, error) {
	var response struct {
		Data []cliEvent `json:"data"`
	}
	err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(name)+"/events", "", nil, &response)
	return response.Data, err
}

func eventsCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane events DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	events, err := deploymentEvents(ctx, cfg, args[0])
	if err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{"data": events}, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	for _, event := range events {
		fmt.Printf("%s  %-24s %s\n", event.CreatedAt.Format(time.RFC3339), event.Type, event.Summary)
	}
	return nil
}

func logsCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane logs DEPLOYMENT [--follow] [--since DURATION] [--type TYPE] [--output human|json]")
	}
	name := args[0]
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "continue streaming new events")
	since := fs.Duration("since", 0, "show events newer than this duration")
	eventType := fs.String("type", "", "filter by event type or type prefix")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for {
		events, err := deploymentEvents(ctx, cfg, name)
		if err != nil {
			return err
		}
		cutoff := time.Time{}
		if *since > 0 {
			cutoff = time.Now().Add(-*since)
		}
		for _, event := range events {
			if !cutoff.IsZero() && event.CreatedAt.Before(cutoff) {
				continue
			}
			if *eventType != "" && event.Type != *eventType && !strings.HasPrefix(event.Type, *eventType+".") {
				continue
			}
			identity := event.CreatedAt.Format(time.RFC3339Nano) + "\x00" + event.Type + "\x00" + event.Summary
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
			if *output == "json" {
				encoded, _ := json.Marshal(event)
				fmt.Println(string(encoded))
			} else {
				fmt.Printf("%s  %-24s %s\n", event.CreatedAt.Format("15:04:05"), event.Type, event.Summary)
			}
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func requestCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane request DEPLOYMENT [--message TEXT] [--stream] [--output human|json]")
	}
	name := args[0]
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	message := fs.String("message", "Say hello in one sentence.", "user message")
	stream := fs.Bool("stream", false, "stream response text as it arrives")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if strings.TrimSpace(*message) == "" {
		return errors.New("--message cannot be empty")
	}
	if *stream && *output == "json" {
		return errors.New("--stream cannot be combined with --output json")
	}
	payload := map[string]any{"model": name, "messages": []map[string]string{{"role": "user", "content": *message}}, "stream": *stream}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode inference request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.ControlURL, "/")+"/v1/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create inference request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return fmt.Errorf("inference endpoint %s is unreachable: %w", cfg.ControlURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("inference request returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if *stream {
		return printStream(response.Body)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read inference response: %w", err)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage,omitempty"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode inference response: %w", err)
	}
	if *output == "json" {
		var complete any
		if err = json.Unmarshal(data, &complete); err != nil {
			return fmt.Errorf("decode inference JSON: %w", err)
		}
		out, _ := json.MarshalIndent(complete, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	if len(result.Choices) == 0 {
		return errors.New("inference response contained no choices")
	}
	fmt.Println(result.Choices[0].Message.Content)
	return nil
}

func printStream(body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			fmt.Println()
			return nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			fmt.Print(choice.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read inference stream: %w", err)
	}
	return errors.New("inference stream ended before [DONE]")
}

func explainCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane explain DEPLOYMENT [--output human|json]")
	}
	if args[0] == "scaling" {
		return explainScalingCommand(ctx, cfg, args[1:])
	}
	if args[0] == "cold-start" {
		if len(args) < 2 {
			return errors.New("usage: infercrane explain cold-start DEPLOYMENT [--output human|json]")
		}
		fs := flag.NewFlagSet("explain cold-start", flag.ContinueOnError)
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		view, err := fetchDeployment(ctx, cfg, args[1])
		if err != nil {
			return err
		}
		stats := view.ColdStartStats
		if *output == "json" {
			encoded, _ := json.MarshalIndent(map[string]any{"deployment": args[1], "cold_start": stats}, "", "  ")
			fmt.Println(string(encoded))
			return nil
		}
		if *output != "human" {
			return errors.New("--output must be human or json")
		}
		fmt.Printf("%s cold-start evidence\nClassified requests  %d\nCold starts          %d\nWarm requests        %d\n", args[1], stats.ClassifiedRequests, stats.ColdStarts, stats.WarmRequests)
		if stats.ColdTTFTP50MS != nil {
			fmt.Printf("Cold TTFT p50        %.1fms\n", *stats.ColdTTFTP50MS)
		}
		if stats.ColdTTFTP95MS != nil {
			fmt.Printf("Cold TTFT p95        %.1fms\n", *stats.ColdTTFTP95MS)
		} else if stats.ColdStarts > 0 {
			fmt.Println("Cold TTFT p95        unavailable (requires at least 20 classified cold starts)")
		}
		if stats.WarmTTFTP50MS != nil {
			fmt.Printf("Warm TTFT p50        %.1fms\n", *stats.WarmTTFTP50MS)
		}
		if stats.WarmTTFTP95MS != nil {
			fmt.Printf("Warm TTFT p95        %.1fms\n", *stats.WarmTTFTP95MS)
		} else if stats.WarmRequests > 0 {
			fmt.Println("Warm TTFT p95        unavailable (requires at least 20 classified warm requests)")
		}
		if stats.TimeToReadyP50MS != nil {
			fmt.Printf("Time-to-ready p50    %.1fms\n", *stats.TimeToReadyP50MS)
		} else {
			fmt.Println("Time-to-ready        unavailable (provider does not expose a trustworthy readiness boundary)")
		}
		if stats.BottleneckCode != "" {
			fmt.Printf("Bottleneck           %s\n", stats.BottleneckCode)
		}
		if len(stats.UnavailableBoundaries) > 0 {
			fmt.Printf("Unavailable          %s\n", strings.Join(stats.UnavailableBoundaries, ", "))
		}
		fmt.Printf("Evidence             %s\n", stats.Evidence)
		return nil
	}
	if args[0] == "rollout" {
		return explainRolloutCommand(ctx, cfg, args[1:])
	}
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	view, err := fetchDeployment(ctx, cfg, args[0])
	if err != nil {
		return err
	}
	reasons := []string{}
	if view.ActiveOperation != nil {
		reason := fmt.Sprintf("operation %s (%s) is %s at %d%%", view.ActiveOperation.ID, view.ActiveOperation.Kind, view.ActiveOperation.Status, view.ActiveOperation.Progress)
		if view.ActiveOperation.Message != "" {
			reason += ": " + view.ActiveOperation.Message
		}
		if view.ActiveOperation.ErrorCode != "" {
			reason += " [" + view.ActiveOperation.ErrorCode + "]"
		}
		reasons = append(reasons, reason)
	}
	for _, replica := range view.Replicas {
		if replica.Health != "healthy" {
			reasons = append(reasons, fmt.Sprintf("replica %s is %s (%s)", replica.ID, replica.LifecycleState, replica.Health))
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "all persisted replicas are healthy")
	}
	result := map[string]any{"deployment": view.Deployment.Name, "state": view.Deployment.ObservedState, "reasons": reasons, "active_revision_id": view.Deployment.ActiveRevisionID, "candidate_revision_id": view.Deployment.CandidateRevisionID, "blocking_operation": view.ActiveOperation}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	fmt.Printf("%s is %s\n", view.Deployment.Name, view.Deployment.ObservedState)
	for _, reason := range reasons {
		fmt.Printf("- %s\n", reason)
	}
	return nil
}

func explainRolloutCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane explain rollout DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("explain rollout", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	view, err := fetchDeployment(ctx, cfg, args[0])
	if err != nil {
		return err
	}
	result := map[string]any{"deployment": view.Deployment.Name, "active_revision_id": view.Deployment.ActiveRevisionID, "candidate_revision_id": view.Deployment.CandidateRevisionID, "explanation_code": "no_candidate", "reasons": []any{}}
	if view.Deployment.CandidateRevisionID != "" {
		result["explanation_code"] = "candidate_pending_evaluation"
		for _, revision := range view.Revisions {
			if revision.ID == view.Deployment.CandidateRevisionID && revision.Reason != "" {
				result["candidate_status"] = revision.Status
				result["reasons"] = []any{map[string]any{"code": "revision_reason", "message": revision.Reason}}
			}
		}
	}
	if len(view.ReleaseGuardEvaluations) > 0 {
		latest := view.ReleaseGuardEvaluations[0]
		result["evaluation_id"], result["decision"], result["evaluated_at"] = latest.ID, latest.Decision, latest.CreatedAt
		result["active_revision_id"], result["candidate_revision_id"] = latest.ActiveRevisionID, latest.CandidateRevisionID
		var reasons any = []any{}
		if len(latest.Reasons) > 0 {
			_ = json.Unmarshal(latest.Reasons, &reasons)
		}
		result["reasons"] = reasons
		var metrics any = map[string]any{}
		if len(latest.Metrics) > 0 {
			_ = json.Unmarshal(latest.Metrics, &metrics)
		}
		result["metrics"] = metrics
		var policy any = map[string]any{}
		if len(latest.Policy) > 0 {
			_ = json.Unmarshal(latest.Policy, &policy)
		}
		result["policy"] = policy
		result["explanation_code"] = "release_guard_" + strings.ToLower(latest.Decision)
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	fmt.Printf("%s rollout\nActive       %s\nCandidate    %s\nExplanation  %s\n", view.Deployment.Name, emptyAs(result["active_revision_id"].(string), "none"), emptyAs(result["candidate_revision_id"].(string), "none"), result["explanation_code"])
	if decision, ok := result["decision"].(string); ok {
		fmt.Printf("Guard        %s\n", decision)
	}
	encoded, _ := json.Marshal(result["reasons"])
	fmt.Printf("Reasons      %s\n", encoded)
	if evaluated, ok := result["evaluated_at"].(time.Time); ok {
		fmt.Printf("Evaluated    %s\n", evaluated.Format(time.RFC3339))
	}
	return nil
}

func explainScalingCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane explain scaling DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("explain scaling", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Data []struct {
			ID          string          `json:"id"`
			Action      string          `json:"action"`
			OldReplicas int             `json:"old_replicas"`
			NewReplicas int             `json:"new_replicas"`
			Reason      string          `json:"reason"`
			Signals     json.RawMessage `json:"signals"`
			CreatedAt   time.Time       `json:"created_at"`
		} `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(args[0])+"/scaling-decisions?limit=20", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if *output != "human" {
		return errors.New("--output must be human or json")
	}
	if len(response.Data) == 0 {
		fmt.Printf("%s has no persisted scaling evaluation yet\n", args[0])
		return nil
	}
	latest := response.Data[0]
	fmt.Printf("%s scaling: %s\nReplicas    %d -> %d\nReason      %s\nSignals     %s\nEvaluated   %s\n", args[0], latest.Action, latest.OldReplicas, latest.NewReplicas, latest.Reason, latest.Signals, latest.CreatedAt.Format(time.RFC3339))
	return nil
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func rolloutCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane rollout inspect|policy|create|provision|evaluate|promote|reject|rollback DEPLOYMENT [REVISION] [flags]")
	}
	action, name := args[0], args[1]
	if action == "inspect" || action == "policy" {
		fs := flag.NewFlagSet("rollout inspect", flag.ContinueOnError)
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var view deploymentView
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(name), "", nil, &view); err != nil {
			return err
		}
		if *output == "json" {
			if action == "policy" {
				encoded, _ := json.MarshalIndent(view.ReleaseGuardPolicy, "", "  ")
				fmt.Println(string(encoded))
				return nil
			}
			encoded, _ := json.MarshalIndent(map[string]any{"deployment": view.Deployment.Name, "active_revision_id": view.Deployment.ActiveRevisionID, "candidate_revision_id": view.Deployment.CandidateRevisionID, "revisions": view.Revisions, "request_stats": view.RequestStats, "release_guard_policy": view.ReleaseGuardPolicy, "release_guard_evaluations": view.ReleaseGuardEvaluations}, "", "  ")
			fmt.Println(string(encoded))
			return nil
		}
		if *output != "human" {
			return errors.New("--output must be human or json")
		}
		if action == "policy" {
			policy := view.ReleaseGuardPolicy
			fmt.Printf("%s Release Guard policy\nEnabled                %t\nMinimum requests       %d\nMax TTFT regression    %.1f%%\nMax latency regression %.1f%%\nMax error increase     %.2f%%\nMax throughput drop    %.1f%%\n", name, policy.Enabled, policy.MinimumRequests, policy.MaxTTFTRegressionPercent, policy.MaxLatencyRegressionPercent, policy.MaxErrorRateIncrease*100, policy.MaxOutputThroughputDropPercent)
			return nil
		}
		fmt.Printf("%s rollout\nACTIVE       %s\nCANDIDATE    %s\n\n", name, emptyAs(view.Deployment.ActiveRevisionID, "none"), emptyAs(view.Deployment.CandidateRevisionID, "none"))
		for _, revision := range view.Revisions {
			fmt.Printf("rev-%d  %-10s %s", revision.Number, strings.ToUpper(revision.Status), revision.ID)
			if revision.Reason != "" {
				fmt.Printf("  %s", revision.Reason)
			}
			fmt.Println()
		}
		if len(view.ReleaseGuardEvaluations) > 0 {
			guard := view.ReleaseGuardEvaluations[0]
			var metrics struct {
				Active    domain.RevisionMetrics `json:"active"`
				Candidate domain.RevisionMetrics `json:"candidate"`
			}
			if err := json.Unmarshal(guard.Metrics, &metrics); err != nil {
				return fmt.Errorf("decode persisted Release Guard metrics: %w", err)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "\nMetric\tActive\tCandidate")
			fmt.Fprintf(w, "Evidence\t%s\t%s\n", formatGuardEvidence(metrics.Active), formatGuardEvidence(metrics.Candidate))
			fmt.Fprintf(w, "Ready replicas\t%d\t%d\n", metrics.Active.ReadyReplicas, metrics.Candidate.ReadyReplicas)
			fmt.Fprintf(w, "Requests\t%d\t%d\n", metrics.Active.Requests, metrics.Candidate.Requests)
			fmt.Fprintf(w, "TTFT p95\t%s\t%s\n", formatGuardMetric(metrics.Active.P95TTFTMS, "ms"), formatGuardMetric(metrics.Candidate.P95TTFTMS, "ms"))
			fmt.Fprintf(w, "Latency p95\t%s\t%s\n", formatGuardMetric(metrics.Active.P95LatencyMS, "ms"), formatGuardMetric(metrics.Candidate.P95LatencyMS, "ms"))
			fmt.Fprintf(w, "Error rate\t%.2f%%\t%.2f%%\n", metrics.Active.ErrorRate*100, metrics.Candidate.ErrorRate*100)
			fmt.Fprintf(w, "Output tok/s\t%s\t%s\n", formatGuardMetric(metrics.Active.OutputTokensPerSecond, ""), formatGuardMetric(metrics.Candidate.OutputTokensPerSecond, ""))
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Printf("\nGuard: %s\n", guard.Decision)
			var reasons []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(guard.Reasons, &reasons); err != nil {
				return fmt.Errorf("decode persisted Release Guard reasons: %w", err)
			}
			for _, reason := range reasons {
				fmt.Printf("- %s: %s\n", reason.Code, reason.Message)
			}
			fmt.Printf("Evaluation: %s at %s\n", guard.ID, guard.CreatedAt.Format(time.RFC3339))
		}
		return nil
	}

	fs := flag.NewFlagSet("rollout "+action, flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	wait := fs.Bool("wait", false, "wait for the operation to finish")
	waitTimeout := fs.Duration("wait-timeout", 0, "stop waiting after this duration; the durable operation continues")
	key := fs.String("idempotency-key", "", "safe retry key")
	reason := fs.String("reason", "", "persisted reason for the transition")
	model := fs.String("model", "", "candidate model")
	runtimeName := fs.String("runtime", "vllm", "candidate runtime")
	routing := fs.String("routing", "round-robin", "candidate routing strategy")
	minReplicas := fs.Int("min", 1, "candidate minimum replicas")
	maxReplicas := fs.Int("max", 1, "candidate maximum replicas")
	cloud := fs.String("cloud", "", "candidate provider cloud")
	gpu := fs.String("gpu", "", "candidate GPU")
	region := fs.String("region", "", "candidate region")
	modelRevision := fs.String("model-revision", "", "candidate model revision")
	runtimeVersion := fs.String("runtime-version", "", "candidate runtime version")
	runtimeArgs := fs.String("runtime-args", "", "comma-separated candidate runtime arguments")
	rest := args[2:]
	revisionID := ""
	if action != "create" && action != "evaluate" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("revision ID is required")
		}
		revisionID, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *output != "human" && *output != "json" {
		return errors.New("--output must be human or json")
	}
	if *waitTimeout < 0 || (*waitTimeout > 0 && !*wait) {
		return errors.New("--wait-timeout must be non-negative and requires --wait")
	}
	if *key == "" {
		*key = fmt.Sprintf("cli-rollout-%s-%s-%d", action, name, time.Now().UnixNano())
	}
	path := ""
	var request any = struct{}{}
	switch action {
	case "create":
		if *model == "" {
			return errors.New("--model is required")
		}
		if *minReplicas < 1 || *maxReplicas < *minReplicas {
			return errors.New("replica bounds must satisfy 1 <= min <= max")
		}
		if (*cloud == "") != (*gpu == "") {
			return errors.New("--cloud and --gpu must be provided together")
		}
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollouts"
		computeMode := "existing"
		if *cloud != "" {
			computeMode = "elastic"
		}
		spec := domain.DeploymentRevisionSpec{Model: *model, ModelRevision: *modelRevision, Runtime: *runtimeName, RuntimeVersion: *runtimeVersion, RoutingStrategy: *routing, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas, AutoscalingEnabled: *maxReplicas > *minReplicas, ComputeMode: computeMode, Cloud: *cloud, GPU: *gpu, Region: *region}
		if *runtimeArgs != "" {
			spec.RuntimeArgs = splitTargets(*runtimeArgs)
		}
		request = map[string]any{"spec": spec}
	case "provision":
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollouts/" + url.PathEscape(revisionID) + "/provision"
	case "evaluate":
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollouts/guard/evaluate"
	case "promote":
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollouts/" + url.PathEscape(revisionID) + "/promote"
	case "reject":
		if *reason == "" {
			return errors.New("--reason is required")
		}
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollouts/" + url.PathEscape(revisionID) + "/reject"
		request = map[string]string{"reason": *reason}
	case "rollback":
		if *reason == "" {
			return errors.New("--reason is required")
		}
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollback"
		request = map[string]string{"revision_id": revisionID, "reason": *reason}
	default:
		return fmt.Errorf("unknown rollout action %q", action)
	}
	var response struct {
		Operation domain.Operation `json:"operation"`
	}
	if err := controlJSON(ctx, cfg, http.MethodPost, path, *key, request, &response); err != nil {
		return err
	}
	if *wait {
		operation, err := waitForOperationWithin(ctx, *waitTimeout, cfg, response.Operation.ID, *output == "human")
		if err != nil {
			return err
		}
		response.Operation = operation
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
	} else if *output == "human" {
		fmt.Printf("Deployment  %s\nAction      %s\nOperation   %s\nStatus      %s\n", name, action, response.Operation.ID, response.Operation.Status)
	}
	return nil
}

func formatGuardMetric(value *float64, suffix string) string {
	if value == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.1f%s", *value, suffix)
}

func formatGuardEvidence(metrics domain.RevisionMetrics) string {
	if metrics.EvidenceSource == "" {
		return "unavailable"
	}
	if metrics.EvidenceID == "" {
		return metrics.EvidenceSource
	}
	return metrics.EvidenceSource + ":" + metrics.EvidenceID
}

func serve(parent context.Context, cfg config.Config, s *store.Store) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	integrationRegistry, err := integration.V02Catalog()
	if err != nil {
		return fmt.Errorf("configure integration contracts: %w", err)
	}
	directory := routes.New()
	backend := router.NewVLLM(cfg.RouterBinary, cfg.APIKey)
	runtime := runtimeadapter.VLLM{APIKey: cfg.APIKey}
	runtimeProfile, err := integrationRegistry.Runtime(support.DefaultRuntime)
	if err != nil {
		return fmt.Errorf("configure runtime contract: %w", err)
	}
	runtimeBackends, err := integration.NewRuntimeBackends(integration.RuntimeBackend{Profile: runtimeProfile, Inspector: runtime})
	if err != nil {
		return fmt.Errorf("bind runtime adapter: %w", err)
	}
	serverless := provision.RunPodServerless{APIKey: cfg.RunPodAPIKey, BaseURL: cfg.RunPodRESTURL, TemplateID: cfg.RunPodServerlessTemplateID}
	logger := slog.Default()
	recorder := accounting.New(s, logger, 8192, 4)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = recorder.Close(closeCtx)
	}()
	rec := &reconcile.Reconciler{Store: s, Routes: directory, Router: backend, Runtimes: runtimeBackends, Interval: cfg.HealthInterval, RouterStartPort: cfg.RouterStartPort, InstanceID: cfg.InstanceID, Logger: logger, DirectTargets: map[string]reconcile.DirectTargetBackend{"runpod-serverless": {Provider: "runpod", APIKey: cfg.RunPodAPIKey, Status: serverless}}}
	go purgeRequests(ctx, s, cfg.RequestRetention, logger)
	go func() {
		_ = rec.Run(ctx)
	}()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConns: 1024, MaxIdleConnsPerHost: 256, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: cfg.UpstreamTimeout, ForceAttemptHTTP2: true}
	client := &http.Client{Transport: transport}
	credentialCache := &authn.Cache{Source: s, Interval: time.Second}
	if err := credentialCache.Refresh(ctx); err != nil {
		return fmt.Errorf("load credential snapshot: %w", err)
	}
	go func() { _ = credentialCache.Run(ctx) }()
	diagnostics := func(checkCtx context.Context, cloud, checkServerless bool) doctor.Report {
		report := doctor.Run(checkCtx, cfg, doctor.Dependencies{})
		if cloud {
			report.Add(doctor.CheckCloudCredentials(checkCtx, doctor.Dependencies{}))
			report.Add(doctor.CheckCapacity(checkCtx, "runpod", support.DefaultGPU, provision.RunPodAvailability{APIKey: cfg.RunPodAPIKey}))
			report.Capabilities = append(report.Capabilities,
				doctor.Capability{Adapter: "skypilot", Name: "durable_replica_lifecycle", State: "supported", Detail: "create, observe, adopt, and delete through a deterministic resource identity"},
				doctor.Capability{Adapter: "runpod", Name: "availability_probe", State: "supported", Detail: "advisory secure-capacity stock signal; not a reservation"},
				doctor.Capability{Adapter: "runpod", Name: "image_streaming", State: "unknown", Detail: "not exposed by the configured elastic provider API"},
				doctor.Capability{Adapter: "runpod", Name: "model_cache", State: "unknown", Detail: "cache locality is not reported for elastic replicas"},
			)
		}
		if checkServerless {
			report.Add(doctor.CheckRunPodServerless(checkCtx, cfg, doctor.Dependencies{}))
			report.Capabilities = append(report.Capabilities,
				doctor.Capability{Adapter: "runpod-serverless", Name: "scale_to_zero", State: "supported", Detail: "zero active workers with provider-native wake-up"},
				doctor.Capability{Adapter: "runpod-serverless", Name: "warm_workers", State: "supported", Detail: "minimum active workers are provider managed"},
				doctor.Capability{Adapter: "runpod-serverless", Name: "fast_resume", State: "unknown", Detail: "FlashBoot state is not exposed by the configured endpoint API"},
				doctor.Capability{Adapter: "runpod-serverless", Name: "model_cache", State: "unknown", Detail: "template cache locality is not exposed by the configured endpoint API"},
			)
		}
		return report
	}
	control := (controlapi.API{Store: s, APIKey: cfg.APIKey, Authenticator: credentialCache, BenchmarkRunner: benchmark.Runner{}, Diagnostics: diagnostics, Backends: map[string]controlapi.BackendMetadata{"skypilot": {APIKey: cfg.APIKey, APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}, "runpod-serverless": {APIKey: cfg.RunPodAPIKey, APIKeyEnv: "RUNPOD_API_KEY", Serverless: true}}, Integrations: integrationRegistry.Snapshot(), GatewayURL: cfg.ControlURL, AIPerfBinary: cfg.AIPerfBinary}).Handler()
	operationTelemetry := &operations.Telemetry{}
	handlers := workflows.DeploymentHandlers(s)
	for kind, handler := range workflows.RolloutHandlers(s) {
		handlers[kind] = handler
	}
	for kind, handler := range workflows.ReleaseGuardHandlers(s) {
		handlers[kind] = handler
	}
	elasticProfile, err := integrationRegistry.Provider("skypilot")
	if err != nil {
		return fmt.Errorf("configure elastic integration: %w", err)
	}
	replicaBackends, err := workflows.NewReplicaBackends(workflows.ReplicaBackend{Name: "skypilot", Cloud: "runpod", Runtime: "vllm", Profile: elasticProfile, Provider: provision.SkyPilot{APIKey: cfg.APIKey}, Capacity: provision.RunPodAvailability{APIKey: cfg.RunPodAPIKey}})
	if err != nil {
		return fmt.Errorf("configure replica backends: %w", err)
	}
	for kind, handler := range workflows.CloudHandlersWithBackendsAndDrain(s, replicaBackends, runtimeBackends, directory, artifact.HuggingFace{}) {
		handlers[kind] = handler
	}
	serverlessProfile, err := integrationRegistry.Provider("runpod-serverless")
	if err != nil {
		return fmt.Errorf("configure serverless integration: %w", err)
	}
	serverlessBackend := workflows.ServerlessBackend{Name: "runpod-serverless", Cloud: "runpod", Runtime: "vllm", Profile: serverlessProfile, Provider: serverless}
	for kind, handler := range workflows.ServerlessHandlers(s, serverlessBackend, artifact.HuggingFace{}) {
		handlers[kind] = handler
	}
	operationWorker := operations.Worker{Repository: s, Handlers: handlers, Owner: cfg.InstanceID, Lease: 30 * time.Second, PollInterval: time.Second, BaseBackoff: 2 * time.Second, MaxBackoff: time.Minute, Telemetry: operationTelemetry}
	go func() {
		if err := operationWorker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("operation worker stopped", "error", err)
		}
	}()
	autoscaler := autoscale.Controller{Repository: s, Signals: autoscale.VLLMSignals{Targets: s, Client: client, APIKey: cfg.APIKey}, Fleet: s}
	go runAutoscaler(ctx, autoscaler, cfg.HealthInterval, logger)
	gatewayTelemetry := &gateway.Telemetry{Extra: func(w io.Writer) {
		operationTelemetry.WritePrometheus(w)
		recorder.WritePrometheus(w)
	}}
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), Handler: (&gateway.Gateway{Routes: directory, APIKey: cfg.APIKey, Authenticator: credentialCache, Recorder: recorder, Logger: logger, Client: client, Ready: s.Ping, Control: control, Telemetry: gatewayTelemetry, CapacityObservers: map[string]gateway.CapacityObserver{"runpod": serverless.ActiveWorkers}}).Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("InferCrane gateway listening on http://%s/v1\n", server.Addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func runAutoscaler(ctx context.Context, controller autoscale.Controller, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := controller.Once(ctx); err != nil && ctx.Err() == nil {
			logger.Error("autoscaling reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func purgeRequests(ctx context.Context, s *store.Store, retention time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		for {
			deleted, err := s.PurgeRequests(ctx, time.Now().Add(-retention), 10000)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("purge request records", "error", err)
				}
				break
			}
			if deleted < 10000 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
