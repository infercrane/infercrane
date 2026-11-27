// Package speculator describes speculative-decoding drafts for one exact
// model: a draft trained in the customer's own account on prompts from their
// traffic, and a published draft someone else trained. It holds the record a
// draft is planned from, the engine arguments that make a server speculate
// with it, the bounded spec of a training job that runs where the prompts
// are, and the numbers-only report that job leaves. Nothing here reads a
// prompt, an answer, or a weight file.
package speculator

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sources of a draft.
const (
	// SourceTrained is a draft trained on the customer's own prompts, in the
	// customer's own account.
	SourceTrained = "trained"
	// SourcePublished is a draft its publisher trained on public data.
	SourcePublished = "published"
)

// MethodEAGLE3 is the only reviewed draft architecture: one decoder layer
// that reads three of the target model's hidden layers and drafts tokens one
// after another. vLLM serves it with "method":"eagle3".
const MethodEAGLE3 = "eagle3"

// Bounds.
const (
	MinDraftTokens = 1
	MaxDraftTokens = 8
	// MaxAcceptancePositions bounds the per-position acceptance a report or
	// record carries; it equals MaxDraftTokens.
	MaxAcceptancePositions = MaxDraftTokens
)

var (
	namePattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	hubRepository     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	objectURIPattern  = regexp.MustCompile(`^(s3|gs)://[a-z0-9][a-z0-9._-]{1,61}[a-z0-9](/[A-Za-z0-9._/=+-]*)?$`)
	versionPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,63}$`)
	frameworkPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	dataSourcePattern = regexp.MustCompile(`^(prompt_set|capture|public_dataset)$`)
)

// Draft is one draft a plan can measure for its exact base model. A draft
// lives where its owner put it: a Hugging Face repository pinned by commit (a
// private one in the customer's organisation for a trained draft), or the
// customer's own bucket pinned by the digest of its file manifest. The
// control plane stores this record only, never the files.
type Draft struct {
	// Name is a short lowercase label, unique among a plan's drafts.
	Name   string `json:"name"`
	Source string `json:"source"`
	Method string `json:"method"`
	// Repository is Hugging Face OWNER/NAME or an s3:// or gs:// location.
	Repository string `json:"repository"`
	// Revision is the 40-character commit of a Hugging Face repository or the
	// sha256 manifest digest of a bucket location.
	Revision string `json:"revision"`
	// BaseModel and BaseRevision are the exact model the draft was trained
	// against. A draft reads that model's hidden layers, so it is never used
	// with another model or another commit of it.
	BaseModel    string `json:"base_model"`
	BaseRevision string `json:"base_revision"`
	// DraftTokens is how many tokens the server drafts per step.
	DraftTokens int `json:"draft_tokens"`
	// ArtifactDigest is the sha256 of the draft's file manifest, as its
	// training report states it (Report.ArtifactDigest). A trained draft
	// carries it, so a candidate names its artifact by digest.
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	// Pinned is the engine version and GPU the draft was built and
	// qualified for. A setup on another engine version or GPU falls back to
	// serving without the draft until the draft is qualified there.
	Pinned *Pin `json:"pinned,omitempty"`
	// Training is the content-free provenance of a trained draft.
	Training *Training `json:"training,omitempty"`
}

// Pin is where a draft was built and qualified.
type Pin struct {
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	Accelerator   string `json:"accelerator"`
}

// Matches reports whether a setup runs where the draft is pinned. A draft
// without a pin matches nothing, so it is never served by default.
func (d Draft) Matches(engine, version, accelerator string) bool {
	return d.Pinned != nil && d.Pinned.Engine == engine && d.Pinned.EngineVersion == version && strings.EqualFold(d.Pinned.Accelerator, accelerator)
}

// Training is what a training job reports about a draft without any of the
// text it learned from: counts, a fingerprint of which examples it used, and
// how often the draft's tokens matched the model's on held-out prompts.
type Training struct {
	Framework        string `json:"framework"`
	FrameworkVersion string `json:"framework_version"`
	// DataSource is prompt_set (a file of prompts the customer chose),
	// capture (prompts the customer's endpoint recorded with capture on), or
	// public_dataset (a public dataset of the same kind of traffic).
	DataSource string `json:"data_source"`
	// DatasetFingerprint is the sha256 of the sorted example digests. It
	// tells two training sets apart without holding either.
	DatasetFingerprint string `json:"dataset_fingerprint"`
	Examples           int    `json:"examples"`
	HeldOutExamples    int    `json:"held_out_examples"`
	TrainingTokens     int64  `json:"training_tokens"`
	// HeldOutAcceptance is, per draft position, the share of held-out
	// positions where the draft's token equalled the model's own, scored on
	// the model's answers. It is an offline estimate; only a measured
	// campaign says whether serving gets faster.
	HeldOutAcceptance []float64 `json:"held_out_acceptance,omitempty"`
	// StartAcceptance is the same estimate for the draft training started
	// from, on the same held-out prompts, when it started from one.
	StartAcceptance []float64 `json:"start_acceptance,omitempty"`
	ProducedAt      time.Time `json:"produced_at"`
}

// ObjectStore reports whether the draft lives in a customer bucket.
func (d Draft) ObjectStore() bool {
	return strings.HasPrefix(d.Repository, "s3://") || strings.HasPrefix(d.Repository, "gs://")
}

// Normalize trims the record so one draft has one spelling in a plan input.
func (d Draft) Normalize() Draft {
	d.Name = strings.ToLower(strings.TrimSpace(d.Name))
	d.Source = strings.ToLower(strings.TrimSpace(d.Source))
	d.Method = strings.ToLower(strings.TrimSpace(d.Method))
	d.Repository = strings.TrimRight(strings.TrimSpace(d.Repository), "/")
	d.Revision = strings.ToLower(strings.TrimSpace(d.Revision))
	d.BaseModel = strings.TrimSpace(d.BaseModel)
	d.BaseRevision = strings.ToLower(strings.TrimSpace(d.BaseRevision))
	d.ArtifactDigest = strings.ToLower(strings.TrimSpace(d.ArtifactDigest))
	if d.Pinned != nil {
		pin := Pin{Engine: strings.ToLower(strings.TrimSpace(d.Pinned.Engine)), EngineVersion: strings.TrimSpace(d.Pinned.EngineVersion), Accelerator: strings.TrimSpace(d.Pinned.Accelerator)}
		d.Pinned = &pin
	}
	if d.Training != nil {
		training := *d.Training
		training.Framework = strings.ToLower(strings.TrimSpace(training.Framework))
		training.FrameworkVersion = strings.TrimSpace(training.FrameworkVersion)
		training.DataSource = strings.ToLower(strings.TrimSpace(training.DataSource))
		training.DatasetFingerprint = strings.ToLower(strings.TrimSpace(training.DatasetFingerprint))
		training.ProducedAt = training.ProducedAt.UTC()
		d.Training = &training
	}
	return d
}

// Validate checks a draft record's shape. It proves identity, never speed.
func (d Draft) Validate() error {
	if !namePattern.MatchString(d.Name) {
		return fmt.Errorf("draft name %q must be 1 to 64 lowercase letters, numbers, dots, underscores, or dashes", d.Name)
	}
	switch d.Source {
	case SourceTrained, SourcePublished:
	default:
		return fmt.Errorf("draft %q source must be trained or published", d.Name)
	}
	if d.Method != MethodEAGLE3 {
		return fmt.Errorf("draft %q method must be eagle3", d.Name)
	}
	if err := validateLocation(d.Repository, d.Revision); err != nil {
		return fmt.Errorf("draft %q: %w", d.Name, err)
	}
	if !hubRepository.MatchString(d.BaseModel) || !commitPattern.MatchString(d.BaseRevision) {
		return fmt.Errorf("draft %q needs the Hugging Face OWNER/NAME and 40-character commit of the model it was trained against", d.Name)
	}
	if d.DraftTokens < MinDraftTokens || d.DraftTokens > MaxDraftTokens {
		return fmt.Errorf("draft %q draft_tokens must be between %d and %d", d.Name, MinDraftTokens, MaxDraftTokens)
	}
	if d.ArtifactDigest != "" && !digestPattern.MatchString(d.ArtifactDigest) {
		return fmt.Errorf("draft %q artifact_digest must be a sha256 digest", d.Name)
	}
	if d.ObjectStore() && d.ArtifactDigest != "" && d.ArtifactDigest != d.Revision {
		return fmt.Errorf("draft %q is in a bucket, so its revision is its artifact digest", d.Name)
	}
	if pin := d.Pinned; pin != nil && (!frameworkPattern.MatchString(pin.Engine) || !versionPattern.MatchString(pin.EngineVersion) || !versionPattern.MatchString(pin.Accelerator)) {
		return fmt.Errorf("draft %q pin names an engine, its exact version, and a GPU", d.Name)
	}
	switch {
	case d.Source == SourceTrained && d.Training == nil:
		return fmt.Errorf("trained draft %q needs its training provenance", d.Name)
	case d.Source == SourceTrained && d.ArtifactDigest == "":
		return fmt.Errorf("trained draft %q needs the artifact_digest from its training report", d.Name)
	case d.Source == SourcePublished && d.Training != nil && d.Training.DataSource != "public_dataset":
		return fmt.Errorf("published draft %q was trained by its publisher; its training data source can only be public_dataset", d.Name)
	}
	if d.Training != nil {
		if err := d.Training.validate(); err != nil {
			return fmt.Errorf("draft %q training: %w", d.Name, err)
		}
	}
	return nil
}

func validateLocation(repository, revision string) error {
	if scheme, rest, found := strings.Cut(repository, "://"); found && (scheme == "s3" || scheme == "gs") {
		if !objectURIPattern.MatchString(repository) || strings.Contains(rest, "//") || strings.Contains(rest, "..") || strings.HasSuffix(rest, "/") {
			return errors.New("repository must be a plain s3:// or gs:// bucket path without a trailing slash")
		}
		if !digestPattern.MatchString(revision) {
			return errors.New("a draft in a bucket is pinned by the sha256 manifest digest of its files")
		}
		return nil
	}
	if !hubRepository.MatchString(repository) || !commitPattern.MatchString(revision) {
		return errors.New("repository must be Hugging Face OWNER/NAME pinned to its 40-character commit, or an s3:// or gs:// location pinned by a sha256 manifest digest")
	}
	return nil
}

func (t Training) validate() error {
	if !frameworkPattern.MatchString(t.Framework) || !versionPattern.MatchString(t.FrameworkVersion) {
		return errors.New("framework and framework_version are required")
	}
	if !dataSourcePattern.MatchString(t.DataSource) {
		return errors.New("data_source must be prompt_set, capture, or public_dataset")
	}
	if !digestPattern.MatchString(t.DatasetFingerprint) {
		return errors.New("dataset_fingerprint must be a sha256 digest")
	}
	if t.Examples < 1 || t.Examples > MaxExamples || t.HeldOutExamples < 0 || t.HeldOutExamples > MaxExamples {
		return fmt.Errorf("examples must be 1 to %d and held_out_examples 0 to %d", MaxExamples, MaxExamples)
	}
	if t.TrainingTokens < 1 || t.TrainingTokens > MaxTrainingTokens {
		return fmt.Errorf("training_tokens must be 1 to %d", MaxTrainingTokens)
	}
	if err := validAcceptance("held_out_acceptance", t.HeldOutAcceptance); err != nil {
		return err
	}
	if err := validAcceptance("start_acceptance", t.StartAcceptance); err != nil {
		return err
	}
	if len(t.HeldOutAcceptance) > 0 && t.HeldOutExamples == 0 {
		return errors.New("held_out_acceptance needs held-out examples")
	}
	if t.ProducedAt.IsZero() || t.ProducedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return errors.New("produced_at must be set and cannot be in the future")
	}
	return nil
}

func validAcceptance(field string, values []float64) error {
	if len(values) > MaxAcceptancePositions {
		return fmt.Errorf("%s holds at most %d positions", field, MaxAcceptancePositions)
	}
	for _, value := range values {
		if math.IsNaN(value) || value < 0 || value > 1 {
			return fmt.Errorf("%s values must be shares between 0 and 1", field)
		}
	}
	return nil
}

// ExpectedTokensPerStep is the offline estimate of tokens a verify step
// writes with this draft: one the model always writes, plus each drafted
// token while every token before it was accepted. Acceptance per position is
// the share conditioned on the earlier positions, as training reports it.
// It is unknown (false) without held-out acceptance.
func (t Training) ExpectedTokensPerStep(draftTokens int) (float64, bool) {
	if len(t.HeldOutAcceptance) == 0 || draftTokens < 1 {
		return 0, false
	}
	expected, survive := 1.0, 1.0
	for position := 0; position < draftTokens && position < len(t.HeldOutAcceptance); position++ {
		survive *= t.HeldOutAcceptance[position]
		expected += survive
	}
	return expected, true
}

// Engines a draft has reviewed arguments for.
const engineVLLM = "vllm"

// SpeculativeFlags lists every engine flag that turns speculation on, so a
// setup can drop the speculation it had before it takes a draft.
var SpeculativeFlags = []string{"--speculative-config", "--speculative-algorithm", "--speculative-num-steps", "--speculative-eagle-topk", "--speculative-num-draft-tokens", "--speculative-draft-model-path", "--speculative-draft-model-revision"}

// ErrNotServable means a valid draft has no reviewed way onto this engine
// yet. Its message says why, for a plan's skip reason.
var ErrNotServable = errors.New("draft not servable")

// RuntimeArgs returns the engine flags that make a server speculate with the
// draft. Only vLLM has reviewed flags: --speculative-config with method
// eagle3, the draft's repository and commit, and its draft length. vLLM
// fetches the draft like the model, with the same Hugging Face credential, so
// a private draft needs the plan's model secret.
func RuntimeArgs(engine string, draft Draft) ([]string, error) {
	if draft.ObjectStore() {
		return nil, fmt.Errorf("%w: a draft in a bucket needs the worker to fetch and check its files before the server starts, which is not built yet; push it to a private Hugging Face repository pinned by commit", ErrNotServable)
	}
	if engine != engineVLLM {
		return nil, fmt.Errorf("%w: only vLLM has reviewed flags for an EAGLE-3 draft; this setup runs %s", ErrNotServable, engine)
	}
	config, err := json.Marshal(struct {
		Method      string `json:"method"`
		Model       string `json:"model"`
		Revision    string `json:"revision"`
		DraftTokens int    `json:"num_speculative_tokens"`
	}{draft.Method, draft.Repository, draft.Revision, draft.DraftTokens})
	if err != nil {
		return nil, err
	}
	return []string{"--speculative-config", string(config)}, nil
}

// Describe is a short human label for a draft, for reasons and reports.
func Describe(draft Draft) string {
	kind := "published draft"
	if draft.Source == SourceTrained {
		kind = "draft trained on your traffic"
	}
	return fmt.Sprintf("%s %s@%s, %s draft tokens", kind, draft.Repository, shortRevision(draft.Revision), strconv.Itoa(draft.DraftTokens))
}

func shortRevision(revision string) string {
	revision = strings.TrimPrefix(revision, "sha256:")
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}
