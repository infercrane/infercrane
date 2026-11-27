package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

// MeasureWindow implements WindowExecutor. It starts the candidate with the
// namespaced provider, warms both servers, runs the current setup and the
// candidate in turn (C X C ... C, every candidate run between two runs of the
// current setup), asks both the answer check, and deletes the candidate on
// every path. A window without a candidate runs the current setup alone. The
// result holds each run's numbers and each item's score; prompts, answers,
// and addresses stay here. Judging is the control plane's.
func (e KubernetesExecutor) MeasureWindow(ctx context.Context, job WindowJob, progress func(string)) (result agentprotocol.WindowResult, err error) {
	spec := job.Spec
	set, err := e.evalSet(agentprotocol.CandidateQuality{EvalSet: spec.Quality.EvalSet, Skip: spec.Quality.Skip})
	if err != nil {
		return result, err
	}
	control := job.Control
	controlNode := e.serverNode(ctx, control.BaseURL)
	candidateNode := "unknown"
	var candidate *ControlServer
	if setup := spec.Candidate; setup != nil {
		result.ImageDigest = setup.Workload.Image
		if !ImageAllowed(e.AllowedImages, setup.Workload.Image) {
			return result, candidateFailure("image_not_allowed", "this agent's operator has not allowed image %s", setup.Workload.Image)
		}
		if len(e.AllowedAccelerators) > 0 && !contains(e.AllowedAccelerators, setup.Accelerator) {
			return result, candidateFailure("accelerator_not_allowed", "this agent's operator has not allowed accelerator %s", setup.Accelerator)
		}
		started, startErr := e.start(ctx, candidateKey(job.ID), *setup, spec.TimeoutSecs, progress)
		defer started.cleanup(ctx, &result.CleanedUp)
		if startErr != nil {
			var failure *CandidateError
			if errors.As(startErr, &failure) {
				failure.Window = &result
			}
			return result, startErr
		}
		result.ReadySeconds = started.readySeconds
		candidate = &started.server
		candidateNode = e.replicaNode(ctx, started.handle)
	}

	// Every invocation, warm-ups included, takes the next seed, as the
	// control plane's own windows do, so no server sees one prompt set twice.
	seed := spec.SeedBase
	invoke := func(server ControlServer, requests int) (benchmark.Result, int64, error) {
		used := seed
		seed++
		measured, runErr := e.window(ctx, server, spec, requests, used)
		if runErr == nil && result.Tool == "" {
			result.Tool = agentprotocol.CleanText(measured.Tool, agentprotocol.MaxText)
			result.ToolVersion = agentprotocol.CleanText(measured.ToolVersion, agentprotocol.MaxText)
		}
		return measured, used, runErr
	}
	failed := func(arm string, runErr error) error {
		code := "control_measurement_failed"
		if arm == agentprotocol.ArmCandidate {
			code = "candidate_measurement_failed"
		}
		return &CandidateError{Code: code, Detail: runErr.Error(), Window: &result}
	}
	turn := func(arm string, server ControlServer) error {
		measured, used, runErr := invoke(server, spec.Load.Requests)
		if runErr != nil {
			return failed(arm, runErr)
		}
		run := windowRun(arm, used, measured)
		run.Node, run.GPU = controlNode, "unknown"
		if arm == agentprotocol.ArmCandidate {
			run.Node = candidateNode
		}
		result.Runs = append(result.Runs, run)
		return nil
	}

	if spec.Warmup > 0 {
		progress("warming_up")
		if _, _, err = invoke(control, spec.Warmup); err != nil {
			return result, failed(agentprotocol.ArmControl, err)
		}
		if candidate != nil {
			if _, _, err = invoke(*candidate, spec.Warmup); err != nil {
				return result, failed(agentprotocol.ArmCandidate, err)
			}
		}
	}
	progress("measuring")
	if candidate == nil {
		for range spec.Rounds {
			if err = turn(agentprotocol.ArmControl, control); err != nil {
				return result, err
			}
		}
	} else {
		if err = turn(agentprotocol.ArmControl, control); err != nil {
			return result, err
		}
		for range spec.Rounds {
			if err = turn(agentprotocol.ArmCandidate, *candidate); err != nil {
				return result, err
			}
			if err = turn(agentprotocol.ArmControl, control); err != nil {
				return result, err
			}
		}
	}

	if !spec.Quality.Skip {
		progress("answer_check")
		// The runs stand whatever the answer check does; a check that could
		// not compare the answers is reported by its code, and the control
		// plane keeps the setup's quality unknown.
		if result.Quality, err = e.windowAnswers(ctx, set, spec, control, candidate); err != nil {
			result.QualityFailure = "answer_check_failed"
			if errors.Is(err, qualitycheck.ErrReferenceUnanswered) {
				result.QualityFailure = "quality_reference_unanswered"
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
		}
	}
	progress("cleaning_up")
	return result, nil
}

// window runs one AIPerf invocation of the window's load against one server.
func (e KubernetesExecutor) window(ctx context.Context, server ControlServer, spec agentprotocol.WindowSpec, requests int, seed int64) (benchmark.Result, error) {
	run := e.Benchmark
	if run == nil {
		run = benchmark.Run
	}
	streaming := spec.Load.Streaming
	tokenizer := spec.Load.Tokenizer
	if tokenizer == "" {
		tokenizer = server.Model
	}
	// The window's runs share its timeout; one run gets at most a third of
	// it, as a candidate run's measurements do.
	timeout := time.Duration(spec.TimeoutSecs) * time.Second / 3
	return run(ctx, benchmark.Config{
		Binary: e.AIPerf, Endpoint: server.BaseURL, APIKey: server.Token, Model: server.Model, Tokenizer: tokenizer,
		Requests: requests, Concurrency: spec.Load.Concurrency, InputTokens: spec.Load.InputTokens, OutputTokens: spec.Load.OutputTokens,
		RandomSeed: seed, Streaming: &streaming, TTFTSLOMS: spec.TTFTLimitMS, TPOTSLOMS: spec.TPOTLimitMS, Timeout: timeout,
	})
}

func windowRun(arm string, seed int64, measured benchmark.Result) agentprotocol.WindowRun {
	return agentprotocol.WindowRun{
		Arm: arm, Seed: seed, Requests: measured.Requests, Succeeded: measured.Succeeded, Failed: measured.Failed, DurationSeconds: measured.DurationSeconds,
		RequestThroughput: measured.RequestThroughput, OutputTokenThroughput: measured.OutputTokenThroughput, Goodput: measured.Goodput,
		UsefulOutputThroughput: measured.UsefulOutputThroughput, OutputTokensPerRequest: measured.OutputTokensPerRequest,
		TTFTP50MS: measured.TTFTP50MS, TTFTP95MS: measured.TTFTP95MS, TPOTP50MS: measured.TPOTP50MS, TPOTP95MS: measured.TPOTP95MS,
		LatencyP50MS: measured.LatencyP50MS, LatencyP95MS: measured.LatencyP95MS,
	}
}

// windowAnswers asks the current setup and the candidate the eval set and
// keeps scores and digests only. The current setup alone is compared with
// itself: on a task set its own items are scored; otherwise it agrees with
// itself by definition. Which rule judges the scores is the control plane's.
func (e KubernetesExecutor) windowAnswers(ctx context.Context, set qualitycheck.EvalSet, spec agentprotocol.WindowSpec, control ControlServer, candidate *ControlServer) (*agentprotocol.WindowAnswers, error) {
	completer := e.Completer
	if completer == nil {
		completer = qualitycheck.HTTPCompleter{Client: &http.Client{Transport: localTransport()}}
	}
	reference := qualitycheck.Target{Endpoint: control.BaseURL, APIKey: control.Token, Model: control.Model, SubjectID: spec.Quality.ReferenceSubject}
	other := reference
	if candidate != nil {
		other = qualitycheck.Target{Endpoint: candidate.BaseURL, APIKey: candidate.Token, Model: candidate.Model, SubjectID: spec.Quality.CandidateSubject}
	}
	comparison, err := qualitycheck.Compare(ctx, completer, set, reference, other, e.now)
	if err != nil {
		return nil, err
	}
	answers := &agentprotocol.WindowAnswers{
		EvalSet: spec.Quality.EvalSet, EvalSetName: agentprotocol.CleanText(set.Name, agentprotocol.MaxText), EvalSetVersion: agentprotocol.CleanText(set.Version, agentprotocol.MaxText),
		EvalSetDigest: set.Digest, PairingDigest: set.PairingDigest(), Grading: set.Grading, Items: len(comparison.Scores), ItemsAtOnce: comparison.ItemsAtOnce,
		ReferenceScores: comparison.ReferenceScores, CandidateScores: comparison.Scores, ToolCalls: comparison.ToolCalls,
		ResultDigest: comparison.ResultDigest, EvaluatedAt: comparison.EvaluatedAt,
	}
	// The divergence is evidence: a server that cannot report log
	// probabilities, or any failure to read it, leaves it unknown.
	if scorer, ok := completer.(qualitycheck.ForcedScorer); ok && spec.Quality.Divergence && candidate != nil {
		// The control plane asks for it only when both setups read text
		// with one tokenizer, so both targets name the same one.
		reference.Tokenizer, other.Tokenizer = "window:"+spec.Quality.CandidateSubject, "window:"+spec.Quality.CandidateSubject
		if divergence, divergenceErr := qualitycheck.MeasureDivergence(ctx, completer, scorer, qualitycheck.DivergencePrompts(set), reference, other); divergenceErr == nil {
			answers.Divergence = divergence
		}
	}
	return answers, nil
}

// String names a window job for events.
func (j WindowJob) String() string {
	return fmt.Sprintf("%s (%s, %d rounds)", j.ID, j.Spec.Label, j.Spec.Rounds)
}

// nodeReader is the read-only placement lookup provision.Kubernetes offers.
type nodeReader interface {
	ReplicaNodes(context.Context, provision.ProviderHandle) ([]string, error)
	ServiceNodes(context.Context, string, string) ([]string, error)
}

// replicaNode is the digest of the node a started setup runs on.
func (e KubernetesExecutor) replicaNode(ctx context.Context, handle provision.ProviderHandle) string {
	reader, ok := e.Provider.(nodeReader)
	if !ok {
		return "unknown"
	}
	nodes, err := reader.ReplicaNodes(ctx, handle)
	if err != nil {
		return "unknown"
	}
	return e.nodeDigest(nodes)
}

// serverNode is the digest of the node behind a server the agent reaches by
// a cluster Service name (NAME.NAMESPACE.svc), or unknown.
func (e KubernetesExecutor) serverNode(ctx context.Context, baseURL string) string {
	reader, ok := e.Provider.(nodeReader)
	if !ok {
		return "unknown"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "unknown"
	}
	labels := strings.Split(parsed.Hostname(), ".")
	if len(labels) < 3 || labels[2] != "svc" {
		return "unknown"
	}
	nodes, err := reader.ServiceNodes(ctx, labels[1], labels[0])
	if err != nil {
		return "unknown"
	}
	return e.nodeDigest(nodes)
}

// nodeDigest names one node by an HMAC of its name with the agent's local
// key, so the control plane can tell two nodes apart without learning them.
func (e KubernetesExecutor) nodeDigest(nodes []string) string {
	unique := map[string]bool{}
	for _, node := range nodes {
		if node != "" {
			unique[node] = true
		}
	}
	switch {
	case len(unique) > 1:
		return "several"
	case len(unique) == 0 || len(e.NodeKey) == 0:
		return "unknown"
	}
	var name string
	for node := range unique {
		name = node
	}
	mac := hmac.New(sha256.New, e.NodeKey)
	mac.Write([]byte("node\x00" + name))
	return "node-" + hex.EncodeToString(mac.Sum(nil))[:16]
}
