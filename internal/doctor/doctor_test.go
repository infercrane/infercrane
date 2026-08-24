package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/provision"
)

type capacityAdvisor func(context.Context, provision.AvailabilityRequest) (provision.Availability, error)

func (f capacityAdvisor) Availability(ctx context.Context, request provision.AvailabilityRequest) (provision.Availability, error) {
	return f(ctx, request)
}

func TestRunReportsRequiredAndOptionalChecks(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name == "sky" {
			return "", errors.New("missing")
		}
		return "/bin/" + name, nil
	}
	r := Run(context.Background(), config.Config{DatabaseURL: "postgres://db", RouterBinary: "router"}, Dependencies{
		LookPath: lookup, Ping: func(context.Context, string) error { return nil },
	})
	if r.Ready {
		t.Fatal("missing API key must make report not ready")
	}
	if r.Checks[3].Status != Warn {
		t.Fatalf("expected optional SkyPilot warning: %#v", r.Checks)
	}
}

func TestRunReadyWithoutSkyPilot(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name == "sky" {
			return "", errors.New("missing")
		}
		return "/bin/router", nil
	}
	r := Run(context.Background(), config.Config{APIKey: "secret", DatabaseURL: "postgres://db", RouterBinary: "router"}, Dependencies{
		LookPath: lookup, Ping: func(context.Context, string) error { return nil },
	})
	if !r.Ready || r.Err() != nil {
		t.Fatalf("optional warning should remain ready: %#v", r)
	}
}

func TestCloudCredentialCheckIsRequiredWhenRequested(t *testing.T) {
	check := CheckCloudCredentials(context.Background(), Dependencies{LookPath: func(string) (string, error) { return "/bin/sky", nil }, SkyCheck: func(context.Context) error { return errors.New("no credentials") }})
	if check.Status != Fail {
		t.Fatalf("unexpected check: %#v", check)
	}
}

func TestRunPodServerlessCheckIsRequiredWhenRequested(t *testing.T) {
	cfg := config.Config{RunPodAPIKey: "secret", RunPodServerlessTemplateID: "template"}
	check := CheckRunPodServerless(context.Background(), cfg, Dependencies{RunPodCheck: func(context.Context) error { return errors.New("mutable template") }})
	if check.Status != Fail || check.Remediation == "" {
		t.Fatalf("unexpected check: %#v", check)
	}
	check = CheckRunPodServerless(context.Background(), cfg, Dependencies{RunPodCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("unexpected check: %#v", check)
	}
}

func TestAWSBYOCCheckIsReadOnlyAndRequiredWhenRequested(t *testing.T) {
	cfg := config.Config{AWSRoleARN: "arn:aws:iam::123456789012:role/infercrane", AWSRegion: "eu-central-1"}
	check := CheckAWSBYOC(context.Background(), cfg, Dependencies{AWSCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("unexpected check: %#v", check)
	}
	check = CheckAWSBYOC(context.Background(), config.Config{}, Dependencies{})
	if check.Status != Fail || check.Remediation == "" {
		t.Fatalf("unexpected unconfigured check: %#v", check)
	}
}

func TestGCPBYOCCheckIsReadOnlyAndRequiredWhenRequested(t *testing.T) {
	cfg := config.Config{GCPProject: "acme-prod", GCPZone: "europe-west4-a"}
	check := CheckGCPBYOC(context.Background(), cfg, Dependencies{GCPCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("unexpected check: %#v", check)
	}
	check = CheckGCPBYOC(context.Background(), config.Config{}, Dependencies{})
	if check.Status != Fail || check.Remediation == "" {
		t.Fatalf("unexpected unconfigured check: %#v", check)
	}
}

func TestGCPDoctorUsesTheCompleteConfiguredProvider(t *testing.T) {
	cfg := config.Config{
		GCPProject: "acme-prod", GCPZone: "europe-west4-a", GCPSubnet: "workers",
		GCPMachineType: "g2-standard-4", GCPGPU: "nvidia-l4",
		GCPServiceAccount: "worker@acme-prod.iam.gserviceaccount.com",
		GCPVMImage:        "projects/cos-cloud/global/images/cos-immutable",
		GCPContainerImage: "registry.example/vllm@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GCPWorkerSecret:   "infercrane-worker-key", GCPBootDiskGiB: 200,
	}
	provider := gcpComputeFromConfig(cfg)
	if provider.Project != cfg.GCPProject || provider.Zone != cfg.GCPZone || provider.Subnet != cfg.GCPSubnet ||
		provider.MachineType != cfg.GCPMachineType || provider.GPUType != cfg.GCPGPU ||
		provider.ServiceAccount != cfg.GCPServiceAccount || provider.VMImage != cfg.GCPVMImage ||
		provider.ContainerImage != cfg.GCPContainerImage || provider.WorkerSecret != cfg.GCPWorkerSecret ||
		provider.BootDiskGiB != cfg.GCPBootDiskGiB {
		t.Fatalf("doctor provider does not match configured provider: %#v", provider)
	}
}

func TestKubernetesCheckIsReadOnlyAndRequiredWhenRequested(t *testing.T) {
	cfg := config.Config{KubernetesContext: "cluster", KubernetesNamespace: "infercrane-system", KubernetesWorkloadAPI: "deployment", KubernetesServiceAccount: "infercrane-runtime", KubernetesWorkerSecretName: "infercrane-worker", KubernetesWorkerSecretKey: "api-key", KubernetesImageDigest: "image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", KubernetesGPUResource: "nvidia.com/gpu", KubernetesGPUProductLabel: "nvidia.com/gpu.product"}
	check := CheckKubernetes(context.Background(), cfg, Dependencies{KubernetesCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("unexpected check: %#v", check)
	}
	check = CheckKubernetes(context.Background(), config.Config{}, Dependencies{})
	if check.Status != Fail || check.Remediation == "" {
		t.Fatalf("unexpected unconfigured check: %#v", check)
	}
}

func TestDynamoCheckIsReadOnlyAndSeparatelyConfigured(t *testing.T) {
	cfg := config.Config{
		KubernetesContext: "cluster", KubernetesNamespace: "infercrane-system", KubernetesServiceAccount: "infercrane-runtime",
		KubernetesGPUResource: "nvidia.com/gpu", KubernetesGPUProductLabel: "nvidia.com/gpu.product",
		DynamoVLLMImageDigest: "image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DynamoVLLMRuntimeVersion: "1.4.0",
	}
	check := CheckDynamo(context.Background(), cfg, Dependencies{DynamoCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("check=%#v", check)
	}
	check = CheckDynamo(context.Background(), config.Config{}, Dependencies{})
	if check.Status != Fail || !strings.Contains(check.Remediation, "digest-pinned") {
		t.Fatalf("check=%#v", check)
	}
}

func TestCapacityCheckWarnsWithoutChangingHardware(t *testing.T) {
	check := CheckCapacity(context.Background(), "provider-a", "L40S", capacityAdvisor(func(_ context.Context, request provision.AvailabilityRequest) (provision.Availability, error) {
		if request.GPU != "L40S" || request.Count != 1 {
			t.Fatalf("unexpected request: %#v", request)
		}
		return provision.Availability{State: "constrained", Message: "low stock"}, nil
	}))
	if check.Status != Warn || check.Message != "low stock" || check.Remediation == "" {
		t.Fatalf("unexpected check: %#v", check)
	}
}
