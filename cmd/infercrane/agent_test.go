package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agent"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/engineingress"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/testtools/fakeengine"
)

type noHostAccelerators struct{}

func (noHostAccelerators) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, exec.ErrNotFound
}

func TestDeployCLITargetsTheTwoSelfServeDestinations(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/compute/connections" {
			_, _ = w.Write([]byte(`{"data":[{"id":"conn-1","name":"my-runpod","provider":"runpod","status":"verified"},{"id":"conn-2","name":"old","provider":"runpod","status":"revoked"}]}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	quiet := func(args ...string) error {
		_, err := captureStdout(t, func() error { return deployAPICommand(context.Background(), cfg, "deploy", args) })
		return err
	}
	if err := quiet("Qwen/Qwen3-8B", "--infercrane-cloud", "--stop-after", "6h", "--idempotency-key", "a"); err != nil {
		t.Fatal(err)
	}
	if body := bodies[0]; body["billing_mode"] != "customer_wallet" || body["managed_runtime_seconds"] != float64(21600) || body["cloud"] != "runpod" || body["compute_connection_id"] != nil {
		t.Fatalf("InferCrane Cloud body: %#v", body)
	}
	if err := quiet("Qwen/Qwen3-8B", "--compute-connection", "my-runpod", "--gpu", "L40S", "--idempotency-key", "b"); err != nil {
		t.Fatal(err)
	}
	if body := bodies[1]; body["billing_mode"] != "provider_account" || body["compute_connection_id"] != "conn-1" || body["cloud"] != "runpod" || body["gpu"] != "L40S" {
		t.Fatalf("own RunPod body: %#v", body)
	}
	for _, args := range [][]string{
		{"m", "--compute-connection", "old"},
		{"m", "--compute-connection", "missing"},
		{"m", "--compute-connection", "my-runpod", "--infercrane-cloud"},
		{"m", "--stop-after", "1h"},
		{"m", "--infercrane-cloud", "--stop-after", "5m"},
		{"m", "--infercrane-cloud", "--compute", "serverless"},
		{"m", "--infercrane-cloud", "--targets", "worker"},
	} {
		if err := quiet(args...); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("refused deploys still submitted: %d", len(bodies))
	}
}

func TestConnectSendsTheServerKeyOnceAndRefusesLoopbackForARemoteControlPlane(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"discovery":{"runtime":"vllm","model":"Qwen/Qwen3-8B","health":"reachable"}}`))
	}))
	defer server.Close()
	t.Setenv("PRIVATE_VLLM_KEY", "server-key-9a")
	output, err := captureStdout(t, func() error {
		return connectCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"https://vllm.internal.example.test/v1", "--as", "private-qwen", "--token-env", "PRIVATE_VLLM_KEY"})
	})
	if err != nil || body["upstream_credential"] != "server-key-9a" || strings.Contains(output, "server-key-9a") || !strings.Contains(output, "stored encrypted") {
		t.Fatalf("output=%s body=%#v err=%v", output, body, err)
	}
	if err = connectCommand(context.Background(), config.Config{ControlURL: server.URL}, []string{"https://x.example.test", "--as", "x", "--token-env", "UNSET_VARIABLE_FOR_TEST"}); err == nil || !strings.Contains(err.Error(), "UNSET_VARIABLE_FOR_TEST") {
		t.Fatalf("unset token env: %v", err)
	}
	err = connectCommand(context.Background(), config.Config{ControlURL: "https://control.example.test"}, []string{"http://127.0.0.1:8000/v1", "--as", "local"})
	if err == nil || !strings.Contains(err.Error(), "infercrane agent run --server http://127.0.0.1:8000/v1") {
		t.Fatalf("loopback guard: %v", err)
	}
}

func TestAgentDetectSendsNothingAndExplainsWhatItFound(t *testing.T) {
	engine := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.Ollama, Models: []string{"llama3.2:3b"}})
	defer engine.Close()
	previousHardware, previousOutput := agentHardware, agentOutput
	agentHardware = agent.HardwareProbe{Runner: noHostAccelerators{}, GOOS: "linux", SysRoot: t.TempDir()}
	var buffer bytes.Buffer
	agentOutput = &buffer
	t.Cleanup(func() { agentHardware, agentOutput = previousHardware, previousOutput })
	if err := agentCommand(context.Background(), []string{"detect", "--no-detect", "--server", engine.URL}); err != nil {
		t.Fatal(err)
	}
	text := buffer.String()
	for _, want := range []string{"ollama 0.12.10", "llama3.2:3b", "does not expose engine metrics", "Nothing was sent", "no accelerator found"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detect output lacks %q:\n%s", want, text)
		}
	}
	if err := agentCommand(context.Background(), []string{"detect", "--no-detect"}); err == nil {
		t.Fatal("--no-detect without --server was accepted")
	}
	if err := agentCommand(context.Background(), []string{"run", "--output", "json"}); err == nil || !strings.Contains(err.Error(), "--once") {
		t.Fatalf("json without --once: %v", err)
	}
	// A server key goes only to servers named with --server.
	t.Setenv("LOCAL_SERVER_KEY", "k")
	for _, args := range [][]string{{"run", "--once", "--token-env", "LOCAL_SERVER_KEY"}, {"detect", "--token-env", "LOCAL_SERVER_KEY"}} {
		if err := agentCommand(context.Background(), args); err == nil || !strings.Contains(err.Error(), "--server") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestAgentRunOnceRefusesPlainHTTPToARemoteControlPlane(t *testing.T) {
	t.Setenv("INFERCRANE_URL", "http://control.example.test")
	t.Setenv("INFERCRANE_AGENT_KEY", "ic_key")
	t.Setenv("INFERCRANE_CONFIG", t.TempDir()+"/config.json")
	err := agentCommand(context.Background(), []string{"run", "--once", "--no-detect", "--server", "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain HTTP control plane: %v", err)
	}
}

func TestAgentRunTolerationsAreParsedStrictlyAndReachTestSetupPods(t *testing.T) {
	const gpuTaint = `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]`
	want := []provision.KubernetesToleration{{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"}}
	flags, err := parseAgentFlags("agent run", []string{"--executor", "kubernetes", "--kubernetes-namespace", "models", "--tolerations", gpuTaint}, true)
	if err != nil || !reflect.DeepEqual(flags.tolerations, want) {
		t.Fatalf("tolerations=%#v err=%v", flags.tolerations, err)
	}
	agentPods := []engineingress.Peer{{PodLabels: map[string]string{"app.kubernetes.io/name": "infercrane-agent"}}}
	provider := flags.kubernetesProvider("in-cluster", "infercrane-worker", "api-key", agentPods)
	if !reflect.DeepEqual(provider.Tolerations, want) || !reflect.DeepEqual(provider.EngineIngress, agentPods) || provider.Context != "in-cluster" || provider.Namespace != "models" || provider.WorkloadAPI != "deployment" ||
		provider.ServiceAccount != "infercrane-runtime" || provider.WorkerSecretName != "infercrane-worker" || provider.WorkerSecretKey != "api-key" ||
		provider.GPUResource != "nvidia.com/gpu" || provider.GPUProductLabel != "nvidia.com/gpu.product" || provider.ArtifactCachePolicy != "disabled" {
		t.Fatalf("provider=%#v", provider)
	}
	// Without the flag, test setup pods get no tolerations, as before.
	flags, err = parseAgentFlags("agent run", []string{"--executor", "kubernetes", "--kubernetes-namespace", "models"}, true)
	if err != nil || flags.tolerations != nil || flags.kubernetesProvider("in-cluster", "infercrane-worker", "api-key", agentPods).Tolerations != nil {
		t.Fatalf("tolerations without the flag: %#v err=%v", flags.tolerations, err)
	}
	for _, args := range [][]string{
		{"--executor", "kubernetes", "--tolerations", "nvidia.com/gpu:NoSchedule"},
		{"--executor", "kubernetes", "--tolerations", `[{"key":"nvidia.com/gpu","effect":"NoSchedule"}]`},
		{"--executor", "kubernetes", "--tolerations", `[{"operator":"Exists","effect":"NoSchedule"}]`},
		{"--executor", "kubernetes", "--tolerations", `[{"key":"nvidia.com/gpu","operator":"Exists"}]`},
		{"--executor", "kubernetes", "--tolerations", `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoExecute","tolerationSeconds":60}]`},
		// The flag changes test setup pods only, so it needs the executor.
		{"--tolerations", gpuTaint},
	} {
		if _, err = parseAgentFlags("agent run", args, true); err == nil || !strings.Contains(err.Error(), "--tolerations") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err = parseAgentFlags("agent detect", []string{"--tolerations", gpuTaint}, false); err == nil {
		t.Fatal("agent detect accepted --tolerations")
	}
}

// The in-cluster executor needs a choice of who may reach its test setup
// pods, as the control plane's Kubernetes adapter does: the flag, else the
// shared setting, or an explicit none. Without one it refuses to start,
// before it calls the cluster.
func TestAgentExecutorRequiresEngineIngress(t *testing.T) {
	t.Setenv("INFERCRANE_KUBERNETES_ENGINE_INGRESS", "")
	t.Setenv("INFERCRANE_WORKER_KEY", "worker-key")
	flags, err := parseAgentFlags("agent run", []string{"--executor", "kubernetes", "--kubernetes-context", "kind-unused", "--kubernetes-namespace", "models",
		"--worker-secret", "agent-worker/api-key", "--allow-image", "vllm/vllm-openai"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = flags.candidateExecutor(context.Background()); err == nil || !strings.Contains(err.Error(), "--executor kubernetes needs --engine-ingress") {
		t.Fatalf("executor without engine ingress: %v", err)
	}
	if _, err = agentEngineIngress("agent"); err == nil || !strings.Contains(err.Error(), "not an address range") {
		t.Fatalf("malformed engine ingress: %v", err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_ENGINE_INGRESS", "none")
	if peers, err := agentEngineIngress(""); err != nil || peers != nil {
		t.Fatalf("none: peers=%v err=%v", peers, err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_ENGINE_INGRESS", "10.8.0.0/16")
	if peers, err := agentEngineIngress(""); err != nil || len(peers) != 1 || peers[0].CIDR != "10.8.0.0/16" {
		t.Fatalf("setting: peers=%v err=%v", peers, err)
	}
	peers, err := agentEngineIngress("namespace:kubernetes.io/metadata.name=models;pods:app.kubernetes.io/name=infercrane-agent")
	if err != nil || len(peers) != 1 || peers[0].NamespaceLabels["kubernetes.io/metadata.name"] != "models" || peers[0].PodLabels["app.kubernetes.io/name"] != "infercrane-agent" {
		t.Fatalf("the flag wins over the setting: peers=%v err=%v", peers, err)
	}
}

// A serving agent keeps serving its endpoints' own servers for 24 hours
// through a control-plane outage unless its operator says otherwise: 0
// fails closed, the flag wins over the environment, a value that is not a
// duration is refused rather than read as 24 hours, and an agent that does
// not serve has no grace.
func TestAgentOfflineGraceDefaultsTo24HoursWhenServing(t *testing.T) {
	parse := func(args ...string) (time.Duration, error) {
		flags, err := parseAgentFlags("agent run", args, true)
		return flags.offlineGrace, err
	}
	serve := []string{"--serve", "127.0.0.1:8080"}
	t.Setenv("INFERCRANE_AGENT_OFFLINE_GRACE", "")
	for _, test := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"serving", serve, 24 * time.Hour},
		{"observing only", nil, 0},
		{"fail closed", append(serve, "--offline-grace", "0s"), 0},
		{"an hour", append(serve, "--offline-grace", "1h"), time.Hour},
	} {
		if got, err := parse(test.args...); err != nil || got != test.want {
			t.Fatalf("%s: %v %v, want %v", test.name, got, err, test.want)
		}
	}
	if _, err := parse("--offline-grace", "1h"); err == nil || !strings.Contains(err.Error(), "needs --serve") {
		t.Fatalf("a grace without --serve: %v", err)
	}
	if _, err := parse(append(serve, "--offline-grace", "25h")...); err == nil || !strings.Contains(err.Error(), "between 0 and 24h") {
		t.Fatalf("25h: %v", err)
	}
	t.Setenv("INFERCRANE_AGENT_OFFLINE_GRACE", "0")
	if got, err := parse(serve...); err != nil || got != 0 {
		t.Fatalf("environment 0: %v %v", got, err)
	}
	t.Setenv("INFERCRANE_AGENT_OFFLINE_GRACE", "2h")
	if got, err := parse(serve...); err != nil || got != 2*time.Hour {
		t.Fatalf("environment 2h: %v %v", got, err)
	}
	if got, err := parse(append(serve, "--offline-grace", "30m")...); err != nil || got != 30*time.Minute {
		t.Fatalf("the flag over the environment: %v %v", got, err)
	}
	t.Setenv("INFERCRANE_AGENT_OFFLINE_GRACE", "24 hours")
	if _, err := parse(serve...); err == nil || !strings.Contains(err.Error(), "INFERCRANE_AGENT_OFFLINE_GRACE") {
		t.Fatalf("a typo was not refused: %v", err)
	}
}
