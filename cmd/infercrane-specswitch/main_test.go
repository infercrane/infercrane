package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/specswitch"
)

// TestMain lets the test binary stand in for a vLLM engine when the command
// under test starts it with SPECSWITCH_FAKE_ENGINE=1.
func TestMain(m *testing.M) {
	if os.Getenv("SPECSWITCH_FAKE_ENGINE") == "1" {
		os.Exit(fakeEngine(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeEngine serves the routes the switch uses, like vLLM with sleep mode,
// and refuses inference while asleep.
func fakeEngine(args []string) int {
	flags := flag.NewFlagSet("fake", flag.ContinueOnError)
	host := flags.String("host", "", "")
	port := flags.String("port", "", "")
	name := flags.String("name", "", "")
	share := flags.String("gpu-memory-utilization", "", "")
	_ = flags.Bool("enable-sleep-mode", false, "")
	if flags.Parse(args) != nil || os.Getenv("VLLM_SERVER_DEV_MODE") != "1" {
		fmt.Println("fake engine: needs --host, --port and VLLM_SERVER_DEV_MODE=1")
		return 2
	}
	var mu sync.Mutex
	asleep := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"fake/model"}]}`)
	})
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if asleep {
			http.Error(w, "asleep", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"engine":%q,"share":%q}`, *name, *share)
	})
	mux.HandleFunc("POST /sleep", func(http.ResponseWriter, *http.Request) { mu.Lock(); asleep = true; mu.Unlock() })
	mux.HandleFunc("POST /wake_up", func(http.ResponseWriter, *http.Request) { mu.Lock(); asleep = false; mu.Unlock() })
	mux.HandleFunc("GET /is_sleeping", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, `{"is_sleeping":%t}`, asleep)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, "vllm:num_requests_running 0\n") })
	fmt.Println("fake engine listening")
	if err := http.ListenAndServe(net.JoinHostPort(*host, *port), mux); err != nil {
		fmt.Println(err)
		return 1
	}
	return 0
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return port
}

func engineFor(t *testing.T, name string, drafts int) specswitch.Engine {
	port := freePort(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return specswitch.Engine{Name: name, Drafts: drafts, URL: "http://127.0.0.1:" + port, Room: 4,
		Command: []string{executable, "--host", "127.0.0.1", "--port", port, "--name", name, "--enable-sleep-mode"},
		Env:     map[string]string{"SPECSWITCH_FAKE_ENGINE": "1"}}
}

func TestCheckEngineCommandKeepsEnginesOnTheLoopback(t *testing.T) {
	good := specswitch.Engine{Name: "drafts-3", URL: "http://127.0.0.1:8101",
		Command: []string{"vllm", "serve", "/weights/m", "--host", "127.0.0.1", "--port", "8101", "--enable-sleep-mode"}}
	if err := checkEngineCommand(good); err != nil {
		t.Fatalf("good command refused: %v", err)
	}
	cases := map[string][]string{
		"no host":       {"vllm", "serve", "m", "--port", "8101", "--enable-sleep-mode"},
		"every address": {"vllm", "serve", "m", "--host", "0.0.0.0", "--port", "8101", "--enable-sleep-mode"},
		"other port":    {"vllm", "serve", "m", "--host", "127.0.0.1", "--port=8102", "--enable-sleep-mode"},
		"no sleep mode": {"vllm", "serve", "m", "--host", "127.0.0.1", "--port", "8101"},
	}
	for name, command := range cases {
		engine := good
		engine.Command = command
		if err := checkEngineCommand(engine); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCommandStartsEnginesSleepsAllButOneAndServes(t *testing.T) {
	three, one := engineFor(t, "drafts-3", 3), engineFor(t, "drafts-1", 1)
	listen := "127.0.0.1:" + freePort(t)
	admin := "127.0.0.1:" + freePort(t)
	cfg := specswitch.Config{Listen: listen, AdminListen: admin, Engines: []specswitch.Engine{three, one},
		Policies: map[string][]specswitch.Band{"throughput": {{UpTo: 8, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, Policy: "throughput"}
	encoded, _ := json.Marshal(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	var out lockedBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"--config-json", string(encoded), "--start-timeout", "30s"}, &out) }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err := http.Get("http://" + listen + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the switch never became healthy:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	response, err := http.Post("http://"+listen+"/v1/completions", "application/json", strings.NewReader(`{"model":"fake/model","prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"drafts-3"`) {
		t.Fatalf("got %d %s, want an answer from drafts-3", response.StatusCode, body)
	}
	sleeping := func(engine specswitch.Engine) bool {
		response, err := http.Get(engine.URL + "/is_sleeping")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var state struct {
			IsSleeping bool `json:"is_sleeping"`
		}
		_ = json.NewDecoder(response.Body).Decode(&state)
		return state.IsSleeping
	}
	if sleeping(three) || !sleeping(one) {
		t.Fatalf("drafts-3 asleep=%t, drafts-1 asleep=%t; want only drafts-1 asleep", sleeping(three), sleeping(one))
	}
	state, err := http.Get("http://" + admin + "/switch/state")
	if err != nil {
		t.Fatal(err)
	}
	state.Body.Close()
	if state.StatusCode != http.StatusOK {
		t.Fatalf("admin state answered %d", state.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run ended with %v", err)
	}
	if _, err := http.Get(three.URL + "/health"); err == nil {
		t.Fatal("an engine still answers after the command stopped")
	}
	if !strings.Contains(out.String(), "[drafts-1] fake engine listening") {
		t.Fatalf("engine output was not passed on with its name:\n%s", out.String())
	}
}

func TestCommandExitsWhenAnEngineCannotStart(t *testing.T) {
	three, one := engineFor(t, "drafts-3", 3), engineFor(t, "drafts-1", 1)
	one.Env = nil // the test binary then runs its tests instead of serving, and exits
	one.Command = []string{"/usr/bin/false", "--host", "127.0.0.1", "--port", strings.TrimPrefix(one.URL, "http://127.0.0.1:"), "--enable-sleep-mode"}
	cfg := specswitch.Config{Listen: "127.0.0.1:" + freePort(t), Engines: []specswitch.Engine{three, one},
		Policies: map[string][]specswitch.Band{"throughput": {{UpTo: 8, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, Policy: "throughput"}
	encoded, _ := json.Marshal(cfg)
	var out lockedBuffer
	err := run(context.Background(), []string{"--config-json", string(encoded), "--start-timeout", "20s"}, &out)
	if err == nil || !strings.Contains(err.Error(), "drafts-1") {
		t.Fatalf("run returned %v, want a failure naming drafts-1", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestCommandSizesEnginesFromWhatTheFirstKeepsAsleep(t *testing.T) {
	three, one, none := engineFor(t, "drafts-3", 3), engineFor(t, "drafts-1", 1), engineFor(t, "drafts-0", 0)
	three.Command = append(three.Command, "--gpu-memory-utilization", specswitch.MemoryShareToken)
	one.Command = append(one.Command, "--gpu-memory-utilization", specswitch.MemoryShareToken)
	none.Command = append(none.Command, "--gpu-memory-utilization", "0.80")
	listen := "127.0.0.1:" + freePort(t)
	cfg := specswitch.Config{Listen: listen, Engines: []specswitch.Engine{three, one, none},
		Policies: map[string][]specswitch.Band{"throughput": {{UpTo: 8, Engine: "drafts-3"}, {Engine: "drafts-1"}}}, Policy: "throughput",
		// One engine asleep keeps 900 MiB of an 81,559 MiB GPU.
		Memory: specswitch.Memory{Probe: []string{"/bin/sh", "-c", "echo '900, 81559'"}, MarginMiB: 512, MaxShare: 0.98}}
	encoded, _ := json.Marshal(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	var out lockedBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"--config-json", string(encoded), "--start-timeout", "30s"}, &out) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run ended with %v", err)
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if response, err := http.Get("http://" + listen + "/health"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the switch never became healthy:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	response, err := http.Post("http://"+listen+"/v1/completions", "application/json", strings.NewReader(`{"model":"fake/model","prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), `"engine":"drafts-3","share":"0.971"`) {
		t.Fatalf("drafts-3 did not start with the measured share: %s\n%s", body, out.String())
	}
	if !strings.Contains(out.String(), `"msg":"memory share"`) || !strings.Contains(out.String(), `"share":0.971`) {
		t.Fatalf("the share was not logged:\n%s", out.String())
	}
}
