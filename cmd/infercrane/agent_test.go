package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/agent"
	"github.com/infercrane/infercrane/internal/config"
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
