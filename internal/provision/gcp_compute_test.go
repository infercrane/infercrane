package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type fakeGCPRunner struct {
	instance         gcpInstance
	external         string
	creates, deletes int
	loseCreate       bool
	lastCreate       []string
	serialOutput     string
	disk             gcpDisk
}

type gcpCheckRunner struct {
	calls               [][]string
	privateGoogleAccess string
	disk                gcpDisk
	regionalQuotas      string
	globalQuotas        string
}

type gcpLaunchProbeRunner struct {
	calls                                        [][]string
	acceleratorMissing, machineMissing           bool
	regionalQuotas, globalQuotas                 string
	omitRegionalQuota, omitGlobalQuota, failAuth bool
}

func (r *gcpLaunchProbeRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "auth" {
		if r.failAuth {
			return []byte("unauthenticated"), errors.New("exit 1")
		}
		return []byte("token"), nil
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "machine-types" && args[2] == "describe" {
		if r.machineMissing {
			return []byte("was not found"), errors.New("exit 1")
		}
		return []byte("g2-standard-4"), nil
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "accelerator-types" && args[2] == "describe" {
		if r.acceleratorMissing {
			return []byte("was not found"), errors.New("exit 1")
		}
		return []byte("nvidia-l4"), nil
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "regions" && args[2] == "describe" {
		if r.omitRegionalQuota {
			return []byte(`{"quotas":[]}`), nil
		}
		if r.regionalQuotas != "" {
			return []byte(r.regionalQuotas), nil
		}
		return []byte(`{"quotas":[{"metric":"GPU_FAMILY:NVIDIA_L4","limit":8,"usage":0}]}`), nil
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "project-info" && args[2] == "describe" {
		if r.omitGlobalQuota {
			return []byte(`{"quotas":[]}`), nil
		}
		if r.globalQuotas != "" {
			return []byte(r.globalQuotas), nil
		}
		return []byte(`{"quotas":[{"metric":"GPUS_ALL_REGIONS","limit":8,"usage":0}]}`), nil
	}
	return nil, errors.New("unexpected gcloud command")
}

func (r *gcpCheckRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 2 && args[0] == "compute" && args[1] == "networks" && args[2] == "subnets" {
		if r.privateGoogleAccess == "" {
			return []byte("true"), nil
		}
		return []byte(r.privateGoogleAccess), nil
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "disks" && args[2] == "describe" {
		return json.Marshal(r.disk)
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "regions" && args[2] == "describe" {
		if r.regionalQuotas != "" {
			return []byte(r.regionalQuotas), nil
		}
		return []byte(`{"quotas":[{"metric":"GPU_FAMILY:NVIDIA_L4","limit":8,"usage":0}]}`), nil
	}
	if len(args) > 2 && args[0] == "compute" && args[1] == "project-info" && args[2] == "describe" {
		if r.globalQuotas != "" {
			return []byte(r.globalQuotas), nil
		}
		return []byte(`{"quotas":[{"metric":"GPUS_ALL_REGIONS","limit":8,"usage":0}]}`), nil
	}
	return []byte("ok"), nil
}

func (f *fakeGCPRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	if len(args) >= 3 && args[0] == "compute" && args[1] == "regions" && args[2] == "describe" {
		return []byte(`{"quotas":[{"metric":"GPU_FAMILY:NVIDIA_L4","limit":8,"usage":0}]}`), nil
	}
	if len(args) >= 3 && args[0] == "compute" && args[1] == "project-info" && args[2] == "describe" {
		return []byte(`{"quotas":[{"metric":"GPUS_ALL_REGIONS","limit":8,"usage":0}]}`), nil
	}
	if len(args) >= 3 && args[0] == "compute" && args[1] == "disks" && args[2] == "describe" {
		if f.disk.Name == "" {
			return []byte("was not found"), errors.New("exit 1")
		}
		return json.Marshal(f.disk)
	}
	if len(args) < 3 || args[0] != "compute" || args[1] != "instances" {
		return nil, errors.New("unexpected gcloud command")
	}
	switch args[2] {
	case "describe":
		if f.instance.Name == "" {
			return []byte("was not found"), errors.New("exit 1")
		}
		return json.Marshal(f.instance)
	case "create":
		f.creates++
		f.lastCreate = append([]string(nil), args...)
		f.instance.Name = args[3]
		f.instance.Status = "RUNNING"
		f.instance.NetworkInterfaces = append(f.instance.NetworkInterfaces, struct {
			NetworkIP string `json:"networkIP"`
		}{NetworkIP: "10.20.0.8"})
		for i, arg := range args {
			if arg == "--metadata" && i+1 < len(args) {
				metadata := strings.TrimPrefix(args[i+1], "^|||^")
				for _, part := range strings.Split(metadata, "|||") {
					if strings.HasPrefix(part, "infercrane-external-key=") {
						f.external = strings.TrimPrefix(part, "infercrane-external-key=")
					}
					if strings.HasPrefix(part, "infercrane-intent-digest=") {
						f.instance.Metadata.Items = append(f.instance.Metadata.Items, struct{ Key, Value string }{Key: "infercrane-intent-digest", Value: strings.TrimPrefix(part, "infercrane-intent-digest=")})
					}
				}
			}
		}
		f.instance.Metadata.Items = append(f.instance.Metadata.Items, struct{ Key, Value string }{Key: "infercrane-external-key", Value: f.external})
		if f.loseCreate {
			f.loseCreate = false
			return nil, errors.New("lost create response")
		}
		return json.Marshal([]gcpInstance{f.instance})
	case "delete":
		f.deletes++
		f.instance = gcpInstance{}
		return []byte(`{}`), nil
	case "list":
		if f.instance.Name == "" {
			return []byte(`[]`), nil
		}
		return json.Marshal([]gcpInstance{f.instance})
	case "get-serial-port-output":
		return []byte(f.serialOutput), nil
	default:
		return nil, errors.New("unexpected instances command")
	}
}

func TestGCPComputePersistsClosedStartupEvidence(t *testing.T) {
	runner := &fakeGCPRunner{instance: gcpInstance{Name: "infercrane-worker", Status: "RUNNING"}, serialOutput: "token=do-not-retain\ninfercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\ninfercrane_startup stage=image_cache_hit at=2026-08-23T10:00:02Z\ninfercrane_startup stage=runtime_container_started at=2026-08-23T10:00:04Z\n"}
	runner.instance.NetworkInterfaces = append(runner.instance.NetworkInterfaces, struct {
		NetworkIP string `json:"networkIP"`
	}{NetworkIP: "10.20.0.8"})
	observed, err := testGCPCompute(runner).ObserveReplica(context.Background(), ProviderHandle{ResourceID: runner.instance.Name}, 8000)
	if err != nil || !strings.Contains(observed.Details, `"current_stage":"runtime_container_started"`) || !strings.Contains(observed.Details, `"image_cache":"hit"`) || strings.Contains(observed.Details, "do-not-retain") {
		t.Fatalf("observation=%#v err=%v", observed, err)
	}
}

func testGCPCompute(runner CommandRunner) GCPCompute {
	return GCPCompute{Runner: runner, Project: "project", Zone: "europe-west4-a", Subnet: "private-inference", MachineType: "g2-standard-4", GPUType: "nvidia-l4", ServiceAccount: "runtime@project.iam.gserviceaccount.com", VMImage: "projects/cos-cloud/global/images/cos-immutable", ContainerImage: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), WorkerSecret: "infercrane-worker", BootDiskGiB: 200}
}

func TestGCPComputeCheckIsReadOnly(t *testing.T) {
	runner := &gcpCheckRunner{}
	provider := testGCPCompute(runner)
	if err := provider.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 10 || runner.calls[0][0] != "auth" || runner.calls[1][2] != "subnets" || runner.calls[2][1] != "zones" || runner.calls[3][1] != "machine-types" || runner.calls[4][1] != "accelerator-types" || runner.calls[5][0] != "iam" || runner.calls[6][0] != "secrets" || runner.calls[7][1] != "images" || runner.calls[8][1] != "regions" || runner.calls[9][1] != "project-info" {
		t.Fatalf("unexpected read-only calls: %#v", runner.calls)
	}
}

func TestGCPComputeProbeLaunchHeadroomDoesNotClaimStockOrDeployability(t *testing.T) {
	runner := &gcpLaunchProbeRunner{}
	evidence, err := testGCPCompute(runner).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ConnectionState != "configured" || evidence.QuotaState != "available" || evidence.AvailabilityState != "unknown" || evidence.Deployability != "unknown" {
		t.Fatalf("positive support or quota headroom became stock/deployability evidence: %#v", evidence)
	}
	if evidence.Source != "gcp.compute.exact-zone-preflight" || evidence.ObservedAt.IsZero() || !evidence.ExpiresAt.After(evidence.ObservedAt) {
		t.Fatalf("probe evidence lacks bounded provenance: %#v", evidence)
	}
	if evidence.Region != "europe-west4-a" {
		t.Fatalf("region-wide request did not retain exact-zone evidence scope: %#v", evidence)
	}
	if len(runner.calls) != 5 || runner.calls[0][0] != "auth" || runner.calls[1][1] != "machine-types" || runner.calls[2][1] != "accelerator-types" || runner.calls[3][1] != "regions" || runner.calls[4][1] != "project-info" {
		t.Fatalf("unexpected exact-zone read-only calls: %#v", runner.calls)
	}
	if machineCall, acceleratorCall := strings.Join(runner.calls[1], " "), strings.Join(runner.calls[2], " "); !strings.Contains(machineCall, "g2-standard-4 --zone europe-west4-a --project project") || !strings.Contains(acceleratorCall, "nvidia-l4 --zone europe-west4-a --project project") {
		t.Fatalf("probe did not bind support checks to the exact configured tuple: %#v", runner.calls)
	}
	for _, call := range runner.calls {
		if len(call) > 2 && call[0] == "compute" && call[1] == "instances" {
			t.Fatalf("launch probe mutated provider state: %#v", runner.calls)
		}
	}
}

func TestGCPComputeProbeLaunchReportsTriStateQuotaEvidence(t *testing.T) {
	tests := []struct {
		name   string
		runner *gcpLaunchProbeRunner
		state  string
		deploy string
	}{
		{name: "available", runner: &gcpLaunchProbeRunner{}, state: "available", deploy: "unknown"},
		{name: "unknown", runner: &gcpLaunchProbeRunner{omitGlobalQuota: true}, state: "unknown", deploy: "unknown"},
		{name: "malformed quota is unknown", runner: &gcpLaunchProbeRunner{regionalQuotas: `{"quotas":[{"metric":"GPU_FAMILY:NVIDIA_L4"}]}`}, state: "unknown", deploy: "unknown"},
		{name: "unavailable", runner: &gcpLaunchProbeRunner{regionalQuotas: `{"quotas":[{"metric":"GPU_FAMILY:NVIDIA_L4","limit":4,"usage":4}]}`}, state: "unavailable", deploy: "unavailable"},
		{name: "known blocker dominates missing signal", runner: &gcpLaunchProbeRunner{omitRegionalQuota: true, globalQuotas: `{"quotas":[{"metric":"GPUS_ALL_REGIONS","limit":0,"usage":0}]}`}, state: "unavailable", deploy: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, err := testGCPCompute(test.runner).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "gcp", Region: "europe-west4-a", GPU: "nvidia-l4", GPUCount: 1})
			if err != nil || evidence.QuotaState != test.state || evidence.Deployability != test.deploy || evidence.AvailabilityState != "unknown" {
				t.Fatalf("evidence=%#v err=%v", evidence, err)
			}
		})
	}
}

func TestGCPComputeProbeLaunchReportsUnsupportedExactZoneWithoutCreating(t *testing.T) {
	tests := []struct {
		name         string
		runner       *gcpLaunchProbeRunner
		availability string
		calls        int
	}{
		{name: "machine", runner: &gcpLaunchProbeRunner{machineMissing: true}, availability: "unknown", calls: 2},
		{name: "accelerator", runner: &gcpLaunchProbeRunner{acceleratorMissing: true}, availability: "unavailable", calls: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, err := testGCPCompute(test.runner).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1})
			if err != nil || evidence.AvailabilityState != test.availability || evidence.QuotaState != "unknown" || evidence.Deployability != "unavailable" {
				t.Fatalf("evidence=%#v err=%v", evidence, err)
			}
			if len(test.runner.calls) != test.calls {
				t.Fatalf("unsupported resource should stop before quota calls: %#v", test.runner.calls)
			}
		})
	}
}

func TestGCPComputeProbeLaunchAuthenticationFailureDoesNotClaimEvidence(t *testing.T) {
	runner := &gcpLaunchProbeRunner{failAuth: true}
	evidence, err := testGCPCompute(runner).ProbeLaunch(context.Background(), LaunchProbeRequest{Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1})
	if !errors.Is(err, ErrProviderAuthorization) || evidence.AvailabilityState != "unknown" || evidence.QuotaState != "unknown" || evidence.Deployability != "unknown" || len(runner.calls) != 1 {
		t.Fatalf("evidence=%#v calls=%#v err=%v", evidence, runner.calls, err)
	}
}

func TestGCPComputeProbeLaunchRejectsUnconfiguredTupleWithoutProviderCalls(t *testing.T) {
	requests := []LaunchProbeRequest{
		{Provider: "gcp", Region: "us-central1", GPU: "nvidia-l4", GPUCount: 1},
		{Provider: "gcp", Region: "europe-west4", GPU: "nvidia-tesla-t4", GPUCount: 1},
	}
	for _, request := range requests {
		runner := &gcpLaunchProbeRunner{}
		evidence, err := testGCPCompute(runner).ProbeLaunch(context.Background(), request)
		if err != nil || evidence.AvailabilityState != "unknown" || evidence.QuotaState != "unknown" || evidence.Deployability != "unavailable" || len(runner.calls) != 0 {
			t.Fatalf("request=%#v evidence=%#v calls=%#v err=%v", request, evidence, runner.calls, err)
		}
	}
}

func TestGCPComputeCheckFailsFastOnObservedGPUQuotaExhaustion(t *testing.T) {
	runner := &gcpCheckRunner{regionalQuotas: `{"quotas":[{"metric":"GPU_FAMILY:NVIDIA_L4","limit":4,"usage":4}]}`}
	err := testGCPCompute(runner).Check(context.Background())
	if !errors.Is(err, ErrProviderQuota) || !strings.Contains(err.Error(), "regional GPU quota") {
		t.Fatalf("quota exhaustion was not typed: %v", err)
	}
	for _, call := range runner.calls {
		if len(call) > 2 && call[0] == "compute" && call[1] == "instances" && call[2] == "create" {
			t.Fatalf("quota preflight created capacity: %#v", runner.calls)
		}
	}
}

func TestGCPComputeCheckRequiresPrivateGoogleAccess(t *testing.T) {
	runner := &gcpCheckRunner{privateGoogleAccess: "False"}
	err := testGCPCompute(runner).Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Private Google Access") {
		t.Fatalf("private worker subnet without Google API access accepted: %v", err)
	}
	for _, call := range runner.calls {
		if len(call) > 2 && call[0] == "compute" && call[1] == "instances" && call[2] == "create" {
			t.Fatalf("preflight created paid capacity: %#v", runner.calls)
		}
	}
}

func TestGCPComputeCompilesExactAcceleratorCountAndPortableEntrypoint(t *testing.T) {
	runner := &fakeGCPRunner{}
	provider := testGCPCompute(runner)
	spec := ReplicaSpec{ExternalKey: "deployment-r0", Model: "zai-org/model", ModelRevision: "commit", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 4, Workload: runtimecontract.Workload{Image: "registry.example/runtime@sha256:" + strings.Repeat("b", 64), Command: []string{"vllm", "serve", "${MODEL}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.lastCreate, "\n")
	for _, expected := range []string{"type=nvidia-l4,count=4", `[ "$actual_gpu_count" -eq 4 ]`, "--entrypoint 'vllm' '" + spec.Workload.Image + "' 'serve' 'zai-org/model'"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("create request missing %q:\n%s", expected, joined)
		}
	}
}

func TestGCPComputeCheckVerifiesConfiguredArtifactDiskWithoutMutation(t *testing.T) {
	const identity = "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567"
	runner := &gcpCheckRunner{disk: gcpDisk{
		Name: "qwen3-8b-cache", Status: "READY", SizeGB: "200",
		Zone:        "https://www.googleapis.com/compute/v1/projects/project/zones/europe-west4-a",
		Description: "infercrane-model-identity-digest=sha256:b89562e9fdfc74f318d6870cc672993f6ededab8985a0689bc2c9f97b7414977",
	}}
	provider := testGCPCompute(runner)
	provider.ArtifactDisks = map[string]string{identity: "qwen3-8b-cache"}
	if err := provider.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range runner.calls {
		if len(call) > 2 && call[0] == "compute" && call[1] == "disks" && call[2] == "describe" {
			found = true
		}
		if len(call) > 2 && call[2] == "create" {
			t.Fatalf("doctor mutated provider state: %#v", runner.calls)
		}
	}
	if !found {
		t.Fatalf("doctor omitted artifact disk verification: %#v", runner.calls)
	}
}

func TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable(t *testing.T) {
	runner := &fakeGCPRunner{loseCreate: true}
	provider := testGCPCompute(runner)
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"}
	handle := provider.Handle(spec.ExternalKey)
	if handle.ResourceID == "" || handle.RequestID == "" {
		t.Fatal("deterministic identity missing")
	}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil {
		t.Fatal("lost create response not surfaced")
	}
	adopted, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || adopted.ResourceID != handle.ResourceID || runner.creates != 1 {
		t.Fatalf("adopted=%#v creates=%d err=%v", adopted, runner.creates, err)
	}
	joined := strings.Join(runner.lastCreate, " ")
	for _, required := range []string{"--no-address", "--boot-disk-size 200GB", "--boot-disk-type pd-balanced", "--boot-disk-auto-delete", "--service-account runtime@project.iam.gserviceaccount.com", "--labels infercrane-managed=true"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("create missing %q: %s", required, joined)
		}
	}
	if !strings.Contains(joined, `-e VLLM_API_KEY="$worker_key"`) || strings.Contains(joined, "--api-key") {
		t.Fatalf("vLLM credential must be injected through a non-argv environment variable: %s", joined)
	}
	observed, err := provider.ObserveReplica(context.Background(), adopted, 8000)
	if err != nil || observed.State != "ready" || observed.Endpoint != "http://10.20.0.8:8000" {
		t.Fatalf("observation=%#v err=%v", observed, err)
	}
	resources, err := provider.Inventory(context.Background(), InventoryFilter{Prefix: "prod-"})
	if err != nil || len(resources) != 1 || resources[0].ExternalKey != spec.ExternalKey {
		t.Fatalf("inventory=%#v err=%v", resources, err)
	}
	if err = provider.DeleteReplica(context.Background(), adopted); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteReplica(context.Background(), adopted); err != nil || runner.deletes != 1 {
		t.Fatalf("delete replay calls=%d err=%v", runner.deletes, err)
	}
}

func TestGCPArtifactDiskIsVerifiedAttachedReadOnlyAndObservable(t *testing.T) {
	const identity = "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567"
	const diskName = "qwen3-8b-cache"
	runner := &fakeGCPRunner{disk: gcpDisk{
		Name: diskName, Status: "READY", SizeGB: "200",
		Zone:        "https://www.googleapis.com/compute/v1/projects/project/zones/europe-west4-a",
		Description: "infercrane-model-identity-digest=sha256:b89562e9fdfc74f318d6870cc672993f6ededab8985a0689bc2c9f97b7414977",
		Type:        "https://www.googleapis.com/compute/v1/projects/project/zones/europe-west4-a/diskTypes/pd-balanced",
	}}
	provider := testGCPCompute(runner)
	provider.ArtifactCachePolicy = "required"
	provider.ArtifactDisks = map[string]string{identity: diskName}
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "0123456789abcdef0123456789abcdef01234567", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.lastCreate, " ")
	for _, required := range []string{
		"--disk name=qwen3-8b-cache,device-name=infercrane-model-cache,mode=ro,auto-delete=no",
		"/dev/disk/by-id/google-infercrane-model-cache",
		"mount -o ro,nosuid,nodev",
		"HF_HUB_OFFLINE=1",
		"-v /var/lib/infercrane/huggingface:/root/.cache/huggingface:ro",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("cached create omitted %q: %s", required, joined)
		}
	}
	request := artifactcache.Request{ArtifactID: "artifact-1", ModelIdentity: identity, Provider: "gcp", Region: "europe-west4-a", Location: "gcp-pd://" + diskName, IdempotencyKey: "prefetch-1"}
	operation, err := provider.Prefetch(context.Background(), request)
	if err != nil || operation.Status != "succeeded" || operation.ProviderOperationID != diskName {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	observation, err := provider.Observe(context.Background(), request)
	if err != nil || observation.State != "present" || observation.Source != "gcp-persistent-disk" || !strings.Contains(observation.EvidenceJSON, `"attachment":"read-only"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestGCPArtifactDiskFailsClosedBeforeInstanceMutation(t *testing.T) {
	const identity = "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567"
	runner := &fakeGCPRunner{disk: gcpDisk{Name: "qwen3-8b-cache", Status: "READY", SizeGB: "200", Zone: "https://www.googleapis.com/compute/v1/projects/project/zones/us-central1-a", Description: "wrong"}}
	provider := testGCPCompute(runner)
	provider.ArtifactCachePolicy = "required"
	provider.ArtifactDisks = map[string]string{identity: "qwen3-8b-cache"}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "0123456789abcdef0123456789abcdef01234567", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"})
	if err == nil || runner.creates != 0 || !strings.Contains(err.Error(), "immutable model identity") {
		t.Fatalf("unverified disk reached instance mutation: creates=%d err=%v", runner.creates, err)
	}
}

func TestGCPArtifactConfigurationCannotBypassProviderValidation(t *testing.T) {
	provider := testGCPCompute(&fakeGCPRunner{})
	provider.ArtifactCachePolicy = "sometimes"
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "disabled, prefer, or required") {
		t.Fatalf("invalid direct provider policy accepted: %v", err)
	}
	provider = testGCPCompute(&fakeGCPRunner{})
	provider.ArtifactDisks = map[string]string{"Qwen/Qwen3-8B": "qwen-cache"}
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable model identities") {
		t.Fatalf("mutable direct provider mapping accepted: %v", err)
	}
}

func TestGCPComputeCheckRejectsIncompleteOrMutableDependenciesBeforeNetwork(t *testing.T) {
	runner := &gcpCheckRunner{}
	provider := testGCPCompute(runner)
	provider.ContainerImage = "mutable:latest"
	if err := provider.Check(context.Background()); err == nil || len(runner.calls) != 0 {
		t.Fatalf("mutable dependency reached GCP: calls=%#v err=%v", runner.calls, err)
	}
	provider = testGCPCompute(runner)
	provider.VMImage = "projects/cos-cloud/global/images/family/cos-stable"
	if err := provider.Check(context.Background()); err == nil || len(runner.calls) != 0 {
		t.Fatalf("image family was accepted: calls=%#v err=%v", runner.calls, err)
	}
}

func TestGCPIdentityParsersFailClosed(t *testing.T) {
	if region, ok := gcpRegionFromZone("europe-west4-a"); !ok || region != "europe-west4" {
		t.Fatalf("region=%q ok=%t", region, ok)
	}
	for _, invalid := range []string{"europe-west4", "europe-west4-aa", "", "-a"} {
		if _, ok := gcpRegionFromZone(invalid); ok {
			t.Fatalf("invalid zone accepted: %q", invalid)
		}
	}
	project, image, ok := gcpImageIdentity("projects/cos-cloud/global/images/cos-immutable")
	if !ok || project != "cos-cloud" || image != "cos-immutable" {
		t.Fatalf("project=%q image=%q ok=%t", project, image, ok)
	}
	for _, invalid := range []string{"cos-immutable", "projects/cos-cloud/global/images/family/cos-stable", "projects//global/images/image"} {
		if _, _, ok := gcpImageIdentity(invalid); ok {
			t.Fatalf("invalid image accepted: %q", invalid)
		}
	}
}

func TestGCPComputeRejectsMutableContainerBeforeProviderCall(t *testing.T) {
	runner := &fakeGCPRunner{}
	provider := testGCPCompute(runner)
	provider.ContainerImage = "mutable:latest"
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "r0", Model: "model", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"})
	if err == nil || runner.creates != 0 {
		t.Fatalf("mutable image accepted err=%v creates=%d", err, runner.creates)
	}
}

func TestGCPComputeNormalizesProviderFailuresWithoutLeakingRawOutput(t *testing.T) {
	tests := []struct {
		name, output, remediation string
		target                    error
	}{
		{"authorization", `PERMISSION_DENIED principal user@example.com lacks compute.instances.create`, "control identity", ErrProviderAuthorization},
		{"global GPU quota", `QUOTA_EXCEEDED: Quota 'GPUS_ALL_REGIONS' exceeded. Limit: 0.0 globally. project=secret-project`, "accelerator quota", ErrProviderQuota},
		{"zonal capacity", `ZONE_RESOURCE_POOL_EXHAUSTED: The zone does not have enough resources`, "selected zone", ErrProviderCapacity},
		{"invalid launch", `INVALID_ARGUMENT: Invalid value for field resource.machineType`, "launch configuration", ErrInvalidReplicaSpec},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := normalizeGCPAPIError([]byte(test.output))
			if !errors.Is(err, test.target) || !strings.Contains(err.Error(), test.remediation) {
				t.Fatalf("normalized error=%v", err)
			}
			for _, secret := range []string{"user@example.com", "secret-project", "GPUS_ALL_REGIONS"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("normalized error leaked provider output: %v", err)
				}
			}
		})
	}
}

func TestGCPNotFoundClassificationIsLimitedToObservationCommands(t *testing.T) {
	if !gcpNotFoundProbe([]string{"compute", "instances", "describe", "worker"}) || !gcpNotFoundProbe([]string{"compute", "instances", "delete", "worker"}) {
		t.Fatal("observation and idempotent deletion must recognize missing resources")
	}
	if gcpNotFoundProbe([]string{"compute", "instances", "create", "worker"}) || gcpNotFoundProbe([]string{"compute", "images", "describe", "image"}) {
		t.Fatal("unrelated provider failures must not be converted to missing instances")
	}
}

func TestGCPComputeRefusesSameNameInstanceWithMismatchedIntent(t *testing.T) {
	provider := testGCPCompute(nil)
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"}
	runner := &fakeGCPRunner{instance: gcpInstance{Name: provider.Handle(spec.ExternalKey).ResourceID, Status: "RUNNING"}}
	provider.Runner = runner
	_, err := provider.EnsureReplica(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "does not match immutable intent") || runner.creates != 0 {
		t.Fatalf("same-name conflicting VM was adopted: creates=%d err=%v", runner.creates, err)
	}
}

func TestGCPComputePortableWorkloadExpandsArgumentsSafely(t *testing.T) {
	provider := testGCPCompute(&fakeGCPRunner{})
	spec := ReplicaSpec{Model: "acme/model; touch /tmp/pwned", ModelRevision: "commit-123", RuntimeArgs: []string{"--trust-remote-code"}, Workload: runtimecontract.Workload{Image: "registry.example/runtime@sha256:" + strings.Repeat("b", 64), Command: []string{"serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--port", "${PORT}", "--key", "${WORKER_API_KEY}"}}}
	startup := provider.startup(spec, 9000)
	for _, expected := range []string{"registry.example/runtime@sha256:", "acme/model; touch /tmp/pwned", "commit-123", "9000", "--trust-remote-code", "INFERCRANE_WORKER_API_KEY"} {
		if !strings.Contains(startup, expected) {
			t.Fatalf("startup script omitted %q: %s", expected, startup)
		}
	}
	if strings.Contains(startup, "${MODEL}") || strings.Contains(startup, "${MODEL_REVISION}") || strings.Contains(startup, "${PORT}") || strings.Contains(startup, "${WORKER_API_KEY}") {
		t.Fatalf("startup retained an unresolved workload placeholder: %s", startup)
	}
	if strings.Contains(startup, " sh -c ") || !strings.Contains(startup, `'acme/model; touch /tmp/pwned'`) {
		t.Fatalf("portable argv was not passed as shell-quoted container argv: %s", startup)
	}
}

func TestGCPComputeStartupUsesWorkloadIdentityWithoutWorkerGcloud(t *testing.T) {
	provider := testGCPCompute(&fakeGCPRunner{})
	script := provider.startup(ReplicaSpec{Model: "Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40)}, 8000)
	for _, required := range []string{"metadata.google.internal", "Metadata-Flavor: Google", "secretmanager.googleapis.com", "base64 -d", "unset token_json access_token secret_json secret_data", "infercrane_stage identity_start", "infercrane_stage identity_ready", "docker image inspect", "infercrane_stage image_cache_hit", "infercrane_stage image_pull_complete", "infercrane_stage runtime_start", "infercrane_stage runtime_container_started"} {
		if !strings.Contains(script, required) {
			t.Fatalf("startup script missing %q: %s", required, script)
		}
	}
	if strings.Contains(script, "gcloud secrets") {
		t.Fatalf("worker startup unexpectedly depends on gcloud: %s", script)
	}
	if !strings.Contains(script, "'Qwen/Qwen3-8B' '--port' '8000'") || strings.Contains(script, "--model") {
		t.Fatalf("vLLM startup does not use the current positional model CLI: %s", script)
	}
	command := exec.Command("sh", "-n")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("startup script is not valid POSIX shell: %v: %s\n%s", err, output, script)
	}
}

func TestGCPComputeStartupInstallsAndExposesGPUOnContainerOptimizedOS(t *testing.T) {
	provider := testGCPCompute(&fakeGCPRunner{})
	script := provider.startup(ReplicaSpec{Model: "Qwen/Qwen3-8B"}, 8000)
	for _, required := range []string{
		"infercrane_stage gpu_driver_start",
		"systemctl is-active --quiet gcr-online.target",
		"cos-extensions install gpu",
		"[ -e /dev/nvidia0 ]",
		"--volume /var/lib/nvidia/lib64:/usr/local/nvidia/lib64:ro",
		"--device /dev/nvidia-uvm:/dev/nvidia-uvm",
		"--env LD_LIBRARY_PATH=/usr/local/nvidia/lib64",
		"infercrane_stage gpu_driver_ready",
		"docker run -d --restart=unless-stopped $gpu_run_args",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("startup script missing GPU prerequisite %q: %s", required, script)
		}
	}
	if strings.Index(script, "infercrane_stage gpu_driver_ready") > strings.Index(script, "infercrane_stage runtime_start") {
		t.Fatalf("runtime can start before GPU driver readiness: %s", script)
	}
}
