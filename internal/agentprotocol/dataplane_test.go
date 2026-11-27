package agentprotocol

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

func validSnapshot(now time.Time) DataPlaneSnapshot {
	return DataPlaneSnapshot{
		Generation: "sha256:" + strings.Repeat("a", 64), ValidUntil: now.Add(DataPlaneValidity), TenantID: "tenant-a",
		Routes: []DataPlaneRoute{{Endpoint: "support-bot", EndpointID: "e1", LogicalModelID: "m1", EnvironmentID: "env", ServingPlanID: "p1", RoutingPolicy: "manual",
			Bindings: []DataPlaneBinding{{BindingID: "b1", TargetID: "t1", ServerKey: "vllm-8000", UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Weight: 100, Capabilities: runtimecontract.ProtocolCapabilities{ChatCompletions: true}}}}},
		Credentials: []DataPlaneCredential{{Hash: strings.Repeat("b", 64), PrincipalID: "p1", Role: "viewer", Scopes: []string{}, EndpointNames: []string{}}},
		Admission:   []DataPlaneAdmission{{Endpoint: "support-bot", MaxConcurrency: 4, MaxQueueDepth: 8, QueueTimeoutMS: 1000, MaxRequestBytes: 1024, MaxOutputTokens: 512}},
	}
}

func TestDataPlaneSnapshotValidationKeepsTheAgentOnItsOwnServers(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if err := validSnapshot(now).Validate(now); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*DataPlaneSnapshot){
		"expired":            func(s *DataPlaneSnapshot) { s.ValidUntil = now.Add(-time.Second) },
		"too far out":        func(s *DataPlaneSnapshot) { s.ValidUntil = now.Add(48 * time.Hour) },
		"bad generation":     func(s *DataPlaneSnapshot) { s.Generation = "v2" },
		"address as key":     func(s *DataPlaneSnapshot) { s.Routes[0].Bindings[0].ServerKey = "http://evil.example/v1" },
		"unknown policy":     func(s *DataPlaneSnapshot) { s.Routes[0].RoutingPolicy = "random" },
		"no bindings":        func(s *DataPlaneSnapshot) { s.Routes[0].Bindings = nil },
		"plaintext key":      func(s *DataPlaneSnapshot) { s.Credentials[0].Hash = "ic_live_key" },
		"repeated endpoint":  func(s *DataPlaneSnapshot) { s.Routes = append(s.Routes, s.Routes[0]) },
		"zero concurrency":   func(s *DataPlaneSnapshot) { s.Admission[0].MaxConcurrency = 0 },
		"negative quota":     func(s *DataPlaneSnapshot) { s.RequestQuota = &DataPlaneQuota{MaxRequestsPerMinute: -1} },
		"weight out of band": func(s *DataPlaneSnapshot) { s.Routes[0].Bindings[0].Weight = 101 },
	}
	for name, mutate := range cases {
		snapshot := validSnapshot(now)
		mutate(&snapshot)
		if err := snapshot.Validate(now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	unchanged := DataPlaneSnapshot{Generation: "sha256:" + strings.Repeat("c", 64), Unchanged: true, ValidUntil: now.Add(time.Minute), TenantID: "tenant-a"}
	if err := unchanged.Validate(now); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
}

func TestQuotaLeaseIsBoundedToTheCurrentMinute(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 30, 0, time.UTC)
	window := now.Truncate(time.Minute)
	if err := (QuotaLeaseRequest{Window: window, Requested: 32}).Validate(now); err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]QuotaLeaseRequest{
		"zero":       {Window: window, Requested: 0},
		"too large":  {Window: window, Requested: MaxQuotaLease + 1},
		"unaligned":  {Window: now, Requested: 1},
		"old window": {Window: window.Add(-10 * time.Minute), Requested: 1},
	} {
		if err := request.Validate(now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCandidateRunSpecCannotCarryAShellOrUnboundedLoad(t *testing.T) {
	spec := CandidateRunSpec{
		Endpoint: "support-bot", Label: "parallel 8", Model: "Qwen/Qwen3-0.6B",
		Workload:    runtimecontract.Workload{Image: "ghcr.io/ggml-org/llama.cpp@sha256:" + strings.Repeat("d", 64), Command: []string{"/app/llama-server", "--port", "8000"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 20},
		Accelerator: "SIMULATED-CPU", GPUCount: 1, Load: CandidateLoad{Requests: 20, Concurrency: 2, InputTokens: 64, OutputTokens: 16},
		Quality: CandidateQuality{EvalSet: "local:support"}, TimeoutSecs: 900,
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*CandidateRunSpec){
		"shell":            func(s *CandidateRunSpec) { s.Workload.Command = []string{"/bin/bash", "-c", "id"} },
		"python":           func(s *CandidateRunSpec) { s.Workload.Command = []string{"python3", "-c", "print(1)"} },
		"mutable image":    func(s *CandidateRunSpec) { s.Workload.Image = "ghcr.io/ggml-org/llama.cpp:latest" },
		"substitution":     func(s *CandidateRunSpec) { s.RuntimeArgs = []string{"$(cat /etc/passwd)"} },
		"multiline arg":    func(s *CandidateRunSpec) { s.RuntimeArgs = []string{"--a\n--b"} },
		"huge load":        func(s *CandidateRunSpec) { s.Load.Requests = MaxCandidateRequests + 1 },
		"eval set path":    func(s *CandidateRunSpec) { s.Quality.EvalSet = "/etc/shadow" },
		"remote eval set":  func(s *CandidateRunSpec) { s.Quality.EvalSet = "https://example.com/set.json" },
		"no gpus":          func(s *CandidateRunSpec) { s.GPUCount = 0 },
		"endless":          func(s *CandidateRunSpec) { s.TimeoutSecs = 7 * 24 * 3600 },
		"label with quote": func(s *CandidateRunSpec) { s.Label = `x"; drop` },
	}
	for name, mutate := range cases {
		candidate := spec
		candidate.Workload.Command = append([]string(nil), spec.Workload.Command...)
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCandidateResultCarriesNumbersOnly(t *testing.T) {
	ok := 12.5
	result := CandidateRunResult{
		Control: &LoadMeasurement{Tool: "aiperf", Requests: 10, Succeeded: 10, OutputTokenThroughput: &ok},
		Quality: &QualityMeasurement{EvalSet: "default", EvalSetDigest: "sha256:" + strings.Repeat("e", 64), Prompts: 2, Scores: []float64{1, 0.5}, Score: 0.75, Verdict: "WATCH", ResultDigest: "sha256:" + strings.Repeat("f", 64)},
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := result
	quality := *result.Quality
	quality.Scores = []float64{1}
	bad.Quality = &quality
	if bad.Validate() == nil {
		t.Fatal("one score per prompt was not enforced")
	}
	negative := -1.0
	bad = result
	bad.Control = &LoadMeasurement{Tool: "aiperf", TTFTP95MS: &negative}
	if bad.Validate() == nil {
		t.Fatal("a negative latency was accepted")
	}
	if (CommandUpdate{State: CommandFailed}).Validate() == nil {
		t.Fatal("a failure without a code was accepted")
	}
	if (CommandUpdate{State: CommandRunning, FailureCode: "x"}).Validate() == nil {
		t.Fatal("a running update with a failure code was accepted")
	}
}

// A candidate's extra arguments to a vLLM or SGLang server are a reviewed
// set, and a few flags are refused in any command.
func TestCandidateRunSpecChecksEngineArguments(t *testing.T) {
	base := func(command []string, args ...string) CandidateRunSpec {
		return CandidateRunSpec{
			Endpoint: "support-bot", Label: "candidate", Model: "Qwen/Qwen3-8B",
			Workload:    runtimecontract.Workload{Image: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), Command: command, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 20},
			RuntimeArgs: args, Accelerator: "H100", GPUCount: 1, Load: CandidateLoad{Requests: 20, Concurrency: 2, InputTokens: 64, OutputTokens: 16},
			Quality: CandidateQuality{EvalSet: "local:support"}, TimeoutSecs: 900,
		}
	}
	vllm := []string{"vllm", "serve", "Qwen/Qwen3-8B", "--port", "8000"}
	// The agent refuses interpreters, so its SGLang candidates use `sglang serve`.
	sglang := []string{"sglang", "serve", "--model-path", "Qwen/Qwen3-8B"}
	for name, spec := range map[string]CandidateRunSpec{
		"vLLM batch":      base(vllm, "--max-num-seqs", "256"),
		"SGLang policy":   base(sglang, "--schedule-policy", "lpm"),
		"other server":    base([]string{"/app/llama-server", "--port", "8000"}, "--parallel", "8"),
		"product command": base(vllm),
	} {
		if err := spec.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, test := range map[string]struct {
		spec CandidateRunSpec
		want string
	}{
		"remote code":       {base(vllm, "--trust-remote-code"), "--trust-remote-code is refused"},
		"logit processor":   {base(sglang, "--enable-custom-logit-processor"), "is refused"},
		"in the command":    {base(append(append([]string{}, sglang...), "--enable-custom-logit-processor")), "workload: engine argument --enable-custom-logit-processor is refused"},
		"remote code, any":  {base([]string{"/app/llama-server"}, "--trust-remote-code"), "is refused"},
		"unreviewed vLLM":   {base(vllm, "--tool-parser-plugin", "/models/p.py"), "--tool-parser-plugin is not reviewed for vllm"},
		"unreviewed SGLang": {base(sglang, "--api-key", "x"), "--api-key is not reviewed for sglang"},
	} {
		if err := test.spec.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: err=%v, want %q", name, err, test.want)
		}
	}
}
