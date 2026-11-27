package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// readyProvider starts every candidate at once and records what was deleted.
type readyProvider struct {
	mu      sync.Mutex
	ensured []provision.ReplicaSpec
	deleted []string
}

func (p *readyProvider) Check(context.Context) error { return nil }
func (p *readyProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key}
}
func (p *readyProvider) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensured = append(p.ensured, spec)
	return provision.ProviderHandle{ExternalKey: spec.ExternalKey}, nil
}
func (p *readyProvider) ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error) {
	return provision.Observation{Exists: true, State: "ready", Endpoint: "http://candidate.models.svc:8000"}, nil
}
func (p *readyProvider) DeleteReplica(_ context.Context, handle provision.ProviderHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, handle.ExternalKey)
	return nil
}

// recordedRun is one AIPerf invocation the stand-in received.
type recordedRun struct {
	endpoint string
	requests int
	seed     int64
}

type aiperfStandIn struct {
	mu     sync.Mutex
	runs   []recordedRun
	failOn int // 1-based invocation that fails; 0 never
}

func (a *aiperfStandIn) run(_ context.Context, config benchmark.Config) (benchmark.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runs = append(a.runs, recordedRun{endpoint: config.Endpoint, requests: config.Requests, seed: config.RandomSeed})
	if a.failOn == len(a.runs) {
		return benchmark.Result{}, errors.New("aiperf exited 1")
	}
	throughput := 800.0
	if strings.Contains(config.Endpoint, "candidate") {
		throughput = 880
	}
	ttft := 120.0
	return benchmark.Result{Tool: "aiperf", ToolVersion: "0.12.0", Command: "aiperf profile --url " + config.Endpoint, Requests: config.Requests, Succeeded: config.Requests,
		DurationSeconds: 30, OutputTokenThroughput: &throughput, TTFTP95MS: &ttft}, nil
}

// taskAnswers answers each item of a task set right, except the items listed
// as wrong for the server's model. It is safe for the check's concurrency.
type taskAnswers map[string]map[string]bool

func (t taskAnswers) Complete(_ context.Context, target qualitycheck.Target, _ qualitycheck.EvalSet, prompt qualitycheck.Prompt) (qualitycheck.Answer, error) {
	if t[target.Model][prompt.ID] {
		return qualitycheck.Answer{Content: "The private working stays here.\nAnswer: 0"}, nil
	}
	return qualitycheck.Answer{Content: "The private working stays here.\nAnswer: " + prompt.Expected[0]}, nil
}

func localTaskSet(t *testing.T, items int) qualitycheck.EvalSet {
	t.Helper()
	prompts := make([]map[string]any, 0, items)
	for index := range items {
		prompts = append(prompts, map[string]any{"id": fmt.Sprintf("support.%d", index), "prompt": "PRIVATE-PROMPT-MARKER item " + fmt.Sprint(index), "expected": []string{fmt.Sprint(index + 1)}})
	}
	body, err := json.Marshal(map[string]any{"schema": qualitycheck.EvalSetSchemaV1, "name": "support", "version": "3", "max_tokens": 256, "grading": qualitycheck.GradingAnswerLine, "prompts": prompts})
	if err != nil {
		t.Fatal(err)
	}
	set, err := qualitycheck.DecodeEvalSet(body)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func windowSpec() agentprotocol.WindowSpec {
	return agentprotocol.WindowSpec{
		Endpoint: "support-bot", Label: "campaign window",
		Candidate: &agentprotocol.CandidateSetup{
			Model: "Qwen/Qwen3-8B-FP8",
			Workload: runtimecontract.Workload{Image: "vllm/vllm-openai@sha256:" + strings.Repeat("d", 64), Command: []string{"vllm", "serve", "${MODEL}", "--port", "8000"},
				Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 20},
			RuntimeArgs: []string{"--max-num-seqs", "256"}, Accelerator: "NVIDIA-L4", GPUCount: 1,
		},
		Rounds: 4, Warmup: 16, Load: agentprotocol.CandidateLoad{Requests: 64, Concurrency: 8, InputTokens: 512, OutputTokens: 128, Streaming: true},
		SeedBase: 4096, Quality: agentprotocol.WindowQuality{EvalSet: "local:support", ReferenceSubject: "agentserver-current", CandidateSubject: "agentsetup-0123456789abcdef01234567"},
		TimeoutSecs: 3600,
	}
}

func windowExecutor(provider CandidateProvider, aiperf *aiperfStandIn, set qualitycheck.EvalSet, answers taskAnswers) KubernetesExecutor {
	return KubernetesExecutor{
		Provider: provider, WorkerKey: "worker-key", AllowedImages: []string{"vllm/vllm-openai"}, PollInterval: time.Millisecond, ReadyTimeout: time.Second,
		EvalSets: map[string]qualitycheck.EvalSet{"support": set}, Benchmark: aiperf.run, Completer: answers,
		Now: func() time.Time { return time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC) },
	}
}

var currentSetup = ControlServer{Endpoint: "support-bot", BaseURL: "http://current.models.svc:8000", Model: "Qwen/Qwen3-8B", Token: "server-key"}

// A window warms both servers, then brackets every candidate run with runs of
// the current setup, each with its own seed, scores the task set on both,
// and deletes the candidate. The result holds numbers and digests only.
func TestMeasureWindowBracketsEveryCandidateRunAndSendsScoresOnly(t *testing.T) {
	provider, aiperf := &readyProvider{}, &aiperfStandIn{}
	set := localTaskSet(t, 40)
	answers := taskAnswers{"Qwen/Qwen3-8B": {"support.3": true}, "Qwen/Qwen3-8B-FP8": {"support.3": true, "support.7": true}}
	var phases []string
	result, err := windowExecutor(provider, aiperf, set, answers).MeasureWindow(context.Background(), WindowJob{ID: "window-00000001", Spec: windowSpec(), Control: currentSetup}, func(phase string) { phases = append(phases, phase) })
	if err != nil {
		t.Fatal(err)
	}
	if err = result.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = result.Turns(4, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"current", "candidate"} // warm-ups
	for range 4 {
		want = append(want, "current", "candidate")
	}
	want = append(want, "current")
	if len(aiperf.runs) != len(want) {
		t.Fatalf("%d invocations, want %d", len(aiperf.runs), len(want))
	}
	for index, run := range aiperf.runs {
		if !strings.Contains(run.endpoint, want[index]) || run.seed != 4096+int64(index) {
			t.Fatalf("invocation %d: %+v, want %s with seed %d", index, run, want[index], 4096+index)
		}
		if requests := map[bool]int{true: 16, false: 64}[index < 2]; run.requests != requests {
			t.Fatalf("invocation %d sent %d requests, want %d", index, run.requests, requests)
		}
	}
	if result.Runs[0].Seed != 4098 || result.Runs[1].Arm != agentprotocol.ArmCandidate || *result.Runs[1].OutputTokenThroughput != 880 {
		t.Fatalf("runs: %+v", result.Runs[:2])
	}
	quality := result.Quality
	if quality == nil || quality.Items != 40 || len(quality.ReferenceScores) != 40 || quality.ReferenceScores[3] != 0 || quality.CandidateScores[7] != 0 || quality.CandidateScores[8] != 1 || quality.Grading != qualitycheck.GradingAnswerLine {
		t.Fatalf("quality: %+v", quality)
	}
	if !result.CleanedUp || len(provider.deleted) != 1 || provider.deleted[0] != "agent-candidate-window-00000001" {
		t.Fatalf("candidate not removed: %+v %v", result, provider.deleted)
	}
	if got := strings.Join(phases, ","); got != "launching,waiting_ready,warming_up,measuring,answer_check,cleaning_up" {
		t.Fatalf("phases: %s", got)
	}
	encoded, _ := json.Marshal(agentprotocol.CommandUpdate{State: agentprotocol.CommandSucceeded, Window: &result})
	for _, leak := range []string{"PRIVATE-PROMPT-MARKER", "private working", "current.models.svc", "candidate.models.svc", "server-key", "worker-key", "aiperf profile"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("the window result carries %q: %s", leak, encoded)
		}
	}
}

// A window of the current setup alone starts nothing and scores the current
// setup's own items.
func TestMeasureWindowOfTheCurrentSetupAloneStartsNothing(t *testing.T) {
	provider, aiperf := &readyProvider{}, &aiperfStandIn{}
	spec := windowSpec()
	spec.Candidate, spec.Quality.CandidateSubject = nil, spec.Quality.ReferenceSubject
	result, err := windowExecutor(provider, aiperf, localTaskSet(t, 10), taskAnswers{"Qwen/Qwen3-8B": {"support.1": true}}).MeasureWindow(context.Background(), WindowJob{ID: "window-00000002", Spec: spec, Control: currentSetup}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if err = result.Turns(4, false); err != nil {
		t.Fatal(err)
	}
	if len(provider.ensured) != 0 || len(aiperf.runs) != 5 {
		t.Fatalf("ensured=%d invocations=%d", len(provider.ensured), len(aiperf.runs))
	}
	if q := result.Quality; q == nil || q.ReferenceScores[1] != 0 || q.CandidateScores[1] != 0 || q.CandidateScores[2] != 1 {
		t.Fatalf("quality: %+v", result.Quality)
	}
}

// A run that fails ends the window with what was measured, and the candidate
// is still deleted; a refused image applies nothing.
func TestMeasureWindowFailsWithItsRunsAndCleansUp(t *testing.T) {
	provider, aiperf := &readyProvider{}, &aiperfStandIn{failOn: 6}
	_, err := windowExecutor(provider, aiperf, localTaskSet(t, 4), taskAnswers{}).MeasureWindow(context.Background(), WindowJob{ID: "window-00000003", Spec: windowSpec(), Control: currentSetup}, func(string) {})
	var failure *CandidateError
	if !errors.As(err, &failure) || failure.Code != "candidate_measurement_failed" || failure.Window == nil || len(failure.Window.Runs) != 3 {
		t.Fatalf("failure: %v %+v", err, failure)
	}
	if !failure.Window.CleanedUp || len(provider.deleted) != 1 {
		t.Fatalf("candidate left behind after a failed run: %v", provider.deleted)
	}
	refused := &readyProvider{}
	spec := windowSpec()
	spec.Candidate.Workload.Image = "docker.io/library/busybox@sha256:" + strings.Repeat("e", 64)
	if _, err = windowExecutor(refused, &aiperfStandIn{}, localTaskSet(t, 4), taskAnswers{}).MeasureWindow(context.Background(), WindowJob{ID: "window-00000004", Spec: spec, Control: currentSetup}, func(string) {}); !errors.As(err, &failure) || failure.Code != "image_not_allowed" {
		t.Fatalf("refused image: %v", err)
	}
	if len(refused.ensured) != 0 || len(refused.deleted) != 0 {
		t.Fatalf("a refused image touched the cluster: %+v", refused)
	}
}

// placedProvider also reads where pods run, as provision.Kubernetes does.
type placedProvider struct{ *readyProvider }

func (placedProvider) ReplicaNodes(context.Context, provision.ProviderHandle) ([]string, error) {
	return []string{"ip-10-0-1-23.ec2.internal"}, nil
}
func (placedProvider) ServiceNodes(_ context.Context, namespace, service string) ([]string, error) {
	if namespace != "models" || service != "current" {
		return nil, errors.New("not found")
	}
	return []string{"ip-10-0-9-77.ec2.internal", "ip-10-0-9-77.ec2.internal"}, nil
}

// Each run names its server's node by a keyed digest, never by name; the
// GPU stays unknown; without the key the node is unknown too.
func TestMeasureWindowNamesNodesByDigestOnly(t *testing.T) {
	executor := windowExecutor(placedProvider{&readyProvider{}}, &aiperfStandIn{}, localTaskSet(t, 4), taskAnswers{})
	executor.NodeKey = []byte("local-hash-key")
	result, err := executor.MeasureWindow(context.Background(), WindowJob{ID: "window-00000005", Spec: windowSpec(), Control: currentSetup}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	control, candidate := result.Runs[0].Node, result.Runs[1].Node
	if !strings.HasPrefix(control, "node-") || !strings.HasPrefix(candidate, "node-") || control == candidate || result.Runs[2].Node != control || result.Runs[1].GPU != "unknown" {
		t.Fatalf("nodes: %+v", result.Runs[:3])
	}
	if encoded, _ := json.Marshal(result); strings.Contains(string(encoded), "ec2.internal") {
		t.Fatalf("a node name left the cluster: %s", encoded)
	}
	executor.NodeKey = nil
	if result, err = executor.MeasureWindow(context.Background(), WindowJob{ID: "window-00000006", Spec: windowSpec(), Control: currentSetup}, func(string) {}); err != nil || result.Runs[0].Node != "unknown" || result.Runs[1].Node != "unknown" {
		t.Fatalf("without a key: %+v %v", result.Runs[:2], err)
	}
}
