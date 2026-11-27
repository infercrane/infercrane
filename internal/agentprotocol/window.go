package agentprotocol

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// A measured window is one candidate's share of an optimization run, run by
// the agent in its own cluster: the candidate is started, both servers are
// warmed, then the current setup and the candidate take turns, C X C X ... C,
// with every candidate run between two runs of the current setup, and the
// answer check asks both. The agent returns each run's numbers and each
// item's score; the control plane judges them with the same code it uses for
// a run it measures itself. A window without a candidate measures the current
// setup alone, for its own evidence row.

const (
	CommandMeasureWindow = "measure_window"

	// MaxWindowRounds bounds the candidate runs in one window.
	MaxWindowRounds = 8
	// MaxWindowWarmup bounds the warm-up requests each server receives.
	MaxWindowWarmup = 256
	// MaxWindowItems bounds the answer check's items, as a task set does.
	MaxWindowItems = 1000

	ArmControl   = "control"
	ArmCandidate = "candidate"
)

var (
	gradingPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// A node is named by a keyed digest of its name, never the name.
	nodePattern    = regexp.MustCompile(`^(node-[a-f0-9]{16}|several|unknown)$`)
	gpuPattern     = regexp.MustCompile(`^(gpu-[a-f0-9]{16}|unknown)$`)
	subjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,127}$`)
)

// CandidateSetup is the exact setup an agent starts: the model it serves, the
// image pinned by digest and its argv, extra engine arguments, and the
// accelerators. The agent refuses an image or accelerator its operator did not
// allow.
type CandidateSetup struct {
	Model         string                   `json:"model"`
	ModelRevision string                   `json:"model_revision,omitempty"`
	Workload      runtimecontract.Workload `json:"workload"`
	RuntimeArgs   []string                 `json:"runtime_args,omitempty"`
	Accelerator   string                   `json:"accelerator"`
	GPUCount      int                      `json:"gpu_count"`
}

// Validate bounds a setup the same way a candidate run's setup is bounded.
func (s CandidateSetup) Validate() error {
	return validateSetup(s.Model, s.ModelRevision, s.Workload, s.RuntimeArgs, s.Accelerator, s.GPUCount)
}

// WindowSpec asks an agent for one measured window.
type WindowSpec struct {
	// Endpoint is the agent-managed or observed endpoint whose server is the
	// current setup.
	Endpoint string `json:"endpoint"`
	Label    string `json:"label"`
	// Candidate is the setup to start; nil measures the current setup alone.
	Candidate *CandidateSetup `json:"candidate,omitempty"`
	// Rounds is the number of candidate runs (or of runs of the current
	// setup alone).
	Rounds int `json:"rounds"`
	// Warmup is the requests each server receives before the first turn.
	Warmup int           `json:"warmup_requests"`
	Load   CandidateLoad `json:"load"`
	// SeedBase is the first run's AIPerf seed; run i uses SeedBase+i, so no
	// server sees a prompt set twice.
	SeedBase int64 `json:"seed_base"`
	// TTFTLimitMS and TPOTLimitMS are the limits a request must meet to
	// count as useful output; zero sets none.
	TTFTLimitMS float64 `json:"ttft_limit_ms,omitempty"`
	TPOTLimitMS float64 `json:"tpot_limit_ms,omitempty"`
	// Fresh says the candidate is started again on a new server to confirm a
	// gain the first window showed.
	Fresh   bool          `json:"fresh,omitempty"`
	Quality WindowQuality `json:"quality"`
	// TimeoutSecs bounds the whole window, start to cleanup.
	TimeoutSecs int `json:"timeout_seconds"`
}

// WindowQuality picks the answer check. EvalSet is "default" or "local:NAME",
// kept beside the agent. ReferenceSubject and CandidateSubject are the
// identities the control plane records the answers under; the result digest
// commits to them. Divergence also reads the candidate's divergence from the
// current setup on forced decoding, for a change of precision; the control
// plane asks for it only when both setups read text with one tokenizer.
type WindowQuality struct {
	EvalSet          string `json:"eval_set"`
	Skip             bool   `json:"skip,omitempty"`
	ReferenceSubject string `json:"reference_subject,omitempty"`
	CandidateSubject string `json:"candidate_subject,omitempty"`
	Divergence       bool   `json:"divergence,omitempty"`
}

// Validate bounds a window before it is queued and again before the agent
// runs it.
func (s WindowSpec) Validate() error {
	if !ValidEndpointName(s.Endpoint) {
		return errors.New("endpoint must be a lowercase endpoint name")
	}
	if !labelPattern.MatchString(s.Label) {
		return errors.New("label must be 1..128 letters, digits, spaces, or ._:/+-")
	}
	if s.Candidate != nil {
		if err := s.Candidate.Validate(); err != nil {
			return fmt.Errorf("candidate: %w", err)
		}
	}
	if s.Rounds < 1 || s.Rounds > MaxWindowRounds {
		return fmt.Errorf("rounds must be between 1 and %d", MaxWindowRounds)
	}
	if s.Warmup < 0 || s.Warmup > MaxWindowWarmup {
		return fmt.Errorf("warmup_requests must be between 0 and %d", MaxWindowWarmup)
	}
	if err := s.Load.validate(); err != nil {
		return err
	}
	if s.SeedBase < 0 || s.SeedBase > 1<<31-1-2*MaxWindowRounds {
		return errors.New("seed_base is out of range")
	}
	for _, limit := range []float64{s.TTFTLimitMS, s.TPOTLimitMS} {
		if math.IsNaN(limit) || limit < 0 || limit > 3_600_000 {
			return errors.New("ttft_limit_ms and tpot_limit_ms must be between 0 and 3600000")
		}
	}
	if s.Fresh && s.Candidate == nil {
		return errors.New("only a candidate can be started on a fresh server")
	}
	if !s.Quality.Skip && !evalSetPattern.MatchString(s.Quality.EvalSet) {
		return errors.New(`quality.eval_set must be "default" or "local:NAME"`)
	}
	if !s.Quality.Skip && (!subjectPattern.MatchString(s.Quality.ReferenceSubject) || !subjectPattern.MatchString(s.Quality.CandidateSubject)) {
		return errors.New("quality needs the reference and candidate subjects as lowercase identifiers")
	}
	if !s.Quality.Skip && (s.Candidate == nil) != (s.Quality.ReferenceSubject == s.Quality.CandidateSubject) {
		return errors.New("the subjects are equal exactly when the window measures the current setup alone")
	}
	if s.Quality.Divergence && (s.Quality.Skip || s.Candidate == nil) {
		return errors.New("divergence is read only beside the answer check of a candidate")
	}
	if s.TimeoutSecs < 60 || time.Duration(s.TimeoutSecs)*time.Second > MaxCandidateRunTimeout {
		return errors.New("timeout_seconds must be between 60 and 21600")
	}
	return nil
}

func (l CandidateLoad) validate() error {
	if l.Requests < 1 || l.Requests > MaxCandidateRequests || l.Concurrency < 1 || l.Concurrency > MaxCandidateConcurrency ||
		l.InputTokens < 1 || l.InputTokens > MaxCandidateTokens || l.OutputTokens < 1 || l.OutputTokens > MaxCandidateTokens {
		return errors.New("load needs 1..5000 requests, 1..256 concurrency, and 1..32768 input and output tokens")
	}
	return boundedText("load.tokenizer", l.Tokenizer, false)
}

// WindowResult is everything a window sends back. No prompt, answer, header,
// address, node name, or log line is in it.
type WindowResult struct {
	// Tool and ToolVersion name the load generator.
	Tool        string `json:"tool"`
	ToolVersion string `json:"tool_version"`
	// Runs are the turns in the order they ran: control, then candidate and
	// control alternately, or the current setup's runs alone. Warm-ups are
	// left out.
	Runs []WindowRun `json:"runs"`
	// ReadySeconds is how long the candidate took to become ready.
	ReadySeconds float64 `json:"ready_seconds"`
	// ImageDigest is the candidate image as the command named it.
	ImageDigest string `json:"image_digest,omitempty"`
	// CleanedUp says the candidate's Deployment and Service were deleted.
	CleanedUp bool           `json:"cleaned_up"`
	Quality   *WindowAnswers `json:"quality,omitempty"`
	// QualityFailure is the answer check's failure code when it could not
	// compare the answers (quality_reference_unanswered when the current
	// setup gave no answer text); the runs still count.
	QualityFailure string `json:"quality_failure,omitempty"`
}

// WindowRun is one AIPerf run's bounded summary. A value AIPerf did not
// report is nil, never zero.
type WindowRun struct {
	Arm  string `json:"arm"`
	Seed int64  `json:"seed"`
	// Node is a digest of the node the run's server was on (keyed with the
	// agent's local hashing key, so it names no node), "several" when the
	// server had pods on more than one node, or "unknown". GPU is a digest
	// of the GPU's UUID, or "unknown" when nothing in the cluster the agent
	// may read reports which GPU a pod holds.
	Node                   string   `json:"node,omitempty"`
	GPU                    string   `json:"gpu,omitempty"`
	Requests               int      `json:"requests"`
	Succeeded              int      `json:"succeeded"`
	Failed                 int      `json:"failed"`
	DurationSeconds        float64  `json:"duration_seconds"`
	RequestThroughput      *float64 `json:"request_throughput,omitempty"`
	OutputTokenThroughput  *float64 `json:"output_token_throughput,omitempty"`
	Goodput                *float64 `json:"goodput,omitempty"`
	UsefulOutputThroughput *float64 `json:"useful_output_throughput,omitempty"`
	OutputTokensPerRequest *float64 `json:"output_tokens_per_request,omitempty"`
	TTFTP50MS              *float64 `json:"ttft_p50_ms,omitempty"`
	TTFTP95MS              *float64 `json:"ttft_p95_ms,omitempty"`
	TPOTP50MS              *float64 `json:"tpot_p50_ms,omitempty"`
	TPOTP95MS              *float64 `json:"tpot_p95_ms,omitempty"`
	LatencyP50MS           *float64 `json:"latency_p50_ms,omitempty"`
	LatencyP95MS           *float64 `json:"latency_p95_ms,omitempty"`
}

// WindowAnswers is the answer check's outcome as scores and digests. For a
// task set (Grading answer_line) each side's score per item is whether its
// answer was right; otherwise CandidateScores holds the candidate's agreement
// with the current setup per prompt and ReferenceScores is empty. Prompts and
// answers stay in the cluster; ResultDigest commits to the answers without
// containing them.
type WindowAnswers struct {
	EvalSet         string                          `json:"eval_set"`
	EvalSetName     string                          `json:"eval_set_name"`
	EvalSetVersion  string                          `json:"eval_set_version"`
	EvalSetDigest   string                          `json:"eval_set_digest"`
	PairingDigest   string                          `json:"pairing_digest"`
	Grading         string                          `json:"grading,omitempty"`
	Items           int                             `json:"items"`
	ItemsAtOnce     int                             `json:"items_at_once,omitempty"`
	ReferenceScores []float64                       `json:"reference_scores,omitempty"`
	CandidateScores []float64                       `json:"candidate_scores"`
	ToolCalls       *qualityevidence.ToolCallScores `json:"tool_calls,omitempty"`
	Divergence      *qualityevidence.Divergence     `json:"divergence,omitempty"`
	ResultDigest    string                          `json:"result_digest"`
	EvaluatedAt     time.Time                       `json:"evaluated_at"`
}

// Validate bounds a window result: the expected turns, finite non-negative
// numbers, and scores between 0 and 1.
func (r WindowResult) Validate() error {
	if err := boundedText("tool", r.Tool, len(r.Runs) > 0); err != nil {
		return err
	}
	if err := boundedText("tool_version", r.ToolVersion, false); err != nil {
		return err
	}
	if len(r.Runs) > 2*MaxWindowRounds+1 {
		return errors.New("a window has at most 17 runs")
	}
	for index, run := range r.Runs {
		if err := run.validate(); err != nil {
			return fmt.Errorf("run %d: %w", index, err)
		}
	}
	if r.ReadySeconds < 0 || r.ReadySeconds > MaxCandidateRunTimeout.Seconds() || math.IsNaN(r.ReadySeconds) {
		return errors.New("ready_seconds is out of range")
	}
	if r.ImageDigest != "" && runtimecontract.ValidateImage(r.ImageDigest) != nil {
		return errors.New("image_digest must be a digest-pinned image")
	}
	if r.QualityFailure != "" && (r.Quality != nil || !phasePattern.MatchString(r.QualityFailure)) {
		return errors.New("quality_failure is a lowercase code reported instead of the answers")
	}
	if r.Quality != nil {
		return r.Quality.validate()
	}
	return nil
}

func (r WindowRun) validate() error {
	if r.Arm != ArmControl && r.Arm != ArmCandidate {
		return errors.New("arm must be control or candidate")
	}
	if r.Seed < 0 || r.Seed > 1<<31-1 {
		return errors.New("seed is out of range")
	}
	if r.Node != "" && !nodePattern.MatchString(r.Node) || r.GPU != "" && !gpuPattern.MatchString(r.GPU) {
		return errors.New("node and gpu are digests, several, or unknown")
	}
	if r.Requests < 0 || r.Succeeded < 0 || r.Failed < 0 || r.Requests > 1_000_000 || r.Succeeded+r.Failed > 1_000_000 {
		return errors.New("request counts are out of range")
	}
	values := []*float64{&r.DurationSeconds, r.RequestThroughput, r.OutputTokenThroughput, r.Goodput, r.UsefulOutputThroughput, r.OutputTokensPerRequest,
		r.TTFTP50MS, r.TTFTP95MS, r.TPOTP50MS, r.TPOTP95MS, r.LatencyP50MS, r.LatencyP95MS}
	for _, value := range values {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1e9) {
			return errors.New("measurements must be finite and non-negative")
		}
	}
	return nil
}

func (q WindowAnswers) validate() error {
	if !evalSetPattern.MatchString(q.EvalSet) || !hashPattern.MatchString(strings.TrimPrefix(q.EvalSetDigest, "sha256:")) || !hashPattern.MatchString(strings.TrimPrefix(q.ResultDigest, "sha256:")) {
		return errors.New("quality needs an eval set name and sha256 digests")
	}
	if q.PairingDigest != "" && !hashPattern.MatchString(strings.TrimPrefix(q.PairingDigest, "sha256:")) {
		return errors.New("quality.pairing_digest must be a sha256 digest")
	}
	for name, value := range map[string]string{"eval_set_name": q.EvalSetName, "eval_set_version": q.EvalSetVersion} {
		if err := boundedText(name, value, true); err != nil {
			return err
		}
	}
	if q.Grading != "" && !gradingPattern.MatchString(q.Grading) {
		return errors.New("quality.grading must be a lowercase identifier")
	}
	if q.Items < 1 || q.Items > MaxWindowItems || len(q.CandidateScores) != q.Items || (len(q.ReferenceScores) != 0 && len(q.ReferenceScores) != q.Items) {
		return errors.New("quality needs one score per item on each side it reports")
	}
	if q.ItemsAtOnce < 0 || q.ItemsAtOnce > MaxCandidateConcurrency {
		return errors.New("quality.items_at_once is out of range")
	}
	for _, score := range append(append([]float64(nil), q.CandidateScores...), q.ReferenceScores...) {
		if math.IsNaN(score) || score < 0 || score > 1 {
			return errors.New("quality scores must be between 0 and 1")
		}
	}
	if calls := q.ToolCalls; calls != nil && (calls.Cases < 0 || calls.Cases > q.Items || calls.Broken < 0 || calls.Broken > calls.Cases || calls.ReferenceInvalid < 0 || calls.ReferenceInvalid > calls.Cases || math.IsNaN(calls.Score) || calls.Score < 0 || calls.Score > 1) {
		return errors.New("quality.tool_calls is out of range")
	}
	if divergence := q.Divergence; divergence != nil {
		for _, value := range []float64{divergence.PrefillMeanKL, divergence.GenerationMeanKL, divergence.GenerationMaxKL} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e6 {
				return errors.New("quality.divergence must be finite and non-negative")
			}
		}
		if divergence.Prompts < 0 || divergence.Prompts > MaxWindowItems || divergence.PrefillTokens < 0 || divergence.GenerationTokens < 0 || divergence.TopK < 0 || divergence.TopK > 64 {
			return errors.New("quality.divergence counts are out of range")
		}
		for name, value := range map[string]string{"divergence.method": divergence.Method, "divergence.prompt_set": divergence.PromptSet, "divergence.prompt_set_version": divergence.PromptSetVersion} {
			if err := boundedText(name, value, false); err != nil {
				return err
			}
		}
	}
	if q.EvaluatedAt.IsZero() {
		return errors.New("quality.evaluated_at is required")
	}
	return nil
}

// Turns checks that runs alternate as a window must: C X C ... C with rounds
// candidate runs, or rounds runs of the current setup alone.
func (r WindowResult) Turns(rounds int, withCandidate bool) error {
	if !withCandidate {
		if len(r.Runs) != rounds {
			return fmt.Errorf("a window of the current setup alone has %d runs, not %d", rounds, len(r.Runs))
		}
		for _, run := range r.Runs {
			if run.Arm != ArmControl {
				return errors.New("a window of the current setup alone has only control runs")
			}
		}
		return nil
	}
	if len(r.Runs) != 2*rounds+1 {
		return fmt.Errorf("a window of %d rounds has %d runs, not %d", rounds, 2*rounds+1, len(r.Runs))
	}
	for index, run := range r.Runs {
		want := ArmControl
		if index%2 == 1 {
			want = ArmCandidate
		}
		if run.Arm != want {
			return fmt.Errorf("run %d is %s, want %s", index, run.Arm, want)
		}
	}
	return nil
}
