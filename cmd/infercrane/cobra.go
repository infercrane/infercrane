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
	specs := []commandSpec{
		{use: "init [flags]", short: "Connect this CLI to a control plane", group: "start"},
		{use: "doctor [flags]", short: "Validate configuration and dependencies", group: "start"},
		{use: "plan MODEL [flags]", short: "Preview deployment changes without side effects", group: "start"},
		{use: "deploy MODEL [flags]", short: "Create a durable inference deployment", group: "start"},
		{use: "ui", short: "Open the interactive operations workspace", group: "operate"},
		{use: "dashboard [flags]", short: "Open or print the browser operations dashboard", group: "operate"},
		{use: "request DEPLOYMENT [flags]", short: "Send a buffered or streaming inference request", group: "start"},
		{use: "version", short: "Print the InferCrane version", group: "start"},
		{use: "apply MODEL_OR_SPEC [flags]", short: "Declaratively converge a deployment", group: "operate"},
		{use: "deployments [flags]", short: "List logical deployments", group: "operate", aliases: []string{"ls"}},
		{use: "status DEPLOYMENT [flags]", short: "Inspect deployment health and traffic", group: "operate"},
		{use: "logs DEPLOYMENT [flags]", short: "Stream the durable operational timeline", group: "operate"},
		{use: "events DEPLOYMENT [flags]", short: "Show durable deployment events", group: "operate"},
		{use: "rollout ACTION [arguments]", short: "Inspect and control immutable revisions", group: "operate"},
		{use: "route DEPLOYMENT --strategy STRATEGY", short: "Change replica routing policy", group: "operate"},
		{use: "delete DEPLOYMENT [flags]", short: "Preview or execute safe deletion", group: "operate"},
		{use: "inspect DEPLOYMENT [flags]", short: "Show raw deployment and infrastructure details", group: "understand"},
		{use: "explain [TOPIC] DEPLOYMENT [flags]", short: "Explain persisted operational decisions", group: "understand"},
		{use: "benchmark DEPLOYMENT [flags]", short: "Run and persist a reproducible benchmark", group: "understand"},
		{use: "passport ACTION [arguments]", short: "Issue, inspect, or verify signed release evidence", group: "understand"},
		{use: "recommend DEPLOYMENT [flags]", short: "Recommend a qualified configuration from persisted evidence", group: "understand"},
		{use: "slo ACTION DEPLOYMENT [flags]", short: "Inspect or set deterministic inference SLO policy", group: "operate"},
		{use: "operation ID | operation watch ID | operation cancel ID", short: "Inspect, resume, or cancel a durable operation", group: "understand"},
		{use: "orphans [flags]", short: "List unmanaged provisioned resources", group: "understand"},
		{use: "integrations [flags]", short: "Inspect registered and qualified integration capabilities", group: "understand"},
		{use: "target ACTION [arguments]", short: "Register or list existing inference targets", group: "admin"},
		{use: "context ACTION [arguments]", short: "List, inspect, or select CLI contexts", group: "admin"},
		{use: "auth status [flags]", short: "Show the authenticated control-plane identity", group: "admin"},
		{use: "tenant ACTION [arguments]", short: "Manage isolated tenants", group: "admin"},
		{use: "principal ACTION [arguments]", short: "Manage scoped credentials", group: "admin"},
		{use: "secret ACTION [arguments]", short: "Manage reference-only secrets", group: "admin"},
		{use: "external ACTION [arguments]", short: "Govern explicit external fallback capacity", group: "operate"},
		{use: "serve", short: "Run the control plane and gateway", group: "admin"},
	}
	for _, spec := range specs {
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
	case "init", "doctor", "plan", "deploy", "apply", "request", "deployments", "status", "logs", "events", "inspect", "explain", "benchmark", "passport", "recommend", "slo", "delete", "orphans", "operation", "integrations", "dashboard":
		stringFlag("output", "human", "output format: human or json")
	}
	switch name {
	case "dashboard":
		boolFlag("open", "open the dashboard in the default browser")
	case "ui":
		boolFlag("read-only", "disable control-plane mutation actions")
	case "init":
		stringFlag("url", "http://127.0.0.1:8080", "control-plane URL")
		stringFlag("api-key", "", "existing control-plane credential")
		stringFlag("context", "default", "context name")
		boolFlag("skip-check", "store configuration without validating connectivity")
	case "doctor":
		boolFlag("cloud", "validate elastic provider dependencies")
		boolFlag("serverless", "validate serverless provider dependencies")
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
		intFlag("random-seed", 17, "reproduction seed")
		stringFlag("revision", "active", "active, candidate, or revision ID")
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
	}
}

func completionFor(command string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	flags := map[string][]string{
		"deploy":    {"--name", "--cloud", "--gpu", "--region", "--compute", "--min", "--max", "--wait", "--wait-timeout", "--idempotency-key", "--output"},
		"apply":     {"--name", "--cloud", "--gpu", "--region", "--compute", "--min", "--max", "--wait", "--wait-timeout", "--idempotency-key", "--output"},
		"plan":      {"--name", "--targets", "--cloud", "--gpu", "--region", "--compute", "--min", "--max", "--output"},
		"request":   {"--message", "--stream", "--output"},
		"status":    {"--watch", "--output"},
		"logs":      {"--follow", "--since", "--type", "--output"},
		"events":    {"--output"},
		"delete":    {"--plan", "--yes", "--wait", "--idempotency-key", "--output"},
		"benchmark": {"--requests", "--concurrency", "--random-seed", "--revision", "--output"},
		"passport":  {"--revision", "--file", "--output"},
		"recommend": {"--history", "--output"},
		"slo":       {"--ttft-p95", "--latency-p95", "--error-rate", "--output-tokens-second", "--hourly-cost", "--output"},
		"external":  {"--target", "--adapter", "--secret-reference", "--request-limit", "--cost-limit-usd", "--max-request-cost-usd", "--mode", "--queue-threshold", "--breach-intervals", "--recovery-intervals", "--cooldown-seconds", "--signal-max-age-seconds", "--acknowledge-external-data", "--enable", "--output"},
		"doctor":    {"--cloud", "--serverless", "--aws", "--kubernetes", "--output"},
		"operation": {"--wait-timeout", "--output"},
		"dashboard": {"--open", "--output"},
		"rollout":   {"--requests", "--concurrency", "--acknowledge-validation-cost", "--wait", "--wait-timeout", "--output"},
	}
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if strings.HasPrefix(toComplete, "-") {
			return flags[command], cobra.ShellCompDirectiveNoFileComp
		}
		switch command {
		case "request", "status", "logs", "events", "inspect", "explain", "benchmark", "passport", "recommend", "slo", "delete", "route", "rollout":
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
