package main

import (
	"context"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type commandSpec struct {
	use, short, group string
	aliases           []string
}

var publicCommandSpecs = []commandSpec{
	{use: "login [flags]", short: "Sign in through the dashboard in your browser", group: "start"},
	{use: "logout [flags]", short: "Sign out and revoke this CLI's key", group: "start"},
	{use: "init [flags]", short: "Connect this CLI to a control plane with an existing key", group: "start"},
	{use: "doctor [flags]", short: "Validate configuration and dependencies", group: "start"},
	{use: "discover local [flags]", short: "Inspect local GPUs without changing the host", group: "start"},
	{use: "plan MODEL [flags]", short: "Preview deployment changes without side effects", group: "start"},
	{use: "deploy MODEL [flags]", short: "Create a durable inference deployment", group: "start"},
	{use: "workload ACTION [PATH] [flags]", short: "Initialize, validate, build, develop, and deploy inference projects", group: "start"},
	{use: "evaluation ACTION [arguments]", short: "Sign, verify, attach, and inspect semantic quality evidence", group: "understand"},
	{use: "agent run|detect|create|list|revoke [flags]", short: "Connect model servers on this machine through an outbound-only agent", group: "start"},
	{use: "connect URL --as NAME [flags]", short: "Connect an existing inference endpoint", group: "start"},
	{use: "adopt endpoint|promote NAME [flags]", short: "Connect an existing inference workload", group: "start"},
	{use: "alert ACTION ENDPOINT [flags]", short: "Configure and evaluate signed webhook alerts", group: "operate"},
	{use: "admission ACTION ENDPOINT [flags]", short: "Bound endpoint concurrency and queueing", group: "operate"},
	{use: "async ACTION SUBJECT [flags]", short: "Submit and resume durable encrypted inference", group: "operate"},
	{use: "ui", short: "Open the interactive operations workspace", group: "operate"},
	{use: "mcp", short: "Serve read-only operational evidence over MCP stdio", group: "understand"},
	{use: "observe ENDPOINT_OR_DEPLOYMENT [flags]", short: "See health, traffic, operations, Guard evidence, and recent events", group: "operate"},
	{use: "telemetry collect dcgm|amd DEPLOYMENT [flags]", short: "Collect bounded GPU evidence from DCGM or AMD's exporter through the control plane", group: "understand"},
	{use: "profile ENDPOINT [flags]", short: "Capture and analyze a GPU profile: where GPU time goes and why", group: "understand"},
	{use: "inbox [flags]", short: "Rank fleet state that needs operator attention", group: "operate"},
	{use: "request ENDPOINT [flags]", short: "Send a buffered or streaming inference request", group: "start"},
	{use: "version [flags]", short: "Print the InferCrane version", group: "start"},
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
	{use: "route DEPLOYMENT --strategy STRATEGY [flags]", short: "Change replica routing policy", group: "operate"},
	{use: "delete DEPLOYMENT [flags]", short: "Preview or execute safe deletion", group: "operate"},
	{use: "inspect DEPLOYMENT [flags]", short: "Show raw deployment and infrastructure details", group: "understand"},
	{use: "explain [TOPIC] DEPLOYMENT [flags]", short: "Explain persisted operational decisions", group: "understand"},
	{use: "benchmark DEPLOYMENT [flags]", short: "Run and persist a reproducible benchmark", group: "understand"},
	{use: "replay DEPLOYMENT [flags]", short: "Capture a privacy-preserving production workload shape", group: "understand"},
	{use: "migrate capture|price|verdicts|canary|primary|rollback ENDPOINT [flags]", short: "Prove an open model you own against your closed API, then switch in stages", group: "understand"},
	{use: "capacity [flags]", short: "Inspect observed capacity reliability", group: "understand"},
	{use: "artifact ACTION ARTIFACT_ID [flags]", short: "Inspect cache evidence and request provider-native prefetch", group: "understand"},
	{use: "sandbox ACTION [arguments]", short: "Compose external sandboxes with expiring endpoint access", group: "operate"},
	{use: "training ACTION [arguments]", short: "Verify and attach immutable external training artifacts", group: "operate"},
	{use: "finops DEPLOYMENT | finops collect opencost DEPLOYMENT [flags]", short: "Collect and report attributed inference cost evidence", group: "understand"},
	{use: "autopilot ACTION SUBJECT [flags]", short: "Create and approve advisory serving plans", group: "operate"},
	{use: "session ACTION SUBJECT [flags]", short: "Manage durable logical session identity", group: "operate"},
	{use: "burst DEPLOYMENT [flags]", short: "Evaluate policy-bounded overflow", group: "operate"},
	{use: "recipe create DEPLOYMENT [flags]", short: "Capture an immutable evidence-backed model recipe", group: "understand"},
	{use: "recipes [QUERY] [flags]", short: "Search immutable model recipes", group: "understand"},
	{use: "models [QUERY] | models inspect NAME | models check OWNER/NAME [flags]", short: "Explore reviewed models, or check any open Hugging Face model", group: "start"},
	{use: "lab MODEL_IDENTITY [flags]", short: "Compare persisted measured serving evidence", group: "understand"},
	{use: "optimize propose|create MODEL [flags] | optimize workload-profile --file FILE | optimize evidence ACTION --file FILE | optimize kernel-run --file FILE | optimize list|inspect|results|watch|report|approve|activate|cancel | optimize doctor", short: "Propose, measure, and roll out evidence-gated serving optimization runs", group: "understand"},
	{use: "passport ACTION [arguments]", short: "Issue, inspect, or verify signed release evidence", group: "understand"},
	{use: "recommend DEPLOYMENT [flags]", short: "Recommend a qualified configuration from persisted evidence", group: "understand"},
	{use: "slo ACTION DEPLOYMENT [flags]", short: "Inspect or set deterministic inference SLO policy", group: "operate"},
	{use: "operation list | operation [inspect] ID | operation watch ID | operation cancel ID", short: "List, inspect, resume, or cancel durable operations", group: "understand"},
	{use: "orphans [flags]", short: "List unmanaged provisioned resources", group: "understand"},
	{use: "integrations [list] [flags]", short: "Inspect registered and qualified integration capabilities", group: "understand"},
	{use: "system instances [flags]", short: "Inspect live control-plane HA membership", group: "understand"},
	{use: "target ACTION [arguments]", short: "Register or list existing inference targets", group: "admin"},
	{use: "provider ACTION [arguments]", short: "Connect and manage external inference APIs", group: "admin"},
	{use: "model-api publish TYPE --file PATH [flags]", short: "Publish immutable shared Model API contracts", group: "admin"},
	{use: "context ACTION [arguments]", short: "List, inspect, or select CLI contexts", group: "admin"},
	{use: "auth status|login|logout [flags]", short: "Show the authenticated control-plane identity, or sign in and out", group: "admin"},
	{use: "whoami [flags]", short: "Show who this CLI is signed in as", group: "admin"},
	{use: "open [endpoint|deployment|run|model|operation NAME] [flags]", short: "Open a dashboard page in the browser", group: "operate"},
	{use: "tenant ACTION [arguments]", short: "Manage isolated tenants", group: "admin"},
	{use: "principal ACTION [arguments]", short: "Manage scoped credentials", group: "admin"},
	{use: "secret ACTION [arguments]", short: "Manage workspace secrets: sealed values or references", group: "admin"},
	{use: "external ACTION [arguments]", short: "Govern explicit external fallback capacity", group: "operate"},
	{use: "serve", short: "Run the control plane and gateway", group: "admin"},
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
	"plan": `  # Common path: preview safe defaults
  infercrane plan meta-llama/Llama-3.1-8B-Instruct

  # Advanced path: preview a reviewable DeploymentSpec
  infercrane plan infercrane.yaml`,
	"deploy": `  # Common path: one durable deployment
  infercrane deploy meta-llama/Llama-3.1-8B-Instruct --wait

  # Attach existing registered capacity
  infercrane deploy meta-llama/Llama-3.1-8B-Instruct --targets gpu-a,gpu-b

  # Optional advanced serving backend
  infercrane deploy meta-llama/Llama-3.1-8B-Instruct --backend dynamo --gpu NVIDIA-L40S

  # Full reviewable configuration
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
  infercrane optimize propose acme/model --model-revision COMMIT --provider runpod-pods --gpu H200 --objective throughput --search recipes --allow-model-metadata-network

  # Check the optional estimator toolchain without provisioning a GPU
  infercrane optimize doctor

  # Show what a finished run changed in cost, speed, and quality, and save a shareable page
  infercrane optimize report RUN_ID --html result.html

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
  report    show what a finished run changed in cost, speed, and answer quality
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
			if containsHelpArgument(args) {
				return cmd.Help()
			}
			return runLegacy(ctx, append([]string{name}, args...))
		},
	}
	command.Long = commandLong[name]
	command.Example = commandExamples[name]
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
		boolFlag("wait", "follow durable deployment progress")
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
			boolFlag("wait", "follow durable progress to completion")
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
		stringFlag("gpu-resource", "nvidia.com/gpu", "device-plugin resource test setups request")
		stringFlag("gpu-product-label", "nvidia.com/gpu.product", "node label that names the accelerator")
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
		stringFlag("objective", "interactive", "interactive, latency, throughput, or cost-efficiency")
		stringFlag("profile", "", "benchmark workload profile")
		stringFlag("source", "auto", "auto, catalog, or aiconfigurator")
		stringFlag("target-concurrency", "", "observed or expected concurrent requests")
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
		boolFlag("allow-model-metadata-network", "allow reading public model metadata from Hugging Face for estimates and --search recipes")
		stringFlag("search", "", "find the best setup for your model by adding setups to measure; supported value: recipes")
		stringFlag("model-config", "", "local copy of the pinned config.json, so --search recipes works offline")
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
		boolFlag("wait", "follow durable progress to completion")
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
