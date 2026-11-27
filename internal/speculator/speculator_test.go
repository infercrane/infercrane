package speculator

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

const (
	qwenRevision  = "b968826d9c46dd6066d109eabc6255188de91218"
	draftRevision = "0123456789abcdef0123456789abcdef01234567"
)

func sampleTraining() *Training {
	return &Training{
		Framework: "speculators", FrameworkVersion: "0.9.0", DataSource: "public_dataset",
		DatasetFingerprint: "sha256:" + strings.Repeat("a", 64), Examples: 9000, HeldOutExamples: 400, TrainingTokens: 4_100_000,
		HeldOutAcceptance: []float64{0.8, 0.6, 0.5}, StartAcceptance: []float64{0.7, 0.5, 0.4},
		ProducedAt: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
	}
}

func trainedDraft() Draft {
	return Draft{
		Name: "oasst-chat", Source: SourceTrained, Method: MethodEAGLE3, Repository: "acme/qwen3-8b-chat-draft", Revision: draftRevision,
		BaseModel: "Qwen/Qwen3-8B", BaseRevision: qwenRevision, DraftTokens: 3, Training: sampleTraining(),
		ArtifactDigest: "sha256:" + strings.Repeat("e", 64), Pinned: &Pin{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4"},
	}
}

func TestDraftValidate(t *testing.T) {
	t.Parallel()
	if err := trainedDraft().Validate(); err != nil {
		t.Fatalf("valid trained draft refused: %v", err)
	}
	published := Draft{Name: "redhat", Source: SourcePublished, Method: MethodEAGLE3, Repository: "RedHatAI/Qwen3-8B-speculator.eagle3", Revision: draftRevision, BaseModel: "Qwen/Qwen3-8B", BaseRevision: qwenRevision, DraftTokens: 3}
	if err := published.Validate(); err != nil {
		t.Fatalf("valid published draft refused: %v", err)
	}
	bucket := trainedDraft()
	bucket.Repository, bucket.Revision, bucket.ArtifactDigest = "s3://acme-drafts/qwen3-8b/chat", "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("b", 64)
	if err := bucket.Validate(); err != nil {
		t.Fatalf("valid bucket draft refused: %v", err)
	}
	cases := map[string]func(*Draft){
		"name":                     func(d *Draft) { d.Name = "Has Spaces" },
		"source":                   func(d *Draft) { d.Source = "borrowed" },
		"method":                   func(d *Draft) { d.Method = "medusa" },
		"branch revision":          func(d *Draft) { d.Revision = "main" },
		"bucket without digest":    func(d *Draft) { d.Repository = "s3://acme-drafts/x" },
		"bucket path escape":       func(d *Draft) { d.Repository, d.Revision = "s3://acme-drafts/../x", "sha256:"+strings.Repeat("b", 64) },
		"bucket double slash":      func(d *Draft) { d.Repository, d.Revision = "gs://acme-drafts//x", "sha256:"+strings.Repeat("b", 64) },
		"unpinned base":            func(d *Draft) { d.BaseRevision = "main" },
		"no draft tokens":          func(d *Draft) { d.DraftTokens = 0 },
		"too many draft tokens":    func(d *Draft) { d.DraftTokens = 9 },
		"trained without training": func(d *Draft) { d.Training = nil },
		"acceptance above one":     func(d *Draft) { d.Training.HeldOutAcceptance = []float64{1.2} },
		"acceptance NaN":           func(d *Draft) { d.Training.HeldOutAcceptance = []float64{math.NaN()} },
		"too many positions":       func(d *Draft) { d.Training.HeldOutAcceptance = make([]float64, 9) },
		"acceptance without held out": func(d *Draft) {
			d.Training.HeldOutExamples = 0
		},
		"fingerprint":         func(d *Draft) { d.Training.DatasetFingerprint = "abc" },
		"data source":         func(d *Draft) { d.Training.DataSource = "scraped" },
		"future":              func(d *Draft) { d.Training.ProducedAt = time.Now().Add(time.Hour) },
		"no tokens":           func(d *Draft) { d.Training.TrainingTokens = 0 },
		"framework blank":     func(d *Draft) { d.Training.Framework = "" },
		"no artifact digest":  func(d *Draft) { d.ArtifactDigest = "" },
		"bad artifact digest": func(d *Draft) { d.ArtifactDigest = "sha256:abc" },
		"bucket digest differs": func(d *Draft) {
			d.Repository, d.Revision = "s3://acme-drafts/x", "sha256:"+strings.Repeat("b", 64)
		},
		"pin without version": func(d *Draft) { d.Pinned = &Pin{Engine: "vllm", Accelerator: "L4"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			draft := trainedDraft()
			training := *draft.Training
			draft.Training = &training
			mutate(&draft)
			if err := draft.Validate(); err == nil {
				t.Fatalf("invalid draft accepted")
			}
		})
	}
	publishedWithCustomerData := published
	publishedWithCustomerData.Training = sampleTraining()
	publishedWithCustomerData.Training.DataSource = "capture"
	if err := publishedWithCustomerData.Validate(); err == nil {
		t.Fatal("a published draft claiming the customer's captured data was accepted")
	}
}

func TestDraftNormalize(t *testing.T) {
	t.Parallel()
	draft := trainedDraft()
	draft.Name, draft.Source, draft.Revision, draft.Repository = " OASST-Chat ", "Trained", strings.ToUpper(draftRevision), "acme/qwen3-8b-chat-draft/"
	draft = draft.Normalize()
	if draft.Name != "oasst-chat" || draft.Source != SourceTrained || draft.Revision != draftRevision || draft.Repository != "acme/qwen3-8b-chat-draft" {
		t.Fatalf("normalized draft = %+v", draft)
	}
	if err := draft.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeArgs(t *testing.T) {
	t.Parallel()
	args, err := RuntimeArgs("vllm", trainedDraft())
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "--speculative-config" {
		t.Fatalf("args = %q", args)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(args[1]), &config); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"method": "eagle3", "model": "acme/qwen3-8b-chat-draft", "revision": draftRevision, "num_speculative_tokens": float64(3)}
	for key, value := range want {
		if config[key] != value {
			t.Fatalf("speculative config %s = %v, want %v (%s)", key, config[key], value, args[1])
		}
	}
	if len(config) != len(want) {
		t.Fatalf("speculative config has extra keys: %s", args[1])
	}
	if _, err := RuntimeArgs("sglang", trainedDraft()); !errors.Is(err, ErrNotServable) {
		t.Fatalf("sglang error = %v, want ErrNotServable", err)
	}
	bucket := trainedDraft()
	bucket.Repository, bucket.Revision, bucket.ArtifactDigest = "s3://acme-drafts/qwen3-8b/chat", "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("b", 64)
	if _, err := RuntimeArgs("vllm", bucket); !errors.Is(err, ErrNotServable) || !strings.Contains(err.Error(), "not built yet") {
		t.Fatalf("bucket draft error = %v", err)
	}
}

func TestExpectedTokensPerStep(t *testing.T) {
	t.Parallel()
	training := Training{HeldOutAcceptance: []float64{0.8, 0.5, 0.5}}
	got, ok := training.ExpectedTokensPerStep(3)
	// 1 + 0.8 + 0.8*0.5 + 0.8*0.5*0.5
	if !ok || math.Abs(got-2.4) > 1e-9 {
		t.Fatalf("expected tokens = %v, %v", got, ok)
	}
	if got, _ := training.ExpectedTokensPerStep(1); math.Abs(got-1.8) > 1e-9 {
		t.Fatalf("one draft token = %v", got)
	}
	if _, ok := (Training{}).ExpectedTokensPerStep(3); ok {
		t.Fatal("expected tokens known without acceptance")
	}
}

func sampleJob() TrainingJob {
	return TrainingJob{
		Name: "oasst-chat", BaseModel: "Qwen/Qwen3-8B", BaseRevision: qwenRevision,
		StartFrom:  &DraftRef{Repository: "RedHatAI/Qwen3-8B-speculator.eagle3", Revision: draftRevision},
		DataSource: "prompt_set", DataPath: "prompts/chat.jsonl",
		Image:       "registry.example.com/infercrane/draft-trainer@sha256:" + strings.Repeat("c", 64),
		Accelerator: "H100", Namespace: "inference", DataClaim: "prompts", OutputClaim: "drafts",
		ModelSecret: "hf-token",
	}.WithDefaults()
}

func TestTrainingJobDefaultsAndValidate(t *testing.T) {
	t.Parallel()
	job := sampleJob()
	if err := job.Validate(); err != nil {
		t.Fatalf("valid job refused: %v", err)
	}
	if job.Method != MethodEAGLE3 || job.HeldOutPercent != 10 || job.DraftTokens != 3 || job.Epochs != 1 || job.GPUCount != 1 || job.ModelSecretKey != "token" || job.MaxTrainingTokens != 20_000_000 {
		t.Fatalf("defaults = %+v", job)
	}
	cases := map[string]func(*TrainingJob){
		"tag image":        func(j *TrainingJob) { j.Image = "registry.example.com/trainer:latest" },
		"path escape":      func(j *TrainingJob) { j.DataPath = "../secrets/key" },
		"absolute path":    func(j *TrainingJob) { j.DataPath = "/etc/passwd" },
		"dot path":         func(j *TrainingJob) { j.DataPath = "a/./b" },
		"same claims":      func(j *TrainingJob) { j.OutputClaim = j.DataClaim },
		"held out":         func(j *TrainingJob) { j.HeldOutPercent = 60 },
		"tokens":           func(j *TrainingJob) { j.MaxTrainingTokens = MaxTrainingTokens + 1 },
		"examples":         func(j *TrainingJob) { j.MaxExamples = MaxExamples + 1 },
		"answer limit":     func(j *TrainingJob) { j.MaxNewTokens = 8 },
		"gpus":             func(j *TrainingJob) { j.GPUCount = 9 },
		"timeout":          func(j *TrainingJob) { j.TimeoutSeconds = 30 },
		"namespace":        func(j *TrainingJob) { j.Namespace = "Inference" },
		"secret key alone": func(j *TrainingJob) { j.ModelSecret = "" },
		"start unpinned":   func(j *TrainingJob) { j.StartFrom = &DraftRef{Repository: "RedHatAI/x", Revision: "main"} },
		"data source":      func(j *TrainingJob) { j.DataSource = "everything" },
		"base unpinned":    func(j *TrainingJob) { j.BaseRevision = "" },
		"long name":        func(j *TrainingJob) { j.Name = strings.Repeat("a", 50) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job := sampleJob()
			mutate(&job)
			if err := job.Validate(); err == nil {
				t.Fatal("invalid job accepted")
			}
		})
	}
}

func TestKubernetesJobManifest(t *testing.T) {
	t.Parallel()
	manifest, err := sampleJob().KubernetesJob()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name, Namespace string
		} `json:"metadata"`
		Spec struct {
			BackoffLimit          int `json:"backoffLimit"`
			ActiveDeadlineSeconds int `json:"activeDeadlineSeconds"`
			Template              struct {
				Spec struct {
					RestartPolicy                string `json:"restartPolicy"`
					AutomountServiceAccountToken bool   `json:"automountServiceAccountToken"`
					Containers                   []struct {
						Image        string   `json:"image"`
						Command      []string `json:"command"`
						Args         []string `json:"args"`
						VolumeMounts []struct {
							Name      string `json:"name"`
							MountPath string `json:"mountPath"`
							ReadOnly  bool   `json:"readOnly"`
						} `json:"volumeMounts"`
						Env []struct {
							Name      string `json:"name"`
							Value     string `json:"value"`
							ValueFrom *struct {
								SecretKeyRef struct{ Name, Key string } `json:"secretKeyRef"`
							} `json:"valueFrom"`
						} `json:"env"`
					} `json:"containers"`
					Volumes []struct {
						Name string `json:"name"`
						PVC  struct {
							ClaimName string `json:"claimName"`
							ReadOnly  bool   `json:"readOnly"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(encoded, &job); err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if job.Kind != "Job" || job.Metadata.Name != "infercrane-draft-oasst-chat" || job.Metadata.Namespace != "inference" || job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds != 6*3600 {
		t.Fatalf("job header = %s", encoded)
	}
	if pod.RestartPolicy != "Never" || pod.AutomountServiceAccountToken || len(pod.Containers) != 1 {
		t.Fatalf("pod = %s", encoded)
	}
	container := pod.Containers[0]
	if len(container.Command) != 0 {
		t.Fatalf("the trainer image's own entrypoint runs; no command override: %q", container.Command)
	}
	joined := strings.Join(container.Args, " ")
	for _, want := range []string{"--prompts /data/prompts/chat.jsonl", "--output /output/oasst-chat", "--report /output/oasst-chat/report.json", "--start-from RedHatAI/Qwen3-8B-speculator.eagle3", "--start-from-revision " + draftRevision, "--base-revision " + qwenRevision, "--draft-tokens 3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q lack %q", joined, want)
		}
	}
	mounts := map[string]bool{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount.ReadOnly
	}
	if !mounts["prompts"] || mounts["output"] {
		t.Fatalf("prompts must be read-only and output writable: %+v", container.VolumeMounts)
	}
	var token bool
	for _, variable := range container.Env {
		if variable.Name == "HF_TOKEN" {
			token = variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef.Name == "hf-token" && variable.ValueFrom.SecretKeyRef.Key == "token" && variable.Value == ""
		}
	}
	if !token {
		t.Fatalf("HF_TOKEN must come from the secret reference only: %s", encoded)
	}
	claims := map[string]bool{}
	for _, volume := range pod.Volumes {
		claims[volume.PVC.ClaimName] = volume.PVC.ReadOnly
	}
	if !claims["prompts"] || claims["drafts"] {
		t.Fatalf("volumes = %+v", pod.Volumes)
	}
	invalid := sampleJob()
	invalid.Image = "trainer:latest"
	if _, err := invalid.KubernetesJob(); err == nil {
		t.Fatal("manifest rendered for an invalid job")
	}
}

func sampleReport() Report {
	files := []File{{Path: "config.json", Size: 812, SHA256: strings.Repeat("1", 64)}, {Path: "model.safetensors", Size: 1_600_000_000, SHA256: strings.Repeat("2", 64)}}
	return Report{
		Schema: ReportSchema, Name: "oasst-chat", BaseModel: "Qwen/Qwen3-8B", BaseRevision: qwenRevision, Method: MethodEAGLE3,
		StartFrom: &DraftRef{Repository: "RedHatAI/Qwen3-8B-speculator.eagle3", Revision: draftRevision}, DraftTokens: 3,
		Training: *sampleTraining(), Files: files, ArtifactDigest: ManifestDigest(files), WallSeconds: 2400,
	}
}

func TestReportRoundTripAndDraft(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(sampleReport())
	if err != nil {
		t.Fatal(err)
	}
	report, err := DecodeReport(encoded)
	if err != nil {
		t.Fatalf("valid report refused: %v", err)
	}
	pin := &Pin{Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4"}
	draft, err := report.Draft("acme/qwen3-8b-chat-draft", draftRevision, pin)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Source != SourceTrained || draft.Training == nil || draft.Training.Examples != 9000 || draft.DraftTokens != 3 || draft.ArtifactDigest != report.ArtifactDigest || !draft.Matches("vllm", "0.34.0", "l4") {
		t.Fatalf("draft = %+v", draft)
	}
	bucket, err := report.Draft("s3://acme-drafts/qwen3-8b/chat", report.ArtifactDigest, pin)
	if err != nil || !bucket.ObjectStore() {
		t.Fatalf("bucket draft = %+v, %v", bucket, err)
	}
	if _, err := report.Draft("s3://acme-drafts/qwen3-8b/chat", "sha256:"+strings.Repeat("9", 64), pin); err == nil {
		t.Fatal("bucket draft pinned to another digest accepted")
	}
}

func TestReportRefusesContentAndBadDigests(t *testing.T) {
	t.Parallel()
	withText, err := json.Marshal(map[string]any{"schema": ReportSchema, "prompts": []string{"what is my password"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeReport(withText); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("report with a text field accepted: %v", err)
	}
	tampered := sampleReport()
	tampered.Files[1].SHA256 = strings.Repeat("3", 64)
	if err := tampered.Validate(); err == nil {
		t.Fatal("report whose digest does not match its files accepted")
	}
	noStart := sampleReport()
	noStart.StartFrom = nil
	if err := noStart.Validate(); err == nil {
		t.Fatal("start acceptance without a starting draft accepted")
	}
	badPath := sampleReport()
	badPath.Files[0].Path = "../escape"
	badPath.ArtifactDigest = ManifestDigest(badPath.Files)
	if err := badPath.Validate(); err == nil {
		t.Fatal("report with an escaping file path accepted")
	}
	if _, err := DecodeReport([]byte(strings.Repeat(" ", 1<<20+1))); err == nil {
		t.Fatal("oversized report accepted")
	}
}

func TestManifestDigestIsOrderFree(t *testing.T) {
	t.Parallel()
	files := sampleReport().Files
	reversed := []File{files[1], files[0]}
	if ManifestDigest(files) != ManifestDigest(reversed) {
		t.Fatal("manifest digest depends on file order")
	}
	// The trainer writes the same lines: "path size sha256\n" sorted by path.
	want := digestOf("config.json 812 " + strings.Repeat("1", 64) + "\nmodel.safetensors 1600000000 " + strings.Repeat("2", 64) + "\n")
	if ManifestDigest(files) != want {
		t.Fatalf("manifest digest = %s, want %s", ManifestDigest(files), want)
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()
	if got := Describe(trainedDraft()); got != "draft trained on your traffic acme/qwen3-8b-chat-draft@0123456789ab, 3 draft tokens" {
		t.Fatalf("describe = %q", got)
	}
}
