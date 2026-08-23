package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
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
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/accounting"
	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/alert"
	"github.com/infercrane/infercrane/internal/artifact"
	"github.com/infercrane/infercrane/internal/asyncinference"
	"github.com/infercrane/infercrane/internal/authn"
	"github.com/infercrane/infercrane/internal/autoscale"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/contextpassport"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
	"github.com/infercrane/infercrane/internal/gateway"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/planning"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/reconcile"
	"github.com/infercrane/infercrane/internal/requestquota"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
	runtimeadapter "github.com/infercrane/infercrane/internal/runtime"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/secrets"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/spec"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/workflows"
)

var version = "2.0.0"

const controlPlaneProtocolMin, controlPlaneProtocolMax = 1, 2

func loadPassportSigningKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect passport signing key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("passport signing key file must not be accessible by group or others")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read passport signing key: %w", err)
	}
	key, err := passport.DecodePrivateKey(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, err
	}
	return key, nil
}

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
	var watcherErr *WatcherStoppedError
	if errors.As(err, &watcherErr) {
		code, message := "operation_watch_interrupted", "Operation watch stopped; the durable operation continues safely in the control plane."
		if errors.Is(watcherErr.Cause, context.DeadlineExceeded) {
			code, message = "operation_watch_timeout", "Wait timeout reached; the durable operation continues safely in the control plane."
		}
		detail = map[string]any{
			"code": code, "category": "operation", "message": message, "retryable": true,
			"remediation": "Resume with: infercrane operation watch " + watcherErr.OperationID + "; cancel only if intended: infercrane operation cancel " + watcherErr.OperationID,
		}
	}
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
	case "workload":
		return workloadCommand(ctx, args[1:])
	case "evaluation":
		return evaluationCommand(ctx, args[1:])
	}
	if args[0] == "training" && len(args) > 1 && (args[1] == "keygen" || args[1] == "sign" || args[1] == "verify") {
		return trainingCommand(ctx, config.Config{}, args[1:])
	}
	if args[0] == "passport" && len(args) > 1 && (args[1] == "keygen" || args[1] == "verify") {
		return passportCommand(ctx, config.Config{}, args[1:])
	}
	if !isPublicCommand(args[0]) {
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
	case "connect":
		return connectCommand(ctx, cfg, args[1:])
	case "adopt":
		return adoptCommand(ctx, cfg, args[1:])
	case "alert":
		return alertCommand(ctx, cfg, args[1:])
	case "admission":
		return admissionCommand(ctx, cfg, args[1:])
	case "async":
		return asyncCommand(ctx, cfg, args[1:])
	case "ui":
		return uiCommand(ctx, cfg, args[1:])
	case "mcp":
		return mcpCommand(ctx, cfg, args[1:])
	case "observe":
		return observeCommand(ctx, cfg, args[1:])
	case "telemetry":
		return telemetryCommand(ctx, cfg, args[1:])
	case "inbox":
		return inboxCommand(ctx, cfg, args[1:])
	case "deploy":
		return deployAPICommand(ctx, cfg, "deploy", args[1:])
	case "apply":
		return deployAPICommand(ctx, cfg, "apply", args[1:])
	case "delete":
		return deleteAPICommand(ctx, cfg, args[1:])
	case "deployments":
		return listDeployments(ctx, cfg, args[1:])
	case "endpoints":
		return endpointCommand(ctx, cfg, append([]string{"list"}, args[1:]...))
	case "endpoint":
		return endpointCommand(ctx, cfg, args[1:])
	case "environment":
		return environmentCommand(ctx, cfg, args[1:])
	case "logical-model":
		return logicalModelCommand(ctx, cfg, args[1:])
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
	case "provider":
		return providerCommand(ctx, cfg, args[1:])
	case "orphans":
		return orphanAPICommand(ctx, cfg, args[1:])
	case "integrations":
		return integrationsCommand(ctx, cfg, args[1:])
	case "system":
		return systemCommand(ctx, cfg, args[1:])
	case "route":
		return routeAPICommand(ctx, cfg, args[1:])
	case "tenant":
		return tenantAPICommand(ctx, cfg, args[1:])
	case "principal":
		return principalAPICommand(ctx, cfg, args[1:])
	case "secret":
		return secretAPICommand(ctx, cfg, args[1:])
	case "external":
		return externalAPICommand(ctx, cfg, args[1:])
	case "benchmark":
		return benchmarkCommand(ctx, cfg, args[1:])
	case "replay":
		return replayCommand(ctx, cfg, args[1:])
	case "capacity":
		return capacityCommand(ctx, cfg, args[1:])
	case "artifact":
		return artifactCommand(ctx, cfg, args[1:])
	case "sandbox":
		return sandboxCommand(ctx, cfg, args[1:])
	case "training":
		return trainingCommand(ctx, cfg, args[1:])
	case "finops":
		return finOpsCommand(ctx, cfg, args[1:])
	case "autopilot":
		return autopilotCommand(ctx, cfg, args[1:])
	case "session":
		return sessionCommand(ctx, cfg, args[1:])
	case "burst":
		return burstCommand(ctx, cfg, args[1:])
	case "recipe":
		return recipeCommand(ctx, cfg, args[1:])
	case "recipes":
		return recipesCommand(ctx, cfg, args[1:])
	case "models":
		return modelsCommand(args[1:])
	case "lab":
		return labCommand(ctx, cfg, args[1:])
	case "passport":
		return passportCommand(ctx, cfg, args[1:])
	case "recommend":
		return recommendCommand(ctx, cfg, args[1:])
	case "slo":
		return sloCommand(ctx, cfg, args[1:])
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
	backend := fs.String("backend", "", "serving backend; use dynamo for a managed Dynamo graph")
	providerAdapter := fs.String("provider-adapter", "", "provider adapter profile (advanced)")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	runtimeEngine := fs.String("runtime", "vllm", "runtime engine: vllm or sglang")
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
	in := planning.Input{Name: *name, Model: model, Runtime: *runtimeEngine, ComputeMode: *computeMode, Cloud: *cloud, ProviderAdapter: *providerAdapter, GPU: *gpu, Region: *region, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas}
	if ext := filepath.Ext(model); ext == ".yaml" || ext == ".yml" {
		file, err := spec.Load(model)
		if err != nil {
			return err
		}
		if *name != "" || *targets != "" || *cloud != "" || *backend != "" || *providerAdapter != "" || *gpu != "" || *region != "" || *runtimeEngine != "vllm" {
			return errors.New("deployment YAML cannot be combined with deployment flags")
		}
		in = planning.Input{Name: file.Name, Model: file.Model.ID, ComputeMode: file.Compute.Mode, Cloud: file.Provider.Cloud, ProviderAdapter: file.Provider.Adapter,
			GPU: file.Resources.GPU, Region: file.Provider.Region, Runtime: file.Runtime.Engine,
			RuntimeArgs: file.Runtime.Args, Routing: file.Routing.Strategy,
			MinReplicas: file.Scaling.MinReplicas, MaxReplicas: file.Scaling.MaxReplicas, Serving: file.Serving}
	} else if *targets != "" {
		if *computeMode == "serverless" {
			return errors.New("--compute serverless cannot be combined with --targets")
		}
		in.Targets = splitTargets(*targets)
	}
	if *backend != "" {
		if *backend != servingcontract.BackendDynamo {
			return errors.New("--backend currently supports dynamo; omit it for the standard provider path")
		}
		if *targets != "" || *computeMode != "elastic" || (*cloud != "" && *cloud != "kubernetes") || (*providerAdapter != "" && *providerAdapter != "kubernetes-dynamo") {
			return errors.New("--backend dynamo requires elastic Kubernetes capacity and cannot be combined with targets or another provider adapter")
		}
		if *gpu == "" {
			return errors.New("--backend dynamo requires --gpu; InferCrane will not guess cluster GPU labels")
		}
		in.Cloud, in.ProviderAdapter, in.MinReplicas, in.MaxReplicas = "kubernetes", "kubernetes-dynamo", 1, 1
		in.Serving = servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
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
		p = planning.Compare(p, planning.Current{Model: current.Deployment.Model, Runtime: current.Deployment.Runtime, Routing: current.Deployment.RoutingStrategy, ComputeMode: activeSpec.ComputeMode, Cloud: activeSpec.Cloud, ProviderAdapter: activeSpec.ProviderAdapter, GPU: activeSpec.GPU, Region: activeSpec.Region, MinReplicas: current.Deployment.MinReplicas, MaxReplicas: current.Deployment.MaxReplicas, ActiveRevision: current.Deployment.ActiveRevisionID, ActiveRevisionNumber: activeNumber, Serving: activeSpec.Serving})
	} else {
		var controlErr *ControlError
		if !errors.As(lookupErr, &controlErr) || controlErr.Code != "not_found" {
			return fmt.Errorf("read current deployment for plan: %w", lookupErr)
		}
	}
	if p.Mode == "provisioned" {
		var response struct {
			Capacity []domain.CapacitySummary `json:"capacity"`
		}
		if evidenceErr := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/capacity/intelligence?window_seconds=2592000", "", nil, &response); evidenceErr == nil {
			evidence := make([]planning.CapacityEvidence, 0, len(response.Capacity))
			for _, row := range response.Capacity {
				evidence = append(evidence, planning.CapacityEvidence{Provider: row.Provider, Runtime: row.Runtime, ComputeMode: row.ComputeMode, Region: row.Region, GPU: row.GPU, Attempts: row.Attempts, Succeeded: row.Succeeded, Pending: row.Pending, SuccessRate: row.SuccessRate, DurationP50Seconds: row.DurationP50Seconds, DurationP95Seconds: row.DurationP95Seconds})
			}
			p = planning.ApplyCapacityEvidence(p, evidence)
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
		fmt.Printf("\nReadiness: %s — %s\n", p.Readiness.EstimateStatus, p.Readiness.Reason)
		if p.Readiness.EstimateP50Seconds != nil {
			fmt.Printf("Observed p50: %.0fs", *p.Readiness.EstimateP50Seconds)
			if p.Readiness.EstimateP95Seconds != nil {
				fmt.Printf(" · p95 %.0fs", *p.Readiness.EstimateP95Seconds)
			}
			fmt.Printf(" · %d successful samples\n", p.Readiness.SuccessfulSamples)
			fmt.Printf("Boundary:      %s\n", p.Readiness.EvidenceBoundary)
		}
		fmt.Printf("Artifact cache: %s\nCapacity:       %s\n", p.Readiness.ArtifactCacheState, p.Readiness.CapacityState)
		if len(p.Readiness.Stages) > 0 {
			fmt.Printf("Startup stages: %s\n", strings.Join(p.Readiness.Stages, " -> "))
		}
		fmt.Printf("\nCost: %s — %s\n", p.Cost.Status, p.Cost.Reason)
	default:
		return errors.New("--output must be human or json")
	}
	return nil
}

func doctorCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return endpointDoctorCommand(ctx, cfg, args)
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	cloud := fs.Bool("cloud", false, "also validate SkyPilot cloud credentials")
	serverless := fs.Bool("serverless", false, "also validate RunPod Serverless credentials and template")
	aws := fs.Bool("aws", false, "also validate the configured AWS BYOC role")
	gcp := fs.Bool("gcp", false, "also validate the configured GCP BYOC identity")
	kubernetes := fs.Bool("kubernetes", false, "also validate the configured Kubernetes provider")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	query := url.Values{}
	query.Set("cloud", fmt.Sprint(*cloud))
	query.Set("serverless", fmt.Sprint(*serverless))
	query.Set("aws", fmt.Sprint(*aws))
	query.Set("gcp", fmt.Sprint(*gcp))
	query.Set("kubernetes", fmt.Sprint(*kubernetes))
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

func endpointDoctorCommand(ctx context.Context, cfg config.Config, args []string) error {
	name := args[0]
	fs := flag.NewFlagSet("doctor endpoint", flag.ContinueOnError)
	window := fs.Duration("window", time.Hour, "persisted evidence window")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *window <= 0 || *window > 30*24*time.Hour {
		return errors.New("usage: infercrane doctor ENDPOINT [--window 1h] [--output human|json]")
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	path := fmt.Sprintf("/api/v1/endpoints/%s/doctor?window_seconds=%d", url.PathEscape(name), int(window.Seconds()))
	if err := controlJSON(ctx, cfg, http.MethodPost, path, "", map[string]any{}, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response.Data)
	}
	for _, finding := range response.Data {
		fmt.Printf("%-8s %s\n         %s\n         Confidence %s · evidence %s\n", strings.ToUpper(fmt.Sprint(finding["severity"])), fmt.Sprint(finding["code"]), fmt.Sprint(finding["summary"]), strings.ToUpper(fmt.Sprint(finding["confidence"])), fmt.Sprint(finding["evidence_digest"]))
	}
	fmt.Printf("\nInspect evidence\n  infercrane request inspect REQUEST_ID\n  infercrane events %s\n", name)
	return nil
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

func printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
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
	inputTokens := fs.Int("input-tokens", 128, "mean input token count")
	outputTokens := fs.Int("output-tokens", 32, "maximum output token count")
	randomSeed := fs.Int64("random-seed", 17, "deterministic AIPerf dataset seed")
	profileName := fs.String("profile", "", "versioned workload profile: "+strings.Join(performanceprofile.Names(), ", "))
	revision := fs.String("revision", "active", "revision to benchmark: active, candidate, or revision ID")
	streaming := fs.Bool("streaming", true, "measure streaming responses; set false for buffered responses")
	ttftSLO := fs.Float64("ttft-slo-ms", 0, "optional TTFT SLO used to compute goodput")
	tPotSLO := fs.Float64("tpot-slo-ms", 0, "optional time-per-output-token SLO used to compute goodput")
	latencySLO := fs.Float64("latency-slo-ms", 0, "optional end-to-end latency SLO used to compute goodput")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane benchmark DEPLOYMENT [flags]")
	}
	if *profileName != "" {
		profile, profileErr := performanceprofile.Get(*profileName)
		if profileErr != nil {
			return profileErr
		}
		explicit := map[string]bool{}
		fs.Visit(func(value *flag.Flag) { explicit[value.Name] = true })
		if !explicit["requests"] {
			*requests = profile.Requests
		}
		if !explicit["concurrency"] {
			*concurrency = profile.Concurrency
		}
		if !explicit["input-tokens"] {
			*inputTokens = profile.InputTokens
		}
		if !explicit["output-tokens"] {
			*outputTokens = profile.OutputTokens
		}
		if !explicit["streaming"] {
			*streaming = profile.Streaming
		}
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Benchmark map[string]any `json:"benchmark"`
	}
	if *ttftSLO < 0 || *tPotSLO < 0 || *latencySLO < 0 {
		return errors.New("benchmark SLO thresholds cannot be negative")
	}
	request := map[string]any{"requests": *requests, "concurrency": *concurrency, "input_tokens": *inputTokens, "output_tokens": *outputTokens, "random_seed": *randomSeed, "revision": *revision, "streaming": *streaming, "profile": *profileName, "profile_version": performanceprofile.Version, "ttft_slo_ms": *ttftSLO, "tpot_slo_ms": *tPotSLO, "latency_slo_ms": *latencySLO}
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

func replayCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane replay DEPLOYMENT [flags]")
	}
	deployment := args[0]
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	windowText := fs.String("window", "24h", "production-shape capture window")
	maxRequests := fs.Int("max-requests", 1000, "maximum observations")
	execute := fs.Bool("execute", false, "run an explicit AIPerf approximation")
	acknowledge := fs.Bool("acknowledge-cost", false, "acknowledge provider cost")
	revision := fs.String("revision", "candidate", "revision for explicit execution")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	window, err := time.ParseDuration(*windowText)
	if err != nil || window <= 0 {
		return errors.New("--window must be a positive duration")
	}
	if *execute && !*acknowledge {
		return errors.New("--execute requires --acknowledge-cost because AIPerf consumes inference capacity")
	}
	var captured struct {
		Replay struct {
			ID           string          `json:"id"`
			ShapeDigest  string          `json:"shape_digest"`
			RequestCount int             `json:"request_count"`
			Summary      json.RawMessage `json:"summary"`
			WindowStart  time.Time       `json:"window_start"`
			WindowEnd    time.Time       `json:"window_end"`
		} `json:"replay"`
	}
	body := map[string]any{"window_seconds": int(window.Seconds()), "max_requests": *maxRequests}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(deployment)+"/replays", "", body, &captured); err != nil {
		return err
	}
	var summary struct{ Requests, InputTokensMean, OutputTokensMean, PeakConcurrency int }
	summaryJSON := captured.Replay.Summary
	if len(summaryJSON) > 0 && summaryJSON[0] == '"' {
		var encoded string
		if err = json.Unmarshal(summaryJSON, &encoded); err == nil {
			summaryJSON = []byte(encoded)
		}
	}
	if err = json.Unmarshal(summaryJSON, &summary); err != nil {
		return fmt.Errorf("decode replay summary: %w", err)
	}
	if *output == "json" && !*execute {
		return printJSON(captured.Replay)
	}
	fmt.Printf("Replay trace  %s\nDeployment    %s\nEvidence      production shape (prompt/output content not stored)\nWindow        %s → %s\nRequests      %d\nConcurrency   %d peak\nInput tokens  %d mean\nOutput tokens %d mean\nDigest        %s\n", captured.Replay.ID, deployment, captured.Replay.WindowStart.Format(time.RFC3339), captured.Replay.WindowEnd.Format(time.RFC3339), summary.Requests, summary.PeakConcurrency, summary.InputTokensMean, summary.OutputTokensMean, captured.Replay.ShapeDigest)
	if !*execute {
		fmt.Println("\nNo workload was sent. Add --execute --acknowledge-cost to run an AIPerf approximation.")
		return nil
	}
	if summary.Requests == 0 {
		return errors.New("captured replay has no requests to execute")
	}
	if summary.PeakConcurrency < 1 {
		summary.PeakConcurrency = 1
	}
	if summary.InputTokensMean < 1 {
		summary.InputTokensMean = 128
	}
	if summary.OutputTokensMean < 1 {
		summary.OutputTokensMean = 32
	}
	var measured map[string]any
	var response struct {
		Benchmark map[string]any `json:"benchmark"`
	}
	benchmarkRequest := map[string]any{"requests": summary.Requests, "concurrency": summary.PeakConcurrency, "input_tokens": summary.InputTokensMean, "output_tokens": summary.OutputTokensMean, "random_seed": 17, "revision": *revision}
	if err = controlJSONWithTimeout(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(deployment)+"/benchmarks", "", benchmarkRequest, &response, 35*time.Minute); err != nil {
		return err
	}
	measured = response.Benchmark
	if *output == "json" {
		return printJSON(map[string]any{"replay": captured.Replay, "benchmark": measured, "execution": "explicit_aiperf_approximation"})
	}
	fmt.Printf("\nExecuted      explicit AIPerf approximation\nBenchmark     %s\nTTFT p95      %s ms\nOutput tok/s  %s\n", benchmarkValue(measured["id"]), benchmarkValue(measured["ttft_p95_ms"]), benchmarkValue(measured["output_token_throughput"]))
	return nil
}

func capacityCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("capacity", flag.ContinueOnError)
	windowText := fs.String("window", "720h", "observation window")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane capacity [--window DURATION] [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	window, err := time.ParseDuration(*windowText)
	if err != nil || window <= 0 {
		return errors.New("--window must be a positive duration")
	}
	var response struct {
		Capacity      []domain.CapacitySummary `json:"capacity"`
		Evidence      string                   `json:"evidence"`
		WindowSeconds int                      `json:"window_seconds"`
	}
	query := url.Values{"window_seconds": {strconv.Itoa(int(window.Seconds()))}}
	if err = controlJSON(ctx, cfg, http.MethodGet, "/api/v1/capacity/intelligence?"+query.Encode(), "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Capacity intelligence · observed tenant evidence · window %s\n\n", window)
	if len(response.Capacity) == 0 {
		fmt.Println("No capacity operations observed in this window.")
		return nil
	}
	for _, row := range response.Capacity {
		gpu := row.GPU
		if gpu == "" {
			gpu = "any"
		}
		fmt.Printf("%-14s %-10s %-10s %-12s attempts %-5d success %6.1f%% capacity failures %d\n", row.Provider, row.Runtime, row.ComputeMode, gpu, row.Attempts, row.SuccessRate*100, row.CapacityFailures)
	}
	return nil
}

func finOpsCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 0 && args[0] == "collect" {
		return finOpsCollectCommand(ctx, cfg, args[1:])
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane finops DEPLOYMENT [--window DURATION] | infercrane finops collect opencost DEPLOYMENT [flags]")
	}
	name := args[0]
	fs := flag.NewFlagSet("finops", flag.ContinueOnError)
	windowText := fs.String("window", "720h", "cost evidence window")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	window, err := time.ParseDuration(*windowText)
	if err != nil || window <= 0 {
		return errors.New("--window must be a positive duration")
	}
	var response struct {
		Report map[string]any `json:"report"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(name)+"/finops/reports", "", map[string]any{"window_seconds": int(window.Seconds())}, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response.Report)
	}
	fmt.Printf("FinOps report  %s\nDeployment     %s\nStatus         %s\nKnown cost     %s %s\nAvoidable      %s (unavailable unless directly evidenced)\nEvidence digest %s\n", benchmarkValue(response.Report["id"]), name, benchmarkValue(response.Report["status"]), benchmarkValue(response.Report["known_cost"]), benchmarkValue(response.Report["currency"]), benchmarkValue(response.Report["estimated_avoidable_cost"]), benchmarkValue(response.Report["input_digest"]))
	return nil
}

func autopilotCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane autopilot plan DEPLOYMENT | autopilot inspect PLAN_ID | autopilot approve PLAN_ID")
	}
	action, subject := args[0], args[1]
	fs := flag.NewFlagSet("autopilot", flag.ContinueOnError)
	objective := fs.String("objective", "minimize_cost", "advisory objective")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Plan     map[string]any `json:"plan"`
		Mutation string         `json:"mutation"`
		Next     string         `json:"next"`
	}
	method, path, body := http.MethodGet, "", any(nil)
	switch action {
	case "plan":
		method = http.MethodPost
		path = "/api/v1/deployments/" + url.PathEscape(subject) + "/autopilot/plans"
		body = map[string]any{"objective": *objective}
	case "inspect":
		path = "/api/v1/autopilot/plans/" + url.PathEscape(subject)
	case "approve":
		method = http.MethodPost
		path = "/api/v1/autopilot/plans/" + url.PathEscape(subject) + "/approve"
		body = map[string]any{}
	default:
		return errors.New("usage: infercrane autopilot plan DEPLOYMENT | autopilot inspect PLAN_ID | autopilot approve PLAN_ID")
	}
	if err := controlJSON(ctx, cfg, method, path, "", body, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Autopilot plan %s\nStatus         %s\nObjective      %s\nRecommendation %s\nMutation       none\n", benchmarkValue(response.Plan["id"]), benchmarkValue(response.Plan["status"]), benchmarkValue(response.Plan["objective"]), benchmarkValue(response.Plan["recommendation_id"]))
	if response.Next != "" {
		fmt.Printf("Next           %s\n", response.Next)
	}
	return nil
}

func sessionCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane session create DEPLOYMENT | session inspect ID")
	}
	action, subject := args[0], args[1]
	fs := flag.NewFlagSet("session", flag.ContinueOnError)
	ttl := fs.String("ttl", "1h", "logical session expiry")
	preferredBinding := fs.String("preferred-binding", "", "best-effort binding hint")
	preferredTarget := fs.String("preferred-target", "", "best-effort target hint")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Passport  map[string]any `json:"context_passport"`
		DurableKV bool           `json:"durable_kv"`
	}
	method, path, body := http.MethodGet, "/api/v1/context-passports/"+url.PathEscape(subject), any(nil)
	if action == "create" {
		duration, err := time.ParseDuration(*ttl)
		if err != nil || duration < time.Minute {
			return errors.New("--ttl must be at least one minute")
		}
		method, path = http.MethodPost, "/api/v1/context-passports"
		body = map[string]any{"deployment": subject, "ttl_seconds": int(duration.Seconds()), "preferred_binding_id": *preferredBinding, "preferred_target_id": *preferredTarget, "cache_hints": map[string]any{}, "metadata": map[string]any{}}
	} else if action != "inspect" {
		return errors.New("usage: infercrane session create DEPLOYMENT | session inspect ID")
	}
	if err := controlJSON(ctx, cfg, method, path, "", body, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Context Passport %s\nStatus           %s\nExpires          %s\nPreferred binding %s\nPreferred target  %s\nDurable KV       no\n\nUse: X-InferCrane-Context-Passport: %s\n", benchmarkValue(response.Passport["id"]), benchmarkValue(response.Passport["status"]), benchmarkValue(response.Passport["expires_at"]), benchmarkValue(response.Passport["preferred_binding_id"]), benchmarkValue(response.Passport["preferred_target_id"]), benchmarkValue(response.Passport["id"]))
	return nil
}

func burstCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane burst DEPLOYMENT [flags]")
	}
	name := args[0]
	fs := flag.NewFlagSet("burst", flag.ContinueOnError)
	queue := fs.Int("queue-depth", 0, "observed queue depth")
	breaches := fs.Int("breaches", 1, "consecutive breach intervals")
	cost := fs.Int64("incremental-cost-microusd-hour", 0, "sourced incremental hourly cost")
	maxCost := fs.Int64("max-incremental-cost-microusd-hour", 1, "hard incremental hourly budget")
	externalHealthy := fs.Bool("external-healthy", false, "fresh qualified overflow health evidence")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	body := map[string]any{"enabled": true, "queue_threshold": 4, "breach_intervals": 3, "recovery_intervals": 2, "cooldown_seconds": 60, "signal_max_age_seconds": 30, "max_incremental_cost_microusd_hour": *maxCost, "queue_depth": *queue, "consecutive_breaches": *breaches, "consecutive_recovery": 0, "incremental_cost_microusd_hour": *cost, "external_healthy": *externalHealthy, "observed_at": time.Now().UTC()}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(name)+"/burst-guard/evaluate", "", body, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	decision, _ := response["decision"].(map[string]any)
	fmt.Printf("Burst Guard  %s\nReason       %s\nCost         %s microUSD/hour\nMutation     policy controller only\n", benchmarkValue(decision["action"]), benchmarkValue(decision["reason"]), benchmarkValue(decision["incremental_cost_microusd_hour"]))
	return nil
}

type cliPassport struct {
	ID         string          `json:"id"`
	RevisionID string          `json:"revision_id"`
	Digest     string          `json:"digest"`
	Signature  string          `json:"signature"`
	PublicKey  string          `json:"public_key"`
	Algorithm  string          `json:"algorithm"`
	KeyID      string          `json:"key_id"`
	Payload    json.RawMessage `json:"payload"`
	Verified   bool            `json:"verified"`
	Complete   bool            `json:"complete"`
	Missing    []string        `json:"missing_evidence"`
	CreatedAt  time.Time       `json:"created_at"`
}

func recipeCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 || args[0] != "create" {
		return errors.New("usage: infercrane recipe create DEPLOYMENT --name NAME --version VERSION [--benchmark ID]")
	}
	deployment := args[1]
	fs := flag.NewFlagSet("recipe create", flag.ContinueOnError)
	name := fs.String("name", "", "stable recipe name")
	recipeVersion := fs.String("version", "", "immutable recipe version")
	benchmarkID := fs.String("benchmark", "", "specific measured benchmark ID")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *name == "" || *recipeVersion == "" {
		return errors.New("recipe name and version are required")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Recipe map[string]any `json:"recipe"`
	}
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(deployment)+"/recipes", "", map[string]any{"name": *name, "version": *recipeVersion, "benchmark_id": *benchmarkID}, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Printf("Recipe       %s@%s\nDigest       %s\nEvidence     measured · %s\nModel        %s\nRuntime      %s %s\nProvider     %s\nGPU          %s\n", benchmarkValue(response.Recipe["name"]), benchmarkValue(response.Recipe["version"]), benchmarkValue(response.Recipe["digest"]), nestedValue(response.Recipe, "payload", "benchmark_id"), nestedValue(response.Recipe, "payload", "model_identity"), nestedValue(response.Recipe, "payload", "runtime"), nestedValue(response.Recipe, "payload", "runtime_version"), nestedValue(response.Recipe, "payload", "provider"), nestedValue(response.Recipe, "payload", "gpu"))
	return nil
}

func recipesCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) > 0 && args[0] == "curated" {
		return curatedRecipesCommand(args[1:])
	}
	query := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("recipes", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "maximum results")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane recipes [QUERY] [--limit N]")
	}
	if *limit < 1 || *limit > 100 {
		return errors.New("limit must be between 1 and 100")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	path := "/api/v1/recipes?limit=" + strconv.Itoa(*limit) + "&query=" + url.QueryEscape(query)
	if err := controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	if len(response.Data) == 0 {
		fmt.Println("No immutable recipes match. Capture one from a measured deployment with `infercrane recipe create`.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RECIPE\tMODEL\tRUNTIME\tGPU\tEVIDENCE\tDIGEST")
	for _, row := range response.Data {
		fmt.Fprintf(w, "%s@%s\t%s\t%s %s\t%s\t%s\t%s\n", benchmarkValue(row["name"]), benchmarkValue(row["version"]), nestedValue(row, "payload", "model_identity"), nestedValue(row, "payload", "runtime"), nestedValue(row, "payload", "runtime_version"), nestedValue(row, "payload", "gpu"), nestedValue(row, "provenance", "evidence_class"), shortID(benchmarkValue(row["digest"])))
	}
	return w.Flush()
}

func curatedRecipesCommand(args []string) error {
	query := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("recipes curated", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane recipes curated [QUERY] [--output human|json]")
	}
	entries := curatedrecipe.Search(query)
	if *output == "json" {
		return printJSON(map[string]any{"data": entries, "evidence_class": "configuration-verified", "performance_claims": false})
	}
	if len(entries) == 0 {
		fmt.Println("No curated configuration recipes match.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RECIPE\tMODEL\tPROTOCOL\tLICENSE\tACCESS\tEVIDENCE")
	for _, entry := range entries {
		access := "public"
		if entry.Gated {
			access = "gated"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", entry.Name, entry.Model, entry.Protocol, entry.License, access, entry.EvidenceClass)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\nCreate a pinned project with: infercrane workload init --recipe NAME\nConfiguration recipes are reviewed starting points, not measured performance claims.")
	return nil
}

func modelsCommand(args []string) error {
	inspect := false
	query := ""
	if len(args) > 0 && args[0] == "inspect" {
		inspect = true
		args = args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		query, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() != 0 || (inspect && query == "") {
		return errors.New("usage: infercrane models [QUERY] | infercrane models inspect NAME [--output human|json]")
	}
	if inspect {
		entry, found := curatedrecipe.Get(query)
		if !found {
			return fmt.Errorf("catalog model %q was not found", query)
		}
		if *output == "json" {
			return printJSON(map[string]any{"model": entry, "performance_claims": false})
		}
		access := "public"
		if entry.Gated {
			access = "gated"
		}
		fmt.Printf("%s\n\nModel       %s@%s\nPublisher   %s\nTasks       %s\nProtocol    %s\nCapabilities %s\nLicense     %s · %s\nAccess      %s\nEvidence    %s\nReviewed    %s\n\nServing profiles\n", entry.DisplayName, entry.Model, entry.Revision, entry.Publisher, strings.Join(entry.Tasks, ", "), entry.Protocol, strings.Join(entry.Capabilities, ", "), entry.License, entry.LicenseURL, access, entry.EvidenceClass, entry.ReviewedAt)
		for _, profile := range entry.Profiles {
			args := "runtime defaults"
			if len(profile.RuntimeArgs) > 0 {
				args = strings.Join(profile.RuntimeArgs, " ")
			}
			fmt.Printf("  %s  %s · %s · %s · replicas %d–%d\n    %s\n    Evidence: %s\n    Args: %s\n", profile.Name, profile.Runtime, profile.ComputeMode, profile.GPUHint, profile.MinReplicas, profile.MaxReplicas, profile.Description, profile.QualificationScope, args)
		}
		fmt.Printf("\nCreate a project:\n  infercrane workload init --recipe %s --profile %s\n", entry.Name, entry.Profiles[0].Name)
		return nil
	}
	entries := curatedrecipe.Search(query)
	if *output == "json" {
		return printJSON(map[string]any{"data": entries, "performance_claims": false})
	}
	if len(entries) == 0 {
		fmt.Println("No reviewed model starting points match. Use `infercrane workload init --model REPOSITORY` for an explicit model.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tTASKS\tPROTOCOL\tLICENSE\tACCESS\tEVIDENCE")
	for _, entry := range entries {
		access := "public"
		if entry.Gated {
			access = "gated"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", entry.Name, strings.Join(entry.Tasks, ","), entry.Protocol, entry.License, access, entry.EvidenceClass)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\nInspect details with: infercrane models inspect NAME")
	return nil
}

func labCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane lab MODEL_IDENTITY [flags]")
	}
	model := args[0]
	fs := flag.NewFlagSet("lab", flag.ContinueOnError)
	objective := fs.String("objective", "latency", "latency, throughput, or cost-efficiency")
	workloadProfile := fs.String("profile", "", "optional benchmark workload profile")
	ttft := fs.String("max-ttft-p95-ms", "", "optional p95 TTFT SLO")
	workloadDigest := fs.String("workload-digest", "", "optional exact workload SHA-256")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane lab MODEL_IDENTITY [flags]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *objective != "latency" && *objective != "throughput" && *objective != "cost-efficiency" {
		return errors.New("objective must be latency, throughput, or cost-efficiency")
	}
	if *workloadProfile != "" {
		if _, err := performanceprofile.Get(*workloadProfile); err != nil {
			return err
		}
	}
	body := map[string]any{"model_identity": model, "objective": *objective, "workload_profile": *workloadProfile, "workload_digest": *workloadDigest}
	if *ttft != "" {
		value, err := strconv.ParseFloat(*ttft, 64)
		if err != nil || value < 0 {
			return errors.New("max TTFT p95 must be a nonnegative number")
		}
		body["max_ttft_p95_ms"] = value
	}
	var response struct {
		Evaluation struct {
			ID          string           `json:"id"`
			InputDigest string           `json:"input_digest"`
			Results     []map[string]any `json:"results"`
		} `json:"evaluation"`
	}
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/lab/evaluations", "", body, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Printf("Inference Lab · %s · optimize %s\nEvidence %s · persisted comparison %s\n\n", model, *objective, shortID(response.Evaluation.InputDigest), response.Evaluation.ID)
	if len(response.Evaluation.Results) == 0 {
		fmt.Println("No comparable measured benchmark evidence. InferCrane did not model or invent a result.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CHOICE\tCLASS\tCANDIDATE\tPROFILE\tTTFT P95\tOUTPUT TOK/S\tERRORS\tSLO\tCOST")
	for _, row := range response.Evaluation.Results {
		candidate := fmt.Sprintf("%s / %s / %s", benchmarkValue(row["provider"]), benchmarkValue(row["runtime"]), benchmarkValue(row["gpu"]))
		choice := ""
		if selected, _ := row["selected"].(bool); selected {
			choice = "RECOMMENDED"
		} else if comparable, _ := row["comparable"].(bool); !comparable {
			choice = "UNRANKED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s ms\t%s\t%s\t%s\t%s\n", choice, strings.ToUpper(benchmarkValue(row["evidence_class"])), candidate, benchmarkValue(row["workload_profile"]), benchmarkValue(row["ttft_p95_ms"]), benchmarkValue(row["output_tokens_second"]), benchmarkValue(row["error_rate"]), benchmarkValue(row["meets_slo"]), nestedValue(row, "cost_metadata", "available"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, row := range response.Evaluation.Results {
		if comparable, _ := row["comparable"].(bool); !comparable {
			fmt.Println("\nNo recommendation: results contain unlike workload shapes. Filter with --profile or --workload-digest.")
			break
		}
	}
	return nil
}

func nestedValue(row map[string]any, object, key string) string {
	nested, ok := row[object].(map[string]any)
	if !ok {
		return "unavailable"
	}
	return benchmarkValue(nested[key])
}

func passportCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: infercrane passport keygen [--file PATH] | passport issue|list DEPLOYMENT [flags] | passport verify FILE")
	}
	action := args[0]
	if action == "keygen" {
		fs := flag.NewFlagSet("passport keygen", flag.ContinueOnError)
		path := fs.String("file", "", "private-key destination")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane passport keygen [--file PATH]")
		}
		if *path == "" {
			configDir, err := os.UserConfigDir()
			if err != nil {
				return err
			}
			*path = filepath.Join(configDir, "infercrane", "passport-signing-key")
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
			return fmt.Errorf("refusing to overwrite existing passport signing key %s", *path)
		}
		if err != nil {
			return err
		}
		if _, err = file.WriteString(passport.EncodePrivateKey(privateKey) + "\n"); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		fmt.Printf("Passport signing key created\nFile  %s\nMode  0600\n\nSet INFERCRANE_PASSPORT_SIGNING_KEY_FILE to this path on the control plane. Back up the key securely; losing it prevents issuing evidence under the same key identity.\n", *path)
		return nil
	}
	if action == "verify" {
		if len(args) != 2 {
			return errors.New("usage: infercrane passport verify FILE")
		}
		body, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		var wrapper struct {
			Passport cliPassport `json:"passport"`
		}
		if err = json.Unmarshal(body, &wrapper); err != nil {
			return fmt.Errorf("decode passport: %w", err)
		}
		value := wrapper.Passport
		if value.Digest == "" {
			if err = json.Unmarshal(body, &value); err != nil {
				return fmt.Errorf("decode passport: %w", err)
			}
		}
		err = passport.Verify(passport.Envelope{PayloadJSON: string(value.Payload), Digest: value.Digest, Signature: value.Signature, PublicKey: value.PublicKey, Algorithm: value.Algorithm, KeyID: value.KeyID})
		if err != nil {
			return fmt.Errorf("passport verification failed: %w", err)
		}
		var signedPayload passport.Payload
		if err = json.Unmarshal(value.Payload, &signedPayload); err != nil || signedPayload.Schema != "infercrane.inference-passport/v1" {
			return errors.New("passport contains an unsupported signed payload")
		}
		complete := len(signedPayload.MissingEvidence) == 0
		fmt.Printf("VERIFIED  %s\nRevision  %s\nKey       %s\nComplete  %t\n", value.Digest, signedPayload.RevisionID, value.KeyID, complete)
		if !complete {
			fmt.Printf("Missing   %s\n", strings.Join(signedPayload.MissingEvidence, ", "))
		}
		return nil
	}
	if len(args) < 2 {
		return errors.New("deployment name is required")
	}
	name := args[1]
	fs := flag.NewFlagSet("passport "+action, flag.ContinueOnError)
	revisionID := fs.String("revision", "", "revision ID; defaults to active")
	output := fs.String("output", "human", "human or json")
	file := fs.String("file", "", "write passport JSON to file")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	switch action {
	case "issue":
		var response struct {
			Passport cliPassport `json:"passport"`
			Verified bool        `json:"verified"`
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(name)+"/passports", "", map[string]string{"revision_id": *revisionID}, &response); err != nil {
			return err
		}
		encoded, _ := json.MarshalIndent(response, "", "  ")
		if *file != "" {
			if err := os.WriteFile(*file, append(encoded, '\n'), 0o644); err != nil {
				return err
			}
		}
		if *output == "json" {
			fmt.Println(string(encoded))
		} else {
			fmt.Printf("Inference Passport\nDeployment  %s\nRevision    %s\nDigest      %s\nKey         %s\nVerified    %t\nComplete    %t\n", name, response.Passport.RevisionID, response.Passport.Digest, response.Passport.KeyID, response.Verified, response.Passport.Complete)
			if len(response.Passport.Missing) > 0 {
				fmt.Printf("Missing     %s\n", strings.Join(response.Passport.Missing, ", "))
			}
			if *file != "" {
				fmt.Printf("File        %s\n", *file)
			}
		}
		return nil
	case "list":
		var response struct {
			Data []cliPassport `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(name)+"/passports", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			encoded, _ := json.MarshalIndent(response, "", "  ")
			fmt.Println(string(encoded))
			return nil
		}
		if len(response.Data) == 0 {
			fmt.Printf("%s has no signed inference passport yet\n", name)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "CREATED\tREVISION\tDIGEST\tKEY\tVERIFIED\tCOMPLETE")
		for _, row := range response.Data {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%t\n", row.CreatedAt.Format(time.RFC3339), shortID(row.RevisionID), row.Digest, row.KeyID, row.Verified, row.Complete)
		}
		return w.Flush()
	default:
		return fmt.Errorf("unknown passport action %q", action)
	}
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

func recommendCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane recommend DEPLOYMENT [--history] [--output human|json]")
	}
	deployment := args[0]
	fs := flag.NewFlagSet("recommend", flag.ContinueOnError)
	history := fs.Bool("history", false, "list persisted history")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane recommend DEPLOYMENT [flags]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *history {
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(deployment)+"/recommendations", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		if len(response.Data) == 0 {
			fmt.Println("No persisted recommendations.")
			return nil
		}
		for _, row := range response.Data {
			fmt.Printf("%-14s %-12s %s\n", benchmarkValue(row["status"]), benchmarkValue(row["id"]), benchmarkValue(row["reason"]))
		}
		return nil
	}
	var response struct {
		Recommendation map[string]any `json:"recommendation"`
	}
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(deployment)+"/recommendations", "", map[string]any{}, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response.Recommendation)
	}
	fmt.Printf("Recommendation  %s\nEvidence        %s\nReason          %s\nAlgorithm       %s\nInput digest    %s\nMissing         %s\n", strings.ToUpper(benchmarkValue(response.Recommendation["status"])), benchmarkValue(response.Recommendation["selected_evidence_id"]), benchmarkValue(response.Recommendation["reason"]), benchmarkValue(response.Recommendation["algorithm_version"]), benchmarkValue(response.Recommendation["input_digest"]), benchmarkValue(response.Recommendation["missing"]))
	return nil
}

func sloCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 || (args[0] != "get" && args[0] != "set" && args[0] != "delete") {
		return errors.New("usage: infercrane slo get DEPLOYMENT | infercrane slo set DEPLOYMENT [threshold flags] | infercrane slo delete DEPLOYMENT")
	}
	action, deployment := args[0], args[1]
	fs := flag.NewFlagSet("slo "+action, flag.ContinueOnError)
	ttft := fs.String("ttft-p95", "", "maximum p95 TTFT milliseconds")
	latency := fs.String("latency-p95", "", "maximum p95 latency milliseconds")
	errorRate := fs.String("error-rate", "", "maximum error ratio")
	throughput := fs.String("output-tokens-second", "", "minimum output tokens/second")
	hourly := fs.String("hourly-cost", "", "maximum sourced hourly cost")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected SLO arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	path := "/api/v1/deployments/" + url.PathEscape(deployment) + "/slo-policy"
	if action == "delete" {
		if err := controlJSON(ctx, cfg, http.MethodDelete, path, "", nil, nil); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(map[string]any{"deleted": true, "deployment": deployment})
		}
		fmt.Printf("SLO policy for %s deleted\n", deployment)
		return nil
	}
	var response struct {
		Policy map[string]any `json:"policy"`
	}
	if action == "get" {
		if err := controlJSON(ctx, cfg, http.MethodGet, path, "", nil, &response); err != nil {
			return err
		}
	} else {
		request := map[string]any{}
		for key, raw := range map[string]string{"max_ttft_p95_ms": *ttft, "max_latency_p95_ms": *latency, "max_error_rate": *errorRate, "min_output_tokens_second": *throughput, "max_hourly_cost": *hourly} {
			if raw == "" {
				continue
			}
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return fmt.Errorf("%s must be numeric: %w", key, err)
			}
			request[key] = value
		}
		if len(request) == 0 {
			return errors.New("slo set requires at least one threshold")
		}
		if err := controlJSON(ctx, cfg, http.MethodPut, path, "", request, &response); err != nil {
			return err
		}
	}
	if *output == "json" {
		return printJSON(response.Policy)
	}
	fmt.Printf("SLO policy for %s\nTTFT p95 max       %s ms\nLatency p95 max    %s ms\nError rate max     %s\nOutput tok/s min   %s\nHourly cost max    %s\n", deployment, benchmarkValue(response.Policy["max_ttft_p95_ms"]), benchmarkValue(response.Policy["max_latency_p95_ms"]), benchmarkValue(response.Policy["max_error_rate"]), benchmarkValue(response.Policy["min_output_tokens_second"]), benchmarkValue(response.Policy["max_hourly_cost"]))
	return nil
}
func targetAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("target requires add or list")
	}
	if args[0] == "list" {
		fs := flag.NewFlagSet("target list", flag.ContinueOnError)
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane target list [--output human|json]")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var response struct {
			Data []targetView `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/targets", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
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
	provider := fs.String("provider", "existing", "existing, openrouter, or openai-compatible-external")
	runtimeName := fs.String("runtime", "", "runtime identity; defaults to vllm for existing targets and openai-compatible-api for external targets")
	upstream := fs.String("upstream-model", "", "upstream model")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: infercrane target add NAME --url URL [--output human|json]")
	}
	if *targetURL == "" {
		return errors.New("--url is required")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	request := map[string]string{"name": args[1], "url": *targetURL, "provider": *provider, "runtime": *runtimeName, "upstream_model": *upstream}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/targets", "", request, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
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
	providerAdapter := fs.String("provider-adapter", "", "provider adapter profile (advanced)")
	backend := fs.String("backend", "", "serving backend; use dynamo for a managed Dynamo graph")
	gpu := fs.String("gpu", "", "GPU")
	region := fs.String("region", "", "region")
	runtimeFlag := fs.String("runtime", "vllm", "runtime engine: vllm or sglang")
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
	var runtimeWorkload runtimecontract.Workload
	var servingTopology servingcontract.Topology
	runtimeEngine := *runtimeFlag
	modelRevision := ""
	runtimeVersion := ""
	if ext := filepath.Ext(model); ext == ".yaml" || ext == ".yml" {
		file, err := spec.Load(model)
		if err != nil {
			return err
		}
		if *name != "" || *targets != "" || *cloud != "" || *backend != "" || *providerAdapter != "" || *gpu != "" || *region != "" || *runtimeFlag != "vllm" {
			return errors.New("deployment YAML cannot be combined with deployment flags")
		}
		*name = file.Name
		model = file.Model.ID
		modelRevision = file.Model.Revision
		runtimeVersion = file.Runtime.Version
		runtimeEngine = file.Runtime.Engine
		*computeMode = file.Compute.Mode
		*cloud = file.Provider.Cloud
		*providerAdapter = file.Provider.Adapter
		*gpu = file.Resources.GPU
		*region = file.Provider.Region
		strategy = file.Routing.Strategy
		runtimeArgs = file.Runtime.Args
		runtimeWorkload = file.Runtime.Workload
		servingTopology = file.Serving
		*minReplicas, *maxReplicas = file.Scaling.MinReplicas, file.Scaling.MaxReplicas
	}
	if *backend != "" {
		if *backend != servingcontract.BackendDynamo {
			return errors.New("--backend currently supports dynamo; omit it for the standard provider path")
		}
		if *targets != "" || *computeMode != "elastic" || (*cloud != "" && *cloud != "kubernetes") || (*providerAdapter != "" && *providerAdapter != "kubernetes-dynamo") {
			return errors.New("--backend dynamo requires elastic Kubernetes capacity and cannot be combined with targets or another provider adapter")
		}
		if *gpu == "" {
			return errors.New("--backend dynamo requires --gpu; InferCrane will not guess cluster GPU labels")
		}
		*cloud, *providerAdapter, *minReplicas, *maxReplicas = "kubernetes", "kubernetes-dynamo", 1, 1
		servingTopology = servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
	}
	if *name == "" {
		*name = planning.DefaultName(model)
	}
	if runtimeVersion == "" {
		switch runtimeEngine {
		case "vllm":
			runtimeVersion = support.DefaultRuntimeVersion
		case "sglang":
			runtimeVersion = support.SGLangRuntimeVersion
		}
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
		request = workflows.CloudRequest{Name: *name, Model: model, ModelRevision: modelRevision, Runtime: runtimeEngine, RuntimeVersion: runtimeVersion, ComputeMode: *computeMode, Cloud: *cloud, ProviderAdapter: *providerAdapter, GPU: *gpu, Region: *region, RuntimeArgs: runtimeArgs, Workload: runtimeWorkload, Serving: servingTopology, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas}
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
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Model               string    `json:"model"`
	Runtime             string    `json:"runtime"`
	RoutingStrategy     string    `json:"routing_strategy"`
	DesiredState        string    `json:"desired_state"`
	ObservedState       string    `json:"observed_state"`
	ActiveRevisionID    string    `json:"active_revision_id"`
	CandidateRevisionID string    `json:"candidate_revision_id"`
	MinReplicas         int       `json:"min_replicas"`
	MaxReplicas         int       `json:"max_replicas"`
	AutoscalingEnabled  bool      `json:"autoscaling_enabled"`
	UpdatedAt           time.Time `json:"updated_at"`
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
	client, err := controlHTTPClient(cfg, timeout)
	if err != nil {
		return err
	}
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

type WatcherStoppedError struct {
	Cause       error
	OperationID string
}

func (e *WatcherStoppedError) Error() string {
	return fmt.Sprintf("%v: watcher stopped; operation %s continues safely in the control plane (resume: infercrane operation watch %s; cancel: infercrane operation cancel %s)", e.Cause, e.OperationID, e.OperationID, e.OperationID)
}

func (e *WatcherStoppedError) Unwrap() error { return e.Cause }

func watcherStoppedError(cause error, id string) error {
	return &WatcherStoppedError{Cause: cause, OperationID: id}
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
			ID       string   `json:"id"`
			TenantID string   `json:"tenant_id"`
			Name     string   `json:"name"`
			Role     string   `json:"role"`
			Kind     string   `json:"kind"`
			Scopes   []string `json:"scopes"`
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
	fmt.Printf("Authenticated  yes\nPrincipal      %s\nRole           %s\nScopes         %s\nTenant         %s\nControl plane  %s\n", response.Principal.Name, response.Principal.Role, strings.Join(response.Principal.Scopes, ","), response.Principal.TenantID, cfg.ControlURL)
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
	fmt.Fprintf(w, "Provider contract\t%s\nRuntime contract\t%s\nComposition contract\t%s\n\n", response.Data.ProviderContract, response.Data.RuntimeContract, response.Data.CompositionContract)
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
	for _, composition := range response.Data.Compositions {
		fmt.Fprintf(w, "%s\t%s\texternal\tcomposition\t%s\n", composition.Kind, composition.Adapter, qualified(composition.Qualification))
	}
	if len(response.Data.Compatibility) > 0 {
		fmt.Fprintln(w, "\nRUNTIME\tCLOUD\tMODE\tEVIDENCE STATE")
		for _, item := range response.Data.Compatibility {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Runtime, item.Cloud, item.Mode, item.State)
		}
	}
	return w.Flush()
}

func systemCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "instances" {
		return errors.New("usage: infercrane system instances [--output human|json]")
	}
	fs := flag.NewFlagSet("system instances", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var response struct {
		Data              []domain.ControlPlaneInstance `json:"data"`
		Count             int                           `json:"count"`
		LiveWindowSeconds int                           `json:"live_window_seconds"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/system/instances", "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(response, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "INSTANCE\tVERSION\tPROTOCOL\tHEARTBEAT\n")
	for _, instance := range response.Data {
		fmt.Fprintf(w, "%s\t%s\t%d..%d\t%s\n", instance.ID, instance.BinaryVersion, instance.ProtocolMin, instance.ProtocolMax, instance.HeartbeatAt.Format(time.RFC3339))
	}
	if len(response.Data) == 0 {
		fmt.Fprintln(w, "-\t-\t-\tno live instances")
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
		scopes := fs.String("scopes", "", "comma-separated scopes (defaults to the role ceiling)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *role == "" {
			return errors.New("--role is required")
		}
		var response struct {
			Principal struct {
				ID, Name, Role, Kind, TenantID string
				Scopes                         []string `json:"scopes"`
			} `json:"principal"`
			Credential string `json:"credential"`
		}
		request := map[string]any{"name": args[1], "role": *role}
		if *scopes != "" {
			requested := strings.Split(*scopes, ",")
			for i := range requested {
				requested[i] = strings.TrimSpace(requested[i])
			}
			request["scopes"] = requested
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/principals", "", request, &response); err != nil {
			return err
		}
		fmt.Printf("service account %s created with role %s and scopes %s\nCredential  %s\n", response.Principal.ID, response.Principal.Role, strings.Join(response.Principal.Scopes, ","), response.Credential)
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

func secretAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane secret create NAME --from-env VARIABLE | secret list [--output human|json] | secret delete ID --yes")
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New("usage: infercrane secret create NAME --from-env VARIABLE")
		}
		fs := flag.NewFlagSet("secret create", flag.ContinueOnError)
		fromEnv := fs.String("from-env", "", "environment variable containing the secret")
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *fromEnv == "" {
			return errors.New("--from-env is required; raw secret values are never accepted")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var response struct {
			Secret domain.SecretReference `json:"secret"`
		}
		request := map[string]string{"name": args[1], "resolver": "env", "reference": *fromEnv}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/secrets", "", request, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("secret reference %s created\nResolver  env:%s\n", response.Secret.ID, response.Secret.Reference)
		return nil
	case "list":
		fs := flag.NewFlagSet("secret list", flag.ContinueOnError)
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var response struct {
			Data []domain.SecretReference `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/secrets", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tRESOLVER\tREFERENCE")
		for _, item := range response.Data {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.ID, item.Name, item.Resolver, item.Reference)
		}
		return w.Flush()
	case "delete":
		if len(args) < 2 {
			return errors.New("usage: infercrane secret delete ID --yes")
		}
		fs := flag.NewFlagSet("secret delete", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "confirm deletion")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if !*yes {
			return errors.New("secret reference deletion requires --yes")
		}
		if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/secrets/"+url.PathEscape(args[1]), "", nil, nil); err != nil {
			return err
		}
		fmt.Printf("secret reference %s deleted\n", args[1])
		return nil
	default:
		return errors.New("usage: infercrane secret create NAME --from-env VARIABLE | secret list | secret delete ID --yes")
	}
}

func externalAPICommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: infercrane external configure DEPLOYMENT [flags] | external inspect DEPLOYMENT [--output human|json]")
	}
	deployment := args[1]
	switch args[0] {
	case "configure":
		fs := flag.NewFlagSet("external configure", flag.ContinueOnError)
		target := fs.String("target", "", "attached external target name")
		adapter := fs.String("adapter", "openrouter", "openrouter or openai-compatible-external")
		secretReference := fs.String("secret-reference", "", "secret reference ID")
		requestLimit := fs.Int64("request-limit", 0, "hard maximum reserved requests")
		costLimit := fs.String("cost-limit-usd", "", "hard USD reservation budget")
		maxRequestCost := fs.String("max-request-cost-usd", "", "worst-case USD reserved per request")
		overflowMode := fs.String("mode", "health", "health or health_and_queue")
		queueThreshold := fs.String("queue-threshold", "", "waiting-request threshold for queue overflow")
		breachIntervals := fs.Int("breach-intervals", 2, "consecutive queue breaches before overflow")
		recoveryIntervals := fs.Int("recovery-intervals", 2, "consecutive healthy observations before recovery")
		cooldownSeconds := fs.Int("cooldown-seconds", 60, "minimum seconds between route changes")
		signalMaxAgeSeconds := fs.Int("signal-max-age-seconds", 30, "maximum queue evidence age")
		acknowledge := fs.Bool("acknowledge-external-data", false, "acknowledge prompts and outputs leave controlled infrastructure")
		enable := fs.Bool("enable", false, "enable fallback after validation")
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *target == "" || *secretReference == "" || *requestLimit < 1 || *costLimit == "" || *maxRequestCost == "" {
			return errors.New("--target, --secret-reference, --request-limit, --cost-limit-usd, and --max-request-cost-usd are required")
		}
		if *enable && !*acknowledge {
			return errors.New("--enable requires --acknowledge-external-data")
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		costMicrousd, err := parseMicrousd(*costLimit)
		if err != nil {
			return fmt.Errorf("--cost-limit-usd: %w", err)
		}
		maxMicrousd, err := parseMicrousd(*maxRequestCost)
		if err != nil {
			return fmt.Errorf("--max-request-cost-usd: %w", err)
		}
		request := map[string]any{"target": *target, "adapter": *adapter, "secret_reference_id": *secretReference, "enabled": *enable, "privacy_acknowledged": *acknowledge, "request_limit": *requestLimit, "cost_limit_microusd": costMicrousd, "max_request_cost_microusd": maxMicrousd, "overflow_mode": *overflowMode, "breach_intervals": *breachIntervals, "recovery_intervals": *recoveryIntervals, "cooldown_seconds": *cooldownSeconds, "signal_max_age_seconds": *signalMaxAgeSeconds}
		if *overflowMode == "health_and_queue" {
			if *queueThreshold == "" {
				return errors.New("--queue-threshold is required for health_and_queue mode")
			}
			threshold, parseErr := strconv.ParseFloat(*queueThreshold, 64)
			if parseErr != nil || threshold <= 0 {
				return errors.New("--queue-threshold must be positive")
			}
			request["queue_threshold"] = threshold
		}
		var response struct {
			Policy domain.ExternalTargetPolicy `json:"policy"`
		}
		if err := controlJSON(ctx, cfg, http.MethodPut, "/api/v1/deployments/"+url.PathEscape(deployment)+"/external-policy", "", request, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		state := "configured (disabled)"
		if response.Policy.Enabled {
			state = "enabled"
		}
		fmt.Printf("External fallback %s\nDeployment       %s\nAdapter          %s\nMode             %s\nRequests         %d hard limit\nCost reservation $%s hard limit\nPrivacy          acknowledged\n", state, deployment, response.Policy.Adapter, response.Policy.OverflowMode, response.Policy.RequestLimit, formatMicrousd(response.Policy.CostLimitMicrousd))
		return nil
	case "inspect":
		fs := flag.NewFlagSet("external inspect", flag.ContinueOnError)
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := validateOutput(*output); err != nil {
			return err
		}
		var response struct {
			Policy domain.ExternalTargetPolicy `json:"policy"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(deployment)+"/external-policy", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("Deployment       %s\nAdapter          %s\nEnabled          %t\nPrivacy          %t\nRequests         %d/%d reserved\nCost reservation $%s/$%s\n", deployment, response.Policy.Adapter, response.Policy.Enabled, response.Policy.PrivacyAcknowledged, response.Policy.RequestsReserved, response.Policy.RequestLimit, formatMicrousd(response.Policy.CostReservedMicrousd), formatMicrousd(response.Policy.CostLimitMicrousd))
		return nil
	default:
		return errors.New("usage: infercrane external configure DEPLOYMENT [flags] | external inspect DEPLOYMENT")
	}
}

func parseMicrousd(value string) (int64, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || len(parts) == 2 && len(parts[1]) > 6 {
		return 0, errors.New("use a positive decimal with at most six fractional digits")
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, errors.New("use a positive decimal with at most six fractional digits")
			}
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > (1<<63-1)/1_000_000 {
		return 0, errors.New("USD amount is too large")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	fractional, _ := strconv.ParseInt(fraction, 10, 64)
	result := whole*1_000_000 + fractional
	if result < 1 {
		return 0, errors.New("amount must be positive")
	}
	return result, nil
}

func formatMicrousd(value int64) string {
	return fmt.Sprintf("%d.%06d", value/1_000_000, value%1_000_000)
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
			for _, line := range startupEvidenceLines(replica.ProviderDetails) {
				fmt.Println(line)
			}
			fmt.Printf("Details    %s\n", replica.ProviderDetails)
		}
	}
	return nil
}

func startupEvidenceLines(raw json.RawMessage) []string {
	var details struct {
		RuntimeReadyAt time.Time `json:"runtime_ready_at"`
		Startup        struct {
			CurrentStage string `json:"current_stage"`
			ImageCache   string `json:"image_cache"`
			Stages       []struct {
				Name string    `json:"name"`
				At   time.Time `json:"at"`
			} `json:"stages"`
		} `json:"startup_evidence"`
	}
	if json.Unmarshal(raw, &details) != nil || len(details.Startup.Stages) == 0 {
		return nil
	}
	labels := map[string]string{
		"identity_start": "machine bootstrap", "identity_ready": "workload identity", "image_check": "image check",
		"image_cache_hit": "image cache hit", "image_cache_miss_required": "required image cache miss", "image_pull_start": "image pull", "image_pull_complete": "image pulled",
		"runtime_start": "runtime start", "runtime_container_started": "container started",
	}
	start := details.Startup.Stages[0].At
	lines := []string{fmt.Sprintf("Image      cache %s", emptyAs(details.Startup.ImageCache, "unknown")), "Startup    measured provider waterfall"}
	for _, stage := range details.Startup.Stages {
		label := labels[stage.Name]
		if label == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("  +%-8s %s", compactDuration(stage.At.Sub(start)), label))
	}
	if !details.RuntimeReadyAt.IsZero() && !start.IsZero() && !details.RuntimeReadyAt.Before(start) {
		lines = append(lines, fmt.Sprintf("  +%-8s runtime ready", compactDuration(details.RuntimeReadyAt.Sub(start))))
	}
	stageAt := map[string]time.Time{}
	for _, stage := range details.Startup.Stages {
		stageAt[stage.Name] = stage.At
	}
	phase := func(name, from, to string) {
		fromAt, fromOK := stageAt[from]
		toAt, toOK := stageAt[to]
		if fromOK && toOK && !toAt.Before(fromAt) {
			lines = append(lines, fmt.Sprintf("  %-18s %s", name, compactDuration(toAt.Sub(fromAt))))
		}
	}
	lines = append(lines, "Phases     measured boundaries")
	phase("workload identity", "identity_start", "identity_ready")
	phase("container transfer", "image_pull_start", "image_pull_complete")
	phase("container launch", "runtime_start", "runtime_container_started")
	if containerAt, ok := stageAt["runtime_container_started"]; ok && !details.RuntimeReadyAt.IsZero() && !details.RuntimeReadyAt.Before(containerAt) {
		lines = append(lines, fmt.Sprintf("  %-18s %s", "model + runtime ready", compactDuration(details.RuntimeReadyAt.Sub(containerAt))))
	}
	lines = append(lines,
		"  provider allocation unavailable (provider boundary precedes machine bootstrap)",
		"  model download      unavailable (runtime does not expose a qualified artifact boundary)",
	)
	return lines
}

func compactDuration(value time.Duration) string {
	if value < 0 {
		return "unknown"
	}
	if value < time.Second {
		return value.Round(time.Millisecond).String()
	}
	return value.Round(time.Second).String()
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
	if len(args) > 0 && args[0] == "inspect" {
		return requestInspectCommand(ctx, cfg, args[1:])
	}
	if len(args) == 0 {
		return errors.New("usage: infercrane request ENDPOINT [--protocol chat|responses|embeddings|completions|batch] [--message TEXT] [--stream] [--output human|json]")
	}
	name := args[0]
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	message := fs.String("message", "Say hello in one sentence.", "user message")
	protocol := fs.String("protocol", "chat", "chat, responses, embeddings, completions, or batch")
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
	path := ""
	payload := map[string]any{"model": name}
	switch *protocol {
	case "chat":
		path = "/v1/chat/completions"
		payload["messages"] = []map[string]string{{"role": "user", "content": *message}}
		payload["stream"] = *stream
	case "responses":
		path = "/v1/responses"
		payload["input"] = *message
	case "embeddings":
		path = "/v1/embeddings"
		payload["input"] = *message
		if *stream {
			return errors.New("--stream is not supported with --protocol embeddings")
		}
	case "completions":
		path = "/v1/completions"
		payload["prompt"] = *message
	case "batch":
		path = "/v1/chat/completions/batch"
		payload["messages"] = [][]map[string]string{{{"role": "user", "content": *message}}}
		if *stream {
			return errors.New("--stream is not supported with --protocol batch")
		}
	default:
		return errors.New("--protocol must be chat, responses, embeddings, completions, or batch")
	}
	if *stream {
		payload["stream"] = true
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode inference request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.ControlURL, "/")+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create inference request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	request.Header.Set("Content-Type", "application/json")
	requestClient, err := controlHTTPClient(cfg, 0)
	if err != nil {
		return err
	}
	response, err := requestClient.Do(request)
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
	if *protocol != "chat" {
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

func requestInspectCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infercrane request inspect REQUEST_ID [--output human|json]")
	}
	requestID := args[0]
	fs := flag.NewFlagSet("request inspect", flag.ContinueOnError)
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || requestID == "" {
		return errors.New("usage: infercrane request inspect REQUEST_ID [--output human|json]")
	}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/requests/"+url.PathEscape(requestID), "", nil, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("Request        %s\n\nResolution\n  Logical model  %v\n  Endpoint       %v\n  Environment    %v\n  Binding        %v\n  Deployment     %s\n  Revision       %s\n  Target         %s\n  Provider       %v\n  Runtime        %v\n\nTiming\n  Queue          %s\n  TTFT           %s\n  Generation     %s\n  Total          %s\n\nUsage\n  Input          %s\n  Output         %s\n  Streaming      %v\n\nOutcome\n  Status         %v\n  Retries        %v\n  Fallback       %s\n  Error          %s\n\nPrivacy\n  Content        not recorded\n", response["request_id"], response["logical_model"], response["endpoint"], response["environment"], response["binding"], inspectionValue(response["deployment"]), inspectionValue(response["revision"]), inspectionValue(response["target"]), response["provider"], response["runtime"], inspectionMetric(response["queue_ms"], "ms"), inspectionMetric(response["ttft_ms"], "ms"), inspectionMetric(response["generation_ms"], "ms"), inspectionMetric(response["latency_ms"], "ms"), inspectionMetric(response["input_tokens"], "tokens"), inspectionMetric(response["output_tokens"], "tokens"), response["streaming"], response["status_code"], response["retry_count"], inspectionValue(response["fallback_reason"]), inspectionValue(response["error_type"]))
	return nil
}

func inspectionValue(value any) string {
	if value == nil || strings.TrimSpace(fmt.Sprint(value)) == "" || fmt.Sprint(value) == "<nil>" {
		return "—"
	}
	return fmt.Sprint(value)
}

func inspectionMetric(value any, unit string) string {
	if inspectionValue(value) == "—" {
		return "unavailable"
	}
	return fmt.Sprintf("%v %s", value, unit)
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
		return errors.New("usage: infercrane rollout inspect|policy|create|provision|validate|evaluate|promote|reject|rollback DEPLOYMENT [REVISION] [flags]")
	}
	if args[0] == "policy" && args[1] == "set" {
		if len(args) < 3 {
			return errors.New("usage: infercrane rollout policy set DEPLOYMENT [policy flags]")
		}
		name := args[2]
		var policy domain.ReleaseGuardPolicy
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(name)+"/release-guard/policy", "", nil, &policy); err != nil {
			return err
		}
		fs := flag.NewFlagSet("rollout policy set", flag.ContinueOnError)
		enabled := fs.Bool("enabled", policy.Enabled, "enable Release Guard")
		compatibility := fs.Bool("require-compatibility", policy.RequireCompatibilityEvidence, "require comparable model/runtime evidence")
		synthetic := fs.Bool("require-synthetic", policy.RequireSyntheticEvidence, "require bounded AIPerf validation")
		quality := fs.Bool("require-quality", policy.RequireQualityEvidence, "require signed comparable semantic quality evidence")
		autoRollback := fs.Bool("auto-rollback", policy.AutoRollbackEnabled, "monitor promoted revision and automatically roll back rejection")
		minimum := fs.Int("minimum-requests", policy.MinimumRequests, "minimum measured requests per revision")
		maxCost := fs.String("max-cost-regression", "", "maximum sourced cost regression percent; omit to preserve")
		minimumQuality := fs.String("minimum-quality-score", "", "minimum candidate score from 0 to 1; omit to preserve")
		maxQualityRegression := fs.String("max-quality-regression", "", "maximum comparable score regression percent; omit to preserve")
		window := fs.Int("auto-rollback-window", policy.AutoRollbackWindowSeconds, "observation window seconds")
		maxRequests := fs.Int("validation-max-requests", policy.ValidationMaxRequests, "hard validation request bound")
		maxConcurrency := fs.Int("validation-max-concurrency", policy.ValidationMaxConcurrency, "hard validation concurrency bound")
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		policy.Enabled = *enabled
		policy.RequireCompatibilityEvidence = *compatibility
		policy.RequireSyntheticEvidence = *synthetic
		policy.RequireQualityEvidence = *quality
		policy.AutoRollbackEnabled = *autoRollback
		policy.MinimumRequests = *minimum
		policy.AutoRollbackWindowSeconds = *window
		policy.ValidationMaxRequests = *maxRequests
		policy.ValidationMaxConcurrency = *maxConcurrency
		if *maxCost != "" {
			value, err := strconv.ParseFloat(*maxCost, 64)
			if err != nil {
				return fmt.Errorf("--max-cost-regression: %w", err)
			}
			policy.MaxCostRegressionPercent = &value
		}
		if *minimumQuality != "" {
			value, err := strconv.ParseFloat(*minimumQuality, 64)
			if err != nil {
				return fmt.Errorf("--minimum-quality-score: %w", err)
			}
			policy.MinimumQualityScore = &value
		}
		if *maxQualityRegression != "" {
			value, err := strconv.ParseFloat(*maxQualityRegression, 64)
			if err != nil {
				return fmt.Errorf("--max-quality-regression: %w", err)
			}
			policy.MaxQualityRegressionPercent = &value
		}
		if err := controlJSON(ctx, cfg, http.MethodPut, "/api/v1/deployments/"+url.PathEscape(name)+"/release-guard/policy", "", policy, &policy); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(policy)
		}
		fmt.Printf("%s Release Guard policy updated\nCompatibility evidence  %t\nSynthetic validation    %t\nSemantic quality         %t\nAutomatic rollback      %t (%ds)\nValidation bound        %d requests x %d concurrency\n", name, policy.RequireCompatibilityEvidence, policy.RequireSyntheticEvidence, policy.RequireQualityEvidence, policy.AutoRollbackEnabled, policy.AutoRollbackWindowSeconds, policy.ValidationMaxRequests, policy.ValidationMaxConcurrency)
		return nil
	}
	if args[0] == "policy" && args[1] == "get" {
		if len(args) < 3 {
			return errors.New("deployment name is required")
		}
		args = append([]string{"policy", args[2]}, args[3:]...)
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
			fmt.Printf("%s Release Guard policy\nEnabled                %t\nMinimum requests       %d\nMax TTFT regression    %.1f%%\nMax latency regression %.1f%%\nMax error increase     %.2f%%\nMax throughput drop    %.1f%%\nCompatibility evidence %t\nSynthetic validation   %t\nSemantic quality       %t\nMinimum quality        %s\nMax quality regression %s\nAutomatic rollback     %t (%ds)\nValidation bound       %d requests x %d concurrency\n", name, policy.Enabled, policy.MinimumRequests, policy.MaxTTFTRegressionPercent, policy.MaxLatencyRegressionPercent, policy.MaxErrorRateIncrease*100, policy.MaxOutputThroughputDropPercent, policy.RequireCompatibilityEvidence, policy.RequireSyntheticEvidence, policy.RequireQualityEvidence, formatGuardMetric(policy.MinimumQualityScore, ""), formatGuardMetric(policy.MaxQualityRegressionPercent, "%"), policy.AutoRollbackEnabled, policy.AutoRollbackWindowSeconds, policy.ValidationMaxRequests, policy.ValidationMaxConcurrency)
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
			fmt.Fprintf(w, "Semantic quality\t%s\t%s\n", formatGuardMetric(metrics.Active.QualityScore, ""), formatGuardMetric(metrics.Candidate.QualityScore, ""))
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
	candidateFile := fs.String("file", "", "candidate DeploymentSpec YAML")
	runtimeName := fs.String("runtime", "vllm", "candidate runtime")
	routing := fs.String("routing", "round-robin", "candidate routing strategy")
	minReplicas := fs.Int("min", 1, "candidate minimum replicas")
	maxReplicas := fs.Int("max", 1, "candidate maximum replicas")
	cloud := fs.String("cloud", "", "candidate provider cloud")
	providerAdapter := fs.String("provider-adapter", "", "candidate provider adapter profile (advanced)")
	gpu := fs.String("gpu", "", "candidate GPU")
	region := fs.String("region", "", "candidate region")
	modelRevision := fs.String("model-revision", "", "candidate model revision")
	runtimeVersion := fs.String("runtime-version", "", "candidate runtime version")
	runtimeArgs := fs.String("runtime-args", "", "comma-separated candidate runtime arguments")
	validationRequests := fs.Int("requests", 20, "bounded AIPerf requests per revision")
	validationConcurrency := fs.Int("concurrency", 1, "bounded AIPerf concurrency")
	acknowledgeValidationCost := fs.Bool("acknowledge-validation-cost", false, "confirm direct active/candidate validation may incur provider cost")
	rest := args[2:]
	revisionID := ""
	if action != "create" && action != "evaluate" && action != "validate" {
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
	if action == "validate" {
		if !*acknowledgeValidationCost {
			return errors.New("rollout validate requires --acknowledge-validation-cost because it sends explicit measured traffic to both revisions")
		}
		var view deploymentView
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(name), "", nil, &view); err != nil {
			return err
		}
		if view.Deployment.CandidateRevisionID == "" {
			return errors.New("deployment has no candidate revision")
		}
		if *validationRequests < 1 || *validationRequests > view.ReleaseGuardPolicy.ValidationMaxRequests || *validationConcurrency < 1 || *validationConcurrency > view.ReleaseGuardPolicy.ValidationMaxConcurrency {
			return fmt.Errorf("validation exceeds persisted bounds: requests <= %d and concurrency <= %d", view.ReleaseGuardPolicy.ValidationMaxRequests, view.ReleaseGuardPolicy.ValidationMaxConcurrency)
		}
		fmt.Fprintln(os.Stderr, "Notice: sending explicit bounded AIPerf validation to active and candidate revisions; provider inference cost may be incurred. User traffic is never duplicated.")
		for _, selector := range []string{"active", "candidate"} {
			var benchmarkResponse map[string]any
			if err := controlJSONWithTimeout(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(name)+"/benchmarks", "", map[string]any{"requests": *validationRequests, "concurrency": *validationConcurrency, "random_seed": 17, "revision": selector}, &benchmarkResponse, 35*time.Minute); err != nil {
				return fmt.Errorf("%s revision validation: %w", selector, err)
			}
		}
		var response struct {
			Operation domain.Operation `json:"operation"`
		}
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/deployments/"+url.PathEscape(name)+"/rollouts/guard/evaluate", *key, struct{}{}, &response); err != nil {
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
		} else {
			fmt.Printf("Deployment  %s\nAction      validate\nOperation   %s\nStatus      %s\n", name, response.Operation.ID, response.Operation.Status)
		}
		return nil
	}
	path := ""
	var request any = struct{}{}
	switch action {
	case "create":
		path = "/api/v1/deployments/" + url.PathEscape(name) + "/rollouts"
		var revisionSpec domain.DeploymentRevisionSpec
		if *candidateFile != "" {
			conflicting := ""
			fs.Visit(func(item *flag.Flag) {
				switch item.Name {
				case "file", "output", "wait", "wait-timeout", "idempotency-key":
				default:
					conflicting = item.Name
				}
			})
			if conflicting != "" {
				return fmt.Errorf("--file cannot be combined with --%s", conflicting)
			}
			candidate, loadErr := spec.Load(*candidateFile)
			if loadErr != nil {
				return loadErr
			}
			if candidate.Name != name {
				return fmt.Errorf("candidate DeploymentSpec name %q must match rollout deployment %q", candidate.Name, name)
			}
			if candidate.Compute.Mode != "elastic" {
				return errors.New("candidate DeploymentSpec must use elastic compute")
			}
			revisionSpec = domain.DeploymentRevisionSpec{
				Model: candidate.Model.ID, ModelRevision: candidate.Model.Revision,
				Runtime: candidate.Runtime.Engine, RuntimeVersion: candidate.Runtime.Version, RuntimeArgs: candidate.Runtime.Args,
				RoutingStrategy: candidate.Routing.Strategy, MinReplicas: candidate.Scaling.MinReplicas, MaxReplicas: candidate.Scaling.MaxReplicas,
				AutoscalingEnabled: candidate.Scaling.MaxReplicas > candidate.Scaling.MinReplicas, ComputeMode: candidate.Compute.Mode,
				Cloud: candidate.Provider.Cloud, ProviderAdapter: candidate.Provider.Adapter, GPU: candidate.Resources.GPU, Region: candidate.Provider.Region,
				Workload: candidate.Runtime.Workload, Serving: candidate.Serving,
			}
		} else {
			if *model == "" {
				return errors.New("--model or --file is required")
			}
			if *minReplicas < 1 || *maxReplicas < *minReplicas {
				return errors.New("replica bounds must satisfy 1 <= min <= max")
			}
			if (*cloud == "") != (*gpu == "") {
				return errors.New("--cloud and --gpu must be provided together")
			}
			computeMode := "existing"
			if *cloud != "" {
				computeMode = "elastic"
			}
			revisionSpec = domain.DeploymentRevisionSpec{Model: *model, ModelRevision: *modelRevision, Runtime: *runtimeName, RuntimeVersion: *runtimeVersion, RoutingStrategy: *routing, MinReplicas: *minReplicas, MaxReplicas: *maxReplicas, AutoscalingEnabled: *maxReplicas > *minReplicas, ComputeMode: computeMode, Cloud: *cloud, ProviderAdapter: *providerAdapter, GPU: *gpu, Region: *region}
			if *runtimeArgs != "" {
				revisionSpec.RuntimeArgs = splitTargets(*runtimeArgs)
			}
		}
		request = map[string]any{"spec": revisionSpec}
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
	const membershipLiveFor = 45 * time.Second
	if err := s.RegisterControlPlaneInstance(ctx, domain.ControlPlaneInstance{ID: cfg.InstanceID, BinaryVersion: version, ProtocolMin: controlPlaneProtocolMin, ProtocolMax: controlPlaneProtocolMax}, membershipLiveFor); err != nil {
		return fmt.Errorf("register control-plane instance: %w", err)
	}
	defer func() {
		unregisterCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if unregisterErr := unregisterControlPlaneInstance(unregisterCtx, s, cfg.InstanceID, 100*time.Millisecond); unregisterErr != nil {
			slog.Error("control-plane membership withdrawal failed", "instance_id", cfg.InstanceID, "error", unregisterErr)
		}
	}()
	go heartbeatControlPlane(ctx, s, cfg.InstanceID, 10*time.Second)
	integrationRegistry, err := integration.V1Catalog()
	if err != nil {
		return fmt.Errorf("configure integration contracts: %w", err)
	}
	directory := routes.New()
	contextPassports := contextpassport.New()
	if err := refreshContextPassports(ctx, s, contextPassports); err != nil {
		return fmt.Errorf("load Context Passport snapshot: %w", err)
	}
	go runContextPassportRefresh(ctx, s, contextPassports, time.Second, slog.Default())
	backend := router.NewVLLM(cfg.RouterBinary, cfg.APIKey)
	runtime := runtimeadapter.OpenAI{APIKey: cfg.APIKey}
	runtimeProfile, err := integrationRegistry.Runtime(support.DefaultRuntime)
	if err != nil {
		return fmt.Errorf("configure runtime contract: %w", err)
	}
	sglangProfile, err := integrationRegistry.Runtime("sglang")
	if err != nil {
		return fmt.Errorf("configure SGLang runtime contract: %w", err)
	}
	customProfile, err := integrationRegistry.Runtime("custom-oci")
	if err != nil {
		return fmt.Errorf("configure custom OCI runtime contract: %w", err)
	}
	runtimeBackends, err := integration.NewRuntimeBackends(integration.RuntimeBackend{Profile: runtimeProfile, Inspector: runtime}, integration.RuntimeBackend{Profile: sglangProfile, Inspector: runtime}, integration.RuntimeBackend{Profile: customProfile, Inspector: runtime})
	if err != nil {
		return fmt.Errorf("bind runtime adapter: %w", err)
	}
	serverless := provision.RunPodServerless{APIKey: cfg.RunPodAPIKey, BaseURL: cfg.RunPodRESTURL, TemplateID: cfg.RunPodServerlessTemplateID}
	externalBudgets := external.NewBudgetPool()
	externalCoordinator := &external.Coordinator{Store: s, Secrets: secrets.Environment{}, Budgets: externalBudgets}
	logger := slog.Default()
	recorder := accounting.New(s, logger, 8192, 4)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = recorder.Close(closeCtx)
	}()
	rec := &reconcile.Reconciler{Store: s, Routes: directory, Router: backend, RouterAPIKey: cfg.APIKey, Runtimes: runtimeBackends, Interval: cfg.HealthInterval, RouterStartPort: cfg.RouterStartPort, InstanceID: cfg.InstanceID, Logger: logger, DirectTargets: map[string]reconcile.DirectTargetBackend{"runpod-serverless": {Provider: "runpod", APIKey: cfg.RunPodAPIKey, Status: serverless}}, ExternalFallback: externalCoordinator, QueueSignals: autoscale.VLLMSignals{Targets: s, APIKey: cfg.APIKey}}
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
	controlAuthenticator := authn.Chain{Authenticators: []authn.PrincipalAuthenticator{credentialCache}}
	if cfg.HostedAuthEnabled() {
		jwtKey, readErr := os.ReadFile(cfg.HostedAuthJWTKeyFile)
		if readErr != nil {
			return fmt.Errorf("read hosted auth JWT public key: %w", readErr)
		}
		hosted, hostedErr := authn.NewClerkAuthenticator(authn.ClerkVerifierConfig{JWTKey: string(jwtKey), Issuer: cfg.HostedAuthIssuer, Audience: cfg.HostedAuthAudience, AuthorizedParties: cfg.HostedAuthAuthorizedParties}, s, store.WebConsoleAccess)
		if hostedErr != nil {
			return fmt.Errorf("configure hosted authentication: %w", hostedErr)
		}
		controlAuthenticator.Authenticators = append(controlAuthenticator.Authenticators, hosted)
	}
	requestQuotas := requestquota.New(s)
	if err := requestQuotas.Refresh(ctx); err != nil {
		return fmt.Errorf("load request quota leases: %w", err)
	}
	go func() {
		if err := requestQuotas.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("request quota lease worker stopped", "error", err)
		}
	}()
	admissionPool := admission.New()
	if err := admissionPool.Refresh(ctx, s); err != nil {
		return fmt.Errorf("load admission policy snapshot: %w", err)
	}
	go func() {
		if err := admissionPool.Run(ctx, s, time.Second); err != nil && ctx.Err() == nil {
			logger.Error("admission policy refresh stopped", "error", err)
		}
	}()
	diagnostics := func(checkCtx context.Context, cloud, checkServerless, checkAWS, checkGCP, checkKubernetes bool) doctor.Report {
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
		if checkAWS {
			report.Add(doctor.CheckAWSBYOC(checkCtx, cfg, doctor.Dependencies{}))
		}
		if checkGCP {
			report.Add(doctor.CheckGCPBYOC(checkCtx, cfg, doctor.Dependencies{}))
		}
		if checkKubernetes {
			report.Add(doctor.CheckKubernetes(checkCtx, cfg, doctor.Dependencies{}))
			report.Capabilities = append(report.Capabilities,
				doctor.Capability{Adapter: "kubernetes", Name: "server_side_apply", State: "supported", Detail: "strict server-side validation and apply without force-conflicts"},
				doctor.Capability{Adapter: "kubernetes", Name: "namespaced_ownership", State: "supported", Detail: "InferCrane owns a bounded Deployment/Service set or one KServe InferenceService"},
			)
			if cfg.DynamoEnabled() {
				report.Add(doctor.CheckDynamo(checkCtx, cfg, doctor.Dependencies{}))
				report.Capabilities = append(report.Capabilities,
					doctor.Capability{Adapter: "kubernetes-dynamo", Name: "graph_ownership", State: "supported", Detail: "InferCrane owns one DGD parent; Dynamo owns the internal graph"},
					doctor.Capability{Adapter: "kubernetes-dynamo", Name: "real_gpu_qualification", State: "unknown", Detail: "doctor proves API and RBAC only; GPU/runtime evidence remains separate"},
				)
			}
		}
		return report
	}
	passportKey, err := loadPassportSigningKey(cfg.PassportSigningKeyFile)
	if err != nil {
		return err
	}
	benchmarkBackends := map[string]controlapi.BackendMetadata{
		"skypilot":          {APIKey: cfg.APIKey, APIKeyEnv: "INFERCRANE_WORKER_API_KEY"},
		"runpod-serverless": {APIKey: cfg.RunPodAPIKey, APIKeyEnv: "RUNPOD_API_KEY", Serverless: true},
	}
	if cfg.AWSEnabled() {
		benchmarkBackends["aws-ec2"] = controlapi.BackendMetadata{APIKey: cfg.APIKey, APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}
	}
	if cfg.GCPEnabled() {
		benchmarkBackends["gcp-compute"] = controlapi.BackendMetadata{APIKey: cfg.APIKey, APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}
	}
	if cfg.KubernetesEnabled() {
		benchmarkBackends["kubernetes"] = controlapi.BackendMetadata{APIKey: cfg.APIKey, APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}
	}
	var asyncService *asyncinference.Service
	if cfg.AsyncEncryptionKey != "" {
		cipher, cipherErr := asyncinference.NewCipher(cfg.AsyncEncryptionKey)
		if cipherErr != nil {
			return fmt.Errorf("configure async inference encryption: %w", cipherErr)
		}
		asyncService = &asyncinference.Service{Store: s, Cipher: cipher, KeyReference: cfg.AsyncEncryptionKeyReference, GatewayURL: cfg.ControlURL, APIKey: cfg.APIKey, Owner: cfg.InstanceID + ":async", Lease: time.Minute, Secrets: secrets.Environment{}}
	}
	controlAPI := controlapi.API{Store: s, APIKey: cfg.APIKey, Authenticator: controlAuthenticator, BenchmarkRunner: benchmark.Runner{}, Diagnostics: diagnostics, Backends: benchmarkBackends, Integrations: integrationRegistry.Snapshot(), GatewayURL: cfg.ControlURL, AIPerfBinary: cfg.AIPerfBinary, PassportPrivateKey: passportKey, EndpointRefresh: rec.RefreshEndpoints, CredentialRefresh: credentialCache.Refresh, AlertDeliverer: alert.Deliverer{Store: s, Secrets: secrets.Environment{}}, ContextPassports: contextPassports, ProductVersion: version}
	// Assign the optional service only when it exists. A nil *Service stored in
	// an interface is non-nil and would otherwise turn the intended capability
	// error into a panic when async encryption is not configured.
	if asyncService != nil {
		controlAPI.AsyncInference = asyncService
	}
	control := controlAPI.Handler()
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
	skyProvider := provision.SkyPilot{APIKey: cfg.APIKey}
	capacity := provision.RunPodAvailability{APIKey: cfg.RunPodAPIKey}
	elasticBackends := []workflows.ReplicaBackend{{Name: "skypilot", Cloud: "runpod", Runtime: "vllm", Default: true, Profile: elasticProfile, Provider: skyProvider, Capacity: capacity}}
	if cfg.AWSEnabled() {
		awsProfile, profileErr := integrationRegistry.Provider("aws-ec2")
		if profileErr != nil {
			return fmt.Errorf("configure AWS integration: %w", profileErr)
		}
		awsProvider := provision.AWSEC2{
			RoleARN: cfg.AWSRoleARN, ExternalID: cfg.AWSExternalID, Region: cfg.AWSRegion,
			SubnetID: cfg.AWSSubnetID, SecurityGroupIDs: cfg.AWSSecurityGroupIDs,
			AMIID: cfg.AWSAMIID, InstanceType: cfg.AWSInstanceType, GPU: cfg.AWSGPU,
			InstanceProfileARN: cfg.AWSInstanceProfileARN, WorkerSecretARN: cfg.AWSWorkerSecretARN,
			ImageDigest: cfg.AWSImageDigest, RootVolumeGiB: cfg.AWSRootVolumeGiB, ImageCachePolicy: cfg.AWSImageCachePolicy,
		}
		elasticBackends = append(elasticBackends, workflows.ReplicaBackend{Name: "aws-ec2", Cloud: "aws", Runtime: "vllm", Default: true, Profile: awsProfile, Provider: awsProvider})
		elasticBackends = append(elasticBackends,
			workflows.ReplicaBackend{Name: "aws-ec2", Cloud: "aws", Runtime: "sglang", Default: true, Profile: awsProfile, Provider: awsProvider},
			workflows.ReplicaBackend{Name: "aws-ec2", Cloud: "aws", Runtime: "custom-oci", Default: true, Profile: awsProfile, Provider: awsProvider},
		)
	}
	if cfg.GCPEnabled() {
		gcpProfile, profileErr := integrationRegistry.Provider("gcp-compute")
		if profileErr != nil {
			return fmt.Errorf("configure GCP integration: %w", profileErr)
		}
		gcpProvider := provision.GCPCompute{Project: cfg.GCPProject, Zone: cfg.GCPZone, Subnet: cfg.GCPSubnet, MachineType: cfg.GCPMachineType, GPUType: cfg.GCPGPU, ServiceAccount: cfg.GCPServiceAccount, VMImage: cfg.GCPVMImage, ContainerImage: cfg.GCPContainerImage, WorkerSecret: cfg.GCPWorkerSecret}
		for _, runtimeName := range []string{"vllm", "sglang", "custom-oci"} {
			elasticBackends = append(elasticBackends, workflows.ReplicaBackend{Name: "gcp-compute", Cloud: "gcp", Runtime: runtimeName, Default: true, Profile: gcpProfile, Provider: gcpProvider})
		}
	}
	if cfg.KubernetesEnabled() {
		kubernetesProfile, profileErr := integrationRegistry.Provider("kubernetes")
		if profileErr != nil {
			return fmt.Errorf("configure Kubernetes integration: %w", profileErr)
		}
		kubernetesProvider := provision.Kubernetes{Context: cfg.KubernetesContext, Namespace: cfg.KubernetesNamespace, WorkloadAPI: cfg.KubernetesWorkloadAPI, ServiceAccount: cfg.KubernetesServiceAccount, WorkerSecretName: cfg.KubernetesWorkerSecretName, WorkerSecretKey: cfg.KubernetesWorkerSecretKey, ImageDigest: cfg.KubernetesImageDigest, GPUResource: cfg.KubernetesGPUResource, GPUProductLabel: cfg.KubernetesGPUProductLabel}
		for _, runtimeName := range []string{"vllm", "sglang", "custom-oci"} {
			elasticBackends = append(elasticBackends, workflows.ReplicaBackend{Name: "kubernetes", Cloud: "kubernetes", Runtime: runtimeName, Profile: kubernetesProfile, Provider: kubernetesProvider})
		}
	}
	if cfg.DynamoEnabled() {
		dynamoProfile, profileErr := integrationRegistry.Provider("kubernetes-dynamo")
		if profileErr != nil {
			return fmt.Errorf("configure Dynamo integration: %w", profileErr)
		}
		dynamoProvider := provision.KubernetesDynamo{
			Context: cfg.KubernetesContext, Namespace: cfg.KubernetesNamespace,
			ServiceAccount:  cfg.KubernetesServiceAccount,
			VLLMImageDigest: cfg.DynamoVLLMImageDigest, VLLMRuntimeVersion: cfg.DynamoVLLMRuntimeVersion,
			SGLangImageDigest: cfg.DynamoSGLangImageDigest, SGLangRuntimeVersion: cfg.DynamoSGLangRuntimeVersion,
			ModelSecretName: cfg.DynamoModelSecretName, GPUResource: cfg.KubernetesGPUResource,
			GPUProductLabel: cfg.KubernetesGPUProductLabel,
		}
		for _, runtimeName := range []string{"vllm", "sglang"} {
			if !cfg.DynamoRuntimeEnabled(runtimeName) {
				continue
			}
			elasticBackends = append(elasticBackends, workflows.ReplicaBackend{Name: "kubernetes-dynamo", Cloud: "kubernetes", Runtime: runtimeName, Profile: dynamoProfile, Provider: dynamoProvider})
		}
	}
	replicaBackends, err := workflows.NewReplicaBackends(elasticBackends...)
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
	if asyncService != nil {
		go func() {
			if err := asyncService.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("async inference worker stopped", "error", err)
			}
		}()
	}
	autoscaler := autoscale.Controller{Repository: s, Signals: autoscale.VLLMSignals{Targets: s, Client: client, APIKey: cfg.APIKey}, Fleet: s}
	go runAutoscaler(ctx, autoscaler, cfg.HealthInterval, logger)
	gatewayTelemetry := &gateway.Telemetry{Extra: func(w io.Writer) {
		operationTelemetry.WritePrometheus(w)
		recorder.WritePrometheus(w)
	}}
	serverTLS, err := serverTLSConfig(cfg)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), Handler: (&gateway.Gateway{Routes: directory, APIKey: cfg.APIKey, Authenticator: credentialCache, Recorder: recorder, Logger: logger, Client: client, Ready: s.Ping, Control: control, Telemetry: gatewayTelemetry, CapacityObservers: map[string]gateway.CapacityObserver{"runpod": serverless.ActiveWorkers}, ExternalAuthorizer: externalBudgets, RequestAuthorizer: requestQuotas, AdmissionAuthorizer: admissionPool, ContextPassports: contextPassports}).Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20, TLSConfig: serverTLS}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	scheme := "http"
	if cfg.TLSCertFile != "" {
		scheme = "https"
	}
	fmt.Printf("InferCrane gateway listening on %s://%s/v1\n", scheme, server.Addr)
	if cfg.TLSCertFile != "" {
		err = server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func controlHTTPClient(cfg config.Config, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.ClientTLSCAFile != "" || cfg.ClientTLSCertFile != "" {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
		if cfg.ClientTLSCAFile != "" {
			body, err := os.ReadFile(cfg.ClientTLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("read control-plane CA: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(body) {
				return nil, errors.New("control-plane CA file contains no certificates")
			}
			tlsConfig.RootCAs = pool
		}
		if cfg.ClientTLSCertFile != "" {
			certificate, err := tls.LoadX509KeyPair(cfg.ClientTLSCertFile, cfg.ClientTLSKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load control-plane client identity: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{certificate}
		}
		transport.TLSClientConfig = tlsConfig
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func serverTLSConfig(cfg config.Config) (*tls.Config, error) {
	if cfg.TLSCertFile == "" {
		return nil, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if cfg.TLSClientCAFile == "" {
		return tlsConfig, nil
	}
	body, err := os.ReadFile(cfg.TLSClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read TLS client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, errors.New("TLS client CA file contains no certificates")
	}
	tlsConfig.ClientCAs = pool
	tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	return tlsConfig, nil
}

type controlPlaneUnregisterer interface {
	UnregisterControlPlaneInstance(context.Context, string) error
}

func unregisterControlPlaneInstance(ctx context.Context, membership controlPlaneUnregisterer, instanceID string, retryInterval time.Duration) error {
	if retryInterval <= 0 {
		return errors.New("positive membership withdrawal retry interval is required")
	}
	var lastErr error
	for {
		if err := membership.UnregisterControlPlaneInstance(ctx, instanceID); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("withdraw control-plane membership after retry: %w", errors.Join(lastErr, ctx.Err()))
		case <-timer.C:
		}
	}
}

func heartbeatControlPlane(ctx context.Context, s *store.Store, instanceID string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.HeartbeatControlPlaneInstance(ctx, instanceID); err != nil && ctx.Err() == nil {
				slog.Error("control-plane membership heartbeat failed", "instance_id", instanceID, "error", err)
			}
		}
	}
}

func refreshContextPassports(ctx context.Context, s *store.Store, directory *contextpassport.Directory) error {
	rows, err := s.ActiveContextPassports(ctx, time.Now().UTC(), 10000)
	if err != nil {
		return err
	}
	hints := make([]contextpassport.Hint, 0, len(rows))
	for _, row := range rows {
		subject := row.DeploymentName
		if subject == "" {
			subject = row.EndpointID
		}
		if subject == "" {
			continue
		}
		hints = append(hints, contextpassport.Hint{ID: row.ID, TenantID: row.TenantID, SubjectID: subject, PreferredBindingID: row.PreferredBindingID, PreferredTargetID: row.PreferredTargetID, ExpiresAt: row.ExpiresAt})
	}
	directory.Replace(hints)
	return nil
}

func runContextPassportRefresh(ctx context.Context, s *store.Store, directory *contextpassport.Directory, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, interval)
			err := refreshContextPassports(refreshCtx, s, directory)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.Error("Context Passport snapshot refresh failed", "error", err)
			}
		}
	}
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
		for {
			deleted, err := s.PurgeOperationalMeasurements(ctx, time.Now().Add(-retention), 10000)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("purge operational measurements", "error", err)
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
