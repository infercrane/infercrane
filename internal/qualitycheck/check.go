package qualitycheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

const (
	// Evaluator names InferCrane's built-in check in evidence. The control API
	// refuses customer evidence that claims this name.
	Evaluator = "infercrane-builtin"
	// EvaluatorVersion covers the request settings, the agreement score, and
	// the thresholds below. Changing any of them requires a new version.
	// Version 2 turns reasoning off where the chat template allows it, removes
	// reasoning a template opened in the prompt, and stops instead of scoring
	// a prompt the reference left without answer text.
	// Version 3 sends v2 cases' conversations and tools, scores tool cases on
	// the calls, counts a call as an answer, and blocks a candidate that
	// breaks a call the reference made correctly.
	EvaluatorVersion = "3"
	// TaskEvaluator and TaskEvaluatorVersion name the check on a task set:
	// each side's answers scored right or wrong against gold answers and
	// compared item by item, not agreement with the reference.
	TaskEvaluator        = "infercrane-task"
	TaskEvaluatorVersion = "1"
	// WatchRegressionPercent and BlockRegressionPercent are the built-in
	// thresholds. A candidate whose answers agree with the reference less than
	// 95% of the way is WATCH; less than 90% is BLOCK.
	WatchRegressionPercent = 5.0
	BlockRegressionPercent = 10.0
)

// ErrReferenceUnanswered means the reference gave no answer text and made no
// tool call to at least one prompt within the eval set's token limit, so its
// answers cannot serve as the reference. Asking again does not help: decoding
// is greedy.
var ErrReferenceUnanswered = errors.New("the reference gave no answer text")

// CodeToolCallBroken is the verdict code of a candidate that broke a tool
// call the reference made correctly.
const CodeToolCallBroken = "tool_call_broken"

// Comparison is the in-memory result of one candidate against its reference.
// It holds scores and digests only; answers are discarded.
type Comparison struct {
	Set EvalSet
	// ReferenceID is the reference's subject: a revision ID, or
	// binding:<id> for a closed API.
	ReferenceID         string
	CandidateRevisionID string
	// Scores holds one agreement score per prompt, in eval-set order.
	Scores []float64
	Score  float64
	// ToolCalls is the score over cases that declare tools, or nil when the
	// eval set has none.
	ToolCalls *qualityevidence.ToolCallScores
	// ReferenceScores and Task are set for a task set: the reference's own
	// score per item and the paired comparison of the two.
	ReferenceScores []float64
	Task            *TaskComparison
	// ItemsAtOnce is how many task items each server was asked at once.
	ItemsAtOnce int
	// Divergence is the candidate's KL divergence from the reference on the
	// reference's own answers (MeasureDivergence), set by the caller for a
	// change of numeric precision. It is evidence and decides nothing.
	Divergence *qualityevidence.Divergence
	// ResultDigest commits to the eval set, both revisions, a digest of every
	// answer, and the scores, without storing the answers.
	ResultDigest string
	Eval         releaseguard.QualityEval
	EvaluatedAt  time.Time
}

// Compare asks the reference and the candidate every prompt in turn and
// scores the candidate's answers against the reference's. When both targets
// are the same revision the candidate is the reference, so each prompt is
// asked once and agreement is 1 by definition.
// CompareOption changes how Compare judges a candidate.
type CompareOption func(*compareOptions)

type compareOptions struct {
	requireHeld bool
	// earlier are server pairs of the same two setups to pool (PoolWith).
	earlier []ServerPair
}

// RequireAnswersHeld judges a task set by the rule for setups that change
// answers (RequireHeld): they pass only when their answers are shown to hold
// within HeldMargin. Other eval sets are judged as before.
func RequireAnswersHeld() CompareOption {
	return func(options *compareOptions) { options.requireHeld = true }
}

func Compare(ctx context.Context, completer Completer, set EvalSet, reference, candidate Target, now func() time.Time, options ...CompareOption) (Comparison, error) {
	var chosen compareOptions
	for _, option := range options {
		option(&chosen)
	}
	if completer == nil || len(set.Prompts) == 0 || !ValidDigest(set.Digest) || reference.SubjectID == "" || candidate.SubjectID == "" {
		return Comparison{}, errors.New("quality check requires a completer, a decoded eval set, and both revisions")
	}
	if set.Task() {
		return compareTask(ctx, completer, set, reference, candidate, now, chosen)
	}
	self := reference.SubjectID == candidate.SubjectID
	scores := make([]float64, 0, len(set.Prompts))
	referenceAnswers := make([]string, 0, len(set.Prompts))
	candidateAnswers := make([]string, 0, len(set.Prompts))
	var unanswered []string
	var toolScores []float64
	broken, referenceInvalid := 0, 0
	var candidateKinds, referenceKinds qualityevidence.ToolCallFailures
	for _, prompt := range set.Prompts {
		referenceAnswer, err := completer.Complete(ctx, reference, set, prompt)
		if err != nil {
			return Comparison{}, fmt.Errorf("reference revision: %w", err)
		}
		if !referenceAnswer.answered() {
			// Scoring this prompt would compare nothing with nothing.
			unanswered = append(unanswered, prompt.ID)
		}
		if len(unanswered) > 0 {
			// The check cannot pass any more; only count what is left.
			continue
		}
		candidateAnswer := referenceAnswer
		if !self {
			if candidateAnswer, err = completer.Complete(ctx, candidate, set, prompt); err != nil {
				return Comparison{}, fmt.Errorf("candidate revision: %w", err)
			}
		}
		score := Agreement(referenceAnswer.Content, candidateAnswer.Content)
		if prompt.HasTools() {
			result := scoreToolCase(prompt, referenceAnswer, candidateAnswer)
			if self {
				// The reference agrees with itself by definition.
				result.score, result.broken, result.candidate = 1, false, qualityevidence.ToolCallFailures{}
			}
			candidateKinds, referenceKinds = candidateKinds.Add(result.candidate), referenceKinds.Add(result.reference)
			score = result.score
			toolScores = append(toolScores, score)
			if result.broken {
				broken++
			}
			if result.referenceInvalid {
				referenceInvalid++
			}
		}
		scores = append(scores, score)
		referenceAnswers = append(referenceAnswers, referenceAnswer.digest())
		candidateAnswers = append(candidateAnswers, candidateAnswer.digest())
	}
	if len(unanswered) > 0 {
		return Comparison{}, fmt.Errorf("%w to %d of %d prompts within %d tokens (%s)", ErrReferenceUnanswered, len(unanswered), len(set.Prompts), set.MaxTokens, promptList(unanswered))
	}
	score := round(mean(scores))
	var toolCalls *qualityevidence.ToolCallScores
	if len(toolScores) > 0 {
		toolCalls = &qualityevidence.ToolCallScores{Cases: len(toolScores), Score: round(mean(toolScores)), Broken: broken, ReferenceInvalid: referenceInvalid, Candidate: candidateKinds, Reference: referenceKinds}
	}
	bundle, _ := json.Marshal(map[string]any{
		"evaluator": Evaluator, "evaluator_version": EvaluatorVersion, "eval_set": set.Digest,
		"reference_revision_id": reference.SubjectID, "candidate_revision_id": candidate.SubjectID,
		"reference_answers": referenceAnswers, "candidate_answers": candidateAnswers, "scores": scores, "tool_calls": toolCalls,
	})
	stamp := time.Now().UTC()
	if now != nil {
		stamp = now().UTC()
	}
	comparison := Comparison{Set: set, ReferenceID: reference.SubjectID, CandidateRevisionID: candidate.SubjectID, Scores: scores, Score: score, ToolCalls: toolCalls, ResultDigest: Digest(bundle), EvaluatedAt: stamp.Truncate(time.Second)}
	comparison.Eval = Classify(set, score, scores, toolCalls)
	return comparison, nil
}

// compareTask asks both setups every item of a task set and scores each
// answer against the item's gold answer. An answer with no text is wrong, on
// either side: the task score counts it, as the map's rule does.
func compareTask(ctx context.Context, completer Completer, set EvalSet, reference, candidate Target, now func() time.Time, options compareOptions) (Comparison, error) {
	self := reference.SubjectID == candidate.SubjectID
	referenceAnswered, err := askAll(ctx, completer, reference, set)
	if err != nil {
		return Comparison{}, fmt.Errorf("reference revision: %w", err)
	}
	candidateAnswered := referenceAnswered
	if !self {
		if candidateAnswered, err = askAll(ctx, completer, candidate, set); err != nil {
			return Comparison{}, fmt.Errorf("candidate revision: %w", err)
		}
	}
	referenceScores := make([]float64, 0, len(set.Prompts))
	candidateScores := make([]float64, 0, len(set.Prompts))
	referenceAnswers := make([]string, 0, len(set.Prompts))
	candidateAnswers := make([]string, 0, len(set.Prompts))
	for index, prompt := range set.Prompts {
		referenceScores = append(referenceScores, Grade(set.Grading, prompt, referenceAnswered[index].Content))
		candidateScores = append(candidateScores, Grade(set.Grading, prompt, candidateAnswered[index].Content))
		referenceAnswers = append(referenceAnswers, referenceAnswered[index].digest())
		candidateAnswers = append(candidateAnswers, candidateAnswered[index].digest())
	}
	task, pooled := judgeTask(set, reference.SubjectID, candidate.SubjectID, referenceScores, candidateScores, options)
	committed := map[string]any{
		"evaluator": TaskEvaluator, "evaluator_version": TaskEvaluatorVersion, "eval_set": set.Digest, "grading": set.Grading,
		"reference_revision_id": reference.SubjectID, "candidate_revision_id": candidate.SubjectID,
		"reference_answers": referenceAnswers, "candidate_answers": candidateAnswers,
		"reference_scores": referenceScores, "candidate_scores": candidateScores, "task": task, "items_at_once": taskAtOnce,
	}
	if len(pooled) > 0 {
		// The digest commits to the earlier servers' scores the verdict read.
		committed["pooled_pairs"] = pooled
	}
	bundle, _ := json.Marshal(committed)
	stamp := time.Now().UTC()
	if now != nil {
		stamp = now().UTC()
	}
	comparison := Comparison{Set: set, ReferenceID: reference.SubjectID, CandidateRevisionID: candidate.SubjectID, Scores: candidateScores, Score: task.CandidateScore,
		ReferenceScores: referenceScores, Task: &task, ItemsAtOnce: taskAtOnce, ResultDigest: Digest(bundle), EvaluatedAt: stamp.Truncate(time.Second)}
	comparison.Eval = ClassifyTask(set, task)
	return comparison, nil
}

// judgeTask is the task-set rule both a check run here (compareTask) and a
// check scored in a customer's cluster (Scored) apply: the paired comparison
// of the servers asked now, pooled with pairs other servers of the same two
// setups answered earlier (PoolWith), read by the rule for setups that change
// answers when RequireAnswersHeld is set. Pairs that cannot pool are left out
// and the comparison says why. It returns the earlier pairs the verdict read,
// which the result digest commits to.
func judgeTask(set EvalSet, referenceID, candidateID string, referenceScores, candidateScores []float64, options compareOptions) (TaskComparison, []ServerPair) {
	task := PairedTask(referenceScores, candidateScores)
	var pooled []ServerPair
	if len(options.earlier) > 0 && referenceID != candidateID {
		current := ServerPair{EvalSet: set.Digest, ReferenceID: referenceID, CandidateID: candidateID, Reference: referenceScores, Candidate: candidateScores}
		if pooledTask, poolErr := PooledTask(append(append([]ServerPair(nil), options.earlier...), current)); poolErr != nil {
			task.PoolRefused = poolErr.Error()
		} else {
			task, pooled = pooledTask, options.earlier
		}
	}
	if options.requireHeld {
		task = RequireHeld(task)
	}
	return task, pooled
}

// ClassifyTask turns a paired task comparison into a verdict: PASS when the
// candidate is better or not measurably different, BLOCK when it is worse,
// and UNKNOWN when the interval reaches past the margin without excluding
// zero, so the items cannot tell.
func ClassifyTask(set EvalSet, task TaskComparison) releaseguard.QualityEval {
	reference := task.ReferenceScore
	eval := releaseguard.QualityEval{Suite: set.Name, SuiteVersion: set.SuiteVersion(), Evaluator: TaskEvaluator, EvaluatorVersion: TaskEvaluatorVersion,
		CandidateScore: task.CandidateScore, CandidatePassed: task.Verdict == TaskBetter || task.Verdict == TaskNoDifference || task.Verdict == TaskHeld, ReferenceScore: &reference}
	interval := fmt.Sprintf("%s right against %s on %d items: difference %+.2f points, 95%% interval %+.2f to %+.2f, margin %.0f points",
		percentText(task.CandidateScore), percentText(task.ReferenceScore), task.Items, task.Difference*100, task.Low*100, task.High*100, task.Margin*100)
	switch task.Verdict {
	case TaskBetter:
		eval.Verdict, eval.Code, eval.Message = releaseguard.QualityPass, CodeTaskBetter, "Task score better than the reference: "+interval+"."
	case TaskNoDifference:
		eval.Verdict, eval.Code, eval.Message = releaseguard.QualityPass, CodeTaskNoDifference, "No measurable difference in task score: "+interval+"."
	case TaskWorse:
		eval.Verdict, eval.Code, eval.Message = releaseguard.QualityBlock, CodeTaskWorse, "Task score worse than the reference: "+interval+"."
	case TaskHeld:
		eval.Verdict, eval.Code, eval.Message = releaseguard.QualityPass, CodeTaskHeld, "Answers held within 1 point: "+interval+"."
	case TaskNotShownHeld:
		eval.Verdict, eval.Code, eval.Message = releaseguard.QualityUnknown, CodeTaskNotShownHeld,
			fmt.Sprintf("Answers not shown to hold within 1 point on %s: %s; a setup that changes answers must show its answers held.", serversText(task.ServersPerSide), interval)
	default:
		eval.Verdict, eval.Code, eval.Message = releaseguard.QualityUnknown, CodeTaskNotSettled, "Task score not settled: "+interval+"; the interval reaches past the margin."
	}
	return eval
}

// Codes of a task set's verdict.
const (
	CodeTaskBetter       = "task_score_better"
	CodeTaskNoDifference = "task_score_no_measurable_difference"
	CodeTaskWorse        = "task_score_worse"
	CodeTaskNotSettled   = "task_score_not_settled"
	CodeTaskHeld         = "task_score_held_within_1_point"
	CodeTaskNotShownHeld = "task_score_not_shown_held_within_1_point"
)

// serversText names how many servers of each setup answered.
func serversText(servers int) string {
	if servers <= 1 {
		return "one server each"
	}
	return strconv.Itoa(servers) + " servers each"
}

func percentText(share float64) string {
	return strconv.FormatFloat(math.Round(share*1000)/10, 'f', 1, 64) + "%"
}

// Policy is the fixed policy the built-in check applies through Release
// Guard's own classifier.
func Policy() domain.ReleaseGuardPolicy {
	watch, block := WatchRegressionPercent, BlockRegressionPercent
	return domain.ReleaseGuardPolicy{Enabled: true, RequireQualityEvidence: true, QualityComparisonMode: "threshold", MaxQualityRegressionPercent: &block, QualityWatchRegressionPercent: &watch}
}

// Classify returns the PASS, WATCH, or BLOCK verdict for a candidate whose
// answers agree with the reference by score. The reference's own score is 1.
// scores may be nil when only the aggregate is known. A candidate that broke
// a tool call the reference made correctly is BLOCK whatever its score.
func Classify(set EvalSet, score float64, scores []float64, toolCalls *qualityevidence.ToolCallScores) releaseguard.QualityEval {
	broken := toolCalls != nil && toolCalls.Broken > 0
	reference := make([]float64, len(scores))
	for index := range reference {
		reference[index] = 1
	}
	pairing := ""
	if len(scores) > 0 {
		pairing = set.PairingDigest()
	}
	eval := domain.QualityEvalEvidence{
		Suite: set.Name, SuiteVersion: set.SuiteVersion(), Evaluator: Evaluator, EvaluatorVersion: EvaluatorVersion,
		Candidate: domain.QualityEvalSide{Score: score, Passed: !broken, SampleCount: len(set.Prompts), PairingDigest: pairing, Scores: scores},
		Reference: &domain.QualityEvalSide{Score: 1, Passed: true, SampleCount: len(set.Prompts), PairingDigest: pairing, Scores: reference},
	}
	report := releaseguard.CompareQuality(Policy(), []domain.QualityEvalEvidence{eval})
	verdict := report.Evals[0]
	if broken {
		verdict.Code, verdict.Message = CodeToolCallBroken, toolCallsBrokenMessage(*toolCalls)
	}
	return verdict
}

// toolCallsBrokenMessage says how many tool calls a candidate broke.
func toolCallsBrokenMessage(toolCalls qualityevidence.ToolCallScores) string {
	return fmt.Sprintf("Candidate broke %d of %d tool calls: the reference's call validated against the function's schema and the candidate's did not, or called a different function", toolCalls.Broken, toolCalls.Cases)
}

// VerdictForScore classifies a stored built-in score. It applies the same
// thresholds as Classify and needs no prompts.
func VerdictForScore(score float64) releaseguard.QualityEval {
	return Classify(EvalSet{Name: "stored", Version: "stored"}, score, nil, nil)
}

// Payload binds one side of the comparison to an exact revision. The
// reference side records score 1: its answers are the reference.
func (c Comparison) Payload(deployment string, reference bool) qualityevidence.Payload {
	revision, score, passed := c.CandidateRevisionID, c.Score, c.Eval.Verdict != releaseguard.QualityBlock
	scores, toolCalls := c.Scores, c.ToolCalls
	evaluator, version := Evaluator, EvaluatorVersion
	if c.Task != nil {
		// A task set passes a candidate only when the items show it is not
		// measurably worse; an interval past the margin does not pass.
		evaluator, version, passed = TaskEvaluator, TaskEvaluatorVersion, c.Eval.Verdict == releaseguard.QualityPass
	}
	if reference && c.Task != nil {
		// A task set scores the reference on its own: its share right.
		revision, score, passed, scores = c.ReferenceID, c.Task.ReferenceScore, true, c.ReferenceScores
	} else if reference {
		revision, score, passed = c.ReferenceID, 1, true
		scores = make([]float64, len(c.Scores))
		for index := range scores {
			scores[index] = 1
		}
		if toolCalls != nil {
			// The reference's own row: it agrees with itself, and its own
			// failures are still its own.
			toolCalls = &qualityevidence.ToolCallScores{Cases: toolCalls.Cases, Score: 1, ReferenceInvalid: toolCalls.ReferenceInvalid, Reference: toolCalls.Reference}
		}
	}
	distribution := &qualityevidence.Distribution{Schema: qualityevidence.DistributionSchema, Kind: "paired_scores", PairingDigest: c.Set.PairingDigest(), Scores: scores, ToolCalls: toolCalls}
	if !reference {
		// Divergence is the candidate's, measured from the reference.
		distribution.Divergence = c.Divergence
	}
	if c.Task != nil {
		// Both rows: the reference and the candidate were asked alike.
		distribution.ItemsAtOnce = c.ItemsAtOnce
		if !reference {
			// The candidate's row keeps its verdict, so a later reader can
			// tell an unshown hold from a worse score.
			distribution.TaskVerdict, distribution.TaskRule, distribution.TaskServers = c.Eval.Code, c.Task.Rule, c.Task.ServersPerSide
			distribution.TaskPoolRefused = c.Task.PoolRefused
		}
	}
	return qualityevidence.Payload{
		Schema: qualityevidence.Schema, Deployment: deployment, RevisionID: revision,
		Suite: c.Set.Name, SuiteVersion: c.Set.SuiteVersion(), Evaluator: evaluator, EvaluatorVersion: version,
		Score: score, Passed: passed, SampleCount: len(c.Scores), ArtifactDigest: c.ResultDigest, EvaluatedAt: c.EvaluatedAt,
		Distribution: distribution,
	}
}

// promptList names up to five prompts and counts the rest.
func promptList(ids []string) string {
	const shown = 5
	if len(ids) <= shown {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:shown], ", "), len(ids)-shown)
}

func answerDigest(answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return hex.EncodeToString(sum[:])
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func round(value float64) float64 { return math.Round(value*10000) / 10000 }

// taskAtOnce is how many task-set items the check asks one server at once. A task set
// holds up to 1,000 items with worked answers; asked one at a time on a small GPU it
// outlasts the check's limit (hours on an L4). The reference and the candidate are asked
// at the same concurrency, so batching treats both alike, and each answer keeps its item's
// place. A server whose answers do not depend on its batch scores as if asked one by one;
// a real engine's batching can change a few answers on both sides.
const taskAtOnce = 32

// askAll asks one server every item of the set, taskAtOnce at a time, and returns the
// answers in the items' order. The first failure stops the rest and is returned.
func askAll(ctx context.Context, completer Completer, target Target, set EvalSet) ([]Answer, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	answers := make([]Answer, len(set.Prompts))
	slots := make(chan struct{}, taskAtOnce)
	var (
		group   sync.WaitGroup
		once    sync.Once
		failure error
	)
	for index, prompt := range set.Prompts {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		group.Add(1)
		go func(index int, prompt Prompt) {
			defer group.Done()
			defer func() { <-slots }()
			answer, err := completer.Complete(ctx, target, set, prompt)
			if err != nil {
				once.Do(func() {
					failure = err
					cancel()
				})
				return
			}
			answers[index] = answer
		}(index, prompt)
	}
	group.Wait()
	if failure != nil {
		return nil, failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return answers, nil
}
