package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/testtools/fakeengine"
)

func TestProbeIdentifiesEachLocalServerAndKeepsUnknownsExplicit(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		options        fakeengine.Options
		engine         string
		version        string
		metrics        bool
		selected       string
		metricsMessage string
	}{
		{fakeengine.Options{Flavor: fakeengine.VLLM, Metrics: true}, agentprotocol.EngineVLLM, "0.11.2", true, "Qwen/Qwen3-8B", ""},
		{fakeengine.Options{Flavor: fakeengine.SGLang}, agentprotocol.EngineSGLang, "0.5.4", false, "Qwen/Qwen3-8B", "--enable-metrics"},
		{fakeengine.Options{Flavor: fakeengine.LlamaCPP, Metrics: true}, agentprotocol.EngineLlamaCPP, "b6900-abcdef1", true, "Qwen/Qwen3-8B", ""},
		{fakeengine.Options{Flavor: fakeengine.Ollama, Models: []string{"llama3.2:3b", "qwen3:8b"}, Loaded: "qwen3:8b"}, agentprotocol.EngineOllama, "0.12.10", false, "qwen3:8b", "does not expose engine metrics"},
		{fakeengine.Options{Flavor: fakeengine.LMStudio, Models: []string{"a", "b"}, Loaded: "b"}, agentprotocol.EngineLMStudio, "", false, "b", "does not expose engine metrics"},
	}
	for _, item := range cases {
		engine := fakeengine.Start(item.options)
		server, found := Detector{}.Probe(ctx, ServerConfig{URL: engine.URL}, false)
		engine.Close()
		if !found || server.Engine != item.engine || server.EngineVersion != item.version || server.MetricsAvailable != item.metrics || server.Health != agentprotocol.HealthHealthy {
			t.Fatalf("%s: %+v", item.options.Flavor, server)
		}
		if item.metricsMessage != "" && !strings.Contains(server.MetricsDetail, item.metricsMessage) {
			t.Fatalf("%s metrics detail = %q", item.options.Flavor, server.MetricsDetail)
		}
		if selected, err := SelectModel(server, ""); err != nil || selected != item.selected {
			t.Fatalf("%s selected %q err=%v", item.options.Flavor, selected, err)
		}
		if err := server.Server.Validate(); err != nil {
			t.Fatalf("%s report is invalid: %v", item.options.Flavor, err)
		}
	}
}

func TestProbeExplainsAuthAndMissingServers(t *testing.T) {
	ctx := context.Background()
	locked := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, APIKey: "server-key"})
	defer locked.Close()
	server, found := Detector{}.Probe(ctx, ServerConfig{URL: locked.URL}, false)
	if !found || server.Health != agentprotocol.HealthUnhealthy || !strings.Contains(server.HealthDetail, "--token-env") {
		t.Fatalf("locked server: %+v", server)
	}
	server, _ = Detector{}.Probe(ctx, ServerConfig{URL: locked.URL, AuthHeader: "Bearer server-key"}, false)
	if server.Health != agentprotocol.HealthHealthy || !server.Authenticated {
		t.Fatalf("authenticated server: %+v", server)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	if _, found = (Detector{}).Probe(ctx, ServerConfig{URL: address}, false); found {
		t.Fatal("a closed default port was reported as a server")
	}
	server, found = Detector{}.Probe(ctx, ServerConfig{URL: address}, true)
	if !found || server.Health != agentprotocol.HealthUnhealthy || server.HealthDetail == "" {
		t.Fatalf("explicit missing server: %+v", server)
	}
	ambiguous := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.Ollama, Models: []string{"a", "b"}})
	defer ambiguous.Close()
	server, _ = Detector{}.Probe(ctx, ServerConfig{URL: ambiguous.URL}, false)
	if _, err := SelectModel(server, ""); err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("ambiguous model choice: %v", err)
	}
	if model, err := SelectModel(server, "b"); err != nil || model != "b" {
		t.Fatalf("requested model: %q %v", model, err)
	}
}

func TestNamesAndKeysAreStableAndValid(t *testing.T) {
	for model, want := range map[string]string{"Qwen/Qwen3-8B": "qwen3-8b", "llama3.2:3b": "llama3.2-3b", "qwen3:latest": "qwen3", "///": "local-model", strings.Repeat("a", 80): strings.Repeat("a", 64)} {
		if got := EndpointName(model); got != want || !agentprotocol.ValidEndpointName(got) {
			t.Fatalf("EndpointName(%q)=%q want %q", model, got, want)
		}
	}
	for raw, want := range map[string]string{"http://127.0.0.1:8000": "127.0.0.1-8000", "https://GPU-Box.local": "gpu-box.local-443", "http://[::1]:1234": "ip6-..1-1234"} {
		base, err := NormalizeBaseURL(raw + "/v1/")
		if err != nil {
			t.Fatal(err)
		}
		if got := ServerKey(base); got != want || !agentprotocol.ValidServerKey(got) {
			t.Fatalf("ServerKey(%q)=%q want %q", base, got, want)
		}
	}
	if _, err := NormalizeBaseURL("http://user:pass@host:1"); err == nil {
		t.Fatal("credentials in a server URL were accepted")
	}
	candidates := Candidates([]ServerConfig{{URL: "http://127.0.0.1:8000/v1", AuthHeader: "Bearer x"}}, true)
	if len(candidates) != len(DefaultPorts) || candidates[0].AuthHeader != "Bearer x" {
		t.Fatalf("candidates=%+v", candidates)
	}
	for _, candidate := range candidates[1:] {
		if candidate.AuthHeader != "" {
			t.Fatalf("a default port received the named server's key: %+v", candidate)
		}
	}
}

type fakeRunner map[string]string

func (f fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if output, ok := f[name+" "+strings.Join(args, " ")]; ok {
		return []byte(output), nil
	}
	return nil, exec.ErrNotFound
}

func TestHardwareInventoryReportsVendorsWithExplicitUnknowns(t *testing.T) {
	ctx := context.Background()
	nvidia := HardwareProbe{GOOS: "linux", GOARCH: "amd64", SysRoot: t.TempDir(), Runner: fakeRunner{
		"nvidia-smi --query-gpu=index,uuid,name,memory.total,driver_version --format=csv,noheader,nounits": "0, GPU-a, NVIDIA RTX 4090, 24564, 580.82\n1, GPU-b, NVIDIA RTX 4090, 24564, 580.82\n",
	}}.Inventory(ctx)
	if nvidia.State != "ready" || len(nvidia.Accelerators) != 1 || nvidia.Accelerators[0].Count != 2 || *nvidia.Accelerators[0].MemoryMiB != 24564 {
		t.Fatalf("nvidia: %+v", nvidia)
	}
	apple := HardwareProbe{GOOS: "darwin", GOARCH: "arm64", Runner: fakeRunner{"sysctl -n machdep.cpu.brand_string": "Apple M3 Max\n", "sysctl -n hw.memsize": "137438953472\n"}}.Inventory(ctx)
	if apple.State != "partial" || apple.Accelerators[0].Vendor != "apple" || apple.Accelerators[0].Name != "Apple M3 Max GPU" || *apple.Accelerators[0].MemoryMiB != 131072 || !strings.Contains(strings.Join(apple.Accelerators[0].Unknown, ","), "memory_bandwidth") {
		t.Fatalf("apple: %+v", apple)
	}
	if !strings.Contains(strings.Join(apple.Limitations, " "), "Apple Silicon is reported as detected") {
		t.Fatalf("apple limitations: %v", apple.Limitations)
	}
	sys := t.TempDir()
	for index, vram := range []string{"", "68702699520"} {
		card := filepath.Join(sys, fmt.Sprintf("sys/class/drm/card%d/device", index))
		if err := os.MkdirAll(card, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(card, "vendor"), []byte("0x1002\n"), 0o644)
		_ = os.WriteFile(filepath.Join(card, "class"), []byte("0x038000\n"), 0o644)
		if vram != "" {
			_ = os.WriteFile(filepath.Join(card, "mem_info_vram_total"), []byte(vram), 0o644)
		}
	}
	amd := HardwareProbe{GOOS: "linux", SysRoot: sys, Runner: fakeRunner{"rocm-smi --showproductname --csv": "device,Card series,Card model\ncard0,AMD Instinct MI300X,0x74a1\n"}}.Inventory(ctx)
	if amd.State != "partial" || amd.Accelerators[0].Count != 2 || amd.Accelerators[0].Name != "AMD Instinct MI300X" || amd.Accelerators[0].MemoryMiB != nil || !strings.Contains(strings.Join(amd.Accelerators[0].Unknown, ","), "memory_mib") {
		t.Fatalf("amd: %+v", amd)
	}
	none := HardwareProbe{GOOS: "linux", SysRoot: t.TempDir(), Runner: fakeRunner{}}.Inventory(ctx)
	if none.State != "unavailable" || len(none.Accelerators) != 0 {
		t.Fatalf("none: %+v", none)
	}
	for _, hardware := range []agentprotocol.Hardware{nvidia, apple, amd, none} {
		request := agentprotocol.SyncRequest{Protocol: agentprotocol.Version, AgentVersion: "1", OS: "x", Arch: "y", SentAt: time.Now(), Hardware: hardware}
		if err := request.Validate(time.Now()); err != nil {
			t.Fatalf("inventory %+v is not a valid report: %v", hardware, err)
		}
	}
}

func TestMetricWindowsRelayEachScrapeOnceAndResetOnRestart(t *testing.T) {
	windows := newMetricWindows()
	start := time.Unix(1_800_000_000, 0)
	first := "vllm:num_requests_running 1\nvllm:prompt_tokens_total 100\n"
	if err := windows.Observe("k", first, start); err != nil {
		t.Fatal(err)
	}
	relays, sent := windows.Relays(start.Add(time.Second))
	if len(relays) != 1 || relays[0].Baseline != nil || relays[0].Current.AgeMS != 1000 {
		t.Fatalf("first relay: %+v", relays)
	}
	windows.Sent(sent)
	if relays, _ = windows.Relays(start.Add(2 * time.Second)); len(relays) != 0 {
		t.Fatalf("a delivered scrape was relayed again: %+v", relays)
	}
	_ = windows.Observe("k", "vllm:num_requests_running 2\nvllm:prompt_tokens_total 150\n", start.Add(30*time.Second))
	relays, sent = windows.Relays(start.Add(31 * time.Second))
	if len(relays) != 1 || relays[0].Baseline == nil || relays[0].Baseline.Text != first {
		t.Fatalf("window relay: %+v", relays)
	}
	// A scrape taken while the relay was in flight stays pending.
	_ = windows.Observe("k", "vllm:num_requests_running 2\nvllm:prompt_tokens_total 160\n", start.Add(60*time.Second))
	windows.Sent(sent)
	if relays, _ = windows.Relays(start.Add(61 * time.Second)); len(relays) != 1 {
		t.Fatalf("newer scrape dropped: %+v", relays)
	}
	// A counter going backwards is a restarted engine: no baseline.
	_ = windows.Observe("k", "vllm:num_requests_running 0\nvllm:prompt_tokens_total 5\n", start.Add(90*time.Second))
	if relays, _ = windows.Relays(start.Add(91 * time.Second)); relays[0].Baseline != nil {
		t.Fatalf("restart kept a baseline: %+v", relays[0])
	}
	// Scrapes older than the trailing window fall out of the baseline.
	for index := 1; index <= 12; index++ {
		_ = windows.Observe("k", fmt.Sprintf("vllm:num_requests_running 0\nvllm:prompt_tokens_total %d\n", 5+index), start.Add(90*time.Second+time.Duration(index)*30*time.Second))
	}
	relays, _ = windows.Relays(start.Add(500 * time.Second))
	if relays[0].Baseline == nil || relays[0].Baseline.AgeMS-relays[0].Current.AgeMS > int64(5*time.Minute/time.Millisecond) {
		t.Fatalf("baseline outside the trailing window: %+v", relays[0])
	}
	if err := windows.Observe("k", "process_open_fds 1\n", start); err == nil {
		t.Fatal("a payload without engine metrics was kept")
	}
}

func TestGatewayRecordsShapesWithoutContent(t *testing.T) {
	engine := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM})
	defer engine.Close()
	shapes := &shapeBuffer{}
	gateway := &Gateway{Upstream: engine.URL, ServerKey: "127.0.0.1-8000", HashKey: []byte("machine-secret"), Shapes: shapes}
	handler, err := gateway.Handler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	for _, body := range []string{
		`{"model":"Qwen/Qwen3-8B","messages":[{"role":"system","content":"shared"},{"role":"user","content":"a"}]}`,
		`{"model":"Qwen/Qwen3-8B","stream":true,"messages":[{"role":"system","content":"shared"},{"role":"user","content":"b"}]}`,
	} {
		response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("post: %v %v", response, err)
		}
		// Read to the end: closing a stream early is a client cancellation.
		_, _ = io.ReadAll(response.Body)
		response.Body.Close()
	}
	if response, err := http.Get(server.URL + "/v1/models"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("models pass-through: %v %v", response, err)
	}
	// Each shape is recorded after its response is written, so two
	// back-to-back requests may be recorded in either order.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending, _ := shapes.stats(); pending >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	recorded := shapes.take(10)
	if len(recorded) != 2 {
		t.Fatalf("shapes: %+v", recorded)
	}
	streamed := 0
	for _, shape := range recorded {
		if shape.Streaming {
			streamed++
		}
	}
	if recorded[0].SharedPrefixHash == "" || recorded[0].SharedPrefixHash != recorded[1].SharedPrefixHash || streamed != 1 {
		t.Fatalf("hashes and streaming: %+v", recorded)
	}
	for _, shape := range recorded {
		if *shape.InputTokens != 12 || *shape.OutputTokens != 5 || shape.TTFTMS == nil || shape.Operation != "chat" || shape.RequestModel != "Qwen/Qwen3-8B" {
			t.Fatalf("shape: %+v", shape)
		}
		if err := shape.Validate(); err != nil {
			t.Fatalf("shape invalid: %v", err)
		}
	}
	if other := (&Gateway{HashKey: []byte("another machine")}).digest([]byte("x")); other == (&Gateway{HashKey: []byte("machine-secret")}).digest([]byte("x")) {
		t.Fatal("hashes do not depend on the machine's secret")
	}
	engine.Close()
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream down: %v %v", response, err)
	}
	response.Body.Close()
	for wait := time.Now().Add(5 * time.Second); time.Now().Before(wait); time.Sleep(5 * time.Millisecond) {
		if pending, _ := shapes.stats(); pending >= 1 {
			break
		}
	}
	if failed := shapes.take(10); len(failed) != 1 || failed[0].ErrorType != "upstream_unreachable" || failed[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("failure shape: %+v", failed)
	}
}

func TestShapeBufferIsBounded(t *testing.T) {
	buffer := &shapeBuffer{}
	for index := 0; index < maxBufferedShapes+5; index++ {
		buffer.add(agentprotocol.TrafficShape{ID: fmt.Sprint(index)})
	}
	pending, dropped := buffer.stats()
	if pending != maxBufferedShapes || dropped != 5 {
		t.Fatalf("pending=%d dropped=%d", pending, dropped)
	}
	taken := buffer.take(3)
	buffer.restore(taken)
	if again := buffer.take(1); again[0].ID != "5" {
		t.Fatalf("restore order: %+v", again)
	}
}

func TestClientRefusesPlainHTTPAndMapsRefusals(t *testing.T) {
	for raw, ok := range map[string]bool{"https://control.example.test": true, "http://127.0.0.1:8080": true, "http://localhost:8080": true, "http://control.example.test": false, "ftp://x": false} {
		if err := CheckControlURL(raw); (err == nil) != ok {
			t.Fatalf("CheckControlURL(%s) = %v", raw, err)
		}
	}
	responses := map[string]struct {
		status int
		body   string
		want   error
	}{
		"revoked":  {http.StatusUnauthorized, `{"error":{"code":"agent_revoked"}}`, ErrRevoked},
		"unknown":  {http.StatusUnauthorized, `{"error":{"code":"agent_unauthenticated"}}`, ErrUnauthenticated},
		"upgrade":  {http.StatusUpgradeRequired, `{"error":{"code":"upgrade_required","message":"too old"}}`, ErrUpgradeRequired},
		"overload": {http.StatusServiceUnavailable, `{"error":{"code":"busy"}}`, nil},
	}
	for name, item := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Authorization") != "Bearer ic_key" {
				t.Errorf("request headers: %v", r.Header)
			}
			w.WriteHeader(item.status)
			_, _ = w.Write([]byte(item.body))
		}))
		_, err := Client{ControlURL: server.URL, Key: "ic_key", HTTP: server.Client()}.Sync(context.Background(), agentprotocol.SyncRequest{})
		server.Close()
		if item.want != nil && !errors.Is(err, item.want) {
			t.Fatalf("%s: %v", name, err)
		}
		var apiErr *APIError
		if item.want == nil && (!errors.As(err, &apiErr) || fatal(err)) {
			t.Fatalf("%s should be retryable: %v", name, err)
		}
	}
	if backoff(15*time.Second, 1) != 15*time.Second || backoff(15*time.Second, 10) != 5*time.Minute {
		t.Fatal("backoff bounds")
	}
}

func TestGatewayRecordsCancelledStreamsAndCleansClientText(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)
	shapes := &shapeBuffer{}
	handler, err := (&Gateway{Upstream: upstream.URL, ServerKey: "127.0.0.1-8000", HashKey: []byte("k"), Shapes: shapes}).Handler()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader("{\"model\":\"qwen\\tx\\n\",\"stream\":true,\"messages\":[]}"))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	_, _ = response.Body.Read(buffer)
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending, _ := shapes.stats(); pending > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	recorded := shapes.take(10)
	if len(recorded) != 1 || recorded[0].ErrorType != "client_cancelled" || !recorded[0].Streaming || recorded[0].RequestModel != "qwenx" {
		t.Fatalf("cancelled stream: %+v", recorded)
	}
	if err := recorded[0].Validate(); err != nil {
		t.Fatalf("recorded shape would be refused: %v", err)
	}
	huge := 20_000_000
	if input, output := boundedTokens(&huge, nil); input != nil || output != nil {
		t.Fatal("an implausible token count was kept")
	}
}

func TestRelaysStayWithinTheSyncBudget(t *testing.T) {
	big := strings.Repeat("vllm:num_requests_running 1\n", (agentprotocol.MaxExpositionBytes-1)/28)
	relays := []agentprotocol.MetricRelay{}
	relayed := map[string]time.Time{}
	for index := 0; index < 8; index++ {
		key := fmt.Sprintf("127.0.0.1-%d", 8000+index)
		relays = append(relays, agentprotocol.MetricRelay{ServerKey: key, Current: agentprotocol.Exposition{AgeMS: 1, Text: big}, Baseline: &agentprotocol.Exposition{AgeMS: 2, Text: big}})
		relayed[key] = time.Unix(int64(index), 0)
	}
	relays = append(relays, agentprotocol.MetricRelay{ServerKey: "127.0.0.1-9999", Current: agentprotocol.Exposition{AgeMS: 1, Text: big + big}})
	relayed["127.0.0.1-9999"] = time.Unix(99, 0)
	kept := withinBudget(relays, relayed)
	total := 0
	for _, relay := range kept {
		total += len(relay.Current.Text)
		if relay.Baseline != nil {
			total += len(relay.Baseline.Text)
		}
		if relay.ServerKey == "127.0.0.1-9999" {
			t.Fatal("an oversized scrape was relayed")
		}
	}
	if total > relayBudget || len(kept) == 0 {
		t.Fatalf("kept %d relays, %d bytes", len(kept), total)
	}
	if _, ok := relayed["127.0.0.1-9999"]; !ok {
		t.Fatal("an oversized scrape would be retried forever")
	}
	if len(relayed) != len(kept)+1 {
		t.Fatalf("relays left out must stay pending: relayed=%d kept=%d", len(relayed), len(kept))
	}
}

func TestDetectedTextIsCleanedOnCharacterBoundaries(t *testing.T) {
	value := strings.Repeat("a", 255) + "é\x01"
	cleaned := boundedDetail(value)
	if len(cleaned) > agentprotocol.MaxText || !utf8.ValidString(cleaned) || strings.ContainsRune(cleaned, 0x01) {
		t.Fatalf("cleaned=%q (%d bytes)", cleaned, len(cleaned))
	}
	engine := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"good/model", "bad\tmodel"}})
	defer engine.Close()
	server, _ := Detector{}.Probe(context.Background(), ServerConfig{URL: engine.URL}, false)
	if len(server.Models) != 1 || server.Models[0] != "good/model" || server.Validate() != nil {
		t.Fatalf("models=%v", server.Models)
	}
}
