package speculator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Bounds of one training job.
const (
	MaxExamples       = 200_000
	MaxTrainingTokens = int64(200_000_000)
	MinTimeoutSeconds = 600
	MaxTimeoutSeconds = 24 * 60 * 60
	MaxGPUs           = 8
	MaxEpochs         = 10
	MinNewTokens      = 16
	MaxNewTokens      = 4096
	// ReportSchema names the numbers-only report a training job writes beside
	// the draft.
	ReportSchema = "infercrane.dev/draft-training-report/v1"
	// ReportFile is the report's file name in the job's output directory.
	ReportFile = "report.json"
)

var (
	imagePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,254}@sha256:[0-9a-f]{64}$`)
	k8sNamePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	secretKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
	dataPathPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	acceleratorName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sha256Hex        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// DraftRef names a draft by location and pin, for a job that starts from it.
type DraftRef struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

// TrainingJob is the bounded spec of one draft training run in the
// customer's own Kubernetes cluster. It is never a shell command: the trainer
// image, pinned by digest and allowed by the cluster's operator, receives
// these values as arguments. The prompts are read from the customer's volume
// and the draft is written to the customer's volume; the model's answers are
// generated inside the job and never leave it. Only the report's numbers are
// meant to leave.
type TrainingJob struct {
	// Name labels the draft and the Kubernetes Job (infercrane-draft-NAME).
	Name         string `json:"name"`
	BaseModel    string `json:"base_model"`
	BaseRevision string `json:"base_revision"`
	Method       string `json:"method"`
	// StartFrom, when set, is a published draft for the same base model the
	// job fine-tunes instead of starting from random weights.
	StartFrom *DraftRef `json:"start_from,omitempty"`
	// DataSource is prompt_set, capture, or public_dataset; DataPath is the
	// prompt file inside the data volume (JSON lines, one conversation's
	// messages per line).
	DataSource string `json:"data_source"`
	DataPath   string `json:"data_path"`
	// HeldOutPercent of the conversations are kept out of training and score
	// the draft afterwards.
	HeldOutPercent int `json:"held_out_percent"`
	MaxExamples    int `json:"max_examples"`
	// MaxTrainingTokens bounds the answer tokens trained on.
	MaxTrainingTokens int64 `json:"max_training_tokens"`
	// MaxNewTokens bounds each answer the model writes for training.
	MaxNewTokens int `json:"max_new_tokens"`
	DraftTokens  int `json:"draft_tokens"`
	Epochs       int `json:"epochs"`
	// Image is the trainer image, pinned by digest.
	Image       string `json:"image"`
	Accelerator string `json:"accelerator"`
	GPUCount    int    `json:"gpu_count"`
	// Namespace, DataClaim and OutputClaim are the customer's: the Job runs in
	// the namespace and mounts the two PersistentVolumeClaims, data read-only.
	Namespace   string `json:"namespace"`
	DataClaim   string `json:"data_claim"`
	OutputClaim string `json:"output_claim"`
	// ModelSecret and ModelSecretKey name a Kubernetes Secret holding the
	// Hugging Face token for a gated base model or a private starting draft.
	ModelSecret    string `json:"model_secret,omitempty"`
	ModelSecretKey string `json:"model_secret_key,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// WithDefaults fills the values a caller usually leaves out.
func (j TrainingJob) WithDefaults() TrainingJob {
	if j.Method == "" {
		j.Method = MethodEAGLE3
	}
	if j.HeldOutPercent == 0 {
		j.HeldOutPercent = 10
	}
	if j.MaxExamples == 0 {
		j.MaxExamples = 50_000
	}
	if j.MaxTrainingTokens == 0 {
		j.MaxTrainingTokens = 20_000_000
	}
	if j.MaxNewTokens == 0 {
		j.MaxNewTokens = 1024
	}
	if j.DraftTokens == 0 {
		j.DraftTokens = 3
	}
	if j.Epochs == 0 {
		j.Epochs = 1
	}
	if j.GPUCount == 0 {
		j.GPUCount = 1
	}
	if j.TimeoutSeconds == 0 {
		j.TimeoutSeconds = 6 * 60 * 60
	}
	if j.ModelSecret != "" && j.ModelSecretKey == "" {
		j.ModelSecretKey = "token"
	}
	return j
}

// Validate bounds a training job before its manifest is rendered.
func (j TrainingJob) Validate() error {
	if !namePattern.MatchString(j.Name) || !k8sNamePattern.MatchString(jobName(j.Name)) {
		return errors.New("name must be 1 to 47 lowercase letters, numbers, or dashes")
	}
	if !hubRepository.MatchString(j.BaseModel) || !commitPattern.MatchString(j.BaseRevision) {
		return errors.New("base_model must be Hugging Face OWNER/NAME pinned by base_revision, its 40-character commit")
	}
	if j.Method != MethodEAGLE3 {
		return errors.New("method must be eagle3")
	}
	if j.StartFrom != nil && (!hubRepository.MatchString(j.StartFrom.Repository) || !commitPattern.MatchString(j.StartFrom.Revision)) {
		return errors.New("start_from must be a Hugging Face OWNER/NAME pinned to its 40-character commit")
	}
	if !dataSourcePattern.MatchString(j.DataSource) {
		return errors.New("data_source must be prompt_set, capture, or public_dataset")
	}
	if !plainPath(j.DataPath) {
		return errors.New("data_path must be a plain relative path inside the data volume")
	}
	if j.HeldOutPercent < 1 || j.HeldOutPercent > 50 {
		return errors.New("held_out_percent must be between 1 and 50")
	}
	if j.MaxExamples < 1 || j.MaxExamples > MaxExamples {
		return fmt.Errorf("max_examples must be between 1 and %d", MaxExamples)
	}
	if j.MaxTrainingTokens < 1 || j.MaxTrainingTokens > MaxTrainingTokens {
		return fmt.Errorf("max_training_tokens must be between 1 and %d", MaxTrainingTokens)
	}
	if j.MaxNewTokens < MinNewTokens || j.MaxNewTokens > MaxNewTokens {
		return fmt.Errorf("max_new_tokens must be between %d and %d", MinNewTokens, MaxNewTokens)
	}
	if j.DraftTokens < MinDraftTokens || j.DraftTokens > MaxDraftTokens {
		return fmt.Errorf("draft_tokens must be between %d and %d", MinDraftTokens, MaxDraftTokens)
	}
	if j.Epochs < 1 || j.Epochs > MaxEpochs {
		return fmt.Errorf("epochs must be between 1 and %d", MaxEpochs)
	}
	if !imagePattern.MatchString(j.Image) {
		return errors.New("image must be pinned by its sha256 digest")
	}
	if !acceleratorName.MatchString(j.Accelerator) {
		return errors.New("accelerator is required")
	}
	if j.GPUCount < 1 || j.GPUCount > MaxGPUs {
		return fmt.Errorf("gpu_count must be between 1 and %d", MaxGPUs)
	}
	for field, value := range map[string]string{"namespace": j.Namespace, "data_claim": j.DataClaim, "output_claim": j.OutputClaim} {
		if !k8sNamePattern.MatchString(value) {
			return fmt.Errorf("%s must be a Kubernetes name", field)
		}
	}
	if j.DataClaim == j.OutputClaim {
		return errors.New("data_claim and output_claim must differ: prompts are mounted read-only")
	}
	if (j.ModelSecret == "") != (j.ModelSecretKey == "") || j.ModelSecret != "" && (!k8sNamePattern.MatchString(j.ModelSecret) || !secretKeyPattern.MatchString(j.ModelSecretKey)) {
		return errors.New("model_secret and model_secret_key name a Kubernetes Secret and its key together")
	}
	if j.TimeoutSeconds < MinTimeoutSeconds || j.TimeoutSeconds > MaxTimeoutSeconds {
		return fmt.Errorf("timeout_seconds must be between %d and %d", MinTimeoutSeconds, MaxTimeoutSeconds)
	}
	return nil
}

// plainPath reports whether path is relative, bounded, and has no . or ..
// segment, so it stays inside the directory it is joined to.
func plainPath(path string) bool {
	if len(path) > 512 || !dataPathPattern.MatchString(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func jobName(name string) string {
	return "infercrane-draft-" + strings.ReplaceAll(strings.ReplaceAll(name, ".", "-"), "_", "-")
}

// Mount points inside the trainer container.
const (
	dataMount   = "/data"
	outputMount = "/output"
)

// TrainerArgs is the trainer's argument list. The trainer reads the prompts,
// has the base model answer them at temperature 0, trains the draft on those
// answers, scores it on the held-out prompts, and writes the draft and
// report.json to the output directory.
func (j TrainingJob) TrainerArgs() []string {
	args := []string{
		"--base-model", j.BaseModel, "--base-revision", j.BaseRevision, "--method", j.Method,
		"--data-source", j.DataSource, "--prompts", path.Join(dataMount, j.DataPath),
		"--held-out-percent", strconv.Itoa(j.HeldOutPercent), "--max-examples", strconv.Itoa(j.MaxExamples),
		"--max-training-tokens", strconv.FormatInt(j.MaxTrainingTokens, 10), "--max-new-tokens", strconv.Itoa(j.MaxNewTokens),
		"--draft-tokens", strconv.Itoa(j.DraftTokens), "--epochs", strconv.Itoa(j.Epochs),
		"--output", path.Join(outputMount, j.Name), "--report", path.Join(outputMount, j.Name, ReportFile),
	}
	if j.StartFrom != nil {
		args = append(args, "--start-from", j.StartFrom.Repository, "--start-from-revision", j.StartFrom.Revision)
	}
	return args
}

// KubernetesJob renders the job as a batch/v1 Job manifest for the
// customer's cluster. The pod gets no service account token, runs as a
// non-root user, mounts the prompts read-only, and is deleted an hour after
// it ends; the Job is never retried, so a failed run does not train twice.
func (j TrainingJob) KubernetesJob() (map[string]any, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	labels := map[string]any{"app.kubernetes.io/managed-by": "infercrane", "app.kubernetes.io/component": "draft-training", "infercrane.dev/draft": j.Name}
	container := map[string]any{
		"name": "trainer", "image": j.Image, "args": stringsToAny(j.TrainerArgs()),
		"resources": map[string]any{"limits": map[string]any{"nvidia.com/gpu": strconv.Itoa(j.GPUCount)}},
		"volumeMounts": []any{
			map[string]any{"name": "prompts", "mountPath": dataMount, "readOnly": true},
			map[string]any{"name": "output", "mountPath": outputMount},
		},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "capabilities": map[string]any{"drop": []any{"ALL"}}},
	}
	env := []any{map[string]any{"name": "HF_HUB_DISABLE_TELEMETRY", "value": "1"}, map[string]any{"name": "VLLM_NO_USAGE_STATS", "value": "1"}, map[string]any{"name": "DO_NOT_TRACK", "value": "1"}}
	if j.ModelSecret != "" {
		env = append(env, map[string]any{"name": "HF_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": j.ModelSecret, "key": j.ModelSecretKey}}})
	}
	container["env"] = env
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": jobName(j.Name), "namespace": j.Namespace, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": 0, "activeDeadlineSeconds": j.TimeoutSeconds, "ttlSecondsAfterFinished": 3600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"nodeSelector": map[string]any{"infercrane.dev/accelerator": j.Accelerator},
					"containers":   []any{container},
					"volumes": []any{
						map[string]any{"name": "prompts", "persistentVolumeClaim": map[string]any{"claimName": j.DataClaim, "readOnly": true}},
						map[string]any{"name": "output", "persistentVolumeClaim": map[string]any{"claimName": j.OutputClaim}},
					},
				},
			},
		},
	}, nil
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

// File is one file of a trained draft, pinned by its sha256.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Report is what a training job writes beside the draft: identity, counts,
// digests, and acceptance shares. It has no field that can hold a prompt or
// an answer, and Validate refuses free text.
type Report struct {
	Schema       string    `json:"schema"`
	Name         string    `json:"name"`
	BaseModel    string    `json:"base_model"`
	BaseRevision string    `json:"base_revision"`
	Method       string    `json:"method"`
	StartFrom    *DraftRef `json:"start_from,omitempty"`
	DraftTokens  int       `json:"draft_tokens"`
	Training
	// Files are the draft's files; ArtifactDigest is the sha256 of their
	// manifest (see ManifestDigest).
	Files          []File  `json:"files"`
	ArtifactDigest string  `json:"artifact_digest"`
	WallSeconds    float64 `json:"wall_seconds"`
}

// DecodeReport reads and validates a report.
func DecodeReport(raw []byte) (Report, error) {
	if len(raw) > 1<<20 {
		return Report{}, errors.New("report is larger than 1 MiB")
	}
	var report Report
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode draft training report: %w", err)
	}
	return report, report.Validate()
}

// Validate checks a report's shape and that its digest commits to its files.
func (r Report) Validate() error {
	if r.Schema != ReportSchema {
		return fmt.Errorf("report schema must be %q", ReportSchema)
	}
	if !namePattern.MatchString(r.Name) || r.Method != MethodEAGLE3 || !hubRepository.MatchString(r.BaseModel) || !commitPattern.MatchString(r.BaseRevision) {
		return errors.New("report needs the draft name, method eagle3, and the pinned base model")
	}
	if r.StartFrom != nil && (!hubRepository.MatchString(r.StartFrom.Repository) || !commitPattern.MatchString(r.StartFrom.Revision)) {
		return errors.New("report start_from must be pinned")
	}
	if r.DraftTokens < MinDraftTokens || r.DraftTokens > MaxDraftTokens {
		return errors.New("report draft_tokens is out of range")
	}
	if err := r.Training.validate(); err != nil {
		return err
	}
	if r.StartFrom == nil && len(r.StartAcceptance) > 0 {
		return errors.New("start_acceptance belongs to a job that started from a draft")
	}
	if len(r.Files) == 0 || len(r.Files) > 64 {
		return errors.New("a draft has 1 to 64 files")
	}
	seen := map[string]bool{}
	for _, file := range r.Files {
		if !plainPath(file.Path) || seen[file.Path] || file.Size < 0 || !sha256Hex.MatchString(file.SHA256) {
			return fmt.Errorf("draft file %q is invalid", file.Path)
		}
		seen[file.Path] = true
	}
	if r.ArtifactDigest != ManifestDigest(r.Files) {
		return errors.New("artifact_digest does not match the files")
	}
	if math.IsNaN(r.WallSeconds) || r.WallSeconds < 0 || r.WallSeconds > float64(MaxTimeoutSeconds) {
		return errors.New("wall_seconds is out of range")
	}
	return nil
}

// ManifestDigest is the sha256 of the draft's files, sorted by path, one
// "path size sha256" line each. The trainer computes it the same way.
func ManifestDigest(files []File) string {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, k int) bool { return sorted[i].Path < sorted[k].Path })
	var builder strings.Builder
	for _, file := range sorted {
		fmt.Fprintf(&builder, "%s %d %s\n", file.Path, file.Size, file.SHA256)
	}
	return digestOf(builder.String())
}

// Draft turns a report into the record a plan takes, once the customer has
// put the files at repository and revision, pinned to where it was built and
// qualified. A bucket location must be pinned by the report's own artifact
// digest. A Hugging Face commit cannot be checked against the file digests
// without reading the repository, which the control plane never does; serving
// relies on the commit pin.
func (r Report) Draft(repository, revision string, pin *Pin) (Draft, error) {
	if err := r.Validate(); err != nil {
		return Draft{}, err
	}
	if strings.HasPrefix(repository, "s3://") || strings.HasPrefix(repository, "gs://") {
		if revision != r.ArtifactDigest {
			return Draft{}, errors.New("a draft in a bucket is pinned by the report's artifact_digest")
		}
	}
	training := r.Training
	training.HeldOutAcceptance = append([]float64(nil), r.HeldOutAcceptance...)
	training.StartAcceptance = append([]float64(nil), r.StartAcceptance...)
	draft := Draft{
		Name: r.Name, Source: SourceTrained, Method: r.Method, Repository: repository, Revision: revision,
		BaseModel: r.BaseModel, BaseRevision: r.BaseRevision, DraftTokens: r.DraftTokens, ArtifactDigest: r.ArtifactDigest, Pinned: pin, Training: &training,
	}.Normalize()
	return draft, draft.Validate()
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}
