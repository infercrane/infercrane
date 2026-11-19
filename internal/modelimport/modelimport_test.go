package modelimport

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func file(path string, content []byte) domain.ModelFile {
	sum := sha256.Sum256(content)
	return domain.ModelFile{Path: path, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
}

var qwenConfig = []byte(`{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","hidden_size":1024,"num_hidden_layers":28,"num_attention_heads":16,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960,"torch_dtype":"bfloat16"}`)

func weightsRequest() Request {
	return Request{Kind: KindWeights, URI: "s3://acme-models/run-42/", Config: qwenConfig, Files: []domain.ModelFile{
		file("config.json", qwenConfig),
		file("tokenizer.json", []byte("{}")),
		file("tokenizer_config.json", []byte("{}")),
		file("model.safetensors", []byte("weights")),
	}}
}

func TestDigestIsOrderIndependentAndPinsEveryByte(t *testing.T) {
	request := weightsRequest()
	reversed := append([]domain.ModelFile(nil), request.Files...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if Digest(request.Files) != Digest(reversed) || !ValidDigest(Digest(request.Files)) {
		t.Fatal("the manifest digest depends on listing order")
	}
	changed := append([]domain.ModelFile(nil), request.Files...)
	changed[3] = file("model.safetensors", []byte("Weights"))
	if Digest(changed) == Digest(request.Files) {
		t.Fatal("a changed file kept the digest")
	}
}

func TestWeightsImportVerifiesConfigAndTokenizerAndKeepsBucketUnknown(t *testing.T) {
	item, report, err := Build(weightsRequest())
	if err != nil {
		t.Fatal(err)
	}
	if item.URI != "s3://acme-models/run-42" || item.Source != "s3" || len(item.Checks.Failed) != 0 || item.Checks.Architecture != "Qwen3ForCausalLM" || item.Checks.ConfigDigest == "" {
		t.Fatalf("unexpected import: %#v", item)
	}
	if !strings.Contains(strings.Join(item.Checks.Unknown, " "), "each worker checks every file's sha256") {
		t.Fatalf("the bucket contents must stay unknown: %#v", item.Checks.Unknown)
	}
	if report == nil || report.Architecture.State != "supported" || report.Source != "s3" || report.Access.State != "customer_bucket" {
		t.Fatalf("model check of the imported config: %#v", report)
	}
	for _, check := range report.Checks {
		if check.Name == "access" && (check.State != "info" || !strings.Contains(check.Detail, "never reads the bucket")) {
			t.Fatalf("access check claims a public model: %#v", check)
		}
	}
}

func TestWeightsImportFailures(t *testing.T) {
	cases := map[string]func(*Request){
		"no config.json": func(r *Request) { r.Files, r.Config = r.Files[1:], nil },
		"config does not match manifest": func(r *Request) {
			r.Config = []byte(`{"architectures":["Other"]}`)
		},
		"no tokenizer": func(r *Request) { r.Files = []domain.ModelFile{r.Files[0], r.Files[3]} },
		"no safetensors": func(r *Request) {
			r.Files = append(r.Files[:3], file("pytorch_model.bin", []byte("x")))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := weightsRequest()
			mutate(&request)
			item, _, err := Build(request)
			if err != nil || len(item.Checks.Failed) == 0 {
				t.Fatalf("expected a failed check: %#v err=%v", item.Checks, err)
			}
		})
	}
	request := weightsRequest()
	request.Config = nil
	item, _, _ := Build(request)
	if len(item.Checks.Failed) != 0 || !strings.Contains(strings.Join(item.Checks.Unknown, " "), "architecture is unknown") {
		t.Fatalf("missing config bytes must be unknown, not failed: %#v", item.Checks)
	}
}

func TestBuildRefusesUnsafeManifests(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"traversal":       func(r *Request) { r.Files[0].Path = "../config.json" },
		"absolute":        func(r *Request) { r.Files[0].Path = "/etc/passwd" },
		"duplicate":       func(r *Request) { r.Files = append(r.Files, r.Files[0]) },
		"bad sha":         func(r *Request) { r.Files[0].SHA256 = "ABC" },
		"http location":   func(r *Request) { r.URI = "https://bucket/x" },
		"query in uri":    func(r *Request) { r.URI = "s3://bucket/x?versionId=1" },
		"unknown kind":    func(r *Request) { r.Kind = "dataset" },
		"empty manifest":  func(r *Request) { r.Files = nil },
		"shell in a path": func(r *Request) { r.Files[0].Path = "$(reboot).json" },
	} {
		t.Run(name, func(t *testing.T) {
			request := weightsRequest()
			mutate(&request)
			if _, _, err := Build(request); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
}

func TestAdapterImportReadsRankAndComparesBaseModel(t *testing.T) {
	config := []byte(`{"peft_type":"LORA","r":32,"base_model_name_or_path":"Qwen/Qwen3-8B","target_modules":["q_proj","v_proj"]}`)
	request := Request{Kind: KindAdapter, URI: "gs://acme-adapters/support", AdapterConfig: config, BaseModel: "Qwen/Qwen3-8B", Files: []domain.ModelFile{file("adapter_config.json", config), file("adapter_model.safetensors", []byte("w"))}}
	item, report, err := Build(request)
	if err != nil || report != nil || item.Source != "gs" || item.Checks.AdapterRank != 32 || len(item.Checks.Failed) != 0 || !strings.Contains(strings.Join(item.Checks.Verified, " "), "Trained on Qwen/Qwen3-8B") {
		t.Fatalf("adapter import: %#v report=%v err=%v", item.Checks, report, err)
	}
	request.BaseModel = "meta-llama/Llama-3.1-8B"
	item, _, _ = Build(request)
	if !strings.Contains(strings.Join(item.Checks.Unknown, " "), "names Qwen/Qwen3-8B as its base model") {
		t.Fatalf("a different declared base model must be shown: %#v", item.Checks)
	}
	local := BaseModelCheck("/checkpoints/base", "Qwen/Qwen3-8B")
	if len(local.Unknown) != 1 || len(local.Verified) != 0 {
		t.Fatalf("a local training path cannot be compared: %#v", local)
	}
	if _, err = ParseAdapterConfig([]byte(`{"peft_type":"PREFIX_TUNING","r":8}`)); err == nil {
		t.Fatal("a non-LoRA adapter was accepted")
	}
}

func TestFromDirectoryHashesFilesAndSkipsHidden(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"config.json": "{}", "shards/a.safetensors": "aa", ".cache/huggingface/x": "skip", ".DS_Store": "skip"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := FromDirectory(dir)
	if err != nil || len(files) != 2 || files[0] != file("config.json", []byte("{}")) || files[1] != file("shards/a.safetensors", []byte("aa")) {
		t.Fatalf("files=%#v err=%v", files, err)
	}
}

func TestValidateDeployment(t *testing.T) {
	hub := domain.ModelAdapter{Name: "support", Repository: "acme/support-lora", Revision: strings.Repeat("a", 40)}
	digest := Digest([]domain.ModelFile{file("a", []byte("a"))})
	valid := []struct {
		model, revision, runtime string
		adapters                 []domain.ModelAdapter
	}{
		{"Qwen/Qwen3-8B", "", "vllm", []domain.ModelAdapter{hub}},
		{"s3://acme-models/run-42", digest, "sglang", nil},
		{"Qwen/Qwen3-8B", "", "", []domain.ModelAdapter{{Name: "x", Repository: "gs://acme/a", Revision: digest}}},
	}
	for _, tc := range valid {
		if err := ValidateDeployment(tc.model, tc.revision, tc.runtime, "elastic", tc.adapters, nil); err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
	}
	invalid := []func() error{
		func() error { return ValidateDeployment("s3://acme-models/run-42", "main", "vllm", "elastic", nil, nil) },
		func() error { return ValidateDeployment("s3://acme-models/run-42/", digest, "vllm", "elastic", nil, nil) },
		func() error { return ValidateDeployment("s3://acme-models/run-42", digest, "vllm", "serverless", nil, nil) },
		func() error {
			return ValidateDeployment("m/m", "", "vllm", "elastic", []domain.ModelAdapter{hub, hub}, nil)
		},
		func() error {
			return ValidateDeployment("m/m", "", "custom-oci", "elastic", []domain.ModelAdapter{hub}, nil)
		},
		func() error {
			return ValidateDeployment("m/m", "", "vllm", "elastic", []domain.ModelAdapter{hub}, []string{"--lora-modules", "x=/y"})
		},
		func() error {
			adapter := hub
			adapter.Revision = "main"
			return ValidateDeployment("m/m", "", "vllm", "elastic", []domain.ModelAdapter{adapter}, nil)
		},
		func() error {
			adapter := hub
			adapter.Name = "Support Bot"
			return ValidateDeployment("m/m", "", "vllm", "elastic", []domain.ModelAdapter{adapter}, nil)
		},
	}
	for index, check := range invalid {
		if check() == nil {
			t.Fatalf("invalid case %d accepted", index)
		}
	}
}
