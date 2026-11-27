package optimizationcampaign

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/technique"
)

// A kernel-tuning proof measures one change, the engine's own kernels tuned
// for the GPU and the workload's shapes, against the same setup without it.
// Both are served in turn on one GPU, restarting the engine between runs:
// control, candidate, control, ..., control, with four candidate runs, the
// campaign's bracketing. Serving both on one machine removes the difference
// between machines, which reached 13% to 18% on one H100 type this month,
// far above a kernel's likely gain. A second window on a fresh machine must
// agree before the change is proposed, as for any campaign winner. The
// paired comparison is the campaign judge's (comparePaired), including its
// machine allowance; the answer check is the task-set rule
// (qualitycheck.PairedTask). tools/kernel-tune writes the windows.

// KernelTuningSchema names the window record tools/kernel-tune writes.
const KernelTuningSchema = "infercrane.kernel-tuning-window/v1"

// KernelInvocationProofSchema names the evidence file kernel-invocation-proof.
const KernelInvocationProofSchema = "infercrane.kernel-invocation-proof/v1"

// Kernel-tuning proof decisions.
const (
	KernelTuningPropose      = "propose"
	KernelTuningNoGain       = "no_measurable_gain"
	KernelTuningReject       = "reject"
	KernelTuningUnknown      = "unknown"
	KernelTuningConfirmed    = "confirmed"
	KernelTuningNotConfirmed = "not_confirmed"
)

const maxKernelTuningCells = 8

// KernelTuningWindow is one machine's measurement.
type KernelTuningWindow struct {
	Schema         string              `json:"schema"`
	Window         string              `json:"window"`
	Machine        KernelTuningMachine `json:"machine"`
	Runtime        string              `json:"runtime"`
	RuntimeVersion string              `json:"runtime_version"`
	SetDigest      string              `json:"set_digest"`
	// Files are the tuned set's file names; the candidate must read every
	// one, and the control must report the default for every one.
	Files   []string             `json:"files"`
	Cells   []string             `json:"cells"`
	Runs    []KernelTuningRun    `json:"runs"`
	Quality *KernelTuningQuality `json:"quality,omitempty"`
	// Sequential holds answers asked one request at a time, so batching
	// cannot change them: whether the tuned kernels change answers at
	// temperature 0.
	Sequential *KernelTuningSequential `json:"sequential,omitempty"`
	// Reproduction is what another party needs to run the window again;
	// windows recorded before it existed have none, and their results stay
	// "not shown".
	Reproduction *KernelTuningReproduction `json:"reproduction,omitempty"`
}

// KernelTuningReproduction names the exact command, commit, image and
// content-free traffic shape of a window, and the precision both arms
// served.
type KernelTuningReproduction struct {
	Command      string            `json:"command"`
	Commit       string            `json:"commit"`
	ImageDigest  string            `json:"image_digest"`
	TrafficShape string            `json:"traffic_shape"`
	ShapeDigests map[string]string `json:"shape_digests"`
	Weights      string            `json:"weights"`
	KVCache      string            `json:"kv_cache"`
}

// KernelTuningSequential is a set of prompts and each named start's answers.
type KernelTuningSequential struct {
	Prompts []string                        `json:"prompts"`
	Answers map[string][]KernelTuningAnswer `json:"answers"`
}

// Whether a tuned set changes answers at temperature 0, one request at a
// time: "no" only when the tuned start matched the control on every prompt
// and two control starts matched each other; "yes" when the controls
// matched and the tuned start did not; otherwise "unknown".
const (
	AnswersUnchanged = "no"
	AnswersChanged   = "yes"
	AnswersUnknown   = "unknown"
)

// KernelTuningMachine names the GPU a window ran on.
type KernelTuningMachine struct {
	GPU    string `json:"gpu"`
	Driver string `json:"driver"`
	Host   string `json:"host,omitempty"`
}

// KernelTuningRun is one server start: its arm, what its log said about the
// fused-MoE kernel, and each cell's measurement.
type KernelTuningRun struct {
	Arm    string                         `json:"arm"`
	Engine kerneltuning.EngineReading     `json:"engine"`
	Cells  map[string]KernelTuningCellRun `json:"cells"`
}

// KernelTuningCellRun is one load measured on one server start.
type KernelTuningCellRun struct {
	Requests              int      `json:"requests"`
	Failed                int      `json:"failed"`
	OutputTokenThroughput *float64 `json:"output_token_throughput"`
	TTFTP95MS             *float64 `json:"ttft_p95_ms,omitempty"`
	TPOTP95MS             *float64 `json:"tpot_p95_ms,omitempty"`
	LatencyP95MS          *float64 `json:"latency_p95_ms,omitempty"`
}

// KernelTuningQuality holds a task set's items and the answers of named
// server starts ("control-1", "candidate-1", "control-2").
type KernelTuningQuality struct {
	Items   []KernelTuningTaskItem          `json:"items"`
	Answers map[string][]KernelTuningAnswer `json:"answers"`
}

// KernelTuningTaskItem is one task with its gold answer, a number or a
// letter from A to D.
type KernelTuningTaskItem struct {
	ID   string `json:"id"`
	Gold string `json:"gold"`
}

// KernelTuningAnswer is one server's answer to one task.
type KernelTuningAnswer struct {
	ID   string `json:"id"`
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

// KernelTuningVerdict is the decision on a tuned set.
type KernelTuningVerdict struct {
	SetDigest string                    `json:"set_digest"`
	Decision  string                    `json:"decision"`
	Reasons   []string                  `json:"reasons,omitempty"`
	Warnings  []string                  `json:"warnings,omitempty"`
	Cells     []KernelTuningCellVerdict `json:"cells"`
	// Quality compares the tuned server's task answers with the control's.
	Quality *qualitycheck.TaskComparison `json:"quality,omitempty"`
	// IdenticalAnswers is the share of task answers that are the same text:
	// two starts of the control, and the control and the tuned server.
	IdenticalAnswers map[string]float64 `json:"identical_answers,omitempty"`
	// ChangesAnswers says whether the tuned kernels change answers asked one
	// at a time (AnswersUnchanged, AnswersChanged or AnswersUnknown), with
	// the shares of identical texts behind it.
	ChangesAnswers      string             `json:"changes_answers,omitempty"`
	SequentialIdentical map[string]float64 `json:"sequential_identical,omitempty"`
	// InvocationProof is the evidence kernel-invocation-proof: what every
	// start's engine log said about the fused-MoE kernel. Its file's sha256
	// is the evidence digest on every result.
	InvocationProof *KernelInvocationProof `json:"invocation_proof,omitempty"`
	// Results are the technique contract's records, one per load.
	Results []technique.Result `json:"results,omitempty"`
}

// KernelInvocationProof lists every server start of both windows with what
// its engine log said about the fused-MoE kernel and whether the start
// served what its arm says (kernelTuningInvoked).
type KernelInvocationProof struct {
	Schema    string                   `json:"schema"`
	SetDigest string                   `json:"set_digest"`
	MountPath string                   `json:"mount_path"`
	Files     []string                 `json:"files"`
	Starts    []KernelInvocationRecord `json:"starts"`
}

// KernelInvocationRecord is one server start's reading.
type KernelInvocationRecord struct {
	Window        string   `json:"window"`
	Run           int      `json:"run"`
	Arm           string   `json:"arm"`
	Backend       string   `json:"backend,omitempty"`
	ReadFromMount []string `json:"read_from_mount,omitempty"`
	DefaultFor    []string `json:"default_for,omitempty"`
	ServedItsArm  bool     `json:"served_its_arm"`
	Reason        string   `json:"reason,omitempty"`
}

// Encode is the evidence file's exact bytes; its sha256 is the digest.
func (p KernelInvocationProof) Encode() ([]byte, error) {
	body, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func kernelInvocationProof(first KernelTuningWindow, fresh *KernelTuningWindow) KernelInvocationProof {
	proof := KernelInvocationProof{Schema: KernelInvocationProofSchema, SetDigest: first.SetDigest, MountPath: kerneltuning.MountPath, Files: slices.Clone(first.Files)}
	for _, window := range []*KernelTuningWindow{&first, fresh} {
		if window == nil {
			continue
		}
		for index, run := range window.Runs {
			reason := kernelTuningInvoked(*window, run)
			proof.Starts = append(proof.Starts, KernelInvocationRecord{Window: window.Window, Run: index + 1, Arm: run.Arm, Backend: run.Engine.Backend,
				ReadFromMount: run.Engine.ReadFrom(kerneltuning.MountPath), DefaultFor: slices.Clone(run.Engine.DefaultFor), ServedItsArm: reason == "", Reason: reason})
		}
	}
	return proof
}

// evidence is the result's kernel-invocation-proof item, or false when any
// start did not serve its arm: then the item is missing and every result
// stays "not shown".
func (p KernelInvocationProof) evidence() (technique.EvidenceItem, bool, error) {
	candidates, controls := 0, 0
	for _, start := range p.Starts {
		if !start.ServedItsArm {
			return technique.EvidenceItem{}, false, nil
		}
		if start.Arm == "candidate" {
			candidates++
		} else {
			controls++
		}
	}
	if candidates == 0 || controls == 0 {
		return technique.EvidenceItem{}, false, nil
	}
	body, err := p.Encode()
	if err != nil {
		return technique.EvidenceItem{}, false, err
	}
	sum := sha256.Sum256(body)
	return technique.EvidenceItem{Name: kerneltuning.EvidenceKernelInvocation, Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Summary: fmt.Sprintf("%d tuned starts read every tuned file from %s; %d control starts reported the engine's default", candidates, p.MountPath, controls)}, true, nil
}

// KernelTuningCellVerdict is one load's result in both windows.
type KernelTuningCellVerdict struct {
	Cell     string                     `json:"cell"`
	Decision string                     `json:"decision"`
	First    KernelTuningWindowVerdict  `json:"first"`
	Fresh    *KernelTuningWindowVerdict `json:"fresh,omitempty"`
}

// KernelTuningWindowVerdict is one window's comparison for one load.
type KernelTuningWindowVerdict struct {
	Valid      bool                        `json:"valid"`
	Reason     string                      `json:"reason,omitempty"`
	MaxDrift   float64                     `json:"max_adjacent_control_drift,omitempty"`
	Comparison map[string]MetricComparison `json:"comparison,omitempty"`
}

// JudgeKernelTuning decides on a tuned set from its first window and,
// when measured, the window on a fresh machine.
func JudgeKernelTuning(first KernelTuningWindow, fresh *KernelTuningWindow) (KernelTuningVerdict, error) {
	if err := checkKernelTuningWindow(first, "first"); err != nil {
		return KernelTuningVerdict{}, err
	}
	if fresh != nil {
		if err := checkKernelTuningWindow(*fresh, "fresh"); err != nil {
			return KernelTuningVerdict{}, err
		}
		if fresh.SetDigest != first.SetDigest || !slices.Equal(fresh.Cells, first.Cells) || !slices.Equal(fresh.Files, first.Files) ||
			fresh.Runtime != first.Runtime || fresh.RuntimeVersion != first.RuntimeVersion {
			return KernelTuningVerdict{}, errors.New("the fresh window measured another set, engine or load than the first")
		}
	}
	verdict := KernelTuningVerdict{SetDigest: first.SetDigest}
	comparisons := len(first.Cells)
	confirmed, unknown, regressed := 0, 0, 0
	for _, cell := range first.Cells {
		result := KernelTuningCellVerdict{Cell: cell, First: judgeKernelTuningCell(first, cell, comparisons)}
		if fresh != nil {
			window := judgeKernelTuningCell(*fresh, cell, comparisons)
			result.Fresh = &window
		}
		result.Decision = kernelTuningCellDecision(result)
		switch result.Decision {
		case KernelTuningConfirmed:
			confirmed++
		case KernelTuningUnknown:
			unknown++
		case DecisionRegressed:
			regressed++
		}
		for _, window := range []*KernelTuningWindowVerdict{&result.First, result.Fresh} {
			if window == nil || !window.Valid {
				continue
			}
			for _, metric := range []string{"ttft_p95_ms", "tpot_p95_ms"} {
				if comparison, ok := window.Comparison[metric]; ok && comparison.Decision == DecisionRegressed {
					verdict.Warnings = append(verdict.Warnings, fmt.Sprintf("%s: %s got worse with the tuned kernels (%+.1f%%)", cell, metric, comparison.Delta*100))
				}
			}
		}
		verdict.Cells = append(verdict.Cells, result)
	}
	quality := "missing"
	if first.Quality != nil {
		comparison, identical, err := judgeKernelTuningAnswers(*first.Quality)
		if err != nil {
			return KernelTuningVerdict{}, err
		}
		verdict.Quality, verdict.IdenticalAnswers, quality = &comparison, identical, comparison.Verdict
	}
	if first.Sequential != nil {
		verdict.ChangesAnswers, verdict.SequentialIdentical = judgeSequentialAnswers(*first.Sequential)
	}
	switch {
	case regressed > 0:
		verdict.Decision = KernelTuningReject
		verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("%d of %d loads served less output with the tuned kernels", regressed, len(first.Cells)))
	case quality == qualitycheck.TaskWorse:
		verdict.Decision = KernelTuningReject
		verdict.Reasons = append(verdict.Reasons, "the tuned server scored worse on the task set")
	case confirmed > 0 && (quality == qualitycheck.TaskNoDifference || quality == qualitycheck.TaskBetter):
		verdict.Decision = KernelTuningPropose
		verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("more output at %d of %d loads, on this machine and on a fresh one, with no measurable difference on the task set", confirmed, len(first.Cells)))
	case confirmed > 0:
		verdict.Decision = KernelTuningUnknown
		verdict.Reasons = append(verdict.Reasons, "the gain held on a fresh machine, but the task set did not settle whether answers stayed as good ("+quality+")")
	case unknown > 0:
		verdict.Decision = KernelTuningUnknown
		verdict.Reasons = append(verdict.Reasons, fmt.Sprintf("%d of %d loads could not be compared reliably", unknown, len(first.Cells)))
	default:
		verdict.Decision = KernelTuningNoGain
		verdict.Reasons = append(verdict.Reasons, "no load served measurably more output with the tuned kernels")
	}
	proof := kernelInvocationProof(first, fresh)
	verdict.InvocationProof = &proof
	item, complete, err := proof.evidence()
	if err != nil {
		return KernelTuningVerdict{}, err
	}
	verdict.Results, err = kernelTuningResults(first, fresh, verdict)
	if err != nil {
		return KernelTuningVerdict{}, err
	}
	for index := range verdict.Results {
		verdict.Results[index].RequiredEvidence = kerneltuning.RequiredEvidence()
		if complete {
			verdict.Results[index].Evidence = append(verdict.Results[index].Evidence, item)
		}
	}
	return verdict, nil
}

func kernelTuningCellDecision(result KernelTuningCellVerdict) string {
	first, ok := result.First.Comparison["output_token_throughput"]
	if !result.First.Valid || !ok {
		return KernelTuningUnknown
	}
	if first.Decision != DecisionImproved {
		return first.Decision
	}
	if result.Fresh == nil || !result.Fresh.Valid {
		return KernelTuningUnknown
	}
	switch fresh, ok := result.Fresh.Comparison["output_token_throughput"]; {
	case !ok:
		return KernelTuningUnknown
	case fresh.Decision == DecisionImproved:
		return KernelTuningConfirmed
	case fresh.Decision == DecisionRegressed:
		return DecisionRegressed
	default:
		return KernelTuningNotConfirmed
	}
}

// checkKernelTuningWindow refuses a record that is not a bracketed window:
// control first and last, control and candidate alternating, the
// campaign's four candidate runs, every cell in every run.
func checkKernelTuningWindow(window KernelTuningWindow, want string) error {
	switch {
	case window.Schema != KernelTuningSchema:
		return fmt.Errorf("%s window: schema %q, want %s", want, window.Schema, KernelTuningSchema)
	case window.Window != want:
		return fmt.Errorf("window is %q where the %s window belongs", window.Window, want)
	case len(window.Cells) == 0 || len(window.Cells) > maxKernelTuningCells:
		return fmt.Errorf("%s window: 1 to %d loads", want, maxKernelTuningCells)
	case len(window.Files) == 0 || window.SetDigest == "":
		return fmt.Errorf("%s window: no tuned set named", want)
	case len(window.Runs) != 2*measurementRunsPerArm+1:
		return fmt.Errorf("%s window: %d runs, want %d (control first and last around %d candidate runs)", want, len(window.Runs), 2*measurementRunsPerArm+1, measurementRunsPerArm)
	}
	for index, run := range window.Runs {
		wantArm := "control"
		if index%2 == 1 {
			wantArm = "candidate"
		}
		if run.Arm != wantArm {
			return fmt.Errorf("%s window: run %d is %q, want %s", want, index+1, run.Arm, wantArm)
		}
		for _, cell := range window.Cells {
			if _, ok := run.Cells[cell]; !ok {
				return fmt.Errorf("%s window: run %d did not measure %s", want, index+1, cell)
			}
		}
	}
	return nil
}

// kernelTuningInvoked is the reason a run did not serve what its arm says,
// or "": the candidate read every tuned file from the mount and fell back
// for none; the control read none and fell back for every one, so the
// comparison is tuned against the engine's default.
func kernelTuningInvoked(window KernelTuningWindow, run KernelTuningRun) string {
	read := run.Engine.ReadFrom(kerneltuning.MountPath)
	if run.Engine.Backend != "" && run.Engine.Backend != "TRITON" {
		return fmt.Sprintf("the engine chose the %s MoE backend, which reads no tuned file", run.Engine.Backend)
	}
	for _, name := range window.Files {
		switch {
		case run.Arm == "candidate" && (!slices.Contains(read, name) || slices.Contains(run.Engine.DefaultFor, name)):
			return "a tuned run's engine did not read " + name
		case run.Arm == "control" && (slices.Contains(read, name) || !slices.Contains(run.Engine.DefaultFor, name)):
			return "a control run's engine did not report its default for " + name
		}
	}
	return ""
}

func judgeKernelTuningCell(window KernelTuningWindow, cell string, comparisons int) KernelTuningWindowVerdict {
	runs := make([]RunSummary, 0, len(window.Runs))
	var previous *float64
	verdict := KernelTuningWindowVerdict{Valid: true}
	for index, run := range window.Runs {
		if reason := kernelTuningInvoked(window, run); reason != "" {
			return KernelTuningWindowVerdict{Reason: fmt.Sprintf("run %d: %s", index+1, reason)}
		}
		measured := run.Cells[cell]
		if measured.Failed > 0 || measured.Requests == 0 || measured.OutputTokenThroughput == nil || *measured.OutputTokenThroughput <= 0 {
			return KernelTuningWindowVerdict{Reason: fmt.Sprintf("run %d was not clean (%d of %d requests failed)", index+1, measured.Failed, measured.Requests)}
		}
		if run.Arm == "control" {
			if previous != nil {
				verdict.MaxDrift = math.Max(verdict.MaxDrift, math.Abs(*measured.OutputTokenThroughput-*previous) / *previous)
			}
			previous = measured.OutputTokenThroughput
		}
		runs = append(runs, RunSummary{Arm: run.Arm, Requests: measured.Requests, Failed: measured.Failed, OutputTokenThroughput: measured.OutputTokenThroughput,
			TTFTP95MS: measured.TTFTP95MS, TPOTP95MS: measured.TPOTP95MS, LatencyP95MS: measured.LatencyP95MS})
	}
	if verdict.MaxDrift > maxControlDrift {
		return KernelTuningWindowVerdict{MaxDrift: verdict.MaxDrift, Reason: fmt.Sprintf("control output drifted %.1f%% between adjacent runs, above the %.0f%% limit", verdict.MaxDrift*100, maxControlDrift*100)}
	}
	verdict.Comparison = comparePairedArms(runs, comparisons)
	if _, ok := verdict.Comparison["output_token_throughput"]; !ok {
		return KernelTuningWindowVerdict{MaxDrift: verdict.MaxDrift, Reason: "output was not compared"}
	}
	return verdict
}

// judgeKernelTuningAnswers scores each task answer by its answer line and
// compares the tuned server's first start with the control's, item by item.
func judgeKernelTuningAnswers(quality KernelTuningQuality) (qualitycheck.TaskComparison, map[string]float64, error) {
	if len(quality.Items) < 2 || len(quality.Items) > qualitycheck.MaxTaskPrompts {
		return qualitycheck.TaskComparison{}, nil, fmt.Errorf("a task set holds 2 to %d items", qualitycheck.MaxTaskPrompts)
	}
	byID := func(name string) (map[string]KernelTuningAnswer, error) {
		answers, ok := quality.Answers[name]
		if !ok {
			return nil, fmt.Errorf("task answers of %s are missing", name)
		}
		out := make(map[string]KernelTuningAnswer, len(answers))
		for _, answer := range answers {
			out[answer.ID] = answer
		}
		return out, nil
	}
	control, err := byID("control-1")
	if err != nil {
		return qualitycheck.TaskComparison{}, nil, err
	}
	candidate, err := byID("candidate-1")
	if err != nil {
		return qualitycheck.TaskComparison{}, nil, err
	}
	score := func(answers map[string]KernelTuningAnswer, item KernelTuningTaskItem) float64 {
		answer, ok := answers[item.ID]
		if !ok || !answer.OK {
			return 0
		}
		return qualitycheck.Grade(qualitycheck.GradingAnswerLine, qualitycheck.Prompt{ID: item.ID, Expected: []string{item.Gold}}, answer.Text)
	}
	reference, tuned := make([]float64, len(quality.Items)), make([]float64, len(quality.Items))
	for index, item := range quality.Items {
		reference[index], tuned[index] = score(control, item), score(candidate, item)
	}
	identical := map[string]float64{"control-1/candidate-1": sameAnswers(quality.Items, control, candidate)}
	if again, err := byID("control-2"); err == nil {
		identical["control-1/control-2"] = sameAnswers(quality.Items, control, again)
	}
	return qualitycheck.PairedTask(reference, tuned), identical, nil
}

func sameAnswers(items []KernelTuningTaskItem, a, b map[string]KernelTuningAnswer) float64 {
	same, both := 0, 0
	for _, item := range items {
		left, leftOK := a[item.ID]
		right, rightOK := b[item.ID]
		if !leftOK || !rightOK || !left.OK || !right.OK {
			continue
		}
		both++
		if left.Text == right.Text {
			same++
		}
	}
	if both == 0 {
		return 0
	}
	return math.Round(float64(same)/float64(both)*1000) / 1000
}

// judgeSequentialAnswers compares answers asked one at a time: the tuned
// start's with the control's, and two control starts with each other. Every
// answer must have succeeded for the comparison to count.
func judgeSequentialAnswers(sequential KernelTuningSequential) (string, map[string]float64) {
	byID := func(name string) (map[string]KernelTuningAnswer, bool) {
		answers, ok := sequential.Answers[name]
		if !ok || len(answers) != len(sequential.Prompts) {
			return nil, false
		}
		out := make(map[string]KernelTuningAnswer, len(answers))
		for _, answer := range answers {
			if !answer.OK {
				return nil, false
			}
			out[answer.ID] = answer
		}
		return out, len(out) == len(sequential.Prompts)
	}
	control, okControl := byID("control-1")
	candidate, okCandidate := byID("candidate-1")
	again, okAgain := byID("control-2")
	if len(sequential.Prompts) == 0 || !okControl || !okCandidate || !okAgain {
		return AnswersUnknown, nil
	}
	share := func(a, b map[string]KernelTuningAnswer) float64 {
		same := 0
		for _, id := range sequential.Prompts {
			if a[id].Text == b[id].Text {
				same++
			}
		}
		return math.Round(float64(same)/float64(len(sequential.Prompts))*1000) / 1000
	}
	identical := map[string]float64{"control-1/candidate-1": share(control, candidate), "control-1/control-2": share(control, again)}
	switch {
	case identical["control-1/control-2"] < 1:
		return AnswersUnknown, identical
	case identical["control-1/candidate-1"] < 1:
		return AnswersChanged, identical
	}
	return AnswersUnchanged, identical
}

// Encode is the window as the fresh-machine-confirmation evidence file holds
// it; a confirmed result's digest is this encoding's sha256.
func (w KernelTuningWindow) Encode() ([]byte, error) {
	return json.Marshal(w)
}

// kernelTuningResults writes one technique-contract record per load: the
// first window's paired change and noise floor, improved only when the
// fresh machine confirmed it, and answers passed only when the task set
// showed no measurable difference or better. A confirmed load carries the
// fresh window's digest as its fresh-machine confirmation. A field the
// window did not record stays empty, so the record says what is unknown.
func kernelTuningResults(first KernelTuningWindow, fresh *KernelTuningWindow, verdict KernelTuningVerdict) ([]technique.Result, error) {
	var confirmation *technique.EvidenceItem
	if fresh != nil {
		body, err := fresh.Encode()
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		confirmation = &technique.EvidenceItem{Name: technique.EvidenceFreshMachineConfirmation, Digest: "sha256:" + hex.EncodeToString(sum[:]),
			Summary: "the fresh machine's window for the same set, engine and loads"}
	}
	reproduction := KernelTuningReproduction{}
	if first.Reproduction != nil {
		reproduction = *first.Reproduction
	}
	correctness := technique.Correctness{Method: "task-score", Verdict: technique.DecisionUnknown}
	if verdict.Quality != nil {
		switch verdict.Quality.Verdict {
		case qualitycheck.TaskNoDifference, qualitycheck.TaskBetter:
			correctness.Verdict = "passed"
		case qualitycheck.TaskWorse:
			correctness.Verdict = "failed"
		}
	}
	if verdict.ChangesAnswers != "" {
		correctness.Method = "task-score+answer-agreement"
	}
	var out []technique.Result
	for _, cell := range verdict.Cells {
		load, requests := 0, 0
		fmt.Sscanf(cell.Cell[strings.LastIndex(cell.Cell, "/")+1:], "%d", &load)
		for _, run := range first.Runs {
			if measured, ok := run.Cells[cell.Cell]; ok && run.Arm == "candidate" {
				requests += measured.Requests
			}
		}
		comparison := cell.First.Comparison["output_token_throughput"]
		decision := technique.DecisionUnknown
		switch cell.Decision {
		case KernelTuningConfirmed:
			decision = technique.DecisionImproved
		case DecisionNoMeasurableDifference, KernelTuningNotConfirmed:
			decision = technique.DecisionSame
		case DecisionRegressed:
			decision = technique.DecisionWorse
		}
		basis := "same-machine"
		var evidence []technique.EvidenceItem
		if cell.Decision == KernelTuningConfirmed && confirmation != nil {
			basis, evidence = "same-machine+fresh-servers", []technique.EvidenceItem{*confirmation}
		}
		out = append(out, technique.Result{
			SchemaVersion: technique.ResultSchemaV1, Technique: kerneltuning.TechniqueID, TechniqueVersion: kerneltuning.TechniqueVersion, Candidate: first.SetDigest,
			Hardware:     technique.Hardware{Accelerator: first.Machine.GPU, Count: 1, Driver: first.Machine.Driver},
			Software:     technique.Software{Engine: first.Runtime, EngineVersion: first.RuntimeVersion, ImageDigest: reproduction.ImageDigest},
			Workload:     technique.Workload{ShapeDigest: reproduction.ShapeDigests[cell.Cell], Kind: cell.Cell, Load: load, Requests: requests},
			Precision:    technique.Precision{Weights: reproduction.Weights, KVCache: reproduction.KVCache},
			Baseline:     technique.Baseline{Setup: "the same engine without the tuned set", Basis: basis},
			Correctness:  correctness,
			Measured:     technique.Measured{Metric: "output_tokens_per_second", Change: comparison.Delta, NoiseFloor: comparison.NoiseFloor, Decision: decision},
			Reproduction: technique.Reproduction{Command: reproduction.Command, Commit: reproduction.Commit, ImageDigest: reproduction.ImageDigest, TrafficShape: reproduction.TrafficShape},
			Evidence:     evidence,
		})
	}
	return out, nil
}
