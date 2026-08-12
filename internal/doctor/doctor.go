// Package doctor performs read-only checks of an InferCrane environment.
package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/provision"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	Ready        bool         `json:"ready"`
	Checks       []Check      `json:"checks"`
	Capabilities []Capability `json:"capabilities,omitempty"`
}

// Capability describes a normalized provider/runtime behavior. State is one
// of supported, unsupported, or unknown. Unknown is intentional: adapters must
// not claim provider optimizations that the provider API did not expose.
type Capability struct {
	Adapter string `json:"adapter"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
}

type Dependencies struct {
	LookPath        func(string) (string, error)
	Ping            func(context.Context, string) error
	SkyCheck        func(context.Context) error
	RunPodCheck     func(context.Context) error
	AWSCheck        func(context.Context) error
	GCPCheck        func(context.Context) error
	KubernetesCheck func(context.Context) error
}

func CheckGCPBYOC(ctx context.Context, cfg config.Config, deps Dependencies) Check {
	if !cfg.GCPEnabled() {
		return Check{"GCP BYOC", Fail, "GCP BYOC is not configured", "Configure the complete INFERCRANE_GCP_* set on the control plane."}
	}
	check := deps.GCPCheck
	if check == nil {
		check = (provision.GCPCompute{Project: cfg.GCPProject, Zone: cfg.GCPZone}).Check
	}
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return Check{"GCP BYOC", Fail, "GCP identity or Compute API probe failed", "Verify Application Default Credentials, project access, zone, Compute API enablement, and the gcloud installation."}
	}
	return Check{"GCP BYOC", Pass, "GCP identity and Compute API probe succeeded", ""}
}

func CheckKubernetes(ctx context.Context, cfg config.Config, deps Dependencies) Check {
	if !cfg.KubernetesEnabled() {
		return Check{"Kubernetes", Fail, "Kubernetes provider is not configured", "Set INFERCRANE_KUBERNETES_CONTEXT and the complete INFERCRANE_KUBERNETES_* configuration on the control plane."}
	}
	check := deps.KubernetesCheck
	if check == nil {
		check = (provision.Kubernetes{Context: cfg.KubernetesContext, Namespace: cfg.KubernetesNamespace, WorkloadAPI: cfg.KubernetesWorkloadAPI, ServiceAccount: cfg.KubernetesServiceAccount, WorkerSecretName: cfg.KubernetesWorkerSecretName, WorkerSecretKey: cfg.KubernetesWorkerSecretKey, ImageDigest: cfg.KubernetesImageDigest, GPUResource: cfg.KubernetesGPUResource, GPUProductLabel: cfg.KubernetesGPUProductLabel}).Check
	}
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return Check{"Kubernetes", Fail, "Kubernetes API, CRD, or namespaced RBAC probe failed", "Verify kubectl access, namespace RBAC, configured workload API, and the optional KServe CRD."}
	}
	return Check{"Kubernetes", Pass, "Kubernetes API and namespaced provider permissions are ready", ""}
}

func CheckAWSBYOC(ctx context.Context, cfg config.Config, deps Dependencies) Check {
	if !cfg.AWSEnabled() {
		return Check{"AWS BYOC", Fail, "AWS BYOC is not configured", "Configure the complete INFERCRANE_AWS_* set on the control plane."}
	}
	check := deps.AWSCheck
	if check == nil {
		check = (provision.AWSEC2{RoleARN: cfg.AWSRoleARN, ExternalID: cfg.AWSExternalID, Region: cfg.AWSRegion}).Check
	}
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return Check{"AWS BYOC", Fail, "AWS role assumption or identity probe failed", "Verify the control-plane source identity, trust policy, external ID, region, and AWS CLI v2 installation."}
	}
	return Check{"AWS BYOC", Pass, "AWS role assumption and identity probe succeeded", ""}
}

type CapacityAdvisor interface {
	Availability(context.Context, provision.AvailabilityRequest) (provision.Availability, error)
}

// CheckCapacity is provider-neutral and advisory. Capacity can change between
// the check and placement, so constrained or unavailable stock must inform the
// operator without making an otherwise valid control plane unhealthy.
func CheckCapacity(ctx context.Context, adapter, gpu string, advisor CapacityAdvisor) Check {
	if advisor == nil {
		return Check{"Capacity", Warn, "Capacity availability is not reported by adapter " + adapter, "Inspect provider availability before creating paid resources."}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	availability, err := advisor.Availability(checkCtx, provision.AvailabilityRequest{GPU: gpu, Count: 1})
	if err != nil {
		return Check{"Capacity", Warn, "Capacity availability check failed", "Retry doctor or inspect the provider inventory before deployment."}
	}
	status := Pass
	remediation := ""
	if availability.State != "available" {
		status = Warn
		remediation = "Wait for capacity, broaden placement, or explicitly choose different hardware; InferCrane will not substitute hardware automatically."
	}
	return Check{"Capacity", status, availability.Message, remediation}
}

func CheckRunPodServerless(ctx context.Context, cfg config.Config, deps Dependencies) Check {
	if cfg.RunPodAPIKey == "" {
		return Check{"RunPod Serverless", Fail, "RUNPOD_API_KEY is not set", "Create a scoped RunPod API key and set RUNPOD_API_KEY on the control plane."}
	}
	if cfg.RunPodServerlessTemplateID == "" {
		return Check{"RunPod Serverless", Fail, "Serverless vLLM template is not configured", "Set INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID to a RunPod Serverless vLLM template pinned to an immutable model revision."}
	}
	check := deps.RunPodCheck
	if check == nil {
		check = (provision.RunPodServerless{APIKey: cfg.RunPodAPIKey, BaseURL: cfg.RunPodRESTURL, TemplateID: cfg.RunPodServerlessTemplateID}).Check
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return Check{"RunPod Serverless", Fail, "RunPod Serverless configuration is invalid: " + err.Error(), "Verify the API key and template; pin MODEL_NAME and MODEL_REVISION and set RAW_OPENAI_OUTPUT=1."}
	}
	return Check{"RunPod Serverless", Pass, "RunPod API credentials and immutable vLLM template are valid", ""}
}

func CheckCloudCredentials(ctx context.Context, deps Dependencies) Check {
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if _, err := deps.LookPath("sky"); err != nil {
		return Check{"Cloud credentials", Fail, "SkyPilot CLI is not installed", "Install and configure SkyPilot, then run `sky check`."}
	}
	check := deps.SkyCheck
	if check == nil {
		check = func(ctx context.Context) error {
			command := exec.CommandContext(ctx, "sky", "check")
			output, err := command.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
			}
			return nil
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return Check{"Cloud credentials", Fail, "SkyPilot credential check failed: " + err.Error(), "Configure at least one supported cloud and confirm `sky check` succeeds."}
	}
	return Check{"Cloud credentials", Pass, "SkyPilot reports usable cloud credentials", ""}
}

func (r *Report) Add(check Check) {
	r.Checks = append(r.Checks, check)
	if check.Status == Fail {
		r.Ready = false
	}
}

func Run(ctx context.Context, cfg config.Config, deps Dependencies) Report {
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if deps.Ping == nil {
		deps.Ping = pingDatabase
	}
	r := Report{Ready: true}
	add := func(c Check) {
		r.Checks = append(r.Checks, c)
		if c.Status == Fail {
			r.Ready = false
		}
	}
	if cfg.APIKey == "" {
		add(Check{"API authentication", Fail, "INFERCRANE_API_KEY is not set", "Set a strong, secret API key in the runtime environment."})
	} else {
		add(Check{"API authentication", Pass, "API key is configured", ""})
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := deps.Ping(pingCtx, cfg.DatabaseURL); err != nil {
		add(Check{"PostgreSQL", Fail, "Database is unreachable: " + err.Error(), "Verify INFERCRANE_DATABASE_URL, networking, TLS, and PostgreSQL readiness."})
	} else {
		add(Check{"PostgreSQL", Pass, "Database connection succeeded", ""})
	}
	if path, err := deps.LookPath(cfg.RouterBinary); err != nil {
		add(Check{"vLLM Router", Fail, fmt.Sprintf("%q was not found on PATH", cfg.RouterBinary), "Install vllm-router or set INFERCRANE_ROUTER_BINARY to its executable path."})
	} else {
		add(Check{"vLLM Router", Pass, "Router binary found at " + path, ""})
	}
	if path, err := deps.LookPath("sky"); err != nil {
		add(Check{"SkyPilot", Warn, "SkyPilot CLI is not installed; existing-target deployments still work", "Install SkyPilot before using --cloud/--gpu provisioning."})
	} else {
		add(Check{"SkyPilot", Pass, "SkyPilot CLI found at " + path, ""})
	}
	if path, err := deps.LookPath(cfg.AIPerfBinary); err != nil {
		add(Check{"AIPerf", Fail, fmt.Sprintf("%q was not found on PATH", cfg.AIPerfBinary), "Install AIPerf with `pipx install aiperf` or set INFERCRANE_AIPERF_BINARY."})
	} else {
		add(Check{"AIPerf", Pass, "AIPerf binary found at " + path, ""})
	}
	return r
}

func pingDatabase(ctx context.Context, databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.PingContext(ctx)
}

func (r Report) Err() error {
	if r.Ready {
		return nil
	}
	return errors.New("doctor found required checks that need attention")
}
