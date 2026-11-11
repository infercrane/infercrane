package kernelgen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/kernelliterature"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/kernelresearch"
)

const (
	generatorName         = "infercrane-kernelgen"
	generatorVersion      = "kernelgen-v1"
	sourceArtifactKind    = "kernel-source"
	verificationTolerance = 0.25
	maxPromptHistory      = 12
	literatureWindow      = 18 * 30 * 24 * time.Hour
)

// Publisher stores retained source in the worker's content-addressed store.
type Publisher interface {
	Publish(ctx context.Context, jobID, kind string, reader io.Reader, size int64) (string, error)
}

type Generator struct {
	PackDirectory string
	Author        Author
	Evaluator     Evaluator
	// Screener optionally checks correctness on CPU first, so a wrong draft
	// is rejected before it spends GPU time.
	Screener   Evaluator
	Publisher  Publisher
	Literature kernelliterature.Searcher
	// MaxAttemptCostUSD is the worst-case spend of one more attempt. The loop
	// stops before it could cross the authorized ceiling.
	MaxAttemptCostUSD float64
	Now               func() time.Time
}

type generation struct {
	generator  Generator
	request    acceleratorlab.GenerationRequest
	pack       Pack
	shapes     []string
	dtypes     []string
	literature []kernelliterature.Reference
	history    []Feedback
	bestSource string
	seen       map[string]struct{}
	spentUSD   float64
	pendingUSD float64
	receipt    kernelresearch.Receipt
	rejected   int
}

// Generate runs the bounded keep-or-reject loop for one measured hotspot. A
// loop that retains nothing returns its receipt with an empty source, so the
// rejected attempts survive as negative evidence.
func (g Generator) Generate(ctx context.Context, request acceleratorlab.GenerationRequest) (acceleratorlab.GenerationEvidence, error) {
	if request.Research == nil {
		return acceleratorlab.GenerationEvidence{}, errors.New("kernel generation requires a profile-bound research intent")
	}
	if g.Author == nil || g.Evaluator == nil || g.Publisher == nil {
		return acceleratorlab.GenerationEvidence{}, errors.New("kernel generation requires an author, isolated evaluator, and publisher")
	}
	pack, err := LoadPack(g.PackDirectory, request.Candidate.OperatorFamily)
	if err != nil {
		return acceleratorlab.GenerationEvidence{}, err
	}
	run := &generation{generator: g, request: request, pack: pack, seen: map[string]struct{}{}}
	run.shapes, run.dtypes = hotspotDetail(request)
	if run.receipt, err = kernelresearch.Start(*request.Research, g.now()); err != nil {
		return acceleratorlab.GenerationEvidence{}, err
	}
	run.searchLiterature(ctx)
	if err = run.search(ctx); err != nil {
		return acceleratorlab.GenerationEvidence{}, err
	}
	if len(run.receipt.Observations) == 0 {
		return acceleratorlab.GenerationEvidence{}, fmt.Errorf("no draft reached measurement: %d rejected before GPU evaluation, $%.4f spent", len(run.history), run.spentUSD)
	}
	if run.receipt.State == "searching" {
		if run.receipt, err = kernelresearch.Complete(run.receipt, g.now()); err != nil {
			return acceleratorlab.GenerationEvidence{}, err
		}
	}
	return run.evidence(ctx)
}

func (g Generator) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func hotspotDetail(request acceleratorlab.GenerationRequest) ([]string, []string) {
	for _, hotspot := range request.Profile.Hotspots {
		if hotspot.ID == request.Candidate.HotspotID {
			return hotspot.Shapes, hotspot.DTypes
		}
	}
	return nil, nil
}

// searchLiterature is best effort: research can improve a draft but must never
// block a run or decide its outcome.
func (r *generation) searchLiterature(ctx context.Context) {
	if r.generator.Literature == nil {
		return
	}
	references, err := r.generator.Literature.Search(ctx, kernelliterature.Query{
		Family: r.request.Candidate.OperatorFamily, Since: r.generator.now().Add(-literatureWindow),
	})
	if err == nil {
		r.literature = references
	}
}

func (r *generation) exhausted() bool {
	policy := r.receipt.Policy
	deadline := r.receipt.StartedAt.Add(time.Duration(policy.MaxDurationSeconds) * time.Second)
	return r.rejected >= policy.MaxConsecutiveRejections || r.spentUSD+r.generator.MaxAttemptCostUSD > policy.MaxCostUSD || r.spentUSD >= policy.MaxCostUSD || !r.generator.now().Before(deadline)
}

func (r *generation) search(ctx context.Context) error {
	for r.receipt.State == "searching" && !r.exhausted() {
		if err := ctx.Err(); err != nil {
			return err
		}
		draft, err := r.generator.Author.Draft(ctx, r.draftRequest())
		r.charge(draft.CostUSD)
		if errors.Is(err, ErrInvalidDraft) {
			r.reject("draft", err.Error())
			continue
		}
		if err != nil {
			return r.interrupted(err)
		}
		if screenErr := r.pack.Screen(draft.Source); screenErr != nil {
			r.reject("screen", screenErr.Error())
			continue
		}
		revision := sourceRevision(draft.Source)
		if _, duplicate := r.seen[revision]; duplicate {
			r.reject("screen", "draft is identical to an earlier attempt")
			continue
		}
		r.seen[revision] = struct{}{}
		if err = r.evaluate(ctx, draft.Source, revision); err != nil {
			return r.interrupted(err)
		}
	}
	return nil
}

// interrupted keeps a partial receipt when earlier attempts completed. With
// nothing measured the failure is infrastructure, not negative evidence.
func (r *generation) interrupted(err error) error {
	if len(r.receipt.Observations) == 0 {
		return err
	}
	return nil
}

func (r *generation) charge(cost float64) {
	if cost > 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) {
		r.spentUSD += cost
		r.pendingUSD += cost
	}
}

func (r *generation) reject(stage, reason string) {
	r.rejected++
	r.history = append(r.history, Feedback{Iteration: len(r.history) + 1, Decision: "reject", Reason: stage + ": " + reason})
}

func (r *generation) evaluate(ctx context.Context, source, revision string) error {
	input := r.evaluationRequest(source, revision, false)
	if r.generator.Screener != nil {
		screen, err := r.generator.Screener.Evaluate(ctx, input)
		if err != nil {
			return err
		}
		r.charge(screen.CostUSD)
		if !correctOnCPU(screen.Correctness) {
			stage := screen.FailedStage
			if stage == "" {
				stage = "correctness"
			}
			r.reject("cpu-screen", stage)
			return nil
		}
	}
	result, err := r.generator.Evaluator.Evaluate(ctx, input)
	if err != nil {
		return err
	}
	r.charge(result.CostUSD)
	if result.BaselineMedianUS <= 0 || result.CandidateMedianUS <= 0 {
		stage := result.FailedStage
		if stage == "" {
			stage = "no timing produced"
		}
		r.reject("evaluation", stage)
		return nil
	}
	r.verifyPlausibility(ctx, input, &result)
	attempt := kernelresearch.Attempt{
		Iteration: len(r.receipt.Observations) + 1, SourceRevision: revision, Backend: r.pack.Backend,
		ArtifactDigest: "sha256:" + revision, Correctness: result.Correctness,
		BaselineMedianUS: result.BaselineMedianUS, CandidateMedianUS: result.CandidateMedianUS,
		RooflineUtilization: result.RooflineUtilization, CostUSD: r.pendingUSD, CompletedAt: r.generator.now(),
	}
	next, observation, err := kernelresearch.Observe(r.receipt, attempt)
	if err != nil {
		if next.State != "searching" {
			r.receipt = next
			return nil
		}
		return err
	}
	r.receipt, r.pendingUSD, r.rejected = next, 0, 0
	if observation.Decision == "keep" {
		r.bestSource = source
	}
	reason := observation.Reason
	if result.FailedStage != "" {
		reason += ": " + result.FailedStage
	}
	r.history = append(r.history, Feedback{Iteration: len(r.history) + 1, Speedup: observation.KernelSpeedup, Decision: observation.Decision, Reason: reason})
	return nil
}

// verifyPlausibility re-runs an implausibly large win on fresh inputs. A
// published agent-written kernel reported 104x by reading uninitialized data,
// so a large number is treated as a reason to check rather than to celebrate.
func (r *generation) verifyPlausibility(ctx context.Context, input EvaluationRequest, result *EvaluationResult) {
	speedup := result.BaselineMedianUS / result.CandidateMedianUS
	if !result.Correctness.Passed() || speedup <= r.pack.MaxPlausibleSpeedup {
		return
	}
	input.Verification = true
	verification, err := r.generator.Evaluator.Evaluate(ctx, input)
	if err == nil {
		r.charge(verification.CostUSD)
	}
	if err == nil && verification.Correctness.Passed() && verification.BaselineMedianUS > 0 && verification.CandidateMedianUS > 0 {
		repeated := verification.BaselineMedianUS / verification.CandidateMedianUS
		if math.Abs(repeated-speedup)/speedup <= verificationTolerance {
			return
		}
	}
	result.Correctness.Determinism = false
	result.FailedStage = "independent verification of an implausible speedup failed"
}

// correctOnCPU ignores the sanitizer, which needs a GPU; the GPU evaluator
// still requires it before anything is kept.
func correctOnCPU(correctness kernelresearch.Correctness) bool {
	return correctness.Smoke && correctness.ShapeSweep && correctness.NumericalStability && correctness.Determinism && correctness.EdgeCases
}

func (r *generation) draftRequest() DraftRequest {
	history := r.history
	if len(history) > maxPromptHistory {
		history = history[len(history)-maxPromptHistory:]
	}
	return DraftRequest{
		Pack: r.pack, Candidate: r.request.Candidate, Shapes: r.shapes, DTypes: r.dtypes,
		BestSource: r.bestSource, History: history, Literature: r.literature,
	}
}

func (r *generation) evaluationRequest(source, revision string, verification bool) EvaluationRequest {
	return EvaluationRequest{
		SchemaVersion: EvaluationSchemaV1, InputDigest: r.request.InputDigest, CandidateID: r.request.Candidate.ID,
		Iteration: len(r.receipt.Observations) + 1, Family: r.pack.Family, Backend: r.pack.Backend, Filename: r.pack.Filename,
		Source: source, SourceRevision: revision, Hardware: r.request.Hardware, RuntimeImageDigest: r.request.Runtime.ImageDigest,
		Shapes: r.shapes, DTypes: r.dtypes, Verification: verification,
	}
}

func (r *generation) evidence(ctx context.Context) (acceleratorlab.GenerationEvidence, error) {
	receipt := r.receipt
	evidence := acceleratorlab.GenerationEvidence{
		InputDigest: r.request.InputDigest, CandidateID: r.request.Candidate.ID, Generator: generatorName,
		GeneratorVersion: generatorVersion + "/" + r.generator.Author.Identity(), ProfileArtifactDigest: r.request.Profile.ArtifactDigest,
		GeneratedAt: r.generator.now(), CostUSD: r.spentUSD, Research: &receipt, Literature: literatureEvidence(r.literature),
	}
	if receipt.BestSourceRevision == "" {
		return evidence, nil
	}
	body := []byte(r.bestSource)
	uri, err := r.generator.Publisher.Publish(ctx, "kernel-"+receipt.BestSourceRevision[:24], sourceArtifactKind, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return acceleratorlab.GenerationEvidence{}, fmt.Errorf("publish generated source: %w", err)
	}
	evidence.Source = acceleratorlab.SourcePin{
		ImplementationID: generatedImplementationID(r.request.Candidate.OperatorFamily), Revision: receipt.BestSourceRevision,
		License: r.pack.License, Backend: r.pack.Backend,
		Artifacts: []acceleratorlab.Artifact{{Kind: sourceArtifactKind, URI: uri, SHA256: "sha256:" + receipt.BestSourceRevision, Size: int64(len(body))}},
	}
	return evidence, nil
}

func generatedImplementationID(family kernelplanner.OperatorFamily) string {
	return "generated-" + string(family)
}

func literatureEvidence(references []kernelliterature.Reference) []acceleratorlab.LiteratureReference {
	if len(references) == 0 {
		return nil
	}
	out := make([]acceleratorlab.LiteratureReference, 0, len(references))
	for _, reference := range references {
		out = append(out, acceleratorlab.LiteratureReference{Source: reference.Source, ID: reference.ID, Title: reference.Title, URL: reference.URL, Published: reference.Published})
	}
	return out
}

func sourceRevision(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}
