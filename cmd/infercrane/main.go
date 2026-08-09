package main

import (
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
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/planning"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/reconcile"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
	runtimeadapter "github.com/infercrane/infercrane/internal/runtime"
	"github.com/infercrane/infercrane/internal/spec"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/workflows"
)

const version = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(2)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "init":
		return initCommand(args[1:])
	case "doctor":
		return doctorCommand(ctx, args[1:])
	case "benchmark":
		return benchmarkCommand(ctx, args[1:])
	}
	switch args[0] {
	case "target", "deploy", "apply", "plan", "deployments", "route", "status", "events", "explain", "delete", "inspect", "operation", "orphans", "tenant", "principal", "serve":
	default:
		usage(os.Stderr)
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
	case "inspect":
		return inspectCommand(ctx, cfg, args[1:])
	case "explain":
		return explainCommand(ctx, cfg, args[1:])
	case "operation":
		return operationCommand(ctx, cfg, args[1:])
	case "target":
		return targetAPICommand(ctx, cfg, args[1:])
	case "orphans":
		return orphanAPICommand(ctx, cfg, args[1:])
	case "route":
		return routeAPICommand(ctx, cfg, args[1:])
	case "tenant":
		return tenantAPICommand(ctx, cfg, args[1:])
	case "principal":
		return principalAPICommand(ctx, cfg, args[1:])
	}
	return fmt.Errorf("%s has not yet been migrated to the control-plane API", args[0])
}
func usage(w *os.File) {
	fmt.Fprintln(w, `InferCrane — operate production LLM inference without hiding the infrastructure.

Usage:
  infercrane <command> [arguments]

Trust and discovery:
  init             Create private local control-plane configuration
  plan MODEL       Preview deployment actions without side effects
  doctor           Validate the local runtime environment
  benchmark        Run a reproducible OpenAI-compatible load check
  help             Show this help
  version          Print the version

Operations:
  target           Register or list existing inference targets
  deploy           Create a deployment from flags or YAML
  apply            Declaratively converge a deployment from flags or YAML
  deployments      List deployments
  route            Change a deployment routing strategy
  status           Inspect deployment health and traffic
  events           Show durable deployment events
  inspect          Inspect deployment details
  explain          Explain persisted operational state
  operation        Inspect or request cancellation of a lifecycle operation
  orphans          List unmanaged provisioned resources
  tenant           Create an isolated tenant
  principal        Create, rotate, or revoke scoped credentials
  delete           Plan or confirm deletion of a deployment
  serve            Run the control plane and gateway`)
}

func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	controlURL := fs.String("url", "http://127.0.0.1:8080", "control-plane URL")
	apiKey := fs.String("api-key", "", "existing API key; generated when omitted")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, generated, err := config.InitializeClient(*controlURL, *apiKey)
	if err != nil {
		return err
	}
	result := map[string]any{"config_path": path, "control_url": *controlURL, "api_key_generated": generated}
	switch *output {
	case "json":
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
	case "human":
		fmt.Printf("InferCrane configured\nControl plane  %s\nConfig         %s\n", *controlURL, path)
		if generated {
			fmt.Println("API key       generated and stored with mode 0600")
		}
	case "":
		return errors.New("--output must be human or json")
	default:
		return errors.New("--output must be human or json")
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
	cloud := fs.String("cloud", "", "SkyPilot cloud")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	minReplicas := fs.Int("min", 1, "minimum replicas")
	maxReplicas := fs.Int("max", 1, "maximum replicas")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	in := planning.Input{Name: *name, Model: model, Cloud: *cloud, GPU: *gpu, Region: *region, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas}
	if ext := filepath.Ext(model); ext == ".yaml" || ext == ".yml" {
		file, err := spec.Load(model)
		if err != nil {
			return err
		}
		if *name != "" || *targets != "" || *cloud != "" || *gpu != "" || *region != "" {
			return errors.New("deployment YAML cannot be combined with deployment flags")
		}
		in = planning.Input{Name: file.Name, Model: file.Model.ID, Cloud: file.Provider.Cloud,
			GPU: file.Resources.GPU, Region: file.Provider.Region, Runtime: file.Runtime.Engine,
			RuntimeArgs: file.Runtime.Args, Routing: file.Routing.Strategy,
			MinReplicas: file.Scaling.MinReplicas, MaxReplicas: file.Scaling.MaxReplicas}
	} else if *targets != "" {
		in.Targets = splitTargets(*targets)
	}
	if len(in.Targets) == 0 && in.Cloud == "" && in.GPU == "" {
		in.Cloud, in.GPU = "runpod", "L40S"
	}
	p, err := planning.Build(in)
	if err != nil {
		return err
	}
	current, lookupErr := fetchDeployment(ctx, cfg, p.Name)
	if lookupErr == nil {
		activeNumber := 0
		for _, revision := range current.Revisions {
			if revision.ID == current.Deployment.ActiveRevisionID {
				activeNumber = revision.Number
				break
			}
		}
		p = planning.Compare(p, planning.Current{Model: current.Deployment.Model, Runtime: current.Deployment.Runtime, Routing: current.Deployment.RoutingStrategy, MinReplicas: current.Deployment.MinReplicas, MaxReplicas: current.Deployment.MaxReplicas, ActiveRevision: current.Deployment.ActiveRevisionID, ActiveRevisionNumber: activeNumber})
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
			fmt.Printf("%d. %-10s %s\n", action.Order, action.Kind, action.Summary)
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

func doctorCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	cloud := fs.Bool("cloud", false, "also validate SkyPilot cloud credentials")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadForDiagnostics()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	report := doctor.Run(ctx, cfg, doctor.Dependencies{})
	if *cloud {
		report.Add(doctor.CheckCloudCredentials(ctx, doctor.Dependencies{}))
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

func benchmarkCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:18000", "InferCrane base URL")
	model := fs.String("model", "", "logical model alias")
	apiKey := fs.String("api-key", os.Getenv("INFERCRANE_API_KEY"), "API key (prefer environment variable)")
	requests := fs.Int("requests", 100, "request count")
	concurrency := fs.Int("concurrency", 10, "concurrent clients")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := benchmark.Run(ctx, benchmark.Config{Endpoint: strings.TrimRight(*endpoint, "/"), APIKey: *apiKey, Model: *model, Requests: *requests, Concurrency: *concurrency})
	encoded, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(encoded))
	if err != nil {
		return err
	}
	if result.Failed > 0 {
		return fmt.Errorf("benchmark had %d failed requests", result.Failed)
	}
	return nil
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
	cloud := fs.String("cloud", "", "SkyPilot cloud")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	minReplicas := fs.Int("min", 1, "minimum replicas")
	maxReplicas := fs.Int("max", 1, "maximum replicas")
	wait := fs.Bool("wait", false, "wait for the operation to finish")
	idempotencyKey := fs.String("idempotency-key", "", "safe retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	strategy := "round-robin"
	runtimeArgs := []string{}
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
	if *targets == "" && *cloud == "" && *gpu == "" {
		*cloud, *gpu = "runpod", "L40S"
	}
	if *minReplicas < 1 || *maxReplicas < *minReplicas {
		return errors.New("replica bounds must satisfy 1 <= min <= max")
	}
	if *idempotencyKey == "" {
		*idempotencyKey = fmt.Sprintf("cli-%s-%d", *name, time.Now().UnixNano())
	}
	path := "/api/v1/deployments"
	var request any
	if *targets != "" {
		if *cloud != "" || *gpu != "" {
			return errors.New("use either --targets or --cloud/--gpu")
		}
		path = "/api/v1/deployments/apply"
		request = workflows.ApplyExistingRequest{Name: *name, Model: model, Targets: splitTargets(*targets), RoutingStrategy: strategy, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas, AutoscalingEnabled: *maxReplicas > *minReplicas}
	} else if *cloud != "" && *gpu != "" {
		request = workflows.CloudRequest{Name: *name, Model: model, Cloud: *cloud, GPU: *gpu, Region: *region, RuntimeArgs: runtimeArgs, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas}
	} else {
		return errors.New("provide --targets or both --cloud and --gpu")
	}
	var response struct {
		Operation domain.Operation `json:"operation"`
	}
	if err := controlJSON(ctx, cfg, http.MethodPost, path, *idempotencyKey, request, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{"deployment": *name, "operation": response.Operation}, "", "  ")
		fmt.Println(string(encoded))
	} else if *output == "human" {
		fmt.Printf("Deployment  %s\nOperation   %s\nStatus      %s\n", *name, response.Operation.ID, response.Operation.Status)
	} else {
		return errors.New("--output must be human or json")
	}
	if *wait {
		return waitForOperation(ctx, cfg, response.Operation.ID)
	}
	_ = operationKind // deploy/apply share API semantics; retained for command UX.
	return nil
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
	Deployment     deploymentSummary   `json:"deployment"`
	Targets        []targetView        `json:"targets"`
	Replicas       []replicaView       `json:"replicas"`
	Revisions      []revisionView      `json:"revisions"`
	ModelArtifacts []artifactView      `json:"model_artifacts"`
	RequestStats   domain.RequestStats `json:"request_stats"`
}

func listDeployments(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("deployments", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
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
			fmt.Printf("%s  %s\nModel       %s\nRuntime     %s\nReplicas    %d\nHealthy     %d\nRouting     %s\nRevision    %s\nRequests/s  %.2f\nError rate  %.1f%%\n", d.Name, strings.ToUpper(d.ObservedState), d.Model, d.Runtime, capacity, healthy, d.RoutingStrategy, d.ActiveRevisionID, view.RequestStats.RequestsPerSecond, view.RequestStats.ErrorRate*100)
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
	idempotencyKey := fs.String("idempotency-key", "", "safe retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *planOnly {
		fmt.Printf("Deletion plan: %s\n", args[0])
		fmt.Println("- withdraw deployment from new routing")
		fmt.Println("- delete every provider resource and verify inventory absence")
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
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{"deployment": args[0], "operation": response.Operation}, "", "  ")
		fmt.Println(string(encoded))
	} else if *output == "human" {
		fmt.Printf("Deployment  %s\nOperation   %s\nStatus      %s\n", args[0], response.Operation.ID, response.Operation.Status)
	} else {
		return errors.New("--output must be human or json")
	}
	if *wait {
		return waitForOperation(ctx, cfg, response.Operation.ID)
	}
	return nil
}

func controlJSON(ctx context.Context, cfg config.Config, method, path, idempotencyKey string, requestBody, responseBody any) error {
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
	client := &http.Client{Timeout: 30 * time.Second}
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
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error.Code != "" {
			apiErr.Code, apiErr.Message = envelope.Error.Code, envelope.Error.Message
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
	StatusCode int
	Code       string
	Message    string
}

func (e *ControlError) Error() string {
	return fmt.Sprintf("control plane %s: %s", e.Code, e.Message)
}

func waitForOperation(ctx context.Context, cfg config.Config, id string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var operation domain.Operation
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/operations/"+url.PathEscape(id), "", nil, &operation); err != nil {
			return err
		}
		fmt.Printf("Progress    %d%%  %s\n", operation.Progress, operation.Message)
		switch operation.Status {
		case "succeeded":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("operation %s %s: %s", id, operation.Status, operation.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func operationCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane operation ID | operation cancel ID")
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
	if len(args) != 1 {
		return errors.New("usage: infercrane operation ID")
	}
	var op domain.Operation
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/operations/"+url.PathEscape(args[0]), "", nil, &op); err != nil {
		return err
	}
	fmt.Printf("%s  %s\nKind       %s\nResource   %s/%s\nProgress   %d%%\nAttempt    %d\nMessage    %s\nRetryable  %t\nCancel     %t\n", op.ID, strings.ToUpper(op.Status), op.Kind, op.ResourceType, op.ResourceName, op.Progress, op.Attempt, op.Message, op.Retryable, op.CancelRequested)
	return nil
}

func orphanAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("orphans", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
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

func eventsCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane events DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var response struct {
		Data []struct {
			Type      string          `json:"type"`
			Summary   string          `json:"summary"`
			Payload   json.RawMessage `json:"payload"`
			CreatedAt time.Time       `json:"created_at"`
		} `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(args[0])+"/events", "", nil, &response); err != nil {
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
	for _, event := range response.Data {
		fmt.Printf("%s  %-24s %s\n", event.CreatedAt.Format(time.RFC3339), event.Type, event.Summary)
	}
	return nil
}

func explainCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane explain DEPLOYMENT [--output human|json]")
	}
	if args[0] == "scaling" {
		return explainScalingCommand(ctx, cfg, args[1:])
	}
	if args[0] == "rollout" || args[0] == "cold-start" {
		return fmt.Errorf("explain %s is not available until its persisted evidence schema is enabled", args[0])
	}
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	view, err := fetchDeployment(ctx, cfg, args[0])
	if err != nil {
		return err
	}
	reasons := []string{}
	for _, replica := range view.Replicas {
		if replica.Health != "healthy" {
			reasons = append(reasons, fmt.Sprintf("replica %s is %s (%s)", replica.ID, replica.LifecycleState, replica.Health))
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "all persisted replicas are healthy")
	}
	result := map[string]any{"deployment": view.Deployment.Name, "state": view.Deployment.ObservedState, "reasons": reasons, "active_revision_id": view.Deployment.ActiveRevisionID, "candidate_revision_id": view.Deployment.CandidateRevisionID}
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

func explainScalingCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane explain scaling DEPLOYMENT [--output human|json]")
	}
	fs := flag.NewFlagSet("explain scaling", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
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
func serve(parent context.Context, cfg config.Config, s *store.Store) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	directory := routes.New()
	backend := router.NewVLLM(cfg.RouterBinary, cfg.APIKey)
	runtime := runtimeadapter.VLLM{APIKey: cfg.APIKey}
	logger := slog.Default()
	recorder := accounting.New(s, logger, 8192, 4)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = recorder.Close(closeCtx)
	}()
	rec := &reconcile.Reconciler{Store: s, Routes: directory, Router: backend, Runtime: runtime, Interval: cfg.HealthInterval, RouterStartPort: cfg.RouterStartPort, InstanceID: cfg.InstanceID, Logger: logger}
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
	control := (controlapi.API{Store: s, APIKey: cfg.APIKey, Authenticator: credentialCache}).Handler()
	operationTelemetry := &operations.Telemetry{}
	handlers := workflows.DeploymentHandlers(s)
	for kind, handler := range workflows.CloudHandlers(s, provision.SkyPilot{APIKey: cfg.APIKey}, runtime, artifact.HuggingFace{}) {
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
	gatewayTelemetry := &gateway.Telemetry{Extra: operationTelemetry.WritePrometheus}
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), Handler: (&gateway.Gateway{Routes: directory, APIKey: cfg.APIKey, Authenticator: credentialCache, Recorder: recorder, Logger: logger, Client: client, Ready: s.Ping, Control: control, Telemetry: gatewayTelemetry}).Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("InferCrane gateway listening on http://%s/v1\n", server.Addr)
	err := server.ListenAndServe()
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
