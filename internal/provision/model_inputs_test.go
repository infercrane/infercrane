package provision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const (
	testAdapterCommit = "0123456789abcdef0123456789abcdef01234567"
	testManifest      = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func testObjectSource(uri string) *ObjectSource {
	return &ObjectSource{URI: uri, Digest: testManifest, Files: []ObjectFile{
		{Path: "config.json", Size: 2, SHA256: strings.Repeat("a", 64)},
		{Path: "tokenizer.json", Size: 2, SHA256: strings.Repeat("b", 64)},
		{Path: "shards/model-00001.safetensors", Size: 4, SHA256: strings.Repeat("c", 64)},
	}}
}

func testHubAdapter() Adapter {
	return Adapter{Name: "support-bot", Source: "huggingface", Repository: "acme/support-lora", Revision: testAdapterCommit, Rank: 12}
}

func sealedCredential(value string) *ModelCredential {
	return &ModelCredential{ReferenceID: "secret-1", Resolver: "sealed", Open: func(context.Context) (string, error) { return value, nil }}
}

// A sealed Hugging Face token becomes a RunPod secret in the account that runs
// the Pod. The Pod only names it; its value never appears in the Pod body.
func TestRunPodPodsDeliversSealedModelCredentialAsAccountSecret(t *testing.T) {
	var mu sync.Mutex
	var pods []runPodRecord
	var secrets []runPodSecret
	secretValues := map[string]string{}
	posts, patches := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/account/secrets":
			_ = json.NewEncoder(w).Encode(map[string]any{"secrets": secrets})
		case r.Method == http.MethodPost && r.URL.Path == "/account/secrets":
			posts++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			secrets = append(secrets, runPodSecret{ID: "sec-1", Name: body["name"]})
			secretValues[body["name"]] = body["value"]
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(secrets[0])
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/account/secrets/"):
			patches++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/pods":
			writeRunPodV2List(w, "pods", pods)
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			encoded, _ := json.Marshal(body)
			if strings.Contains(string(encoded), "hf_sealedvalue") {
				t.Errorf("Pod body carries the token value: %s", encoded)
			}
			pods = []runPodRecord{body.pod("pod-1", "US-TX-3")}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pods[0])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := RunPodPods{APIKey: "tenant-runpod-key", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client(), ContainerDiskGiB: 100}
	spec := ReplicaSpec{TenantID: "acme", ExternalKey: "dep-r0", Model: "acme/private-model", ModelRevision: testAdapterCommit, Runtime: "vllm", Cloud: "runpod", GPU: "H100", GPUCount: 1, Workload: testRunPodWorkload(), ModelCredential: sealedCredential("hf_sealedvalue")}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	name := runPodModelSecretName("acme", "secret-1")
	if posts != 1 || secretValues[name] != "hf_sealedvalue" || strings.Contains(name, "acme") || strings.Contains(name, "secret-1") {
		t.Fatalf("secret not stored in the account under an opaque name: posts=%d values=%v name=%s", posts, secretValues, name)
	}
	if pods[0].Environment["HF_TOKEN"] != "{{ RUNPOD_SECRET_"+name+" }}" {
		t.Fatalf("Pod does not reference the RunPod secret: %v", pods[0].Environment)
	}
	// A replayed ensure adopts the Pod without writing the secret again.
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil || posts != 1 || patches != 0 {
		t.Fatalf("replay rewrote the secret: posts=%d patches=%d err=%v", posts, patches, err)
	}
	// A new Pod with the secret already in the account keeps its value current.
	pods = nil
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil || posts != 1 || patches != 1 {
		t.Fatalf("existing secret was not patched: posts=%d patches=%d err=%v", posts, patches, err)
	}
}

func TestRunPodPodsRefusesWhatItCannotDeliver(t *testing.T) {
	provider := RunPodPods{APIKey: "key", WorkerAPIKey: "worker", BaseURL: "http://127.0.0.1:1"}
	base := ReplicaSpec{TenantID: "acme", ExternalKey: "dep-r0", Model: "acme/model", ModelRevision: testAdapterCommit, Runtime: "vllm", Cloud: "runpod", GPU: "H100", GPUCount: 1, Workload: testRunPodWorkload()}
	cases := map[string]func(*ReplicaSpec){
		"object store weights": func(spec *ReplicaSpec) {
			spec.Model, spec.ModelRevision, spec.ModelObjects = "s3://acme-models/run-42", testManifest, testObjectSource("s3://acme-models/run-42")
		},
		"kubernetes credential": func(spec *ReplicaSpec) {
			spec.ModelCredential = &ModelCredential{ReferenceID: "secret-1", Resolver: "kubernetes", Reference: "hf-token/token"}
		},
		"two adapters": func(spec *ReplicaSpec) {
			second := testHubAdapter()
			second.Name = "other"
			spec.Adapters = []Adapter{testHubAdapter(), second}
		},
		"lora flag in runtime args": func(spec *ReplicaSpec) {
			spec.Adapters, spec.RuntimeArgs = []Adapter{testHubAdapter()}, []string{"--max-lora-rank=64"}
		},
		"unpinned adapter": func(spec *ReplicaSpec) {
			adapter := testHubAdapter()
			adapter.Revision = "main"
			spec.Adapters = []Adapter{adapter}
		},
		"custom runtime adapter": func(spec *ReplicaSpec) {
			spec.Runtime, spec.Adapters = "custom-oci", []Adapter{testHubAdapter()}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := base
			mutate(&spec)
			if _, err := provider.EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) {
				t.Fatalf("expected an invalid spec before any provider call, got %v", err)
			}
		})
	}
}

// A Hugging Face adapter is fetched at its pinned commit before the server
// starts, and the reviewed vLLM LoRA flags are rendered from the spec.
func TestRunPodPodsRendersPinnedLoRAAdapter(t *testing.T) {
	var created runPodV2PodCreate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/pods":
			writeRunPodV2List(w, "pods", []runPodRecord{})
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			created = body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body.pod("pod-1", "US-TX-3"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "key", WorkerAPIKey: "worker", BaseURL: server.URL, Client: server.Client(), ContainerDiskGiB: 100}
	spec := ReplicaSpec{TenantID: "acme", ExternalKey: "dep-r0", Model: "Qwen/Qwen3-8B", ModelRevision: testAdapterCommit, Runtime: "vllm", Cloud: "runpod", GPU: "H100", GPUCount: 1, Workload: testRunPodWorkload(), Adapters: []Adapter{testHubAdapter()}}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if strings.Join(created.Entrypoint, " ") != "/bin/sh" || created.Cmd[0] != "-c" || created.Cmd[2] != "infercrane-serve" {
		t.Fatalf("command is not wrapped by the fetch step: %v %v", created.Entrypoint, created.Cmd)
	}
	script := created.Cmd[1]
	if !strings.Contains(script, "snapshot_download") || !strings.Contains(script, "'acme/support-lora' '"+testAdapterCommit+"' '/infercrane/adapters/support-bot'") || !strings.HasSuffix(script, "exec \"$@\"\n") {
		t.Fatalf("fetch script does not pin the adapter: %s", script)
	}
	served := strings.Join(created.Cmd[3:], " ")
	if !strings.Contains(served, "vllm serve Qwen/Qwen3-8B --revision "+testAdapterCommit) || !strings.Contains(served, "--enable-lora --max-loras 1 --max-lora-rank 16 --lora-modules support-bot=/infercrane/adapters/support-bot") {
		t.Fatalf("served command misses the reviewed LoRA flags: %s", served)
	}
}

func TestSGLangAdapterArgsAndVLLMRankRounding(t *testing.T) {
	adapter := testHubAdapter()
	if got := strings.Join(adapterArgs("sglang", []Adapter{adapter}), " "); got != "--enable-lora --max-loras-per-batch 1 --max-lora-rank 12 --lora-paths support-bot=/infercrane/adapters/support-bot" {
		t.Fatalf("sglang args: %s", got)
	}
	for rank, want := range map[int]int{1: 1, 7: 8, 16: 16, 17: 32, 300: 320, 512: 512} {
		if got := vllmRank(rank); got != want {
			t.Fatalf("rank %d rounded to %d, want %d", rank, got, want)
		}
	}
	adapter.Rank = 0
	if got := strings.Join(adapterArgs("vllm", []Adapter{adapter}), " "); strings.Contains(got, "--max-lora-rank") {
		t.Fatalf("an unknown rank must keep the runtime's default: %s", got)
	}
}

// Object store weights are fetched by an init container with the Pod's own
// identity, every file is checked, and the server serves the local copy under
// the requested name. A cluster token stays in its own Secret.
func TestKubernetesFetchesObjectStoreWeightsAndReferencesClusterSecret(t *testing.T) {
	provider := Kubernetes{Context: "kind", Namespace: "infercrane-system", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key", GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product", ArtifactCachePolicy: "disabled", ObjectStoreSecretName: "minio-reader"}
	workload := runtimecontract.Workload{Image: "registry.example/runtime@sha256:" + strings.Repeat("a", 64), Command: []string{"vllm", "serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--port", "${PORT}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
	spec := ReplicaSpec{ExternalKey: "dep-r0", Model: "s3://acme-models/run-42", ModelRevision: testManifest, Runtime: "vllm", Cloud: "kubernetes", GPU: "L4", Workload: workload, ModelObjects: testObjectSource("s3://acme-models/run-42"), ModelCredential: &ModelCredential{ReferenceID: "secret-1", Resolver: "kubernetes", Reference: "hf-token/HF_TOKEN"}, Adapters: []Adapter{testHubAdapter()}}
	if err := provider.validate(spec); err != nil {
		t.Fatal(err)
	}
	manifest, err := provider.manifest(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						InitContainers []struct {
							Name, Image string
							Command     []string
							EnvFrom     []map[string]map[string]string `json:"envFrom"`
						} `json:"initContainers"`
						Containers []struct {
							Command []string
							Args    []string
							Env     []map[string]any
						} `json:"containers"`
						Volumes []map[string]any `json:"volumes"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err = json.Unmarshal(manifest, &list); err != nil {
		t.Fatal(err)
	}
	pod := list.Items[0].Spec.Template.Spec
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Image != defaultS3FetchImage || pod.InitContainers[0].EnvFrom[0]["secretRef"]["name"] != "minio-reader" {
		t.Fatalf("object store fetch container missing: %#v", pod.InitContainers)
	}
	fetch := pod.InitContainers[0].Command[2]
	for _, expected := range []string{"aws s3 cp --only-show-errors 's3://acme-models/run-42/shards/model-00001.safetensors' '/infercrane/model/shards/model-00001.safetensors'", "sha256sum -c -)", strings.Repeat("c", 64) + "  shards/model-00001.safetensors"} {
		if !strings.Contains(fetch, expected) {
			t.Fatalf("fetch script missing %q:\n%s", expected, fetch)
		}
	}
	runtime := pod.Containers[0]
	served := strings.Join(runtime.Args, " ")
	if !strings.Contains(served, "vllm serve /infercrane/model --port 8000") || strings.Contains(served, "--revision") || !strings.Contains(served, "--served-model-name s3://acme-models/run-42") || !strings.Contains(served, "--lora-modules support-bot=/infercrane/adapters/support-bot") {
		t.Fatalf("runtime does not serve the verified local copy: %s", served)
	}
	token := false
	for _, variable := range runtime.Env {
		if variable["name"] == "HF_TOKEN" {
			ref := variable["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			token = ref["name"] == "hf-token" && ref["key"] == "HF_TOKEN" && variable["value"] == nil
		}
	}
	if !token || strings.Contains(string(manifest), "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("token is not a secretKeyRef or object store keys leaked into the manifest: %s", manifest)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0]["name"] != "infercrane-fetch" {
		t.Fatalf("fetch volume missing: %#v", pod.Volumes)
	}
}

func TestKubernetesRefusesSealedCredentialAndMixedStores(t *testing.T) {
	provider := Kubernetes{Context: "kind", Namespace: "infercrane-system", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key", GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product", ArtifactCachePolicy: "disabled"}
	workload := testRunPodWorkload()
	sealed := ReplicaSpec{ExternalKey: "dep-r0", Model: "acme/model", Runtime: "vllm", Cloud: "kubernetes", GPU: "L4", Workload: workload, ModelCredential: sealedCredential("hf_value")}
	if err := provider.validate(sealed); err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("a sealed token must not be written into the cluster: %v", err)
	}
	gcsAdapter := Adapter{Name: "lora", Source: "gs", Revision: testManifest, Objects: testObjectSource("gs://acme-models/lora")}
	gcsAdapter.Objects.Files = []ObjectFile{{Path: "adapter_config.json", Size: 1, SHA256: strings.Repeat("d", 64)}}
	mixed := ReplicaSpec{ExternalKey: "dep-r0", Model: "s3://acme-models/run-42", ModelRevision: testManifest, Runtime: "vllm", Cloud: "kubernetes", GPU: "L4", Workload: workload, ModelObjects: testObjectSource("s3://acme-models/run-42"), Adapters: []Adapter{gcsAdapter}}
	if err := provider.validate(mixed); err == nil || !strings.Contains(err.Error(), "one object store") {
		t.Fatalf("mixed object stores accepted: %v", err)
	}
}

func TestKubernetesIntentHashUnchangedWithoutModelInputs(t *testing.T) {
	provider := Kubernetes{Namespace: "ns", WorkloadAPI: "deployment", ObjectStoreSecretName: "minio-reader", S3FetchImage: "registry.example/aws@sha256:" + strings.Repeat("e", 64)}
	spec := ReplicaSpec{ExternalKey: "dep-r0", Model: "acme/model", Cloud: "kubernetes", GPU: "L4"}
	withSettings, err := provider.intentHash(spec)
	if err != nil {
		t.Fatal(err)
	}
	provider.ObjectStoreSecretName, provider.S3FetchImage = "", ""
	without, _ := provider.intentHash(spec)
	if withSettings != without {
		t.Fatal("object store settings changed the intent of a replica that fetches nothing")
	}
}

// On AWS the instance profile reads the token's Secrets Manager ARN and the
// customer's S3 files. The token reaches the container through the inherited
// environment, never the docker command line.
func TestAWSEC2ReadsModelInputsWithInstanceProfile(t *testing.T) {
	provider := testAWSEC2(&fakeAWSRunner{})
	spec := awsReplicaSpec()
	spec.Model, spec.ModelRevision, spec.Runtime = "s3://acme-models/run-42", testManifest, "vllm"
	spec.Workload = testRunPodWorkload()
	spec.ModelObjects = testObjectSource("s3://acme-models/run-42")
	spec.ModelCredential = &ModelCredential{ReferenceID: "secret-1", Resolver: "aws-secrets-manager", Reference: "arn:aws:secretsmanager:us-east-1:123456789012:secret:hf-token-AbCdEf"}
	if err := provider.validate(spec); err != nil {
		t.Fatal(err)
	}
	startup := provider.userData(spec, 8000, "")
	for _, expected := range []string{
		"HF_TOKEN=$(aws secretsmanager get-secret-value --region 'us-east-1' --secret-id 'arn:aws:secretsmanager:us-east-1:123456789012:secret:hf-token-AbCdEf' --query SecretString --output text)",
		"export HF_TOKEN",
		"aws s3 cp --only-show-errors 's3://acme-models/run-42/config.json' '/infercrane/model/config.json'",
		"sha256sum -c -)",
		"-e HF_TOKEN -v /infercrane:/infercrane -e INFERCRANE_WORKER_API_KEY",
		"'serve' '/infercrane/model' '--port' '8000' '--served-model-name' 's3://acme-models/run-42'",
	} {
		if !strings.Contains(startup, expected) {
			t.Fatalf("startup missing %q:\n%s", expected, startup)
		}
	}
	if strings.Index(startup, "model_files_verified") > strings.Index(startup, "runtime_start") {
		t.Fatal("the runtime starts before the files are verified")
	}
	spec.ModelObjects = testObjectSource("gs://acme-models/run-42")
	spec.Model = "gs://acme-models/run-42"
	if err := provider.validate(spec); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("AWS accepted GCS files: %v", err)
	}
	spec.Model, spec.ModelObjects = "Qwen/Qwen3-8B", nil
	spec.ModelCredential = sealedCredential("hf_value")
	if err := provider.validate(spec); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("AWS accepted a sealed token it cannot deliver: %v", err)
	}
}

func TestProvidersWithoutDeliveryRefuseModelInputs(t *testing.T) {
	spec := ReplicaSpec{ExternalKey: "dep-r0", Model: "acme/model", Cloud: "runpod", GPU: "H100", Runtime: "vllm", ModelCredential: sealedCredential("hf_value")}
	if _, err := (SkyPilot{}).EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("SkyPilot accepted a model credential: %v", err)
	}
	if _, err := (GCPCompute{}).EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("GCP accepted a model credential: %v", err)
	}
}

// The fetch script runs under sh: a file whose bytes differ from the manifest
// stops the replica before its server starts.
func TestFetchScriptVerifiesEveryFile(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not installed")
	}
	dir := t.TempDir()
	source := ObjectSource{URI: "s3://acme-models/x", Digest: testManifest, Files: []ObjectFile{{Path: "config.json", Size: 2, SHA256: "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"}}}
	var script strings.Builder
	writeObjectFetch(&script, source, dir)
	// Replace the download with a local write so the check itself runs.
	good := strings.Replace(script.String(), "aws s3 cp --only-show-errors 's3://acme-models/x/config.json' '"+dir+"/config.json'", "printf '{}' > '"+dir+"/config.json'", 1)
	if output, err := exec.Command("/bin/sh", "-c", "set -eu\n"+good).CombinedOutput(); err != nil {
		t.Fatalf("matching file refused: %v %s", err, output)
	}
	bad := strings.Replace(script.String(), "aws s3 cp --only-show-errors 's3://acme-models/x/config.json' '"+dir+"/config.json'", "printf '[]' > '"+dir+"/config.json'", 1)
	if _, err := exec.Command("/bin/sh", "-c", "set -eu\n"+bad).CombinedOutput(); err == nil {
		t.Fatal("a file that does not match the manifest was accepted")
	}
}
