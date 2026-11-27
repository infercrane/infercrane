package agentprotocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

func validWindow() WindowSpec {
	return WindowSpec{
		Endpoint: "support-bot", Label: "campaign c1 window 1",
		Candidate: &CandidateSetup{
			Model: "Qwen/Qwen3-0.6B",
			Workload: runtimecontract.Workload{Image: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), Command: []string{"vllm", "serve", "Qwen/Qwen3-0.6B", "--port", "8000"},
				Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 20},
			RuntimeArgs: []string{"--max-num-seqs", "256"}, Accelerator: "L4", GPUCount: 1,
		},
		Rounds: 4, Warmup: 16, Load: CandidateLoad{Requests: 64, Concurrency: 8, InputTokens: 512, OutputTokens: 128, Streaming: true},
		SeedBase: 1 << 20, Quality: WindowQuality{EvalSet: "local:support", ReferenceSubject: "agentserver-current", CandidateSubject: "agentsetup-0123456789abcdef01234567", Divergence: true}, TimeoutSecs: 3600,
	}
}

func TestWindowSpecIsBoundedLikeACandidateRun(t *testing.T) {
	if err := validWindow().Validate(); err != nil {
		t.Fatal(err)
	}
	alone := validWindow()
	alone.Candidate, alone.Quality.Divergence, alone.Quality.CandidateSubject = nil, false, alone.Quality.ReferenceSubject
	if err := alone.Validate(); err != nil {
		t.Fatalf("the current setup alone: %v", err)
	}
	cases := map[string]func(*WindowSpec){
		"shell":             func(s *WindowSpec) { s.Candidate.Workload.Command = []string{"/bin/sh", "-c", "id"} },
		"mutable image":     func(s *WindowSpec) { s.Candidate.Workload.Image = "vllm/vllm-openai:latest" },
		"unreviewed flag":   func(s *WindowSpec) { s.Candidate.RuntimeArgs = []string{"--trust-remote-code"} },
		"substitution":      func(s *WindowSpec) { s.Candidate.RuntimeArgs = []string{"$(id)"} },
		"no rounds":         func(s *WindowSpec) { s.Rounds = 0 },
		"too many rounds":   func(s *WindowSpec) { s.Rounds = MaxWindowRounds + 1 },
		"huge warm-up":      func(s *WindowSpec) { s.Warmup = MaxWindowWarmup + 1 },
		"huge load":         func(s *WindowSpec) { s.Load.Concurrency = MaxCandidateConcurrency + 1 },
		"negative seed":     func(s *WindowSpec) { s.SeedBase = -1 },
		"seed past 32 bits": func(s *WindowSpec) { s.SeedBase = 1 << 31 },
		"negative limit":    func(s *WindowSpec) { s.TTFTLimitMS = -1 },
		"remote eval set":   func(s *WindowSpec) { s.Quality.EvalSet = "https://example.com/set.json" },
		"fresh without one": func(s *WindowSpec) {
			s.Candidate, s.Fresh, s.Quality.Divergence, s.Quality.Skip = nil, true, false, true
		},
		"subject with space": func(s *WindowSpec) { s.Quality.CandidateSubject = "candidate one" },
		"same subjects":      func(s *WindowSpec) { s.Quality.CandidateSubject = s.Quality.ReferenceSubject },
		"divergence alone":   func(s *WindowSpec) { s.Candidate = nil },
		"divergence no eval": func(s *WindowSpec) { s.Quality.Skip = true },
		"endless":            func(s *WindowSpec) { s.TimeoutSecs = 7 * 24 * 3600 },
		"endpoint":           func(s *WindowSpec) { s.Endpoint = "Support Bot" },
	}
	for name, mutate := range cases {
		spec := validWindow()
		candidate := *spec.Candidate
		spec.Candidate = &candidate
		mutate(&spec)
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func windowResult() WindowResult {
	value := func(v float64) *float64 { return &v }
	runs := make([]WindowRun, 0, 9)
	for index := range 9 {
		arm := ArmControl
		if index%2 == 1 {
			arm = ArmCandidate
		}
		runs = append(runs, WindowRun{Arm: arm, Seed: int64(1<<20 + index), Requests: 64, Succeeded: 64, DurationSeconds: 30, OutputTokenThroughput: value(800), TTFTP95MS: value(120), TPOTP95MS: value(20)})
	}
	return WindowResult{
		Tool: "aiperf", ToolVersion: "0.12.0", Runs: runs, ReadySeconds: 41.5, ImageDigest: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), CleanedUp: true,
		Quality: &WindowAnswers{
			EvalSet: "local:support", EvalSetName: "support", EvalSetVersion: "1", EvalSetDigest: "sha256:" + strings.Repeat("b", 64),
			PairingDigest: "sha256:" + strings.Repeat("c", 64), Grading: "answer_line", Items: 3, ItemsAtOnce: 32,
			ReferenceScores: []float64{1, 0, 1}, CandidateScores: []float64{1, 1, 1},
			Divergence:   &qualityevidence.Divergence{Method: "forced_decoding_kl", TopK: 16, Prompts: 3, GenerationMeanKL: 0.01},
			ResultDigest: "sha256:" + strings.Repeat("d", 64), EvaluatedAt: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
		},
	}
}

func TestWindowResultCarriesNumbersAndTurnsOnly(t *testing.T) {
	result := windowResult()
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := result.Turns(4, true); err != nil {
		t.Fatal(err)
	}
	if result.Turns(3, true) == nil {
		t.Fatal("a window with the wrong number of rounds passed")
	}
	if result.Turns(9, false) == nil {
		t.Fatal("candidate runs passed as a window of the current setup alone")
	}
	swapped := windowResult()
	swapped.Runs[1].Arm, swapped.Runs[2].Arm = ArmControl, ArmCandidate
	if swapped.Turns(4, true) == nil {
		t.Fatal("two control runs in a row passed as a bracketed window")
	}
	bad := map[string]func(*WindowResult){
		"arm":              func(r *WindowResult) { r.Runs[0].Arm = "copy" },
		"negative latency": func(r *WindowResult) { negative := -1.0; r.Runs[2].TTFTP95MS = &negative },
		"too many runs":    func(r *WindowResult) { r.Runs = append(r.Runs, make([]WindowRun, 2*MaxWindowRounds)...) },
		"mutable image":    func(r *WindowResult) { r.ImageDigest = "vllm/vllm-openai:latest" },
		"score per item":   func(r *WindowResult) { r.Quality.CandidateScores = []float64{1} },
		"reference count":  func(r *WindowResult) { r.Quality.ReferenceScores = []float64{1, 1} },
		"score above one":  func(r *WindowResult) { r.Quality.CandidateScores = []float64{1, 2, 1} },
		"digest":           func(r *WindowResult) { r.Quality.ResultDigest = "answers" },
		"grading":          func(r *WindowResult) { r.Quality.Grading = "Answer Line" },
		"eval set version": func(r *WindowResult) { r.Quality.EvalSetVersion = "" },
		"broken > cases": func(r *WindowResult) {
			r.Quality.ToolCalls = &qualityevidence.ToolCallScores{Cases: 1, Broken: 2, Score: 1}
		},
		"divergence is nan": func(r *WindowResult) { nan := 0.0; r.Quality.Divergence.GenerationMaxKL = nan / nan },
		"no time":           func(r *WindowResult) { r.Quality.EvaluatedAt = time.Time{} },
	}
	for name, mutate := range bad {
		result := windowResult()
		quality := *result.Quality
		divergence := *quality.Divergence
		quality.Divergence = &divergence
		result.Quality = &quality
		result.Runs = append([]WindowRun(nil), result.Runs...)
		mutate(&result)
		if err := result.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The wire form of a window result names numbers, digests, and eval-set
// identities only. A field that could carry text from the cluster (a prompt,
// an answer, an address, a node, a log line) would show up here.
func TestWindowResultWireFieldsAreContentFree(t *testing.T) {
	encoded, err := json.Marshal(CommandUpdate{State: CommandSucceeded, Window: func() *WindowResult { r := windowResult(); return &r }()})
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				fields = append(fields, key)
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	var decoded any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	walk(decoded)
	for _, field := range fields {
		for _, forbidden := range []string{"prompt", "answer", "message", "content", "address", "url", "host", "node", "log", "header", "text"} {
			if strings.Contains(field, forbidden) && field != "prompt_set" && field != "prompt_set_version" && field != "prompts" {
				t.Errorf("wire field %q could carry content (%s)", field, forbidden)
			}
		}
	}
	if (CommandUpdate{State: CommandSucceeded, Result: &CandidateRunResult{}, Window: &WindowResult{}}).Validate() == nil {
		t.Fatal("an update carrying both results was accepted")
	}
	if err = (CommandUpdate{State: CommandSucceeded, Window: &WindowResult{}}).Validate(); err != nil {
		t.Fatalf("an empty window result (no runs, as a cancelled window reports) was refused: %v", err)
	}
}
