package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

// KubernetesExecutor runs candidate runs in the agent's own cluster. It
// starts the candidate with the same namespaced Kubernetes provider the
// control plane uses (provision.Kubernetes: one Deployment and one Service,
// applied server-side), measures the current setup, the candidate, and the
// current setup again with AIPerf, runs the answer check on both, and
// deletes the candidate. Prompts, answers, and engine logs never leave the
// cluster; only the numbers in the result do.
type KubernetesExecutor struct {
	// Provider is provision.Kubernetes in production.
	Provider CandidateProvider
	// WorkerKey is the value of the Secret the provider mounts into candidate
	// pods. The agent uses it to call the candidate.
	WorkerKey string
	// AllowedImages lists image repositories (or exact references) the
	// operator allows candidates to use. A command naming any other image is
	// refused before anything is applied.
	AllowedImages []string
	// AllowedAccelerators, when set, lists the accelerator names candidates
	// may request.
	AllowedAccelerators []string
	// EvalSets are the operator's local eval sets, by name ("local:NAME").
	EvalSets map[string]qualitycheck.EvalSet
	// AIPerf is the AIPerf executable; empty means aiperf on PATH.
	AIPerf string
	// Benchmark and Completer are replaced in tests.
	Benchmark func(context.Context, benchmark.Config) (benchmark.Result, error)
	Completer qualitycheck.Completer
	// ReadyTimeout bounds how long a candidate may take to become ready;
	// zero means half the run's timeout.
	ReadyTimeout time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

// CandidateProvider is the part of the provider contract a candidate run
// uses. provision.Kubernetes implements it.
type CandidateProvider interface {
	Check(context.Context) error
	Handle(string) provision.ProviderHandle
	EnsureReplica(context.Context, provision.ReplicaSpec) (provision.ProviderHandle, error)
	ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error)
	DeleteReplica(context.Context, provision.ProviderHandle) error
}

func (e KubernetesExecutor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// ImageAllowed reports whether an operator allowlist admits image.
func ImageAllowed(allowed []string, image string) bool {
	repository := image
	if index := strings.Index(repository, "@"); index >= 0 {
		repository = repository[:index]
	}
	if slash, colon := strings.LastIndex(repository, "/"), strings.LastIndex(repository, ":"); colon > slash {
		repository = repository[:colon]
	}
	for _, entry := range allowed {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "":
		case entry == image, entry == repository:
			return true
		case strings.HasSuffix(entry, "/") && strings.HasPrefix(repository, entry):
			return true
		}
	}
	return false
}

// Check verifies the cluster and RBAC once at start.
func (e KubernetesExecutor) Check(ctx context.Context) error {
	if len(e.AllowedImages) == 0 {
		return errors.New("candidate runs need at least one allowed image (--allow-image)")
	}
	if strings.TrimSpace(e.WorkerKey) == "" {
		return errors.New("candidate runs need the worker key the candidate pods receive")
	}
	if e.Provider == nil {
		return errors.New("candidate runs need a Kubernetes provider")
	}
	return e.Provider.Check(ctx)
}

// RunCandidate implements CandidateExecutor.
// The result is a named return so the deferred cleanup's outcome is part of
// it on every path.
func (e KubernetesExecutor) RunCandidate(ctx context.Context, run CandidateRun, progress func(string)) (result agentprotocol.CandidateRunResult, err error) {
	spec := run.Spec
	result = agentprotocol.CandidateRunResult{ImageDigest: spec.Workload.Image}
	if !ImageAllowed(e.AllowedImages, spec.Workload.Image) {
		return result, candidateFailure("image_not_allowed", "this agent's operator has not allowed image %s", spec.Workload.Image)
	}
	if len(e.AllowedAccelerators) > 0 && !contains(e.AllowedAccelerators, spec.Accelerator) {
		return result, candidateFailure("accelerator_not_allowed", "this agent's operator has not allowed accelerator %s", spec.Accelerator)
	}
	set, setErr := e.evalSet(spec.Quality)
	if setErr != nil {
		return result, setErr
	}
	replica := provision.ReplicaSpec{
		ExternalKey: "agent-candidate-" + strings.ToLower(run.ID), Name: "candidate", Model: spec.Model, ModelRevision: spec.ModelRevision,
		Cloud: "kubernetes", GPU: spec.Accelerator, GPUCount: spec.GPUCount, Runtime: "custom-oci", RuntimeArgs: spec.RuntimeArgs,
		Port: spec.Workload.Port, Workload: spec.Workload,
	}
	progress("launching")
	started := e.now()
	handle, err := e.Provider.EnsureReplica(ctx, replica)
	if err != nil {
		code := "launch_failed"
		if errors.Is(err, provision.ErrInvalidReplicaSpec) {
			code = "invalid_candidate"
		}
		e.cleanup(ctx, e.Provider.Handle(replica.ExternalKey), &result)
		return result, &CandidateError{Code: code, Detail: err.Error(), Result: &result}
	}
	defer e.cleanup(ctx, handle, &result)
	progress("waiting_ready")
	endpoint, err := e.waitReady(ctx, handle, spec.Workload.Port, spec)
	if err != nil {
		return result, err
	}
	result.ReadySeconds = roundMS(e.now().Sub(started).Seconds())
	candidate := ControlServer{Endpoint: "candidate", BaseURL: endpoint, Model: spec.Model, Token: e.WorkerKey}

	progress("measuring_control")
	if result.Control, err = e.measure(ctx, run.Control, spec); err != nil {
		return result, &CandidateError{Code: "control_measurement_failed", Detail: err.Error(), Result: &result}
	}
	progress("measuring_candidate")
	if result.Candidate, err = e.measure(ctx, candidate, spec); err != nil {
		return result, &CandidateError{Code: "candidate_measurement_failed", Detail: err.Error(), Result: &result}
	}
	progress("measuring_control_again")
	if result.ControlAfter, err = e.measure(ctx, run.Control, spec); err != nil {
		return result, &CandidateError{Code: "control_measurement_failed", Detail: err.Error(), Result: &result}
	}
	if before, after := result.Control.OutputTokenThroughput, result.ControlAfter.OutputTokenThroughput; before != nil && after != nil && *before > 0 {
		noise := roundMS((*after - *before) / *before)
		result.Noise = &noise
	}
	if !spec.Quality.Skip {
		progress("answer_check")
		completer := e.Completer
		if completer == nil {
			completer = qualitycheck.HTTPCompleter{Client: &http.Client{Transport: localTransport()}}
		}
		comparison, compareErr := qualitycheck.Compare(ctx, completer, set,
			qualitycheck.Target{Endpoint: run.Control.BaseURL, APIKey: run.Control.Token, Model: run.Control.Model, SubjectID: "current-setup"},
			qualitycheck.Target{Endpoint: candidate.BaseURL, APIKey: candidate.Token, Model: candidate.Model, SubjectID: "candidate-" + run.ID},
			e.now)
		if compareErr != nil {
			return result, &CandidateError{Code: "answer_check_failed", Detail: compareErr.Error(), Result: &result}
		}
		result.Quality = &agentprotocol.QualityMeasurement{
			EvalSet: spec.Quality.EvalSet, EvalSetDigest: set.Digest, Prompts: len(comparison.Scores), Scores: comparison.Scores,
			Score: comparison.Score, Verdict: comparison.Eval.Verdict, ResultDigest: comparison.ResultDigest,
		}
	}
	progress("cleaning_up")
	return result, nil
}

func (e KubernetesExecutor) evalSet(quality agentprotocol.CandidateQuality) (qualitycheck.EvalSet, error) {
	if quality.Skip {
		return qualitycheck.EvalSet{}, nil
	}
	if quality.EvalSet == "default" {
		return qualitycheck.Default(), nil
	}
	name := strings.TrimPrefix(quality.EvalSet, "local:")
	set, ok := e.EvalSets[name]
	if !ok {
		return qualitycheck.EvalSet{}, candidateFailure("eval_set_unknown", "this agent has no local eval set named %s", name)
	}
	return set, nil
}

func (e KubernetesExecutor) waitReady(ctx context.Context, handle provision.ProviderHandle, port int, spec agentprotocol.CandidateRunSpec) (string, error) {
	timeout := e.ReadyTimeout
	if timeout <= 0 {
		timeout = time.Duration(spec.TimeoutSecs) * time.Second / 2
	}
	interval := e.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		observation, err := e.Provider.ObserveReplica(ctx, handle, port)
		if err == nil && observation.State == "ready" && observation.Endpoint != "" {
			return observation.Endpoint, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", candidateFailure("candidate_not_ready", "the candidate did not become ready within %s", timeout)
		case <-time.After(interval):
		}
	}
}

func (e KubernetesExecutor) measure(ctx context.Context, server ControlServer, spec agentprotocol.CandidateRunSpec) (*agentprotocol.LoadMeasurement, error) {
	run := e.Benchmark
	if run == nil {
		run = benchmark.Run
	}
	streaming := spec.Load.Streaming
	tokenizer := spec.Load.Tokenizer
	if tokenizer == "" {
		tokenizer = spec.Model
	}
	timeout := time.Duration(spec.TimeoutSecs) * time.Second / 3
	measured, err := run(ctx, benchmark.Config{
		Binary: e.AIPerf, Endpoint: server.BaseURL, APIKey: server.Token, Model: server.Model, Tokenizer: tokenizer,
		Requests: spec.Load.Requests, Concurrency: spec.Load.Concurrency, InputTokens: spec.Load.InputTokens, OutputTokens: spec.Load.OutputTokens,
		ProfileRuns: 1, WarmupRequests: min(spec.Load.Concurrency, 8), Streaming: &streaming, Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	return &agentprotocol.LoadMeasurement{
		Tool: agentprotocol.CleanText(measured.Tool, agentprotocol.MaxText), ToolVersion: agentprotocol.CleanText(measured.ToolVersion, agentprotocol.MaxText),
		Requests: measured.Requests, Succeeded: measured.Succeeded, Failed: measured.Failed, DurationSeconds: measured.DurationSeconds,
		RequestThroughput: measured.RequestThroughput, OutputTokenThroughput: measured.OutputTokenThroughput,
		TTFTP50MS: measured.TTFTP50MS, TTFTP95MS: measured.TTFTP95MS, TPOTP50MS: measured.TPOTP50MS, TPOTP95MS: measured.TPOTP95MS,
		LatencyP50MS: measured.LatencyP50MS, LatencyP95MS: measured.LatencyP95MS,
	}, nil
}

// cleanup deletes the candidate even when the run was cancelled.
func (e KubernetesExecutor) cleanup(ctx context.Context, handle provision.ProviderHandle, result *agentprotocol.CandidateRunResult) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := e.Provider.DeleteReplica(cleanupCtx, handle); err == nil {
		result.CleanedUp = true
	}
}

func localTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

func roundMS(value float64) float64 { return float64(int64(value*1000+0.5)) / 1000 }

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// WriteInClusterKubeconfig writes a kubeconfig for the pod's own service
// account, so kubectl runs with exactly the namespaced RBAC the Helm chart
// grants. The token file is referenced, not copied, so rotation works.
func WriteInClusterKubeconfig(dir string) (string, error) {
	const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST is unset)")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	for _, file := range []string{"token", "ca.crt"} {
		if _, err := os.Stat(filepath.Join(serviceAccount, file)); err != nil {
			return "", fmt.Errorf("service account %s is missing: %w", file, err)
		}
	}
	body := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: in-cluster
  cluster:
    server: https://%s:%s
    certificate-authority: %s/ca.crt
users:
- name: in-cluster
  user:
    tokenFile: %s/token
contexts:
- name: in-cluster
  context:
    cluster: in-cluster
    user: in-cluster
current-context: in-cluster
`, host, port, serviceAccount, serviceAccount)
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
