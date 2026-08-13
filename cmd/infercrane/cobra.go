package main

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/spf13/cobra"
)

type commandSpec struct {
	use, short, group string
	aliases           []string
}

var publicCommandSpecs = []commandSpec{
	{use: "init [flags]", short: "Connect this CLI to a control plane", group: "start"},
	{use: "doctor [flags]", short: "Validate configuration and dependencies", group: "start"},
	{use: "plan MODEL [flags]", short: "Preview deployment changes without side effects", group: "start"},
	{use: "deploy MODEL [flags]", short: "Create a durable inference deployment", group: "start"},
	{use: "workload ACTION [PATH] [flags]", short: "Initialize, validate, build, develop, and deploy inference projects", group: "start"},
	{use: "evaluation ACTION [arguments]", short: "Sign, verify, attach, and inspect semantic quality evidence", group: "understand"},
	{use: "connect URL --as NAME [flags]", short: "Connect an existing inference endpoint", group: "start"},
	{use: "adopt endpoint|promote NAME [flags]", short: "Connect an existing inference workload", group: "start"},
	{use: "alert ACTION ENDPOINT [flags]", short: "Configure and evaluate signed webhook alerts", group: "operate"},
	{use: "admission ACTION ENDPOINT [flags]", short: "Bound endpoint concurrency and queueing", group: "operate"},
	{use: "async ACTION SUBJECT [flags]", short: "Submit and resume durable encrypted inference", group: "operate"},
	{use: "ui", short: "Open the interactive operations workspace", group: "operate"},
	{use: "dashboard [flags]", short: "Open or print the browser operations dashboard", group: "operate"},
	{use: "mcp", short: "Serve read-only operational evidence over MCP stdio", group: "understand"},
	{use: "observe ENDPOINT_OR_DEPLOYMENT [flags]", short: "See health, traffic, operations, Guard evidence, and recent events", group: "operate"},
	{use: "request DEPLOYMENT [flags]", short: "Send a buffered or streaming inference request", group: "start"},
	{use: "version", short: "Print the InferCrane version", group: "start"},
	{use: "apply MODEL_OR_SPEC [flags]", short: "Declaratively converge a deployment", group: "operate"},
	{use: "deployments [flags]", short: "List logical deployments", group: "operate", aliases: []string{"ls"}},
	{use: "endpoints [flags]", short: "List stable application endpoints", group: "operate"},
	{use: "endpoint ACTION [arguments]", short: "Create and manage stable endpoint serving plans", group: "operate"},
	{use: "environment ACTION [arguments]", short: "Manage endpoint environments", group: "admin"},
	{use: "logical-model ACTION [arguments]", short: "Manage stable logical model identities", group: "admin"},
	{use: "status DEPLOYMENT [flags]", short: "Inspect deployment health and traffic", group: "operate"},
	{use: "logs DEPLOYMENT [flags]", short: "Stream the durable operational timeline", group: "operate"},
	{use: "events DEPLOYMENT [flags]", short: "Show durable deployment events", group: "operate"},
	{use: "rollout ACTION [arguments]", short: "Inspect and control immutable revisions", group: "operate"},
	{use: "route DEPLOYMENT --strategy STRATEGY", short: "Change replica routing policy", group: "operate"},
	{use: "delete DEPLOYMENT [flags]", short: "Preview or execute safe deletion", group: "operate"},
	{use: "inspect DEPLOYMENT [flags]", short: "Show raw deployment and infrastructure details", group: "understand"},
	{use: "explain [TOPIC] DEPLOYMENT [flags]", short: "Explain persisted operational decisions", group: "understand"},
	{use: "benchmark DEPLOYMENT [flags]", short: "Run and persist a reproducible benchmark", group: "understand"},
	{use: "replay DEPLOYMENT [flags]", short: "Capture a privacy-preserving production workload shape", group: "understand"},
	{use: "capacity [flags]", short: "Inspect observed capacity reliability", group: "understand"},
	{use: "artifact ACTION ARTIFACT_ID [flags]", short: "Inspect cache evidence and request provider-native prefetch", group: "understand"},
	{use: "finops DEPLOYMENT [flags]", short: "Build an evidence-backed cost report", group: "understand"},
	{use: "autopilot ACTION SUBJECT [flags]", short: "Create and approve advisory serving plans", group: "operate"},
	{use: "session ACTION SUBJECT [flags]", short: "Manage durable logical session identity", group: "operate"},
	{use: "burst DEPLOYMENT [flags]", short: "Evaluate policy-bounded overflow", group: "operate"},
	{use: "recipe create DEPLOYMENT [flags]", short: "Capture an immutable evidence-backed model recipe", group: "understand"},
	{use: "recipes [QUERY] [flags]", short: "Search immutable model recipes", group: "understand"},
	{use: "lab MODEL_IDENTITY [flags]", short: "Compare persisted measured serving evidence", group: "understand"},
	{use: "passport ACTION [arguments]", short: "Issue, inspect, or verify signed release evidence", group: "understand"},
	{use: "recommend DEPLOYMENT [flags]", short: "Recommend a qualified configuration from persisted evidence", group: "understand"},
	{use: "slo ACTION DEPLOYMENT [flags]", short: "Inspect or set deterministic inference SLO policy", group: "operate"},
	{use: "operation ID | operation watch ID | operation cancel ID", short: "Inspect, resume, or cancel a durable operation", group: "understand"},
	{use: "orphans [flags]", short: "List unmanaged provisioned resources", group: "understand"},
	{use: "integrations [flags]", short: "Inspect registered and qualified integration capabilities", group: "understand"},
	{use: "system instances [flags]", short: "Inspect live control-plane HA membership", group: "understand"},
	{use: "target ACTION [arguments]", short: "Register or list existing inference targets", group: "admin"},
	{use: "context ACTION [arguments]", short: "List, inspect, or select CLI contexts", group: "admin"},
	{use: "auth status [flags]", short: "Show the authenticated control-plane identity", group: "admin"},
	{use: "tenant ACTION [arguments]", short: "Manage isolated tenants", group: "admin"},
	{use: "principal ACTION [arguments]", short: "Manage scoped credentials", group: "admin"},
	{use: "secret ACTION [arguments]", short: "Manage reference-only secrets", group: "admin"},
	{use: "external ACTION [arguments]", short: "Govern explicit external fallback capacity", group: "operate"},
	{use: "serve", short: "Run the control plane and gateway", group: "admin"},
}

func isPublicCommand(name string) bool {
	for _, spec := range publicCommandSpecs {
		if strings.Fields(spec.use)[0] == name {
			return true
		}
	}
	return false
}

func newRootCommand(ctx context.Context) *cobra.Command {
	var contextName string
	var noColor bool
	root := &cobra.Command{
		Use:           "infercrane",
		Short:         "Production inference without the platform engineering",
		Long:          "InferCrane operates durable, explainable inference deployments without hiding the infrastructure.",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			if noColor {
				if err := os.Setenv("NO_COLOR", "1"); err != nil {
					return err
				}
			}
			if contextName != "" {
				return os.Setenv("INFERCRANE_CONTEXT", contextName)
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&contextName, "context", "", "use a named CLI context")
	root.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable ANSI color output")
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.AddGroup(
		&cobra.Group{ID: "start", Title: "Start:"},
		&cobra.Group{ID: "operate", Title: "Operate:"},
		&cobra.Group{ID: "understand", Title: "Understand:"},
		&cobra.Group{ID: "admin", Title: "Administration:"},
	)
	for _, spec := range publicCommandSpecs {
		root.AddCommand(newLegacyCommand(ctx, spec))
	}
	return root
}

func newLegacyCommand(ctx context.Context, spec commandSpec) *cobra.Command {
	name := strings.Fields(spec.use)[0]
	command := &cobra.Command{
		Use:                spec.use,
		Short:              spec.short,
		GroupID:            spec.group,
		Aliases:            spec.aliases,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		ValidArgsFunction:  completionFor(name),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			return runLegacy(ctx, append([]string{name}, args...))
		},
	}
	addHelpFlags(command, name)
	return command
}

func addHelpFlags(command *cobra.Command, name string) {
	stringFlag := func(flag, value, help string) { command.Flags().String(flag, value, help) }
	boolFlag := func(flag, help string) { command.Flags().Bool(flag, false, help) }
	intFlag := func(flag string, value int, help string) { command.Flags().Int(flag, value, help) }
	switch name {
	case "init", "workload", "evaluation", "doctor", "connect", "adopt", "alert", "admission", "async", "plan", "deploy", "apply", "request", "deployments", "endpoints", "endpoint", "environment", "logical-model", "status", "logs", "events", "inspect", "explain", "benchmark", "replay", "capacity", "artifact", "finops", "autopilot", "session", "burst", "recipe", "recipes", "lab", "passport", "recommend", "slo", "delete", "orphans", "operation", "integrations", "dashboard", "observe", "target", "auth", "system", "secret", "external", "rollout":
		stringFlag("output", "human", "output format: human or json")
	}
	switch name {
	case "workload":
		stringFlag("model", "", "Hugging Face model identity")
		stringFlag("recipe", "", "reviewed curated recipe name")
		stringFlag("name", "", "deployment name")
		stringFlag("runtime", "vllm", "vllm or sglang")
		stringFlag("cloud", "runpod", "infrastructure provider")
		stringFlag("gpu", "L40S", "GPU type")
		stringFlag("region", "", "provider region")
		stringFlag("tag", "", "registry image tag")
		stringFlag("file", "Dockerfile", "Dockerfile path inside the project")
		stringFlag("platform", "", "target image platform")
		boolFlag("push", "push and record an immutable registry digest")
		boolFlag("force", "replace an existing project spec")
		boolFlag("wait", "follow durable deployment progress")
		boolFlag("detach", "run a local workload in the background")
		intFlag("port", 8000, "local loopback port")
	case "evaluation":
		stringFlag("file", "", "input or output evidence file")
		stringFlag("key", "", "Ed25519 evaluator private key")
		stringFlag("suite", "", "evaluation suite name")
		stringFlag("suite-version", "", "immutable suite version")
		stringFlag("evaluator", "", "evaluator name")
		stringFlag("evaluator-version", "", "evaluator version")
		stringFlag("score", "", "normalized quality score")
		stringFlag("artifact-digest", "", "private result artifact digest")
		stringFlag("evaluated-at", "", "RFC3339 evaluation time")
		intFlag("samples", 0, "evaluated sample count")
		boolFlag("passed", "external suite pass result")
	case "artifact":
		stringFlag("provider", "", "provider adapter")
		stringFlag("region", "", "provider region")
		stringFlag("location", "", "provider-native cache location")
		stringFlag("state", "unknown", "cache observation state")
		stringFlag("source", "operator", "observation source")
		stringFlag("ttl", "5m", "observation validity")
		stringFlag("idempotency-key", "", "stable safe-retry key")
	case "connect":
		stringFlag("as", "", "stable endpoint and logical model name")
		stringFlag("model", "", "physical upstream model; discovered when omitted")
		stringFlag("type", "auto", "auto, vllm, litellm, or openai-compatible")
		boolFlag("manage-traffic", "route requests through InferCrane after qualification")
	case "dashboard":
		boolFlag("open", "open the dashboard in the default browser")
	case "observe":
		boolFlag("watch", "refresh until interrupted")
		boolFlag("diagnose", "persist a fresh deterministic Doctor evaluation")
		intFlag("events", 5, "recent deployment events")
		stringFlag("window", "1h", "evidence window")
	case "adopt":
		stringFlag("url", "", "existing OpenAI-compatible base URL")
		stringFlag("model", "", "stable logical model name")
		stringFlag("upstream-model", "", "physical model exposed by the workload")
		stringFlag("source", "vllm", "source type: vllm or openai-compatible")
		stringFlag("ownership", "observe-only", "observe-only or traffic-managed")
		stringFlag("runtime", "vllm", "qualified runtime inspector")
	case "alert":
		stringFlag("name", "operations", "alert policy name")
		stringFlag("webhook", "", "HTTPS webhook URL")
		stringFlag("secret-reference", "", "signing secret reference ID")
		stringFlag("minimum-severity", "warning", "info, warning, or critical")
		intFlag("max-attempts", 3, "bounded delivery attempts")
	case "admission":
		intFlag("max-concurrency", 32, "maximum concurrently executing requests")
		intFlag("max-queue", 64, "maximum queued requests")
		intFlag("queue-timeout-ms", 5000, "maximum queue wait in milliseconds")
		intFlag("max-request-bytes", 16<<20, "maximum encoded request size")
		intFlag("max-output-tokens", 8192, "maximum requested output tokens")
		stringFlag("priorities", "normal", "comma-separated allowed priorities")
		intFlag("retry-budget", 0, "bounded inference retry budget")
		boolFlag("disabled", "store the policy without enforcing it")
	case "async":
		stringFlag("file", "", "protocol-native JSON request file")
		stringFlag("protocol", "chat", "request protocol")
		stringFlag("idempotency-key", "", "stable submission idempotency key")
		intFlag("priority", 0, "job priority from -100 to 100")
		intFlag("deadline-seconds", 900, "execution deadline")
		intFlag("retention-seconds", 86400, "encrypted result retention")
		stringFlag("webhook", "", "HTTPS completion webhook")
		stringFlag("webhook-secret-reference", "", "webhook signing secret reference")
	case "ui":
		boolFlag("read-only", "disable control-plane mutation actions")
	case "endpoint":
		stringFlag("model", "", "logical model name")
		stringFlag("environment", "production", "environment name")
		stringFlag("name", "primary", "binding name")
		stringFlag("deployment", "", "deployment-backed binding")
		stringFlag("target", "", "external target-backed binding")
		stringFlag("ownership", "lifecycle-managed", "binding ownership mode")
		stringFlag("policy", "manual", "serving-plan routing policy")
		stringFlag("bindings", "", "ordered NAME[:WEIGHT] bindings")
		boolFlag("evaluate", "evaluate endpoint Release Guard")
		stringFlag("window", "1h", "Release Guard telemetry window")
		boolFlag("disable", "disable endpoint Release Guard")
		boolFlag("yes", "confirm endpoint deletion")
		intFlag("minimum-requests", 0, "minimum requests per plan")
		stringFlag("max-ttft-regression", "", "maximum TTFT regression percent")
	case "environment":
		stringFlag("to", "", "destination endpoint for environment promotion")
		stringFlag("policy", "{}", "bounded environment policy JSON")
		stringFlag("idempotency-key", "", "stable safe-retry key")
		boolFlag("yes", "stage the destination candidate")
	case "init":
		stringFlag("url", "http://127.0.0.1:8080", "control-plane URL")
		stringFlag("api-key", "", "existing control-plane credential")
		stringFlag("context", "default", "context name")
		boolFlag("skip-check", "store configuration without validating connectivity")
	case "doctor":
		boolFlag("cloud", "validate elastic provider dependencies")
		boolFlag("serverless", "validate serverless provider dependencies")
		stringFlag("window", "1h", "persisted endpoint evidence window")
	case "plan", "deploy", "apply":
		stringFlag("name", "", "logical deployment name")
		stringFlag("targets", "", "comma-separated existing target names")
		stringFlag("cloud", "", "provider cloud")
		stringFlag("gpu", "", "GPU type")
		stringFlag("region", "", "provider region")
		stringFlag("compute", "elastic", "compute mode: elastic or serverless")
		intFlag("min", 1, "minimum replicas")
		intFlag("max", 1, "maximum replicas")
		if name != "plan" {
			boolFlag("wait", "follow durable progress to completion")
			stringFlag("wait-timeout", "", "stop watching locally without cancelling the operation")
			stringFlag("idempotency-key", "", "stable safe-retry key")
		}
	case "request":
		stringFlag("message", "Say hello in one sentence.", "user message")
		stringFlag("protocol", "chat", "chat, responses, embeddings, completions, or batch")
		boolFlag("stream", "stream response text as it arrives")
	case "status":
		boolFlag("watch", "refresh until interrupted")
	case "logs":
		boolFlag("follow", "continue streaming new events")
		stringFlag("since", "", "show events newer than this duration")
		stringFlag("type", "", "filter by event type or prefix")
	case "delete":
		boolFlag("plan", "preview deletion without mutation")
		boolFlag("yes", "confirm destructive deletion")
		boolFlag("wait", "follow provider cleanup")
		stringFlag("idempotency-key", "", "stable safe-retry key")
	case "benchmark":
		intFlag("requests", 100, "request count")
		intFlag("concurrency", 10, "concurrent requests")
		intFlag("input-tokens", 128, "mean input tokens")
		intFlag("output-tokens", 32, "maximum output tokens")
		intFlag("random-seed", 17, "reproduction seed")
		stringFlag("revision", "active", "active, candidate, or revision ID")
	case "replay":
		stringFlag("window", "24h", "production-shape capture window")
		intFlag("max-requests", 1000, "maximum persisted request observations")
		boolFlag("execute", "run an explicit AIPerf approximation of the captured shape")
		boolFlag("acknowledge-cost", "acknowledge that execution can consume provider capacity")
		stringFlag("revision", "candidate", "revision used by explicit execution")
	case "capacity":
		stringFlag("window", "720h", "observation window")
	case "finops":
		stringFlag("window", "720h", "cost evidence window")
	case "autopilot":
		stringFlag("objective", "minimize_cost", "advisory objective")
	case "session":
		stringFlag("ttl", "1h", "logical session expiry")
		stringFlag("preferred-binding", "", "best-effort binding hint")
		stringFlag("preferred-target", "", "best-effort target hint")
	case "burst":
		intFlag("queue-depth", 0, "observed queue depth")
		intFlag("breaches", 1, "consecutive breach intervals")
		intFlag("incremental-cost-microusd-hour", 0, "sourced incremental hourly cost")
		intFlag("max-incremental-cost-microusd-hour", 1, "hard incremental hourly budget")
		boolFlag("external-healthy", "fresh qualified overflow health evidence")
	case "recipe":
		stringFlag("name", "", "stable recipe name")
		stringFlag("version", "", "immutable recipe version")
		stringFlag("benchmark", "", "specific measured benchmark ID")
	case "recipes":
		intFlag("limit", 20, "maximum results")
	case "lab":
		stringFlag("max-ttft-p95-ms", "", "optional p95 TTFT SLO")
		stringFlag("workload-digest", "", "optional exact benchmark workload SHA-256")
	case "passport":
		stringFlag("revision", "", "revision ID; defaults to active")
		stringFlag("file", "", "write the issued passport JSON to a file")
	case "recommend":
		boolFlag("history", "list persisted recommendation history without evaluating")
	case "slo":
		stringFlag("ttft-p95", "", "maximum p95 time to first token in milliseconds")
		stringFlag("latency-p95", "", "maximum p95 request latency in milliseconds")
		stringFlag("error-rate", "", "maximum request error ratio from 0 to 1")
		stringFlag("output-tokens-second", "", "minimum output-token throughput")
		stringFlag("hourly-cost", "", "maximum sourced hourly cost")
	case "external":
		stringFlag("target", "", "attached external target name")
		stringFlag("adapter", "openrouter", "external adapter")
		stringFlag("secret-reference", "", "secret reference ID")
		intFlag("request-limit", 0, "hard request reservation limit")
		stringFlag("cost-limit-usd", "", "hard USD reservation budget")
		stringFlag("max-request-cost-usd", "", "worst-case USD per request")
		stringFlag("mode", "health", "health or health_and_queue")
		stringFlag("queue-threshold", "", "waiting-request threshold")
		intFlag("breach-intervals", 2, "consecutive breaches")
		intFlag("recovery-intervals", 2, "consecutive recovery observations")
		intFlag("cooldown-seconds", 60, "route-change cooldown")
		intFlag("signal-max-age-seconds", 30, "maximum queue evidence age")
		boolFlag("acknowledge-external-data", "acknowledge external data transmission")
		boolFlag("enable", "enable policy")
	case "operation":
		stringFlag("wait-timeout", "", "stop watching locally without cancelling the operation")
	case "rollout":
		intFlag("requests", 20, "bounded validation requests per revision")
		intFlag("concurrency", 1, "bounded validation concurrency")
		boolFlag("acknowledge-validation-cost", "confirm explicit validation traffic and provider cost")
		boolFlag("require-quality", "require signed comparable semantic quality evidence")
		stringFlag("minimum-quality-score", "", "minimum candidate semantic quality score")
		stringFlag("max-quality-regression", "", "maximum semantic quality regression percent")
	}
}

func completionFor(command string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	flags := map[string][]string{
		"workload":    {"--model", "--recipe", "--name", "--runtime", "--cloud", "--gpu", "--region", "--tag", "--file", "--platform", "--push", "--force", "--wait", "--detach", "--port", "--output"},
		"evaluation":  {"--file", "--key", "--suite", "--suite-version", "--evaluator", "--evaluator-version", "--score", "--passed", "--samples", "--artifact-digest", "--evaluated-at", "--output"},
		"artifact":    {"--provider", "--region", "--location", "--state", "--source", "--ttl", "--idempotency-key", "--output"},
		"deploy":      {"--name", "--cloud", "--gpu", "--region", "--compute", "--min", "--max", "--wait", "--wait-timeout", "--idempotency-key", "--output"},
		"apply":       {"--name", "--cloud", "--gpu", "--region", "--compute", "--min", "--max", "--wait", "--wait-timeout", "--idempotency-key", "--output"},
		"plan":        {"--name", "--targets", "--cloud", "--gpu", "--region", "--compute", "--min", "--max", "--output"},
		"request":     {"--message", "--stream", "--output"},
		"status":      {"--watch", "--output"},
		"logs":        {"--follow", "--since", "--type", "--output"},
		"events":      {"--output"},
		"delete":      {"--plan", "--yes", "--wait", "--idempotency-key", "--output"},
		"benchmark":   {"--requests", "--concurrency", "--input-tokens", "--output-tokens", "--random-seed", "--revision", "--output"},
		"recipe":      {"--name", "--version", "--benchmark", "--output"},
		"recipes":     {"--limit", "--output"},
		"lab":         {"--max-ttft-p95-ms", "--workload-digest", "--output"},
		"passport":    {"--revision", "--file", "--output"},
		"recommend":   {"--history", "--output"},
		"slo":         {"--ttft-p95", "--latency-p95", "--error-rate", "--output-tokens-second", "--hourly-cost", "--output"},
		"external":    {"--target", "--adapter", "--secret-reference", "--request-limit", "--cost-limit-usd", "--max-request-cost-usd", "--mode", "--queue-threshold", "--breach-intervals", "--recovery-intervals", "--cooldown-seconds", "--signal-max-age-seconds", "--acknowledge-external-data", "--enable", "--output"},
		"doctor":      {"--cloud", "--serverless", "--aws", "--gcp", "--kubernetes", "--output"},
		"operation":   {"--wait-timeout", "--output"},
		"dashboard":   {"--open", "--output"},
		"observe":     {"--watch", "--diagnose", "--events", "--window", "--output"},
		"rollout":     {"--requests", "--concurrency", "--acknowledge-validation-cost", "--require-quality", "--minimum-quality-score", "--max-quality-regression", "--wait", "--wait-timeout", "--output"},
		"endpoint":    {"--model", "--environment", "--name", "--deployment", "--target", "--ownership", "--policy", "--bindings", "--evaluate", "--window", "--disable", "--minimum-requests", "--max-ttft-regression", "--yes", "--output"},
		"environment": {"--to", "--policy", "--idempotency-key", "--yes", "--output"},
	}
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if strings.HasPrefix(toComplete, "-") {
			return flags[command], cobra.ShellCompDirectiveNoFileComp
		}
		switch command {
		case "endpoint":
			if len(args) == 0 {
				actions := []string{"list", "inspect", "create", "bind", "plan", "guard", "stage", "promote", "delete"}
				var values []string
				for _, action := range actions {
					if strings.HasPrefix(action, toComplete) {
						values = append(values, action)
					}
				}
				return values, cobra.ShellCompDirectiveNoFileComp
			}
			if args[0] == "list" || args[0] == "create" {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			cfg, err := config.LoadClient()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var response struct {
				Data []endpointView `json:"data"`
			}
			if err = controlJSON(cmd.Context(), cfg, http.MethodGet, "/api/v1/endpoints", "", nil, &response); err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			values := make([]string, 0, len(response.Data))
			for _, endpoint := range response.Data {
				if strings.HasPrefix(endpoint.Name, toComplete) {
					values = append(values, endpoint.Name+"\t"+endpoint.ObservedState)
				}
			}
			return values, cobra.ShellCompDirectiveNoFileComp
		case "request", "status", "observe", "logs", "events", "inspect", "explain", "benchmark", "passport", "recommend", "slo", "delete", "route", "rollout":
			cfg, err := config.LoadClient()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var response struct {
				Data []deploymentSummary `json:"data"`
			}
			if err = controlJSON(cmd.Context(), cfg, http.MethodGet, "/api/v1/deployments", "", nil, &response); err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			values := make([]string, 0, len(response.Data))
			for _, deployment := range response.Data {
				if strings.HasPrefix(deployment.Name, toComplete) {
					values = append(values, deployment.Name+"\t"+deployment.ObservedState)
				}
			}
			return values, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveDefault
	}
}
