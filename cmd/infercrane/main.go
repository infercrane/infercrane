package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/accounting"
	"github.com/infercrane/infercrane/internal/authn"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gateway"
	"github.com/infercrane/infercrane/internal/metrics"
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
	if err := run(context.Background(), os.Args[1:]); err != nil {
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
	case "plan":
		return planCommand(args[1:])
	case "doctor":
		return doctorCommand(ctx, args[1:])
	case "benchmark":
		return benchmarkCommand(ctx, args[1:])
	}
	switch args[0] {
	case "target", "deploy", "apply", "deployments", "route", "status", "delete", "inspect", "operation", "orphans", "tenant", "principal", "serve":
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL, store.Options{MaxOpenConns: cfg.DatabaseMaxOpen, MaxIdleConns: cfg.DatabaseMaxIdle})
	if err != nil {
		return err
	}
	defer s.Close()
	switch args[0] {
	case "target":
		return targetCommand(ctx, s, args[1:])
	case "deploy":
		return deployCommand(ctx, cfg, s, "deploy", args[1:])
	case "apply":
		return deployCommand(ctx, cfg, s, "apply", args[1:])
	case "deployments":
		return listDeployments(ctx, s)
	case "route":
		return routeCommand(ctx, s, args[1:])
	case "status":
		return statusCommand(ctx, cfg, s, args[1:])
	case "delete":
		return deleteCommand(ctx, cfg, s, args[1:])
	case "inspect":
		return inspectCommand(ctx, s, args[1:])
	case "operation":
		return operationCommand(ctx, s, args[1:])
	case "orphans":
		return orphanCommand(ctx, s)
	case "tenant":
		return tenantCommand(ctx, s, args[1:])
	case "principal":
		return principalCommand(ctx, s, args[1:])
	case "serve":
		return serve(ctx, cfg, s)
	}
	return nil
}
func usage(w *os.File) {
	fmt.Fprintln(w, `InferCrane — operate production LLM inference without hiding the infrastructure.

Usage:
  infercrane <command> [arguments]

Trust and discovery:
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
  inspect          Inspect deployment details
  operation        Inspect or request cancellation of a lifecycle operation
  orphans          List unmanaged provisioned resources
  tenant           Create an isolated tenant
  principal        Create, rotate, or revoke scoped credentials
  delete           Plan or confirm deletion of a deployment
  serve            Run the control plane and gateway`)
}

func planCommand(args []string) error {
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
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	in := planning.Input{Name: *name, Model: model, Cloud: *cloud, GPU: *gpu, Region: *region}
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
	p, err := planning.Build(in)
	if err != nil {
		return err
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
func targetCommand(ctx context.Context, s *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("target requires add or list")
	}
	if args[0] == "list" {
		rows, err := s.Targets(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tURL\tRUNTIME\tHEALTH")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.URL, r.Runtime, r.Health)
		}
		return w.Flush()
	}
	if args[0] != "add" || len(args) < 2 {
		return errors.New("usage: infercrane target add NAME --url URL")
	}
	fs := flag.NewFlagSet("target add", flag.ContinueOnError)
	url := fs.String("url", "", "target URL")
	runtime := fs.String("runtime", "vllm", "runtime")
	upstream := fs.String("upstream-model", "", "upstream model")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *url == "" {
		return errors.New("--url is required")
	}
	row, err := s.AddTarget(ctx, domain.Target{Name: args[1], URL: *url, Provider: "existing", Runtime: *runtime, UpstreamModel: *upstream})
	if err == nil {
		fmt.Printf("target %s registered\n", row.Name)
	}
	return err
}
func deployCommand(ctx context.Context, cfg config.Config, s *store.Store, operationKind string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane deploy MODEL --name NAME --targets TARGETS")
	}
	model := args[0]
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	name := fs.String("name", "", "deployment name")
	targets := fs.String("targets", "", "comma-separated targets")
	cloud := fs.String("cloud", "", "SkyPilot cloud")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	idempotencyKey := fs.String("idempotency-key", "", "safe retry key")
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
	}
	if *name == "" {
		*name = planning.DefaultName(model)
	}
	request, _ := json.Marshal(map[string]any{"name": *name, "model": model, "targets": *targets, "cloud": *cloud, "gpu": *gpu, "region": *region})
	op, created, err := s.StartOperation(ctx, domain.Operation{Kind: operationKind, ResourceType: "deployment", ResourceName: *name, IdempotencyKey: *idempotencyKey, RequestJSON: string(request)})
	if err != nil {
		return err
	}
	if !created {
		if op.Status == "succeeded" {
			fmt.Printf("deployment %s already applied (operation %s)\n", *name, op.ID)
			return nil
		}
		return fmt.Errorf("operation %s already exists with status %s", op.ID, op.Status)
	}
	failOperation := func(code string, cause error, retryable bool) error {
		_ = s.FailOperation(context.WithoutCancel(ctx), op.ID, code, cause.Error(), retryable)
		return fmt.Errorf("operation %s: %w", op.ID, cause)
	}
	_ = s.UpdateOperation(ctx, op.ID, 10, "validated deployment request")
	var targetNames []string
	if *targets != "" {
		if *cloud != "" || *gpu != "" {
			return failOperation("invalid_request", errors.New("use either --targets or --cloud/--gpu"), false)
		}
		targetNames = splitTargets(*targets)
	} else if *cloud != "" && *gpu != "" {
		p := provision.SkyPilot{APIKey: cfg.APIKey}
		created, err := p.Deploy(ctx, provision.DeploymentSpec{Name: *name, Model: model, Cloud: *cloud, GPU: *gpu, Region: *region, RuntimeArgs: runtimeArgs})
		if err != nil {
			return failOperation("provision_failed", err, true)
		}
		_ = s.UpdateOperation(ctx, op.ID, 65, "capacity provisioned and runtime ready")
		current, checkErr := s.Operation(ctx, op.ID)
		if checkErr != nil {
			return checkErr
		}
		if current.CancelRequested {
			if destroyErr := p.Destroy(context.WithoutCancel(ctx), created.ProviderResourceID); destroyErr != nil {
				return failOperation("cancel_cleanup_failed", destroyErr, true)
			}
			if cancelErr := s.CancelOperation(context.WithoutCancel(ctx), op.ID, "cancelled after provisioning boundary"); cancelErr != nil {
				return cancelErr
			}
			return fmt.Errorf("operation %s cancelled", op.ID)
		}
		row, err := s.AddTarget(ctx, domain.Target{Name: created.Name, URL: created.URL, Provider: "skypilot", Runtime: "vllm", UpstreamModel: created.UpstreamModel})
		if err != nil {
			return failOperation("target_registration_failed", err, false)
		}
		if err = s.UpdateProvisionedTarget(ctx, row.ID, created.ProviderResourceID, created.Details); err != nil {
			return failOperation("target_metadata_failed", err, false)
		}
		targetNames = []string{row.Name}
	} else {
		return failOperation("invalid_request", errors.New("provide --targets or both --cloud and --gpu"), false)
	}
	_ = s.UpdateOperation(ctx, op.ID, 80, "persisting logical deployment")
	row, err := s.ApplyDeployment(ctx, domain.Deployment{Name: *name, Model: model, RoutingStrategy: strategy}, targetNames)
	if err == nil {
		result, _ := json.Marshal(map[string]string{"deployment": row.Name})
		if finishErr := s.FinishOperation(ctx, op.ID, string(result)); finishErr != nil {
			return finishErr
		}
		_ = s.Audit(ctx, domain.AuditEvent{Action: "deployment.deploy", ResourceType: "deployment", ResourceName: row.Name, Outcome: "succeeded", Payload: string(request)})
		fmt.Printf("deployment %s applied\n", row.Name)
		fmt.Printf("operation %s succeeded\n", op.ID)
		return nil
	}
	return failOperation("persistence_failed", err, false)
}
func listDeployments(ctx context.Context, s *store.Store) error {
	rows, err := s.Deployments(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMODEL\tRUNTIME\tROUTING\tSTATUS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Model, r.Runtime, r.RoutingStrategy, r.ObservedState)
	}
	return w.Flush()
}
func routeCommand(ctx context.Context, s *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane route DEPLOYMENT --strategy STRATEGY")
	}
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	strategy := fs.String("strategy", "", "strategy")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *strategy == "" {
		return errors.New("--strategy is required")
	}
	if err := s.SetRoute(ctx, args[0], *strategy); err != nil {
		return err
	}
	fmt.Printf("%s routing set to %s\n", args[0], *strategy)
	return nil
}
func statusCommand(ctx context.Context, cfg config.Config, s *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: infercrane status DEPLOYMENT")
	}
	resolved, err := s.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	healthy := 0
	for _, t := range resolved.Targets {
		if t.Health == "healthy" {
			healthy++
		}
	}
	d := resolved.Deployment
	fmt.Printf("%s  %s\nModel       %s\nRuntime     %s\nReplicas    %d\nHealthy     %d\nRouting     %s\n", d.Name, strings.ToUpper(d.ObservedState), d.Model, d.Runtime, len(resolved.Targets), healthy, d.RoutingStrategy)
	collector := metrics.Collector{APIKey: cfg.APIKey}
	for _, target := range resolved.Targets {
		m, e := collector.Collect(ctx, target.URL)
		if e != nil {
			fmt.Printf("%s metrics N/A\n", target.Name)
		} else {
			fmt.Printf("%s running=%s waiting=%s kv=%s\n", target.Name, floatValue(m.RequestsRunning), floatValue(m.RequestsWaiting), floatValue(m.KVCacheUsage))
		}
	}
	stats, err := s.RequestStats(ctx, d.ID, 5*time.Minute)
	if err != nil {
		return err
	}
	fmt.Printf("Requests/s  %.2f\nError rate  %.1f%%\n", stats.RequestsPerSecond, stats.ErrorRate*100)
	return nil
}
func deleteCommand(ctx context.Context, cfg config.Config, s *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane delete DEPLOYMENT [--keep-resources]")
	}
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	keep := fs.Bool("keep-resources", false, "keep cloud resources")
	planOnly := fs.Bool("plan", false, "show deletion actions without mutating")
	yes := fs.Bool("yes", false, "confirm destructive deletion")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	resolved, err := s.Resolve(ctx, args[0])
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if *planOnly {
		fmt.Printf("Deletion plan: %s\n", args[0])
		if errors.Is(err, store.ErrNotFound) {
			fmt.Println("1. No deployment exists; no action required")
			return nil
		}
		for _, target := range resolved.Targets {
			if target.ProviderResourceID != "" && !*keep {
				fmt.Printf("- destroy %s resource %s\n", target.Provider, target.ProviderResourceID)
			}
		}
		fmt.Println("- mark deployment deleted and remove it from routing")
		return nil
	}
	if !*yes {
		return errors.New("deletion requires --yes; run with --plan first")
	}
	if err == nil && !*keep {
		p := provision.SkyPilot{APIKey: cfg.APIKey}
		for _, target := range resolved.Targets {
			if target.Provider == "skypilot" && target.ProviderResourceID != "" {
				if err := p.Destroy(ctx, target.ProviderResourceID); err != nil {
					return fmt.Errorf("resource cleanup failed; retry or use --keep-resources: %w", err)
				}
			}
		}
	}
	if err := s.DeleteDeployment(ctx, args[0]); err != nil {
		return err
	}
	fmt.Printf("deployment %s deleted\n", args[0])
	return nil
}

func operationCommand(ctx context.Context, s *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane operation ID | operation cancel ID")
	}
	if args[0] == "cancel" {
		if len(args) != 2 {
			return errors.New("usage: infercrane operation cancel ID")
		}
		if err := s.RequestOperationCancel(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("cancellation requested for operation %s\n", args[1])
		return nil
	}
	if len(args) != 1 {
		return errors.New("usage: infercrane operation ID")
	}
	op, err := s.Operation(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s\nKind       %s\nResource   %s/%s\nProgress   %d%%\nAttempt    %d\nMessage    %s\nRetryable  %t\nCancel     %t\n", op.ID, strings.ToUpper(op.Status), op.Kind, op.ResourceType, op.ResourceName, op.Progress, op.Attempt, op.Message, op.Retryable, op.CancelRequested)
	return nil
}

func orphanCommand(ctx context.Context, s *store.Store) error {
	rows, err := s.OrphanedTargets(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tRESOURCE\tCREATED")
	for _, item := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Name, item.Provider, item.ProviderResourceID, item.CreatedAt.Format(time.RFC3339))
	}
	return w.Flush()
}

func tenantCommand(ctx context.Context, s *store.Store, args []string) error {
	if len(args) < 2 || args[0] != "create" {
		return errors.New("usage: infercrane tenant create ID [--name NAME]")
	}
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	name := fs.String("name", args[1], "display name")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := s.CreateTenant(ctx, args[1], *name); err != nil {
		return err
	}
	fmt.Printf("tenant %s created\n", args[1])
	return nil
}
func principalCommand(ctx context.Context, s *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane principal <create|rotate|revoke>")
	}
	switch args[0] {
	case "create":
		if len(args) < 3 {
			return errors.New("usage: infercrane principal create TENANT NAME --role ROLE")
		}
		fs := flag.NewFlagSet("principal create", flag.ContinueOnError)
		role := fs.String("role", "viewer", "viewer, operator, or admin")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		principal, token, err := s.CreatePrincipal(ctx, args[1], args[2], authz.Role(*role))
		if err != nil {
			return err
		}
		fmt.Printf("principal %s created (%s)\ncredential %s\nStore this credential now; it will not be shown again.\n", principal.ID, principal.Role, token)
		return nil
	case "rotate":
		if len(args) != 2 {
			return errors.New("usage: infercrane principal rotate ID")
		}
		token, err := s.RotatePrincipal(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("credential %s\nPrevious credential is now invalid.\n", token)
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: infercrane principal revoke ID")
		}
		if err := s.RevokePrincipal(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("principal %s revoked\n", args[1])
		return nil
	default:
		return errors.New("usage: infercrane principal <create|rotate|revoke>")
	}
}
func inspectCommand(ctx context.Context, s *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: infercrane inspect DEPLOYMENT")
	}
	r, err := s.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s\nModel      %s\nRuntime    %s\n", r.Deployment.Name, r.Deployment.Model, r.Deployment.Runtime)
	for _, t := range r.Targets {
		resource := t.ProviderResourceID
		if resource == "" {
			resource = "external"
		}
		fmt.Printf("\n%s\nProvider   %s\nResource   %s\nEndpoint   %s\n", t.Name, t.Provider, resource, t.URL)
		if t.ProviderDetails != "" {
			fmt.Println(t.ProviderDetails)
		}
	}
	return nil
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
	operationWorker := operations.Worker{Repository: s, Handlers: workflows.DeploymentHandlers(s), Owner: cfg.InstanceID, Lease: 30 * time.Second, PollInterval: time.Second, BaseBackoff: 2 * time.Second, MaxBackoff: time.Minute, Telemetry: operationTelemetry}
	go func() {
		if err := operationWorker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("operation worker stopped", "error", err)
		}
	}()
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
func floatValue(value *float64) string {
	if value == nil {
		return "N/A"
	}
	return fmt.Sprintf("%g", *value)
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
