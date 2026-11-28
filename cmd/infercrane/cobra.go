package main

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type commandSpec struct {
	use, short, section string
	aliases             []string
	// suggestFor lists words people type for this command, so a typo or a
	// habit from another CLI ("infercrane ps") points at it.
	suggestFor []string
}

// publicCommandSpecs is every command the CLI dispatches, in the order help
// lists it within its section. Everyday sections show in `infercrane help`;
// the rest show with `infercrane help --all`. Descriptions are short, start
// with a verb, and say what you get.
var publicCommandSpecs = []commandSpec{
	{use: "login [flags]", short: "Sign in through your browser", section: "start", suggestFor: []string{"signin", "setup"}},
	{use: "deploy MODEL [flags]", short: "Deploy a model and get an endpoint", section: "start", suggestFor: []string{"run", "create", "up"}},
	{use: "request ENDPOINT [flags]", short: "Send a test request to an endpoint", section: "start", suggestFor: []string{"chat", "curl", "invoke", "call"}},
	{use: "models [QUERY] | models ACTION [arguments] [flags]", short: "Browse models, or check any Hugging Face model", section: "start", suggestFor: []string{"model"}},
	{use: "doctor [flags]", short: "Check your setup and connection", section: "start"},

	{use: "deployments [flags]", short: "List your deployments", section: "deployments", aliases: []string{"ls"}, suggestFor: []string{"list", "ps", "deployment"}},
	{use: "endpoints [flags]", short: "List your endpoints", section: "deployments"},
	{use: "status DEPLOYMENT [flags]", short: "Show a deployment's health and traffic", section: "deployments"},
	{use: "logs DEPLOYMENT [flags]", short: "Stream what happens to a deployment", section: "deployments", suggestFor: []string{"log", "tail"}},
	{use: "rollout ACTION [arguments]", short: "Roll out or roll back a revision", section: "deployments", suggestFor: []string{"rollback", "promote", "release"}},
	{use: "delete DEPLOYMENT [flags]", short: "Preview and delete a deployment", section: "deployments", suggestFor: []string{"rm", "remove", "destroy", "down", "stop"}},

	{use: "optimize ACTION [arguments]", short: "Find a cheaper, faster setup and prove it", section: "optimize", suggestFor: []string{"optimise", "tune"}},
	{use: "benchmark DEPLOYMENT [flags]", short: "Measure a deployment's speed", section: "optimize", suggestFor: []string{"bench", "perf"}},
	{use: "profile ENDPOINT [flags]", short: "See where GPU time goes", section: "optimize", suggestFor: []string{"trace", "profiler"}},
	{use: "recommend DEPLOYMENT [flags]", short: "Suggest a better setup from measured results", section: "legacy"},
	{use: "migrate ACTION ENDPOINT [flags]", short: "Test your model against a closed API, then switch", section: "optimize", suggestFor: []string{"migration", "switch"}},

	{use: "inbox [flags]", short: "See what needs your attention", section: "observe", suggestFor: []string{"alerts", "issues"}},
	{use: "observe ENDPOINT_OR_DEPLOYMENT [flags]", short: "Watch health, traffic, and recent events", section: "observe", suggestFor: []string{"watch", "monitor", "top"}},
	{use: "explain [TOPIC] DEPLOYMENT [flags]", short: "Explain why InferCrane made a decision", section: "observe", suggestFor: []string{"why"}},
	{use: "finops DEPLOYMENT | finops collect opencost DEPLOYMENT [flags]", short: "Show what a deployment costs", section: "observe", suggestFor: []string{"cost", "costs", "billing", "spend"}},
	{use: "open [endpoint|deployment|run|model|operation NAME] [flags]", short: "Open the dashboard in your browser", section: "observe", aliases: []string{"dashboard"}, suggestFor: []string{"browse", "web", "console"}},
	{use: "ui", short: "Open the dashboard in this terminal", section: "observe", suggestFor: []string{"tui"}},

	{use: "whoami [flags]", short: "Show who you're signed in as", section: "config", suggestFor: []string{"me", "account"}},
	{use: "context ACTION [arguments]", short: "Switch between control planes", section: "config", suggestFor: []string{"ctx", "profile", "workspace"}},
	{use: "target ACTION [arguments]", short: "Add GPU servers and clusters you already run", section: "config", suggestFor: []string{"cluster", "node", "gpu"}},
	{use: "provider ACTION [arguments]", short: "Connect hosted model APIs", section: "config", suggestFor: []string{"providers"}},
	{use: "secret ACTION [arguments]", short: "Manage secrets", section: "config", suggestFor: []string{"secrets"}},
	{use: "principal ACTION [arguments]", short: "Manage API keys and service accounts", section: "config", suggestFor: []string{"token", "tokens", "key", "keys", "apikey"}},
	{use: "logout [flags]", short: "Sign out and revoke this CLI's key", section: "config", suggestFor: []string{"signout"}},
	{use: "version [flags]", short: "Show the CLI version", section: "config"},

	{use: "plan MODEL [flags]", short: "Preview a deployment without changing anything", section: "serving", suggestFor: []string{"preview", "diff"}},
	{use: "apply MODEL_OR_SPEC [flags]", short: "Make a deployment match a spec file", section: "serving"},
	{use: "connect URL --as NAME [flags]", short: "Put an endpoint you already run behind InferCrane", section: "serving"},
	{use: "adopt endpoint|promote NAME [flags]", short: "Take over a workload you already run", section: "serving"},
	{use: "agent run|detect|create|list|revoke [flags]", short: "Connect model servers on this machine, outbound only", section: "serving"},
	{use: "discover local [flags]", short: "List the GPUs on this machine without changing it", section: "serving"},
	{use: "workload ACTION [PATH] [flags]", short: "Build, run, and deploy a custom inference project", section: "serving"},
	{use: "endpoint ACTION [arguments]", short: "Create an endpoint and manage its serving plan", section: "serving"},
	{use: "environment ACTION [arguments]", short: "Manage endpoint environments", section: "serving"},
	{use: "logical-model ACTION [arguments]", short: "Give a model a stable name across versions", section: "serving"},
	{use: "events DEPLOYMENT [flags]", short: "List a deployment's events", section: "legacy"},
	{use: "route DEPLOYMENT --strategy STRATEGY [flags]", short: "Change how traffic is spread across replicas", section: "serving"},
	{use: "admission ACTION ENDPOINT [flags]", short: "Limit concurrent requests and queueing", section: "serving"},
	{use: "slo ACTION DEPLOYMENT [flags]", short: "Set latency and availability targets", section: "serving"},
	{use: "alert ACTION ENDPOINT [flags]", short: "Send signed webhook alerts", section: "serving"},
	{use: "autopilot ACTION SUBJECT [flags]", short: "Create and approve suggested serving plans", section: "legacy"},
	{use: "burst DEPLOYMENT [flags]", short: "Check when traffic can overflow to extra capacity", section: "legacy"},
	{use: "external ACTION [arguments]", short: "Control fallback to an external API", section: "serving"},
	{use: "async ACTION SUBJECT [flags]", short: "Submit long requests and fetch results later", section: "serving"},
	{use: "session ACTION SUBJECT [flags]", short: "Prefer one replica for a conversation", section: "legacy"},
	{use: "sandbox ACTION [arguments]", short: "Give sandboxes short-lived endpoint access", section: "serving"},
	{use: "operation list | operation [inspect] ID | operation watch ID | operation cancel ID", short: "Follow, resume, or cancel a long-running change", section: "serving", suggestFor: []string{"op", "ops", "operations"}},
	{use: "inspect DEPLOYMENT [flags]", short: "Show raw deployment and infrastructure details", section: "serving", suggestFor: []string{"describe", "show", "get"}},

	{use: "evaluation ACTION [arguments]", short: "Sign, verify, and attach answer-quality results", section: "evidence", suggestFor: []string{"eval", "evals"}},
	{use: "passport ACTION [arguments]", short: "Issue and verify signed release records", section: "evidence"},
	{use: "results ACTION [arguments]", short: "Keep measured results and rank setups from them", section: "evidence", suggestFor: []string{"result", "measurements"}},
	{use: "report proof RUN [RUN...] [flags]", short: "Print the signed before-and-after Proof Report of your runs", section: "evidence", suggestFor: []string{"reports", "proof"}},
	{use: "lab MODEL_IDENTITY [flags]", short: "Compare measured results for a model", section: "legacy"},
	{use: "recipe create DEPLOYMENT [flags]", short: "Save a deployment's proven setup", section: "legacy"},
	{use: "recipes [QUERY] [flags]", short: "Find saved setups", section: "legacy"},
	{use: "replay DEPLOYMENT [flags]", short: "Record your traffic's shape without its content", section: "evidence"},
	{use: "capacity [flags]", short: "Show how reliably GPU capacity was available", section: "evidence"},
	{use: "telemetry collect dcgm|amd DEPLOYMENT [flags]", short: "Collect GPU metrics from DCGM or AMD's exporter", section: "evidence"},
	{use: "artifact ACTION ARTIFACT_ID [flags]", short: "Check model caches and request prefetch", section: "evidence"},
	{use: "training ACTION [arguments]", short: "Verify and attach checkpoints trained elsewhere", section: "evidence"},
	{use: "orphans [flags]", short: "Find cloud resources InferCrane doesn't manage", section: "evidence"},
	{use: "integrations [list] [flags]", short: "List connected integrations and what they support", section: "evidence"},

	{use: "init [flags]", short: "Connect to a control plane with an existing key", section: "legacy"},
	{use: "auth status|login|logout [flags]", short: "Sign in, sign out, or show your identity", section: "legacy"},
	{use: "tenant ACTION [arguments]", short: "Manage isolated tenants", section: "platform"},
	{use: "model-api publish TYPE --file PATH [flags]", short: "Publish a shared Model API contract", section: "platform"},
	{use: "system instances [flags]", short: "List the running control-plane instances", section: "platform"},
	{use: "mcp", short: "Give AI agents read-only access over MCP", section: "platform"},
	{use: "serve", short: "Run the control plane and gateway", section: "platform", suggestFor: []string{"server"}},
}

var commandExamples = map[string]string{
	"agent": `  # Connect every model server on this machine (vLLM, SGLang, llama.cpp,
  # Ollama, LM Studio). Only outbound HTTPS; prompts never leave the machine.
  infercrane login
  infercrane agent run

  # A server started with an API key, observed under a chosen name
  VLLM_API_KEY=... infercrane agent run --server http://127.0.0.1:8000 --token-env VLLM_API_KEY --as qwen-local

  # Also record request timings: point your app at the local gateway
  infercrane agent run --gateway 127.0.0.1:4000

  # See what would be connected, without sending anything
  infercrane agent detect

  # Serve agent-managed endpoints inside your cluster and test new setups
  # there (normally through the Helm chart deploy/helm/infercrane-agent)
  infercrane agent run --server http://vllm:8000 --serve 0.0.0.0:8080 \
    --executor kubernetes --kubernetes-namespace models --worker-secret infercrane-worker/api-key \
    --allow-image vllm/vllm-openai`,
	"profile": `  # On the GPU host: record 10 s through the engine's own profiler, analyze, and record
  infercrane profile coder --engine-url http://127.0.0.1:8000 --trace-dir /profiles --duration 10s

  # Also sample dcgm-exporter during the capture, then open the timeline
  infercrane profile coder --engine-url http://127.0.0.1:8000 --trace-dir /profiles --dcgm-url http://127.0.0.1:9400/metrics --open

  # Analyze traces you already have (Kineto JSON, rocprofv3 CSV, or Nsight Systems CSV)
  infercrane profile coder --trace ./profiles --no-upload

  # The newest recorded verdict, the history, and before versus after
  infercrane profile coder
  infercrane profile coder --list
  infercrane profile coder --compare gpc-BEFORE,gpc-AFTER --output json`,
	"logs": `  # Follow the timeline in a terminal
  infercrane logs qwen-prod --follow

  # Machine-readable: JSON Lines, one event object per line
  infercrane logs qwen-prod --since 15m --output json`,
	"operation": `  # Find recent operations, newest first
  infercrane operation list

  # Resume following one after a disconnect
  infercrane operation watch OPERATION_ID`,
	"models": `  # Reviewed starting points
  infercrane models qwen

  # Check any open Hugging Face model, then deploy the setup it proposes
  infercrane models check microsoft/phi-4 --spec phi-4.yaml
  infercrane deploy phi-4.yaml --wait`,
	"plan": `  # See what a deployment would create, without changing anything
  infercrane plan Qwen/Qwen3-8B

  # Preview a spec file before you apply it
  infercrane plan infercrane.yaml`,
	"deploy": `  # Deploy a model and wait until its endpoint is ready
  infercrane deploy Qwen/Qwen3-8B --wait

  # Use GPU servers you already registered with infercrane target
  infercrane deploy Qwen/Qwen3-8B --targets gpu-a,gpu-b

  # Pick the GPU and serving backend yourself
  infercrane deploy Qwen/Qwen3-8B --backend dynamo --gpu NVIDIA-L40S

  # Deploy from a spec file you can review and keep in git
  infercrane deploy infercrane.yaml --idempotency-key production-r1`,
	"login": `  # Sign in through the dashboard; the terminal and the browser show the same code
  infercrane login

  # Over SSH: print the URL and code instead of opening a browser
  infercrane login --no-browser

  # A local or self-hosted control plane with its own dashboard
  infercrane login --url http://127.0.0.1:8080 --console-url http://127.0.0.1:3200`,
	"open": `  # Open the dashboard home, an endpoint, or an optimization run
  infercrane open
  infercrane open endpoint support-bot
  infercrane open run 4f1c2d9e`,
	"rollout": `  # Create a candidate revision from a complete DeploymentSpec
  infercrane rollout create production --file candidate.yaml --idempotency-key production-r2

  # Follow its durable provisioning operation
  infercrane rollout provision production REVISION_ID --wait

  # Roll out the candidate revision once Release Guard accepts it
  infercrane rollout promote production REVISION_ID --wait

  # Roll back to an earlier revision
  infercrane rollout rollback production REVISION_ID --reason "p95 latency regressed" --wait`,
	"migrate": `  # 1. Route your closed API through InferCrane and capture its traffic
  OPENAI_API_KEY=... infercrane connect https://api.openai.com/v1 --as support-bot --model gpt-4.1-mini --manage-traffic --token-env OPENAI_API_KEY
  infercrane migrate capture support-bot --binding primary --window 24h

  # 2. Record its price (list price or your contract)
  infercrane migrate price support-bot primary --authority list_price --input-usd 0.40 --cached-input-usd 0.10 --output-usd 1.60 --valid-until 2026-12-31

  # 3. Compare open models with it (graded eval set, at least 30 prompts)
  infercrane optimize create Qwen/Qwen3-8B --provider runpod --gpu H100 --migrate-from support-bot/primary --dialect openai --workload-fingerprint sha256:DIGEST --eval-set sha256:EVAL_DIGEST

  # 4. Switch in stages: 5% canary, then your deployment first with the closed API as fallback
  infercrane migrate verdicts RUN_ID
  infercrane migrate canary support-bot --verdict VERDICT_ID
  infercrane migrate primary support-bot
  infercrane migrate rollback support-bot`,
	"optimize": `  # One command: use pinned AIConfigurator when available, otherwise safe reviewed setups
  infercrane optimize propose Qwen/Qwen3-8B --provider aws --region eu-central-1 --gpu L40S --objective interactive

  # Find the best setup for any model pinned to a commit; reads its public config.json
  infercrane optimize propose acme/model --model-revision COMMIT --provider runpod-pods --gpu H200 --objective throughput --allow-model-metadata-network

  # Check the optional estimator toolchain without provisioning a GPU
  infercrane optimize doctor

  # Show what a finished run changed in cost, speed, and quality, and save a shareable page
  infercrane optimize report RUN_ID --html result.html

  # Save the signed report and check its signature offline
  infercrane optimize report RUN_ID --output json > report.json
  infercrane optimize report verify report.json --public-key OPERATOR_PUBLIC_KEY

  # Roll out the winning setup of a run
  infercrane optimize activate RUN_ID --candidate SETUP_ID

  # Inspect a remote-derived, content-free public workload profile
  infercrane optimize workload-profile --file chutes-profile.json

  # Advanced: bind the proposal to an observed content-free workload shape
  infercrane optimize propose Qwen/Qwen3-8B --provider kubernetes-dynamo --gpu NVIDIA-L40S --workload-fingerprint sha256:WORKLOAD_DIGEST --source aiconfigurator`,
}

// commandLong explains a command whose action names are the control API's
// verbs, in the words the dashboard uses.
var commandLong = map[string]string{
	"migrate": `A migration Proof measures your closed API (OpenAI, Anthropic, or any
OpenAI-compatible API) as the baseline, then compares open models you own with it
on cost, speed, and graded answer quality before anything switches.

  capture   capture the closed API's traffic shape (no prompts or answers stored)
  price     record what the closed API charges per token: list price or your contract
  verdicts  list the signed quality verdicts a migration run recorded
  canary    send at most 5% of the endpoint's traffic to your deployment
  primary   serve from your deployment first, with the closed API as fallback;
            Release Guard compares the two on the canary's live traffic
  rollback  go back to the closed API alone

Run the comparison with: infercrane optimize create MODEL --migrate-from ENDPOINT/BINDING`,
	"optimize": `An optimization run measures setups side by side on the same traffic and checks
answer quality. Nothing changes in production until you roll out a winner.

  propose   preview the setups a run would measure, without creating a run
  create    save a run and its setups; it waits for your spend approval
  list      list runs
  inspect   show one run and every setup in it
  approve   approve a spend cap and an expiry so the run can start
  watch     follow a run live
  results   show the measured comparison and the decision
  report    show what a finished run changed in cost, speed, and answer quality;
            report verify FILE checks the signature of a saved report
  activate  roll out the winning setup, named with --candidate SETUP_ID
  cancel    stop a run and remove its measurement servers`,
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
		Short:         "Run open models at the lowest cost that meets your targets",
		Long:          rootTagline,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          rootArgs,
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
	root.PersistentFlags().StringVar(&contextName, "context", "", "use another saved context for one command")
	root.PersistentFlags().BoolVar(&noColor, "no-color", false, "turn off colors")
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetVersionTemplate("infercrane {{.Version}}\n")
	root.SetUsageTemplate(usageTemplate)
	root.SuggestionsMinimumDistance = 2
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if cmd == root {
			renderRootHelp(cmd.OutOrStdout(), helpTheme(), root, false)
			return
		}
		if text := strings.TrimSpace(cmd.Long); text != "" {
			_, _ = io.WriteString(cmd.OutOrStdout(), text+"\n\n")
		} else if cmd.Short != "" {
			_, _ = io.WriteString(cmd.OutOrStdout(), cmd.Short+"\n\n")
		}
		_, _ = io.WriteString(cmd.OutOrStdout(), cmd.UsageString())
	})
	root.SetHelpCommand(newHelpCommand(root))
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
		Aliases:            spec.aliases,
		SuggestFor:         spec.suggestFor,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		ValidArgsFunction:  completionFor(name),
		RunE: func(cmd *cobra.Command, args []string) error {
			if containsHelpArgument(args) {
				return cmd.Help()
			}
			noteLegacyCommand(name, args)
			return runLegacy(ctx, append([]string{name}, args...))
		},
	}
	command.Long = commandLong[name]
	command.Example = commandExamples[name]
	if command.Example == "" {
		command.Example = moreExamples[name]
	}
	if replacement, legacy := legacyReplacements[name]; legacy {
		command.Long = legacyLong(spec.short, replacement)
	}
	command.Flags().BoolP("help", "h", false, "show this help")
	addHelpFlags(command, name)
	return command
}

func containsHelpArgument(args []string) bool {
	for _, arg := range args {
		if arg == "help" || arg == "-h" || arg == "-help" || arg == "--help" {
			return true
		}
	}
	return false
}

func addHelpFlags(command *cobra.Command, name string) {
	stringFlag := func(flag, value, help string) { command.Flags().String(flag, value, help) }
	boolFlag := func(flag, help string) { command.Flags().Bool(flag, false, help) }
	intFlag := func(flag string, value int, help string) { command.Flags().Int(flag, value, help) }
	switch name {
	case "init", "workload", "evaluation", "doctor", "connect", "adopt", "alert", "admission", "async", "plan", "deploy", "apply", "request", "deployments", "endpoints", "endpoint", "environment", "logical-model", "status", "events", "inspect", "explain", "benchmark", "replay", "migrate", "capacity", "artifact", "sandbox", "training", "finops", "autopilot", "session", "burst", "recipe", "recipes", "models", "lab", "optimize", "passport", "recommend", "slo", "delete", "orphans", "operation", "integrations", "observe", "telemetry", "profile", "inbox", "target", "provider", "model-api", "auth", "system", "secret", "external", "rollout", "route", "version", "login", "logout", "whoami":
		stringFlag("output", "human", "output format: human or json")
	case "logs":
		stringFlag("output", "human", "output format: human, or json for JSON Lines (one event object per line)")
	}
	switch name {
	case "login":
		stringFlag("url", "", "control-plane URL (default: the context's URL)")
		stringFlag("console-url", "", "dashboard URL that approves the sign-in")
		boolFlag("no-browser", "print the URL and code instead of opening a browser")
	case "open":
		boolFlag("no-browser", "print the URL instead of opening a browser")
	case "telemetry":
		stringFlag("url", "", "exporter Prometheus metrics URL (dcgm: http://127.0.0.1:9400/metrics, amd: http://127.0.0.1:5000/metrics)")
		stringFlag("file", "", "read a saved exporter Prometheus snapshot instead of a URL")
		stringFlag("selector", "", "comma-separated exact Prometheus label selectors")
		stringFlag("replica", "", "InferCrane replica identity represented by this snapshot")
		stringFlag("source", "", "bounded evidence source identity (dcgm_exporter or amd_device_metrics_exporter)")
		stringFlag("utilization-unit", "percent", "dcgm only: legacy DCGM_FI_DEV_GPU_UTIL input unit, percent or ratio")
		stringFlag("metric-prefix", "", "amd only: the exporter's MetricsFieldPrefix, if it sets one")
		stringFlag("nvlink-peak-gbps", "0", "declared per-GPU, per-direction NVLink peak in GB/s; enables NVLink saturation")
		stringFlag("pcie-peak-gbps", "0", "declared per-GPU, per-direction PCIe peak in GB/s; enables PCIe saturation")
		stringFlag("ttl", "2m", "maximum time this snapshot may be presented as fresh")
		stringFlag("interval", "0", "keep collecting at this interval (5s to 10m) until stopped; the TTL defaults to three intervals")
	case "profile":
		stringFlag("deployment", "", "deployment the capture belongs to (default: ENDPOINT)")
		stringFlag("engine-url", "", "serving engine base URL that has /start_profile and /stop_profile")
		stringFlag("trace-dir", "", "directory, readable here, where the engine writes its traces")
		stringFlag("engine-trace-dir", "", "the same directory as the engine sees it (default: --trace-dir)")
		stringFlag("engine", "auto", "auto, vllm, or sglang")
		stringFlag("engine-key-env", "", "environment variable that holds a bearer key for the engine")
		stringFlag("duration", "10s", "how long to record, 2s to 30s")
		stringFlag("flush-timeout", "5m", "how long to wait for the engine to write its trace")
		stringFlag("dcgm-url", "", "dcgm-exporter metrics URL to sample every second during the capture")
		stringFlag("dcgm-selector", "", "comma-separated exact label selectors for --dcgm-url")
		boolFlag("yes", "capture without asking, accepting that the replica slows while it runs")
		stringFlag("trace", "", "analyze an existing trace file or directory instead of capturing (repeatable)")
		stringFlag("format", "", "trace format for --trace: kineto, rocprofv3, or nsys (default: detect)")
		stringFlag("accelerator", "", "accelerator name when the trace does not say, for the roofline peak")
		stringFlag("peak-tflops", "0", "peak dense 16-bit tensor TFLOPS, when no datasheet peak is listed")
		stringFlag("peak-tbps", "0", "peak memory bandwidth in TB/s, with --peak-tflops")
		boolFlag("no-upload", "analyze locally without recording the capture")
		boolFlag("open", "open the capture's timeline in the dashboard")
		boolFlag("list", "list the endpoint's retained captures")
		stringFlag("compare", "", "compare two captures: BASE[,HEAD]; HEAD defaults to the newest")
		stringFlag("capture", "", "read this capture instead of the newest")
	case "workload":
		stringFlag("model", "", "Hugging Face model identity")
		stringFlag("recipe", "", "reviewed curated recipe name")
		stringFlag("profile", "", "reviewed serving profile")
		stringFlag("name", "", "deployment name")
		stringFlag("runtime", "vllm", "vllm or sglang")
		stringFlag("cloud", "runpod", "infrastructure provider")
		stringFlag("provider-adapter", "", "exact provider adapter")
		stringFlag("gpu", "L40S", "GPU type")
		intFlag("gpu-count", 1, "GPUs allocated to each runtime replica")
		stringFlag("region", "", "provider region")
		stringFlag("tag", "", "registry image tag")
		stringFlag("file", "Dockerfile", "Dockerfile path inside the project")
		stringFlag("platform", "", "target image platform")
		boolFlag("push", "push and record an immutable registry digest")
		boolFlag("force", "replace an existing project spec")
		boolFlag("wait", "wait and show progress until the deployment is ready")
		boolFlag("detach", "run a local workload in the background")
		intFlag("port", 8000, "local loopback port")
	case "evaluation":
		stringFlag("file", "", "input or output evidence file")
		stringFlag("result", "", "content-free evaluator result JSON")
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
		boolFlag("attach", "attach ingested evidence through the control-plane API")
	case "artifact":
		stringFlag("provider", "", "provider adapter")
		stringFlag("region", "", "provider region")
		stringFlag("location", "", "provider-native cache location")
		stringFlag("state", "unknown", "cache observation state")
		stringFlag("source", "operator", "observation source")
		stringFlag("ttl", "5m", "observation validity")
		stringFlag("idempotency-key", "", "stable safe-retry key")
	case "sandbox":
		stringFlag("provider", "", "external sandbox provider or adapter name")
		stringFlag("external-id", "", "externally owned sandbox identity")
		stringFlag("external-revision", "", "external sandbox template or revision")
		stringFlag("endpoint", "", "single endpoint the credential may invoke")
		stringFlag("ttl", "30m", "credential lifetime")
		boolFlag("yes", "confirm credential revocation without deleting the external sandbox")
	case "training":
		stringFlag("provider", "", "external training system")
		stringFlag("run", "", "external immutable run identity")
		stringFlag("repository", "", "checkpoint repository or approved URI")
		stringFlag("immutable-revision", "", "immutable checkpoint or registry version")
		stringFlag("digest", "", "checkpoint sha256 digest")
		stringFlag("base-model", "", "immutable base model identity")
		stringFlag("method", "", "training method")
		stringFlag("framework", "", "training framework")
		stringFlag("framework-version", "", "training framework version")
		stringFlag("dataset-fingerprint", "", "content-free dataset fingerprint")
		stringFlag("produced-at", "", "RFC3339 production time")
		stringFlag("key", "", "Ed25519 training-system private key")
		stringFlag("file", "", "signed handoff file")
	case "connect":
		stringFlag("as", "", "stable endpoint and logical model name")
		stringFlag("model", "", "physical upstream model; discovered when omitted")
		stringFlag("type", "auto", "auto, vllm, litellm, or openai-compatible")
		boolFlag("manage-traffic", "route requests through InferCrane after qualification")
		stringFlag("token-env", "", "environment variable holding the server's API key; sent once, stored encrypted, and used on routed requests")
	case "observe":
		boolFlag("watch", "refresh until interrupted")
		boolFlag("diagnose", "persist a fresh deterministic Doctor evaluation")
		intFlag("events", 5, "recent deployment events")
		stringFlag("window", "1h", "evidence window")
	case "inbox":
		intFlag("limit", 50, "maximum attention items")
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
		stringFlag("connection", "", "configured external provider connection")
		stringFlag("ownership", "lifecycle-managed", "binding ownership mode")
		stringFlag("external-adapter", "", "authenticated external adapter")
		stringFlag("secret-reference", "", "external credential reference ID")
		intFlag("request-limit", 0, "hard external request reservation limit")
		stringFlag("cost-limit-usd", "", "hard external USD reservation budget")
		stringFlag("max-request-cost-usd", "", "worst-case external USD per request")
		boolFlag("acknowledge-external-data", "acknowledge external data transmission")
		boolFlag("enable-external", "enable authenticated external binding")
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
		if name != "plan" {
			stringFlag("endpoint", "", "stable application endpoint name")
		}
		stringFlag("targets", "", "comma-separated existing target names")
		stringFlag("cloud", "", "provider cloud")
		stringFlag("backend", "", "serving backend; use dynamo for a managed Dynamo graph")
		stringFlag("provider-adapter", "", "exact provider adapter profile")
		stringFlag("gpu", "", "GPU type")
		intFlag("gpu-count", 1, "GPUs allocated to each runtime replica")
		stringFlag("region", "", "provider region")
		stringFlag("runtime", "vllm", "runtime engine")
		stringFlag("compute", "elastic", "compute mode: elastic or serverless")
		intFlag("min", 1, "minimum replicas")
		intFlag("max", 1, "maximum replicas")
		if name != "plan" {
			boolFlag("wait", "wait and show progress until it finishes")
			stringFlag("wait-timeout", "", "stop watching locally without cancelling the operation")
			stringFlag("idempotency-key", "", "stable safe-retry key")
			stringFlag("compute-connection", "", "deploy on your own RunPod account: a verified compute connection name or ID")
			boolFlag("infercrane-cloud", "deploy on InferCrane Cloud, paid from prepaid credits")
			stringFlag("stop-after", "1h", "with --infercrane-cloud: stop the deployment after this long (15m to 24h)")
			stringFlag("model-secret", "", "saved secret ID with the Hugging Face token for a private or gated model")
			stringFlag("model-revision", "", "model commit, or the sha256 manifest digest of imported s3:// or gs:// weights")
			stringFlag("adapter", "", "serve a LoRA adapter: NAME=OWNER/REPO@COMMIT or NAME=s3://BUCKET/PREFIX@sha256:DIGEST")
		}
	case "agent":
		stringFlag("output", "human", "output format: human or json (json needs --once)")
		stringFlag("server", "", "a model server URL (repeatable); default: look on ports 8000, 30000, 8080, 11434, 1234")
		stringFlag("token-env", "", "environment variable holding the local server's API key; it never leaves this machine")
		stringFlag("auth-header-env", "", "environment variable holding the local server's full Authorization header")
		stringFlag("model", "", "model to observe when a server lists several")
		stringFlag("as", "", "endpoint name when one server is observed")
		stringFlag("name", "", "agent name when a new agent key is created (default: this host's name)")
		stringFlag("gateway", "", "listen address of the local recording gateway, for example 127.0.0.1:4000")
		stringFlag("gateway-server", "", "server URL the gateway forwards to when several are observed")
		stringFlag("key-env", "INFERCRANE_AGENT_KEY", "environment variable holding the agent key")
		boolFlag("no-detect", "only observe servers named with --server")
		boolFlag("once", "detect, register, sync once, and exit")
		stringFlag("serve", "", "serve this agent's agent-managed endpoints inside your cloud on this address, for example 0.0.0.0:8080")
		stringFlag("executor", "", "test setups the control plane queues, in this cluster: kubernetes")
		stringFlag("kubernetes-context", "", "kubectl context for setup tests (default: this pod's service account)")
		stringFlag("kubernetes-namespace", "", "namespace test setups are started in")
		stringFlag("runtime-service-account", "infercrane-runtime", "service account of test setup pods")
		stringFlag("worker-secret", "", "Secret NAME/KEY mounted into test setup pods as their API key")
		stringFlag("worker-key-env", "INFERCRANE_WORKER_KEY", "environment variable holding that Secret's value")
		stringFlag("engine-ingress", "", "the only sources that may reach test setup pods: pods:KEY=VALUE, namespace:KEY=VALUE, address ranges, or none (default: $INFERCRANE_KUBERNETES_ENGINE_INGRESS)")
		stringFlag("gpu-resource", "nvidia.com/gpu", "device-plugin resource test setups request")
		stringFlag("gpu-product-label", "nvidia.com/gpu.product", "node label that names the accelerator")
		stringFlag("tolerations", "", `taints test setup pods tolerate, as JSON: [{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]`)
		stringFlag("allow-image", "", "image repository test setups may use (repeatable); nothing else runs")
		stringFlag("allow-accelerator", "", "accelerator test setups may request (repeatable); default any")
		stringFlag("eval-set", "", "a local eval set NAME=PATH for the answer check (repeatable); it never leaves this cluster")
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
		stringFlag("profile", "", "versioned benchmark workload profile")
		stringFlag("revision", "active", "active, candidate, or revision ID")
		command.Flags().Bool("streaming", true, "measure streaming responses; set false for buffered responses")
		stringFlag("ttft-slo-ms", "", "optional TTFT goodput threshold")
		stringFlag("tpot-slo-ms", "", "optional TPOT goodput threshold")
		stringFlag("latency-slo-ms", "", "optional end-to-end goodput threshold")
	case "replay":
		stringFlag("window", "24h", "production-shape capture window")
		intFlag("max-requests", 1000, "maximum persisted request observations")
		boolFlag("execute", "run an explicit AIPerf approximation of the captured shape")
		boolFlag("acknowledge-cost", "acknowledge that execution can consume provider capacity")
		stringFlag("revision", "candidate", "revision used by explicit execution")
	case "migrate":
		stringFlag("binding", "", "the closed API's binding (capture)")
		stringFlag("window", "", "capture window (default 24h) or Release Guard window for primary (default 1h)")
		intFlag("max-requests", 1000, "maximum captured requests")
		stringFlag("authority", "", "list_price or contract (price)")
		stringFlag("input-usd", "", "USD per 1M input tokens (price)")
		stringFlag("cached-input-usd", "", "USD per 1M cached input tokens (price)")
		stringFlag("output-usd", "", "USD per 1M output tokens (price)")
		stringFlag("valid-until", "", "date after which the price is no longer used (price)")
		stringFlag("source", "", "where the price comes from (price)")
		stringFlag("verdict", "", "signed PASS migration verdict (canary)")
		stringFlag("percent", "5", "share of traffic the canary sends to your deployment, at most 5")
	case "capacity":
		stringFlag("window", "720h", "observation window")
	case "finops":
		stringFlag("window", "720h", "cost evidence window")
		stringFlag("url", "http://127.0.0.1:9003/allocation", "OpenCost allocation API URL")
		stringFlag("file", "", "read a saved OpenCost allocation response")
		stringFlag("aggregate", "controller", "OpenCost allocation aggregation")
		stringFlag("allocation", "", "comma-separated exact allocation keys to import")
		stringFlag("currency", "", "explicit three-letter evidence currency")
		stringFlag("source", "opencost/allocation", "bounded cost evidence source")
		stringFlag("ttl", "1h", "maximum time this cost snapshot may be treated as fresh")
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
	case "optimize":
		stringFlag("model-secret", "", "saved secret ID with the Hugging Face token for a private or gated model")
		stringFlag("adapter", "", "serve a LoRA adapter on every setup: NAME=OWNER/REPO@COMMIT or NAME=s3://BUCKET/PREFIX@sha256:DIGEST")
		stringFlag("provider", "", "provider cloud or exact adapter")
		stringFlag("region", "", "exact provider region")
		stringFlag("gpu", "", "exact accelerator identity")
		stringFlag("runtimes", "", "comma-separated runtime allowlist")
		stringFlag("objective", "interactive", "interactive, latency, throughput, cost-efficiency, or useful-output")
		stringFlag("profile", "", "benchmark workload profile")
		stringFlag("source", "auto", "auto, catalog, or aiconfigurator")
		stringFlag("target-concurrency", "", "observed or expected concurrent requests; a run measures at this load, rounded up (1 to 512)")
		stringFlag("workload-fingerprint", "", "content-free observed workload fingerprint")
		stringFlag("aiconfigurator-python", "python3", "Python executable containing the pinned estimator tuple")
		stringFlag("max-ttft-p95-ms", "", "required measured p95 TTFT")
		stringFlag("max-tpot-p95-ms", "", "required measured p95 TPOT")
		stringFlag("max-error-rate", "", "maximum measured failed-request ratio")
		stringFlag("min-goodput", "", "minimum measured requests per second satisfying the SLO")
		stringFlag("min-output-tokens-second", "", "required measured output throughput")
		stringFlag("max-hourly-cost", "", "required sourced hourly cost")
		stringFlag("write-dir", "", "write each setup's DeploymentSpec into a new or empty directory")
		stringFlag("idempotency-key", "", "stable safe-retry key for run creation")
		stringFlag("target-deployment", "", "existing deployment to evolve")
		stringFlag("max-cost-usd", "", "hard maximum run spend for approval")
		stringFlag("expires-in", "1h", "paid-resource approval validity")
		stringFlag("candidate", "", "ID of the qualified setup that optimize activate rolls out")
		boolFlag("yes", "approve without the confirmation prompt")
		intFlag("max-candidates", 10, "maximum setups")
		intFlag("limit", 20, "maximum runs to list")
		boolFlag("include-simulated", "include simulated compatibility")
		boolFlag("allow-model-metadata-network", "allow reading the model's public config from Hugging Face to plan setups")
		stringFlag("model-config", "", "local copy of the pinned config.json, to plan setups offline")
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
	case "provider":
		stringFlag("adapter", "openrouter", providerAdapterHelp)
		stringFlag("url", "", "provider OpenAI-compatible base URL")
		stringFlag("model", "", "provider model identifier")
		stringFlag("secret-reference", "", "existing secret reference ID")
		stringFlag("from-env", "", "control-plane environment variable containing the provider credential")
	case "operation":
		stringFlag("wait-timeout", "", "stop watching locally without cancelling the operation")
		intFlag("limit", 20, "operation list: maximum operations, newest first (1-500)")
		stringFlag("before", "", "operation list: only operations created before this RFC3339 time")
	case "route":
		stringFlag("strategy", "", "round-robin, consistent-hash, power-of-two, or cache-aware")
	case "model-api":
		stringFlag("file", "", "JSON manifest containing one immutable Model API contract")
	case "rollout":
		stringFlag("file", "", "candidate DeploymentSpec YAML")
		boolFlag("wait", "wait and show progress until it finishes")
		stringFlag("wait-timeout", "", "stop watching locally without cancelling the operation")
		stringFlag("idempotency-key", "", "stable safe-retry key")
		stringFlag("reason", "", "persisted transition reason")
		stringFlag("model", "", "candidate model")
		stringFlag("runtime", "vllm", "candidate runtime")
		stringFlag("routing", "round-robin", "candidate routing strategy")
		intFlag("min", 1, "candidate minimum replicas")
		intFlag("max", 1, "candidate maximum replicas")
		stringFlag("cloud", "", "candidate provider cloud")
		stringFlag("provider-adapter", "", "candidate provider adapter")
		stringFlag("gpu", "", "candidate GPU")
		intFlag("gpu-count", 1, "candidate GPUs per replica")
		stringFlag("region", "", "candidate region")
		stringFlag("model-revision", "", "candidate model revision")
		stringFlag("runtime-version", "", "candidate runtime version")
		stringFlag("runtime-args", "", "comma-separated candidate runtime arguments")
		intFlag("requests", 20, "bounded validation requests per revision")
		intFlag("concurrency", 1, "bounded validation concurrency")
		boolFlag("acknowledge-validation-cost", "confirm explicit validation traffic and provider cost")
		boolFlag("enabled", "enable Release Guard")
		boolFlag("require-compatibility", "require comparable model/runtime evidence")
		boolFlag("require-synthetic", "require bounded AIPerf validation")
		boolFlag("require-quality", "require signed comparable semantic quality evidence")
		boolFlag("auto-rollback", "automatically roll back a rolled-out revision that Release Guard rejects")
		intFlag("minimum-requests", 0, "minimum measured requests per revision")
		stringFlag("max-cost-regression", "", "maximum sourced cost regression percent")
		stringFlag("minimum-quality-score", "", "minimum candidate semantic quality score")
		stringFlag("max-quality-regression", "", "maximum semantic quality regression percent")
		intFlag("auto-rollback-window", 0, "observation window after a rollout, in seconds")
		intFlag("validation-max-requests", 0, "hard validation request bound")
		intFlag("validation-max-concurrency", 0, "hard validation concurrency bound")
	}
}

func completionFor(command string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	flags := map[string][]string{
		"workload":    {"--model", "--recipe", "--profile", "--name", "--runtime", "--cloud", "--gpu", "--region", "--tag", "--file", "--platform", "--push", "--force", "--wait", "--detach", "--port", "--output"},
		"evaluation":  {"--file", "--result", "--key", "--suite", "--suite-version", "--evaluator", "--evaluator-version", "--score", "--passed", "--samples", "--artifact-digest", "--evaluated-at", "--attach", "--output"},
		"artifact":    {"--provider", "--region", "--location", "--state", "--source", "--ttl", "--idempotency-key", "--output"},
		"deploy":      {"--name", "--cloud", "--backend", "--provider-adapter", "--runtime", "--gpu", "--region", "--compute", "--min", "--max", "--model-secret", "--model-revision", "--adapter", "--wait", "--wait-timeout", "--idempotency-key", "--output"},
		"apply":       {"--name", "--cloud", "--backend", "--provider-adapter", "--runtime", "--gpu", "--region", "--compute", "--min", "--max", "--wait", "--wait-timeout", "--idempotency-key", "--output"},
		"plan":        {"--name", "--targets", "--cloud", "--backend", "--provider-adapter", "--runtime", "--gpu", "--region", "--compute", "--min", "--max", "--output"},
		"request":     {"--message", "--stream", "--output"},
		"status":      {"--watch", "--output"},
		"logs":        {"--follow", "--since", "--type", "--output"},
		"events":      {"--output"},
		"delete":      {"--plan", "--yes", "--wait", "--idempotency-key", "--output"},
		"migrate":     {"--binding", "--window", "--max-requests", "--authority", "--input-usd", "--cached-input-usd", "--output-usd", "--valid-until", "--source", "--verdict", "--percent", "--output"},
		"benchmark":   {"--requests", "--concurrency", "--input-tokens", "--output-tokens", "--random-seed", "--profile", "--revision", "--streaming", "--ttft-slo-ms", "--tpot-slo-ms", "--latency-slo-ms", "--output"},
		"recipe":      {"--name", "--version", "--benchmark", "--output"},
		"recipes":     {"--limit", "--output"},
		"models":      {"--revision", "--secret", "--context-tokens", "--concurrency", "--spec", "--from", "--kind", "--base-model", "--output"},
		"lab":         {"--objective", "--profile", "--max-ttft-p95-ms", "--workload-digest", "--output"},
		"optimize":    {"--model-secret", "--adapter", "--provider", "--region", "--gpu", "--runtimes", "--objective", "--profile", "--source", "--target-concurrency", "--workload-fingerprint", "--aiconfigurator-python", "--allow-model-metadata-network", "--search", "--model-config", "--max-ttft-p95-ms", "--max-tpot-p95-ms", "--max-error-rate", "--min-goodput", "--min-output-tokens-second", "--max-hourly-cost", "--include-simulated", "--max-candidates", "--write-dir", "--file", "--idempotency-key", "--target-deployment", "--limit", "--max-cost-usd", "--expires-in", "--candidate", "--yes", "--output", "--once", "--html"},
		"passport":    {"--revision", "--file", "--output"},
		"recommend":   {"--history", "--output"},
		"slo":         {"--ttft-p95", "--latency-p95", "--error-rate", "--output-tokens-second", "--hourly-cost", "--output"},
		"external":    {"--target", "--adapter", "--secret-reference", "--request-limit", "--cost-limit-usd", "--max-request-cost-usd", "--mode", "--queue-threshold", "--breach-intervals", "--recovery-intervals", "--cooldown-seconds", "--signal-max-age-seconds", "--acknowledge-external-data", "--enable", "--output"},
		"doctor":      {"--cloud", "--gpu", "--serverless", "--aws", "--gcp", "--kubernetes", "--output"},
		"operation":   {"--wait-timeout", "--limit", "--before", "--output"},
		"route":       {"--strategy", "--output"},
		"version":     {"--output"},
		"observe":     {"--watch", "--diagnose", "--events", "--window", "--output"},
		"inbox":       {"--limit", "--output"},
		"profile":     {"--deployment", "--engine-url", "--trace-dir", "--engine-trace-dir", "--engine", "--engine-key-env", "--duration", "--flush-timeout", "--dcgm-url", "--dcgm-selector", "--yes", "--trace", "--format", "--accelerator", "--peak-tflops", "--peak-tbps", "--no-upload", "--open", "--list", "--compare", "--capture", "--output"},
		"rollout":     {"--file", "--model", "--model-revision", "--runtime", "--runtime-version", "--runtime-args", "--cloud", "--provider-adapter", "--gpu", "--region", "--routing", "--min", "--max", "--reason", "--requests", "--concurrency", "--acknowledge-validation-cost", "--enabled", "--require-compatibility", "--require-synthetic", "--require-quality", "--auto-rollback", "--minimum-requests", "--max-cost-regression", "--minimum-quality-score", "--max-quality-regression", "--auto-rollback-window", "--validation-max-requests", "--validation-max-concurrency", "--idempotency-key", "--wait", "--wait-timeout", "--output"},
		"endpoint":    {"--model", "--environment", "--name", "--deployment", "--target", "--connection", "--ownership", "--policy", "--bindings", "--evaluate", "--window", "--disable", "--minimum-requests", "--max-ttft-regression", "--yes", "--output"},
		"provider":    {"--adapter", "--url", "--model", "--secret-reference", "--from-env", "--output"},
		"model-api":   {"--file", "--output"},
		"environment": {"--to", "--policy", "--idempotency-key", "--yes", "--output"},
	}
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		source, cancel := newCompletionSource(cmd)
		defer cancel()
		if name, flagPrefix, valuePrefix, ok := pendingFlag(cmd, args, toComplete); ok {
			values, known := completeFlagValue(source, command, args, name, valuePrefix)
			if !known {
				return nil, cobra.ShellCompDirectiveDefault
			}
			for index := range values {
				values[index] = flagPrefix + values[index]
			}
			return values, cobra.ShellCompDirectiveNoFileComp
		}
		if strings.HasPrefix(toComplete, "-") {
			// Cobra already offers the registered flags, with their help text.
			var extra []string
			for _, flag := range flags[command] {
				if cmd.Flags().Lookup(strings.TrimPrefix(flag, "--")) == nil {
					extra = append(extra, flag)
				}
			}
			return matching(extra, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
		return completePositional(source, command, positional(cmd, args), toComplete)
	}
}
