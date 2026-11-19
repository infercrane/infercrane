package agentprotocol

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// Commands are work the control plane queues for one agent. The agent fetches
// them with an outbound long poll (GET /api/v1/agent/commands) and reports
// each result (POST /api/v1/agent/commands/{id}); nothing dials the agent.
// A command is a bounded spec, never a shell command. The agent checks it
// against its own local allowlists (images, namespace, accelerators) before
// it runs anything, and reports only numbers back.

const (
	CommandRunCandidate = "run_candidate"

	CommandQueued    = "queued"
	CommandRunning   = "running"
	CommandSucceeded = "succeeded"
	CommandFailed    = "failed"
	CommandCancelled = "cancelled"
	CommandExpired   = "expired"

	// MaxCommandWait bounds one long poll.
	MaxCommandWait = 25 * time.Second
	// CommandLease is how long a running command stays leased without a
	// progress report before it may be handed out again.
	CommandLease = 10 * time.Minute
	// MaxCommandsPerPoll bounds one long-poll answer.
	MaxCommandsPerPoll = 4
	// MaxQueuedCommandsPerAgent bounds what can wait for one agent.
	MaxQueuedCommandsPerAgent = 16
	MaxCandidateRunTimeout    = 6 * time.Hour
	DefaultCandidateRunTime   = time.Hour
	MaxCandidateRequests      = 5000
	MaxCandidateConcurrency   = 256
	MaxCandidateTokens        = 32768
	// MaxRuntimeArgs bounds the candidate's extra engine arguments.
	MaxRuntimeArgs = 64
)

var (
	phasePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	commandIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,63}$`)
	evalSetPattern   = regexp.MustCompile(`^(default|local:[a-z0-9][a-z0-9._-]{0,62})$`)
	labelPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/+-]{0,127}$`)
)

// Command is one unit of work for an agent.
type Command struct {
	ID           string              `json:"id"`
	Kind         string              `json:"kind"`
	DeadlineAt   time.Time           `json:"deadline_at,omitempty"`
	RunCandidate *CandidateRunSpec   `json:"run_candidate,omitempty"`
	Cancelled    bool                `json:"cancelled,omitempty"`
	Attempt      int                 `json:"attempt,omitempty"`
	Control      *CandidateRunServer `json:"control,omitempty"`
}

// CandidateRunServer names the current setup a candidate is measured
// against, by the agent's own server key. The agent resolves the address.
type CandidateRunServer struct {
	Endpoint  string `json:"endpoint"`
	ServerKey string `json:"server_key"`
	Model     string `json:"model"`
}

// CandidateRunSpec asks the agent to start one candidate setup in its own
// cluster, measure it against the current setup with AIPerf, run the answer
// check on both, and remove the candidate. Everything runs where the agent
// runs; only the numbers in CandidateRunResult come back.
type CandidateRunSpec struct {
	// Endpoint is the agent-managed or observed endpoint whose server is the
	// current setup.
	Endpoint string `json:"endpoint"`
	Label    string `json:"label"`
	// Model and ModelRevision name what the candidate serves. Workload is the
	// candidate's image (pinned by digest) and argv; the agent refuses an
	// image its operator did not allow.
	Model         string                   `json:"model"`
	ModelRevision string                   `json:"model_revision,omitempty"`
	Workload      runtimecontract.Workload `json:"workload"`
	RuntimeArgs   []string                 `json:"runtime_args,omitempty"`
	Accelerator   string                   `json:"accelerator"`
	GPUCount      int                      `json:"gpu_count"`
	Load          CandidateLoad            `json:"load"`
	Quality       CandidateQuality         `json:"quality"`
	TimeoutSecs   int                      `json:"timeout_seconds"`
}

// CandidateLoad is the AIPerf profile run against the current setup, the
// candidate, and the current setup again (the noise check).
type CandidateLoad struct {
	Requests     int  `json:"requests"`
	Concurrency  int  `json:"concurrency"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	Streaming    bool `json:"streaming"`
	// Tokenizer is a Hugging Face tokenizer name AIPerf uses to size
	// synthetic prompts; empty means the candidate's model.
	Tokenizer string `json:"tokenizer,omitempty"`
}

// CandidateQuality picks the eval set for the answer check: "default" (the
// public suite built into InferCrane) or "local:NAME", an eval set the
// agent's operator keeps beside the agent. Eval prompts named local never
// leave the customer's cloud, and neither do any answers.
type CandidateQuality struct {
	EvalSet string `json:"eval_set"`
	Skip    bool   `json:"skip,omitempty"`
}

// CommandUpdate is the agent's report on one command: progress while it runs
// and the result when it ends.
type CommandUpdate struct {
	State         string              `json:"state"`
	Phase         string              `json:"phase,omitempty"`
	FailureCode   string              `json:"failure_code,omitempty"`
	FailureDetail string              `json:"failure_detail,omitempty"`
	Result        *CandidateRunResult `json:"result,omitempty"`
}

// CommandUpdateResponse tells the agent whether to keep going.
type CommandUpdateResponse struct {
	State     string    `json:"state"`
	Cancelled bool      `json:"cancelled"`
	LeaseTill time.Time `json:"lease_expires_at,omitempty"`
}

// CommandsResponse is one long-poll answer.
type CommandsResponse struct {
	Commands []Command `json:"commands"`
}

// CandidateRunResult is everything a candidate run sends back: timings,
// throughput, token counts, and answer-check scores. No prompt, answer,
// header, log line, or address is in it.
type CandidateRunResult struct {
	Control      *LoadMeasurement    `json:"control"`
	Candidate    *LoadMeasurement    `json:"candidate"`
	ControlAfter *LoadMeasurement    `json:"control_after,omitempty"`
	Quality      *QualityMeasurement `json:"quality,omitempty"`
	// ReadySeconds is how long the candidate took to answer its readiness
	// check after it was applied.
	ReadySeconds float64 `json:"ready_seconds"`
	// ImageDigest is the candidate image's digest, as the command named it.
	ImageDigest string `json:"image_digest"`
	// CleanedUp says the candidate's Deployment and Service were deleted.
	CleanedUp bool `json:"cleaned_up"`
	// Noise is the relative change in output throughput between the two
	// control measurements; unknown when either run failed.
	Noise *float64 `json:"noise,omitempty"`
}

// LoadMeasurement is a bounded AIPerf summary. A value AIPerf did not report
// is nil, never zero.
type LoadMeasurement struct {
	Tool                  string   `json:"tool"`
	ToolVersion           string   `json:"tool_version"`
	Requests              int      `json:"requests"`
	Succeeded             int      `json:"succeeded"`
	Failed                int      `json:"failed"`
	DurationSeconds       float64  `json:"duration_seconds"`
	RequestThroughput     *float64 `json:"request_throughput,omitempty"`
	OutputTokenThroughput *float64 `json:"output_token_throughput,omitempty"`
	TTFTP50MS             *float64 `json:"ttft_p50_ms,omitempty"`
	TTFTP95MS             *float64 `json:"ttft_p95_ms,omitempty"`
	TPOTP50MS             *float64 `json:"tpot_p50_ms,omitempty"`
	TPOTP95MS             *float64 `json:"tpot_p95_ms,omitempty"`
	LatencyP50MS          *float64 `json:"latency_p50_ms,omitempty"`
	LatencyP95MS          *float64 `json:"latency_p95_ms,omitempty"`
}

// QualityMeasurement is the answer check's outcome: one agreement score per
// prompt (prompt order of the eval set), their mean, the verdict, and a
// digest that commits to the answers without containing them.
type QualityMeasurement struct {
	EvalSet       string    `json:"eval_set"`
	EvalSetDigest string    `json:"eval_set_digest"`
	Prompts       int       `json:"prompts"`
	Scores        []float64 `json:"scores"`
	Score         float64   `json:"score"`
	Verdict       string    `json:"verdict"`
	ResultDigest  string    `json:"result_digest"`
}

// ValidCommandID reports whether id is a bounded command identifier.
func ValidCommandID(id string) bool { return commandIDPattern.MatchString(id) }

// Validate bounds a candidate run before it is queued and again before the
// agent runs it.
func (s CandidateRunSpec) Validate() error {
	if !ValidEndpointName(s.Endpoint) {
		return errors.New("endpoint must be a lowercase endpoint name")
	}
	if !labelPattern.MatchString(s.Label) {
		return errors.New("label must be 1..128 letters, digits, spaces, or ._:/+-")
	}
	if err := boundedText("model", s.Model, true); err != nil {
		return err
	}
	if err := boundedText("model_revision", s.ModelRevision, false); err != nil {
		return err
	}
	if err := s.Workload.Validate(); err != nil {
		return fmt.Errorf("workload: %w", err)
	}
	if isShell(s.Workload.Command[0]) {
		return errors.New("workload.command must start the model server itself, not a shell")
	}
	if len(s.RuntimeArgs) > MaxRuntimeArgs {
		return fmt.Errorf("at most %d runtime_args are allowed", MaxRuntimeArgs)
	}
	for _, arg := range s.RuntimeArgs {
		if len(arg) > 512 || strings.ContainsAny(arg, "\x00\r\n") || strings.Contains(arg, "${") || strings.Contains(arg, "$(") {
			return errors.New("runtime_args must be bounded single-line arguments without substitutions")
		}
	}
	if err := boundedText("accelerator", s.Accelerator, true); err != nil {
		return err
	}
	if s.GPUCount < 1 || s.GPUCount > 16 {
		return errors.New("gpu_count must be between 1 and 16")
	}
	load := s.Load
	if load.Requests < 1 || load.Requests > MaxCandidateRequests || load.Concurrency < 1 || load.Concurrency > MaxCandidateConcurrency ||
		load.InputTokens < 1 || load.InputTokens > MaxCandidateTokens || load.OutputTokens < 1 || load.OutputTokens > MaxCandidateTokens {
		return errors.New("load needs 1..5000 requests, 1..256 concurrency, and 1..32768 input and output tokens")
	}
	if err := boundedText("load.tokenizer", load.Tokenizer, false); err != nil {
		return err
	}
	if !s.Quality.Skip && !evalSetPattern.MatchString(s.Quality.EvalSet) {
		return errors.New(`quality.eval_set must be "default" or "local:NAME"`)
	}
	if s.TimeoutSecs < 60 || time.Duration(s.TimeoutSecs)*time.Second > MaxCandidateRunTimeout {
		return errors.New("timeout_seconds must be between 60 and 21600")
	}
	return nil
}

func isShell(executable string) bool {
	base := executable[strings.LastIndex(executable, "/")+1:]
	switch base {
	case "sh", "bash", "ash", "dash", "zsh", "ksh", "busybox", "env", "python", "python3", "perl", "node":
		return true
	}
	return false
}

// Validate bounds an agent's command report.
func (u CommandUpdate) Validate() error {
	switch u.State {
	case CommandRunning, CommandSucceeded, CommandFailed:
	default:
		return errors.New("state must be running, succeeded, or failed")
	}
	if u.Phase != "" && !phasePattern.MatchString(u.Phase) {
		return errors.New("phase must be a lowercase identifier")
	}
	if u.State == CommandFailed && !phasePattern.MatchString(u.FailureCode) {
		return errors.New("a failed command needs a failure_code")
	}
	if u.State != CommandFailed && (u.FailureCode != "" || u.FailureDetail != "") {
		return errors.New("failure_code and failure_detail belong to a failed command")
	}
	if err := boundedText("failure_detail", u.FailureDetail, false); err != nil {
		return err
	}
	if u.State == CommandSucceeded && u.Result == nil {
		return errors.New("a succeeded command needs a result")
	}
	if u.Result != nil {
		return u.Result.Validate()
	}
	return nil
}

// Validate bounds a candidate result: finite, non-negative numbers only.
func (r CandidateRunResult) Validate() error {
	if r.ReadySeconds < 0 || r.ReadySeconds > MaxCandidateRunTimeout.Seconds() || math.IsNaN(r.ReadySeconds) {
		return errors.New("ready_seconds is out of range")
	}
	if r.ImageDigest != "" && runtimecontract.ValidateImage(r.ImageDigest) != nil {
		return errors.New("image_digest must be a digest-pinned image")
	}
	for name, measurement := range map[string]*LoadMeasurement{"control": r.Control, "candidate": r.Candidate, "control_after": r.ControlAfter} {
		if measurement == nil {
			continue
		}
		if err := measurement.validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if r.Noise != nil && (math.IsNaN(*r.Noise) || math.IsInf(*r.Noise, 0) || math.Abs(*r.Noise) > 1000) {
		return errors.New("noise is out of range")
	}
	if q := r.Quality; q != nil {
		if !evalSetPattern.MatchString(q.EvalSet) || !hashPattern.MatchString(strings.TrimPrefix(q.EvalSetDigest, "sha256:")) || !hashPattern.MatchString(strings.TrimPrefix(q.ResultDigest, "sha256:")) {
			return errors.New("quality needs an eval set name and sha256 digests")
		}
		if q.Prompts < 1 || q.Prompts > 1000 || len(q.Scores) != q.Prompts {
			return errors.New("quality needs one score per prompt")
		}
		for _, score := range append(append([]float64(nil), q.Scores...), q.Score) {
			if math.IsNaN(score) || score < 0 || score > 1 {
				return errors.New("quality scores must be between 0 and 1")
			}
		}
		switch q.Verdict {
		case "PASS", "WATCH", "BLOCK":
		default:
			return errors.New("quality verdict must be PASS, WATCH, or BLOCK")
		}
	}
	return nil
}

func (m LoadMeasurement) validate() error {
	if err := boundedText("tool", m.Tool, true); err != nil {
		return err
	}
	if err := boundedText("tool_version", m.ToolVersion, false); err != nil {
		return err
	}
	if m.Requests < 0 || m.Succeeded < 0 || m.Failed < 0 || m.Requests > 1_000_000 || m.Succeeded+m.Failed > 1_000_000 {
		return errors.New("request counts are out of range")
	}
	values := []*float64{m.RequestThroughput, m.OutputTokenThroughput, m.TTFTP50MS, m.TTFTP95MS, m.TPOTP50MS, m.TPOTP95MS, m.LatencyP50MS, m.LatencyP95MS}
	for _, value := range append(values, &m.DurationSeconds) {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1e9) {
			return errors.New("measurements must be finite and non-negative")
		}
	}
	return nil
}
