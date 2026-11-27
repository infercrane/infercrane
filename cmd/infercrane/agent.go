package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/infercrane/infercrane/internal/engineingress"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/agent"
	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

const agentUsage = `usage:
  infercrane agent run [--server URL]... [--token-env VAR] [--model MODEL] [--as NAME] [--gateway ADDR] [--serve ADDR] [--executor kubernetes ...] [--name NAME] [--once] [--output human|json]
  infercrane agent detect [--server URL]... [--token-env VAR] [--output human|json]
  infercrane agent create NAME [--output human|json]
  infercrane agent list [--output human|json]
  infercrane agent revoke NAME_OR_ID [--output human|json]`

// agentOutput is where agent progress is written; tests replace it.
var agentOutput io.Writer = os.Stdout

// agentHardware is replaced in tests so they do not read the host.
var agentHardware = agent.HardwareProbe{}

func agentCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(agentUsage)
	}
	switch args[0] {
	case "run":
		return agentRunCommand(ctx, args[1:])
	case "detect":
		return agentDetectCommand(ctx, args[1:])
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		return agentCreateCommand(ctx, cfg, args[1:])
	case "list":
		return agentListCommand(ctx, cfg, args[1:])
	case "revoke":
		return agentRevokeCommand(ctx, cfg, args[1:])
	}
	return errors.New(agentUsage)
}

// localAuthHeader reads the local server's API key from an environment
// variable. The value is sent only to servers on this machine.
func localAuthHeader(tokenEnv, headerEnv string) (string, error) {
	if tokenEnv != "" && headerEnv != "" {
		return "", errors.New("use either --token-env or --auth-header-env")
	}
	if tokenEnv != "" {
		value := strings.TrimSpace(os.Getenv(tokenEnv))
		if value == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", tokenEnv)
		}
		return "Bearer " + value, nil
	}
	if headerEnv != "" {
		value := strings.TrimSpace(os.Getenv(headerEnv))
		if value == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", headerEnv)
		}
		return value, nil
	}
	return "", nil
}

type agentFlags struct {
	servers               stringList
	tokenEnv, headerEnv   string
	noDetect              bool
	output                string
	model, endpoint, name string
	gateway, gatewayFor   string
	keyEnv                string
	once                  bool
	serve, executor       string
	offlineGrace          time.Duration
	stateDir              string
	kubeContext, kubeNS   string
	runtimeSA             string
	workerSecret          string
	workerKeyEnv          string
	engineIngress         string
	gpuResource, gpuLabel string
	tolerations           []provision.KubernetesToleration
	allowImages           stringList
	allowAccelerators     stringList
	evalSets              stringList
}

func parseAgentFlags(name string, args []string, run bool) (agentFlags, error) {
	var flags agentFlags
	var tolerations string
	fs := newFlagSet(name)
	fs.Var(&flags.servers, "server", "a model server URL on this machine or network (repeatable); default: look on the usual local ports")
	fs.StringVar(&flags.tokenEnv, "token-env", "", "environment variable holding the API key of the --server servers, sent to them as a bearer token")
	fs.StringVar(&flags.headerEnv, "auth-header-env", "", "environment variable holding the full Authorization header for the local server")
	fs.BoolVar(&flags.noDetect, "no-detect", false, "only observe servers named with --server")
	fs.StringVar(&flags.output, "output", "human", "human or json")
	if run {
		fs.StringVar(&flags.model, "model", "", "model to observe when the server lists several")
		fs.StringVar(&flags.endpoint, "as", "", "endpoint name when one server is observed (default: from the model)")
		fs.StringVar(&flags.name, "name", "", "agent name when a new agent key is created (default: this host's name)")
		fs.StringVar(&flags.gateway, "gateway", "", "listen address of the local recording gateway, for example 127.0.0.1:4000")
		fs.StringVar(&flags.gatewayFor, "gateway-server", "", "server URL the gateway forwards to when several are observed")
		fs.StringVar(&flags.keyEnv, "key-env", "INFERCRANE_AGENT_KEY", "environment variable holding the agent key")
		fs.BoolVar(&flags.once, "once", false, "detect, register, sync once, and exit")
		fs.StringVar(&flags.serve, "serve", "", "serve this agent's agent-managed endpoints on this address, for example 0.0.0.0:8080")
		fs.DurationVar(&flags.offlineGrace, "offline-grace", 0, "with --serve: keep serving the endpoints' own servers this long past the data plane snapshot's validity while the control plane cannot be reached, at most 24h (default 24h, or $INFERCRANE_AGENT_OFFLINE_GRACE); a key revoked meanwhile keeps working until the agent reconnects or the grace ends; 0 refuses at the validity")
		fs.StringVar(&flags.stateDir, "state-dir", os.Getenv("INFERCRANE_AGENT_STATE_DIR"), "with --serve: keep the last data plane snapshot (key digests, never keys) in this directory, so a restart during an outage serves inside the offline grace")
		fs.StringVar(&flags.executor, "executor", "", "test setups the control plane queues, in this cluster: kubernetes")
		fs.StringVar(&flags.kubeContext, "kubernetes-context", "", "kubectl context for setup tests (default: this pod's service account)")
		fs.StringVar(&flags.kubeNS, "kubernetes-namespace", "", "namespace test setups are started in")
		fs.StringVar(&flags.runtimeSA, "runtime-service-account", "infercrane-runtime", "service account of test setup pods")
		fs.StringVar(&flags.workerSecret, "worker-secret", "", "Secret NAME/KEY mounted into test setup pods as their API key")
		fs.StringVar(&flags.workerKeyEnv, "worker-key-env", "INFERCRANE_WORKER_KEY", "environment variable holding that Secret's value")
		fs.StringVar(&flags.engineIngress, "engine-ingress", "", "the only sources that may reach test setup pods: pods:KEY=VALUE, namespace:KEY=VALUE, address ranges, or none (default: $INFERCRANE_KUBERNETES_ENGINE_INGRESS)")
		fs.StringVar(&flags.gpuResource, "gpu-resource", "nvidia.com/gpu", "device-plugin resource test setups request")
		fs.StringVar(&flags.gpuLabel, "gpu-product-label", "nvidia.com/gpu.product", "node label that names the accelerator")
		fs.StringVar(&tolerations, "tolerations", "", `taints test setup pods tolerate, as JSON: [{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]`)
		fs.Var(&flags.allowImages, "allow-image", "image repository test setups may use (repeatable); nothing else runs")
		fs.Var(&flags.allowAccelerators, "allow-accelerator", "accelerator test setups may request (repeatable); default any")
		fs.Var(&flags.evalSets, "eval-set", "a local eval set NAME=PATH for the answer check (repeatable)")
	}
	if err := fs.Parse(args); err != nil {
		return agentFlags{}, err
	}
	if fs.NArg() != 0 {
		return agentFlags{}, errors.New(agentUsage)
	}
	if err := validateOutput(flags.output); err != nil {
		return agentFlags{}, err
	}
	if flags.noDetect && len(flags.servers) == 0 {
		return agentFlags{}, errors.New("--no-detect needs at least one --server")
	}
	if flags.executor != "" && flags.executor != "kubernetes" {
		return agentFlags{}, errors.New("--executor must be kubernetes")
	}
	if flags.executor != "" && flags.once {
		return agentFlags{}, errors.New("--executor runs until stopped; it cannot be combined with --once")
	}
	if flags.serve != "" && flags.once {
		return agentFlags{}, errors.New("--serve runs until stopped; it cannot be combined with --once")
	}
	if run {
		if err := flags.resolveOfflineGrace(fs); err != nil {
			return agentFlags{}, err
		}
	}
	if flags.offlineGrace < 0 || flags.offlineGrace > agentprotocol.MaxOfflineGrace {
		return agentFlags{}, errors.New("--offline-grace must be between 0 and 24h")
	}
	if flags.offlineGrace > 0 && flags.serve == "" {
		return agentFlags{}, errors.New("--offline-grace applies to the data plane; it needs --serve")
	}
	// Tolerations are checked here, before the agent contacts anything, so a
	// setting the provider would refuse never reaches a manifest.
	parsed, err := provision.ParseKubernetesTolerations(tolerations)
	if err != nil {
		return agentFlags{}, fmt.Errorf("--tolerations: %w", err)
	}
	if len(parsed) > 0 && flags.executor == "" {
		return agentFlags{}, errors.New("--tolerations applies to test setup pods; it needs --executor kubernetes")
	}
	flags.tolerations = parsed
	return flags, nil
}

// kubernetesProvider is the namespaced provider test setups start with: one
// Deployment and one Service per setup, no model cache, the sources that may
// reach the pods, and the operator's tolerations for tainted GPU nodes.
func (f agentFlags) kubernetesProvider(contextName, secretName, secretKey string, engineIngress []engineingress.Peer) provision.Kubernetes {
	return provision.Kubernetes{
		Context: contextName, Namespace: f.kubeNS, WorkloadAPI: "deployment", ServiceAccount: f.runtimeSA,
		WorkerSecretName: secretName, WorkerSecretKey: secretKey, GPUResource: f.gpuResource, GPUProductLabel: f.gpuLabel,
		ArtifactCachePolicy: "disabled", EngineIngress: engineIngress, Tolerations: f.tolerations,
	}
}

// resolveOfflineGrace picks the offline grace: the flag when given, else
// INFERCRANE_AGENT_OFFLINE_GRACE when set, else 24h for an agent that serves
// and none for one that does not. A value that is not a duration is refused
// rather than read as the default, so a typo never turns into 24 hours of
// serving keys the control plane may have revoked.
func (f *agentFlags) resolveOfflineGrace(fs *flag.FlagSet) error {
	given := false
	fs.Visit(func(set *flag.Flag) { given = given || set.Name == "offline-grace" })
	if given {
		return nil
	}
	if value := strings.TrimSpace(os.Getenv("INFERCRANE_AGENT_OFFLINE_GRACE")); value != "" {
		grace, err := time.ParseDuration(value)
		if err != nil {
			return errors.New("INFERCRANE_AGENT_OFFLINE_GRACE must be a duration such as 24h, or 0s to refuse at the snapshot's validity")
		}
		f.offlineGrace = grace
		return nil
	}
	if f.serve != "" {
		f.offlineGrace = agentprotocol.DefaultOfflineGrace
	}
	return nil
}

// candidateExecutor builds the in-cluster executor from flags. It refuses to
// start without an image allowlist, so a command can only run images the
// cluster's operator chose.
func (f agentFlags) candidateExecutor(ctx context.Context) (agent.CandidateExecutor, error) {
	if f.executor == "" {
		return nil, nil
	}
	if f.kubeNS == "" {
		return nil, errors.New("--executor kubernetes needs --kubernetes-namespace")
	}
	secretName, secretKey, ok := strings.Cut(f.workerSecret, "/")
	if !ok || secretName == "" || secretKey == "" {
		return nil, errors.New("--executor kubernetes needs --worker-secret NAME/KEY")
	}
	workerKey := strings.TrimSpace(os.Getenv(f.workerKeyEnv))
	if workerKey == "" {
		return nil, fmt.Errorf("environment variable %s (the worker Secret's value) is empty or unset", f.workerKeyEnv)
	}
	engineIngress, err := agentEngineIngress(f.engineIngress)
	if err != nil {
		return nil, err
	}
	contextName := f.kubeContext
	if contextName == "" {
		dir, err := os.MkdirTemp("", "infercrane-agent-kube-")
		if err != nil {
			return nil, err
		}
		path, err := agent.WriteInClusterKubeconfig(dir)
		if err != nil {
			return nil, fmt.Errorf("--executor kubernetes without --kubernetes-context must run in a pod: %w", err)
		}
		if err = os.Setenv("KUBECONFIG", path); err != nil {
			return nil, err
		}
		contextName = "in-cluster"
	}
	evalSets := map[string]qualitycheck.EvalSet{}
	for _, entry := range f.evalSets {
		name, path, ok := strings.Cut(entry, "=")
		if !ok || !agentprotocol.ValidEndpointName(name) {
			return nil, fmt.Errorf("--eval-set %q must be NAME=PATH with a lowercase name", entry)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("--eval-set %s: %w", name, err)
		}
		set, err := qualitycheck.DecodeEvalSet(body)
		if err != nil {
			return nil, fmt.Errorf("--eval-set %s: %w", name, err)
		}
		evalSets[name] = set
	}
	executor := agent.KubernetesExecutor{
		Provider:  f.kubernetesProvider(contextName, secretName, secretKey, engineIngress),
		WorkerKey: workerKey, AllowedImages: f.allowImages, AllowedAccelerators: f.allowAccelerators, EvalSets: evalSets,
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := executor.Check(checkCtx); err != nil {
		return nil, fmt.Errorf("test setups cannot start in namespace %s: %w", f.kubeNS, err)
	}
	return executor, nil
}

// agentEngineIngress reads which sources may reach test setup pods, from
// --engine-ingress or else INFERCRANE_KUBERNETES_ENGINE_INGRESS, the setting
// the control plane's Kubernetes adapter reads. A choice is required, as it
// is for the control plane: the agent's own pods, by label, or "none".
func agentEngineIngress(flag string) ([]engineingress.Peer, error) {
	value := flag
	if strings.TrimSpace(value) == "" {
		value = os.Getenv("INFERCRANE_KUBERNETES_ENGINE_INGRESS")
	}
	peers, _, err := engineingress.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("--executor kubernetes needs --engine-ingress: %w; name this agent's pods (pods:<label>=<value>) or address ranges that may reach test setup pods, or set none", err)
	}
	return peers, nil
}

func (f agentFlags) serverConfigs(auth string) ([]agent.ServerConfig, error) {
	out := make([]agent.ServerConfig, 0, len(f.servers))
	for _, raw := range f.servers {
		if _, err := agent.NormalizeBaseURL(raw); err != nil {
			return nil, fmt.Errorf("--server %s: %w", raw, err)
		}
		out = append(out, agent.ServerConfig{URL: raw, AuthHeader: auth})
	}
	return out, nil
}

func agentDetectCommand(ctx context.Context, args []string) error {
	flags, err := parseAgentFlags("agent detect", args, false)
	if err != nil {
		return err
	}
	auth, err := localAuthHeader(flags.tokenEnv, flags.headerEnv)
	if err != nil {
		return err
	}
	if auth != "" && len(flags.servers) == 0 {
		return errors.New("name the server the key belongs to with --server; a key is never sent to servers found on the default ports")
	}
	explicit, err := flags.serverConfigs(auth)
	if err != nil {
		return err
	}
	hardware := agentHardware.Inventory(ctx)
	detector := agent.Detector{}
	servers := []agent.Detected{}
	named := map[string]bool{}
	for _, server := range explicit {
		base, _ := agent.NormalizeBaseURL(server.URL)
		named[base] = true
	}
	for _, candidate := range agent.Candidates(explicit, !flags.noDetect) {
		if server, ok := detector.Probe(ctx, candidate, named[candidate.URL]); ok {
			servers = append(servers, server)
		}
	}
	if flags.output == "json" {
		items := make([]agentprotocol.Server, 0, len(servers))
		for _, server := range servers {
			items = append(items, server.Server)
		}
		return printJSON(map[string]any{"hardware": hardware, "servers": items, "sent": []string{}})
	}
	printHardware(agentOutput, hardware)
	if len(servers) == 0 {
		fmt.Fprintf(agentOutput, "\nNo model server answered on ports %s. Name one with --server URL.\n", joinPorts())
	} else {
		fmt.Fprintln(agentOutput, "\nServers")
		for _, server := range servers {
			printServer(agentOutput, agent.ServerStatus{Server: server.Server})
		}
	}
	fmt.Fprintln(agentOutput, "\nNothing was sent. Connect these servers with: infercrane agent run")
	return nil
}

func joinPorts() string {
	parts := make([]string, 0, len(agent.DefaultPorts))
	for _, port := range agent.DefaultPorts {
		parts = append(parts, fmt.Sprint(port))
	}
	return strings.Join(parts, ", ")
}

func printHardware(w io.Writer, hardware agentprotocol.Hardware) {
	fmt.Fprintln(w, "Hardware")
	if len(hardware.Accelerators) == 0 {
		fmt.Fprintln(w, "  no accelerator found")
	}
	for _, accelerator := range hardware.Accelerators {
		memory := "memory unknown"
		if accelerator.MemoryMiB != nil {
			memory = fmt.Sprintf("%d MiB", *accelerator.MemoryMiB)
			if accelerator.Vendor == "apple" {
				memory += " unified"
			}
		}
		line := fmt.Sprintf("  %d × %s · %s", accelerator.Count, accelerator.Name, memory)
		if accelerator.Driver != "" {
			line += " · driver " + accelerator.Driver
		}
		if len(accelerator.Unknown) > 0 {
			line += " · unknown: " + strings.Join(accelerator.Unknown, ", ")
		}
		fmt.Fprintln(w, line)
	}
	for _, limitation := range hardware.Limitations {
		fmt.Fprintf(w, "  - %s\n", limitation)
	}
}

func printServer(w io.Writer, server agent.ServerStatus) {
	fmt.Fprintf(w, "  %s  %s\n", server.Health, agent.Describe(server))
	if server.HealthDetail != "" {
		fmt.Fprintf(w, "      %s\n", server.HealthDetail)
	}
	if server.MetricsAvailable {
		fmt.Fprintln(w, "      engine metrics: available")
	} else if server.MetricsDetail != "" {
		fmt.Fprintf(w, "      engine metrics: not available (%s)\n", server.MetricsDetail)
	}
	if server.Registered {
		fmt.Fprintf(w, "      endpoint %s (observe-only)\n", server.Endpoint)
	} else if server.NotRegistered != "" {
		fmt.Fprintf(w, "      not connected: %s\n", server.NotRegistered)
	}
	if server.TrafficDetail != "" && server.Registered {
		fmt.Fprintf(w, "      traffic: %s\n", server.TrafficDetail)
	}
}

// resolveAgentKey finds the agent key: the environment, a key saved by an
// earlier run, or a new key minted with this CLI's own sign-in.
func resolveAgentKey(ctx context.Context, cfg config.Config, flags agentFlags, human bool) (string, error) {
	if value := strings.TrimSpace(os.Getenv(flags.keyEnv)); value != "" {
		return value, nil
	}
	saved, ok, err := config.LoadAgentCredential(cfg.ClientContextName, cfg.ControlURL)
	if err != nil {
		return "", err
	}
	if ok {
		return saved.Key, nil
	}
	if cfg.APIKey == "" {
		return "", fmt.Errorf("no agent key: run infercrane login first, or create an agent key in the dashboard and set %s", flags.keyEnv)
	}
	name := flags.name
	if name == "" {
		hostname, _ := os.Hostname()
		name = agent.EndpointName(hostname)
	}
	var created struct {
		Agent map[string]any `json:"agent"`
		Key   string         `json:"key"`
	}
	if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/agents", "", map[string]string{"name": name}, &created); err != nil {
		var controlErr *ControlError
		if errors.As(err, &controlErr) && controlErr.StatusCode == http.StatusConflict {
			return "", fmt.Errorf("an agent named %s already exists, and its key is not saved on this machine; revoke it with `infercrane agent revoke %s` or pick another name with --name", name, name)
		}
		return "", fmt.Errorf("create an agent key: %w", err)
	}
	agentID, _ := created.Agent["id"].(string)
	stored, err := config.SaveAgentCredential(cfg.ClientContextName, cfg.ControlURL, config.AgentCredential{AgentID: agentID, Name: name, Key: created.Key})
	if err != nil {
		return "", fmt.Errorf("the agent key was created but could not be saved: %w; revoke agent %s and try again", err, name)
	}
	if human {
		where := stored.Store
		if stored.Store == config.CredentialFromConfigFile {
			where = stored.Path + " (mode 0600)"
		}
		fmt.Fprintf(agentOutput, "Created agent %s. Its key is saved in %s and can only report what this machine sees.\n\n", name, where)
	}
	return created.Key, nil
}

func agentRunCommand(ctx context.Context, args []string) error {
	flags, err := parseAgentFlags("agent run", args, true)
	if err != nil {
		return err
	}
	human := flags.output == "human"
	if !flags.once && flags.output == "json" {
		return errors.New("--output json needs --once; a running agent prints progress lines")
	}
	auth, err := localAuthHeader(flags.tokenEnv, flags.headerEnv)
	if err != nil {
		return err
	}
	if auth != "" && len(flags.servers) == 0 {
		return errors.New("name the server the key belongs to with --server; a key is never sent to servers found on the default ports")
	}
	explicit, err := flags.serverConfigs(auth)
	if err != nil {
		return err
	}
	if (flags.endpoint != "" || flags.model != "") && len(explicit) > 1 {
		return errors.New("--as and --model apply to one server; run one agent per server or omit them")
	}
	// An agent key alone is enough to run; the CLI's own sign-in is needed
	// only to mint a key.
	cfg, err := config.LoadClientTarget(os.Getenv("INFERCRANE_CONTEXT"))
	if err != nil {
		return err
	}
	if err = agent.CheckControlURL(cfg.ControlURL); err != nil {
		return err
	}
	key, err := resolveAgentKey(ctx, cfg, flags, human)
	if err != nil {
		return err
	}
	client, err := controlHTTPClient(cfg, 30*time.Second)
	if err != nil {
		return err
	}
	hashKey, err := agentHashKey(cfg)
	if err != nil {
		return fmt.Errorf("prepare the local hashing key: %w", err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	executor, err := flags.candidateExecutor(ctx)
	if err != nil {
		return err
	}
	if kubernetes, ok := executor.(agent.KubernetesExecutor); ok {
		// Nodes are named by a digest keyed like the request hashes.
		kubernetes.NodeKey = hashKey
		executor = kubernetes
	}
	runner := agent.New(agent.Config{
		Client:  agent.Client{ControlURL: cfg.ControlURL, Key: key, HTTP: client, UserAgent: "infercrane-agent/" + version},
		Version: version, Servers: explicit, AutoDetect: !flags.noDetect, HashKey: hashKey,
		Model: flags.model, EndpointName: flags.endpoint, Gateway: flags.gateway, GatewayServer: flags.gatewayFor,
		Serve: flags.serve, Executor: executor, OfflineGrace: flags.offlineGrace, StateDir: flags.stateDir,
		Once: flags.once, Hardware: agentHardware, Events: agentEventPrinter(human),
	})
	if human {
		fmt.Fprintf(agentOutput, "InferCrane agent %s → %s (outbound HTTPS only; no port is opened for InferCrane)\n", version, cfg.ControlURL)
		fmt.Fprintln(agentOutput, "Prompts and completions never leave this machine. Sent:")
		for _, item := range agent.SentWith(flags.gateway != "", flags.serve != "", executor != nil) {
			fmt.Fprintf(agentOutput, "  - %s\n", item)
		}
		fmt.Fprintln(agentOutput)
	}
	status, runErr := runner.Run(ctx)
	if errors.Is(runErr, agent.ErrRevoked) {
		return errors.New("this agent's key was revoked in InferCrane, so the agent stopped and sends nothing more. To connect again, run infercrane agent run with a new key")
	}
	if errors.Is(runErr, agent.ErrUnauthenticated) {
		if status.AgentID != "" {
			return errors.New("the control plane no longer accepts this agent's key (it was revoked), so the agent stopped and sends nothing more. To connect again, run infercrane agent run with a new key")
		}
		return fmt.Errorf("the control plane does not accept this agent key; check %s, or create a new key with infercrane agent create", flags.keyEnv)
	}
	if flags.output == "json" {
		if runErr != nil {
			return runErr
		}
		return printJSON(status)
	}
	if flags.once {
		printAgentSummary(agentOutput, status)
	}
	return runErr
}

// agentHashKey reads the request-hashing key from INFERCRANE_AGENT_HASH_KEY
// (64 hex characters, for example from a Kubernetes Secret so hashes stay
// comparable across pod restarts), or keeps one beside the saved agent key.
// Either way it stays where the agent runs and is never sent.
func agentHashKey(cfg config.Config) ([]byte, error) {
	if value := strings.TrimSpace(os.Getenv("INFERCRANE_AGENT_HASH_KEY")); value != "" {
		key, err := hex.DecodeString(value)
		if err != nil || len(key) != 32 {
			return nil, errors.New("INFERCRANE_AGENT_HASH_KEY must be 64 hexadecimal characters (32 random bytes)")
		}
		return key, nil
	}
	return config.LoadOrCreateAgentHashKey(cfg.ClientContextName, cfg.ControlURL)
}

func agentEventPrinter(human bool) func(agent.Event) {
	if !human {
		return nil
	}
	return func(event agent.Event) {
		stamp := time.Now().Format("15:04:05")
		switch event.Kind {
		case "hardware":
			fmt.Fprintf(agentOutput, "%s hardware  %s\n", stamp, event.Message)
		case "server_found":
			fmt.Fprintf(agentOutput, "%s found     %s\n", stamp, agent.Describe(*event.Server))
		case "server_registered":
			fmt.Fprintf(agentOutput, "%s observing %s as endpoint %s (observe-only)\n", stamp, event.Server.Address, event.Server.Endpoint)
			if event.Server.MetricsAvailable {
				fmt.Fprintf(agentOutput, "          engine metrics relayed every %s; bottleneck diagnosis can use them\n", agentprotocol.ScrapeInterval)
			} else {
				fmt.Fprintf(agentOutput, "          engine metrics not available: %s\n", event.Server.MetricsDetail)
			}
			if event.Server.TrafficSource != "agent-gateway" {
				fmt.Fprintln(agentOutput, "          traffic not visible: requests go straight to the server; add --gateway 127.0.0.1:4000 and point your app at it to record request shapes")
			}
		case "server_skipped":
			fmt.Fprintf(agentOutput, "%s skipped   %s: %s\n", stamp, event.Server.Address, event.Message)
		case "gateway_listening":
			fmt.Fprintf(agentOutput, "%s gateway   %s (forwards unchanged; records timings and token counts only)\n", stamp, event.Message)
		case "data_plane_listening":
			fmt.Fprintf(agentOutput, "%s serving   %s (agent-managed endpoints, InferCrane API keys; prompts stay here)\n", stamp, event.Message)
		case "candidate_started", "candidate_phase", "candidate_done", "candidate_failed":
			fmt.Fprintf(agentOutput, "%s setup     %s\n", stamp, event.Message)
		case "rejected":
			fmt.Fprintf(agentOutput, "%s rejected  %s\n", stamp, event.Message)
		case "retrying":
			fmt.Fprintf(agentOutput, "%s retrying  %s\n", stamp, event.Message)
		case "upgrade_advice":
			fmt.Fprintf(agentOutput, "%s upgrade   %s\n", stamp, event.Message)
		}
	}
}

func printAgentSummary(w io.Writer, status agent.Status) {
	fmt.Fprintf(w, "\nAgent %s\n", status.AgentName)
	printHardware(w, status.Hardware)
	fmt.Fprintln(w, "Servers")
	if len(status.Servers) == 0 {
		fmt.Fprintf(w, "  none found on ports %s; name one with --server URL\n", joinPorts())
	}
	for _, server := range status.Servers {
		printServer(w, server)
	}
}

func agentCreateCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane agent create NAME [--output human|json]")
	}
	fs := newFlagSet("agent create")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var created map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/agents", "", map[string]string{"name": args[0]}, &created); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(created)
	}
	fmt.Printf("Agent %s created. Its key is shown once:\n\n  %v\n\nOn the machine that runs the model server:\n  INFERCRANE_AGENT_KEY=<key> INFERCRANE_URL=%s infercrane agent run\n", args[0], created["key"], cfg.ControlURL)
	return nil
}

func agentListCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := newFlagSet("agent list")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var listed struct {
		Data []map[string]any `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/agents", "", nil, &listed); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(listed)
	}
	if len(listed.Data) == 0 {
		fmt.Println("No agents yet. Start one on a machine with a model server: infercrane agent run")
		return nil
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tCONNECTION\tVERSION\tLAST SEEN\tSERVERS")
	for _, item := range listed.Data {
		servers, _ := item["servers"].([]any)
		lastSeen := "never"
		if value, ok := item["last_seen_at"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				lastSeen = relativeTime(time.Now(), parsed)
			}
		}
		fmt.Fprintf(writer, "%v\t%v\t%v\t%s\t%d\n", item["name"], item["connection"], anyOr(item["agent_version"], "unknown"), lastSeen, len(servers))
	}
	return writer.Flush()
}

func anyOr(value any, fallback string) any {
	if value == nil {
		return fallback
	}
	return value
}

func agentRevokeCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane agent revoke NAME_OR_ID [--output human|json]")
	}
	fs := newFlagSet("agent revoke")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	var revoked struct {
		Agent map[string]any `json:"agent"`
	}
	if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/agents/"+url.PathEscape(args[0]), "", nil, &revoked); err != nil {
		return err
	}
	if saved, ok, _ := config.LoadAgentCredential(cfg.ClientContextName, cfg.ControlURL); ok && (saved.AgentID == revoked.Agent["id"] || saved.Name == args[0]) {
		_ = config.ClearAgentCredential(cfg.ClientContextName, cfg.ControlURL)
	}
	if *output == "json" {
		return printJSON(revoked)
	}
	fmt.Printf("Agent %v revoked. It stops at its next sync and sends nothing more; its endpoints stay, marked unhealthy.\n", revoked.Agent["name"])
	return nil
}
