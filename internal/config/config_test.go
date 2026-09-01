package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOptimizationPricesRequiresExactSourcedEvidence(t *testing.T) {
	t.Setenv("INFERCRANE_OPTIMIZATION_PRICES_JSON", `[{"cloud":"aws","region":"eu-central-1","gpu":"L40S","replicas":1,"hourly_usd":2.1,"currency":"USD","source":"aws-price-list/2026-08-24","observed_at":"2026-08-24T10:00:00Z","valid_until":"2026-08-25T10:00:00Z"}]`)
	prices, err := envOptimizationPrices("INFERCRANE_OPTIMIZATION_PRICES_JSON")
	if err != nil || len(prices) != 1 || prices[0].HourlyUSD != 2.1 || prices[0].Replicas != 1 {
		t.Fatalf("prices=%+v err=%v", prices, err)
	}
	t.Setenv("INFERCRANE_OPTIMIZATION_PRICES_JSON", `[{"cloud":"aws","gpu":"L40S","replicas":1,"hourly_usd":2.1,"currency":"USD","source":"catalog","observed_at":"2026-08-25T10:00:00Z","valid_until":"2026-08-24T10:00:00Z"}]`)
	if _, err = envOptimizationPrices("INFERCRANE_OPTIMIZATION_PRICES_JSON"); err == nil {
		t.Fatal("reversed price evidence window was accepted")
	}
}

func TestStripePrepaidFundingRequiresCompleteFixedSandboxConfiguration(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_STRIPE_SECRET_KEY", "sk_test_fixture")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial Stripe funding configuration accepted: %v", err)
	}

	t.Setenv("INFERCRANE_STRIPE_WEBHOOK_SECRET", "whsec_fixture")
	t.Setenv("INFERCRANE_BILLING_RETURN_URL", "http://localhost:3200/settings/billing")
	t.Setenv("INFERCRANE_STRIPE_PRICE_IDS_JSON", `{"25":"price_25","50":"price_50","100":"price_100","250":"price_250","500":"price_500"}`)
	cfg, err := Load()
	if err != nil || !cfg.StripeEnabled() || cfg.StripeLivemode || cfg.StripePriceIDs[25_000_000] != "price_25" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}

	t.Setenv("INFERCRANE_STRIPE_PRICE_IDS_JSON", `{"25":"price_25","50":"price_50","100":"price_100","250":"price_250","999":"price_999"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "25, 50, 100, 250, and 500") {
		t.Fatalf("unexpected Stripe amount accepted: %v", err)
	}
}

func TestStripeModeMustMatchSecretKey(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_STRIPE_SECRET_KEY", "sk_test_fixture")
	t.Setenv("INFERCRANE_STRIPE_WEBHOOK_SECRET", "whsec_fixture")
	t.Setenv("INFERCRANE_BILLING_RETURN_URL", "https://console.infercrane.com/settings/billing")
	t.Setenv("INFERCRANE_STRIPE_PRICE_IDS_JSON", `{"25":"price_25","50":"price_50","100":"price_100","250":"price_250","500":"price_500"}`)
	t.Setenv("INFERCRANE_STRIPE_LIVEMODE", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "live-mode") {
		t.Fatalf("test key accepted for live mode: %v", err)
	}
	t.Setenv("INFERCRANE_STRIPE_LIVEMODE", "maybe")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "true or false") {
		t.Fatalf("invalid Stripe mode accepted: %v", err)
	}
}

func TestLoadRejectsInvalidInteger(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_PORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid port to fail")
	}
}

func TestRunPodContainerDiskIsBounded(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_RUNPOD_CONTAINER_DISK_GIB", "500")
	cfg, err := Load()
	if err != nil || cfg.RunPodContainerDiskGiB != 500 {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_RUNPOD_CONTAINER_DISK_GIB", "49")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "RUNPOD_CONTAINER_DISK_GIB") {
		t.Fatalf("undersized RunPod disk accepted: %v", err)
	}
}

func TestGPUPriceSyncIntervalIsBounded(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_GPU_PRICE_SYNC_SECONDS", "3600")
	cfg, err := Load()
	if err != nil || cfg.GPUPriceSyncInterval != time.Hour {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_GPU_PRICE_SYNC_SECONDS", "60")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "GPU_PRICE_SYNC") {
		t.Fatalf("unsafe price refresh interval accepted: %v", err)
	}
}

func TestRunPodNetworkVolumesRequireImmutableExactMappings(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_RUNPOD_ARTIFACT_CACHE_POLICY", "required")
	t.Setenv("INFERCRANE_RUNPOD_NETWORK_VOLUMES_JSON", `{"org/model@0123456789abcdef0123456789abcdef01234567":"volume_1234"}`)
	cfg, err := Load()
	if err != nil || cfg.RunPodNetworkVolumes["org/model@0123456789abcdef0123456789abcdef01234567"] != "volume_1234" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_RUNPOD_NETWORK_VOLUMES_JSON", `{"org/model@main":"volume_1234"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "immutable model identities") {
		t.Fatalf("mutable RunPod mapping accepted: %v", err)
	}
}

func TestRunPodHuggingFaceCredentialIsReferenceOnly(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_RUNPOD_HF_TOKEN_SECRET", "infercrane-hf-token")
	cfg, err := Load()
	if err != nil || cfg.RunPodHFTokenSecret != "infercrane-hf-token" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_RUNPOD_HF_TOKEN_SECRET", "{{unsafe}}")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "secret name") {
		t.Fatalf("unsafe secret reference accepted: %v", err)
	}
}

func TestSkyPilotExecutionBoundary(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "test-key")
	t.Setenv("INFERCRANE_SKYPILOT_API", "disabled")
	t.Setenv("RUNPOD_API_KEY", "runpod-key")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkyPilotEnabled() {
		t.Fatal("disabled SkyPilot was executable")
	}

	t.Setenv("INFERCRANE_SKYPILOT_API", "auto")
	cfg, err = Load()
	if err != nil || !cfg.SkyPilotEnabled() {
		t.Fatalf("auto SkyPilot did not follow RunPod credentials: enabled=%v err=%v", cfg.SkyPilotEnabled(), err)
	}
	providers := cfg.ConfiguredSkyPilotProviders()
	if len(providers) != 1 || providers[0].Cloud != "runpod" || strings.Join(providers[0].Runtimes, ",") != "vllm,sglang,custom-oci" {
		t.Fatalf("unexpected RunPod SkyPilot defaults: %#v", providers)
	}

	t.Setenv("INFERCRANE_SKYPILOT_API", "invalid")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "INFERCRANE_SKYPILOT_API") {
		t.Fatalf("invalid SkyPilot mode accepted: %v", err)
	}
}

func TestSkyPilotEnabledRequiresConfiguredManifestCredentials(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "test-key")
	t.Setenv("INFERCRANE_SKYPILOT_API", "enabled")
	t.Setenv("RUNPOD_API_KEY", "")
	t.Setenv("INFERCRANE_SKYPILOT_PROVIDERS_JSON", `[{"cloud":"lambda","runtimes":["vllm","sglang"],"credential_env":["LAMBDA_API_KEY"]}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at least one manifest") {
		t.Fatalf("credential-less enabled SkyPilot accepted: %v", err)
	}
	t.Setenv("LAMBDA_API_KEY", "configured-outside-the-manifest")
	cfg, err := Load()
	if err != nil || !cfg.SkyPilotEnabled() || len(cfg.ConfiguredSkyPilotProviders()) != 1 || cfg.ConfiguredSkyPilotProviders()[0].Cloud != "lambda" {
		t.Fatalf("configured generic SkyPilot manifest was not enabled: cfg=%#v err=%v", cfg, err)
	}
}

func TestSkyPilotProviderManifestIsStrictAndDoesNotContainCredentialValues(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "test-key")
	t.Setenv("INFERCRANE_SKYPILOT_PROVIDERS_JSON", `[{"cloud":"lambda","runtimes":["vllm"],"credential_env":["LAMBDA_API_KEY"],"api_key":"unsafe"}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("secret-bearing manifest accepted: %v", err)
	}
	t.Setenv("INFERCRANE_SKYPILOT_PROVIDERS_JSON", `[{"cloud":"Lambda Cloud","runtimes":["vllm"],"credential_env":["LAMBDA_API_KEY"]}]`)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "safe cloud ID") {
		t.Fatalf("unsafe cloud identity accepted: %v", err)
	}
}

func TestLoadRequiresAPIKey(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected missing API key to fail")
	}
}

func TestProductionRequiresStrongSecretAndDatabaseTLS(t *testing.T) {
	t.Setenv("INFERCRANE_ENV", "production")
	t.Setenv("INFERCRANE_API_KEY", "short")
	t.Setenv("INFERCRANE_DATABASE_URL", "postgres://db/infercrane?sslmode=disable")
	if _, err := Load(); err == nil {
		t.Fatal("expected insecure production configuration to fail")
	}
	t.Setenv("INFERCRANE_API_KEY", "01234567890123456789012345678901")
	t.Setenv("INFERCRANE_DATABASE_URL", "postgres://db/infercrane?sslmode=verify-full")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsUnsafeControlPlaneURL(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_URL", "https://user:password@control.example/api?token=secret")
	if _, err := Load(); err == nil {
		t.Fatal("expected embedded control-plane credentials and query to be rejected")
	}
}

func TestTLSConfigurationFailsClosedWhenIdentityIsPartial(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_TLS_CERT_FILE", "/cert.pem")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("partial server TLS identity accepted: %v", err)
	}
	t.Setenv("INFERCRANE_TLS_CERT_FILE", "")
	t.Setenv("INFERCRANE_TLS_CLIENT_CA_FILE", "/ca.pem")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("client CA without server TLS accepted: %v", err)
	}
	t.Setenv("INFERCRANE_TLS_CLIENT_CA_FILE", "")
	t.Setenv("INFERCRANE_CLIENT_TLS_CERT_FILE", "/client.pem")
	if _, err := LoadClient(); err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("partial client TLS identity accepted: %v", err)
	}
}

func TestHostedAuthenticationIsExplicitAndProductionPartiesRequireHTTPS(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_HOSTED_AUTH_ISSUER", "https://infercrane.clerk.accounts.dev")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial hosted auth accepted: %v", err)
	}
	t.Setenv("INFERCRANE_HOSTED_AUTH_JWT_KEY_FILE", "/run/secrets/clerk-jwt.pem")
	t.Setenv("INFERCRANE_HOSTED_AUTH_AUTHORIZED_PARTIES", "http://localhost:3200")
	config, err := Load()
	if err != nil || !config.HostedAuthEnabled() {
		t.Fatalf("development hosted auth config=%#v err=%v", config, err)
	}
	t.Setenv("INFERCRANE_ENV", "production")
	t.Setenv("INFERCRANE_API_KEY", "01234567890123456789012345678901")
	t.Setenv("INFERCRANE_DATABASE_URL", "postgres://db/infercrane?sslmode=verify-full")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "authorized party") {
		t.Fatalf("production localhost party accepted: %v", err)
	}
	t.Setenv("INFERCRANE_HOSTED_AUTH_AUTHORIZED_PARTIES", "https://app.infercrane.ai")
	if _, err = Load(); err != nil {
		t.Fatalf("production hosted auth rejected: %v", err)
	}
	t.Setenv("INFERCRANE_HOSTED_AUTH_JWT_KEY", "-----BEGIN PUBLIC KEY-----\nfixture\n-----END PUBLIC KEY-----")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("ambiguous hosted auth key source accepted: %v", err)
	}
	t.Setenv("INFERCRANE_HOSTED_AUTH_JWT_KEY_FILE", "")
	t.Setenv("INFERCRANE_HOSTED_AUTH_AUTO_PROVISION", "true")
	config, err = Load()
	if err != nil || config.HostedAuthJWTKey == "" || !config.HostedAuthEnabled() || !config.HostedAuthAutoProvision {
		t.Fatalf("secret-manager hosted auth config=%#v err=%v", config, err)
	}
}

func TestAWSBYOCConfigurationIsAllOrNothingAndImmutable(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/infercrane")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("expected partial AWS configuration failure, got %v", err)
	}
	for key, value := range map[string]string{
		"INFERCRANE_AWS_REGION": "eu-central-1", "INFERCRANE_AWS_SUBNET_ID": "subnet-private",
		"INFERCRANE_AWS_SECURITY_GROUP_IDS": "sg-runtime, sg-egress", "INFERCRANE_AWS_AMI_ID": "ami-gpu",
		"INFERCRANE_AWS_INSTANCE_TYPE": "g6e.xlarge", "INFERCRANE_AWS_GPU": "L40S",
		"INFERCRANE_AWS_INSTANCE_PROFILE_ARN": "arn:aws:iam::123456789012:instance-profile/infercrane-worker",
		"INFERCRANE_AWS_WORKER_SECRET_ARN":    "arn:aws:secretsmanager:eu-central-1:123456789012:secret:worker",
		"INFERCRANE_AWS_IMAGE_DIGEST":         "ghcr.io/infercrane/runtime:mutable",
	} {
		t.Setenv(key, value)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected mutable image failure, got %v", err)
	}
	t.Setenv("INFERCRANE_AWS_IMAGE_DIGEST", "ghcr.io/infercrane/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cfg, err := Load()
	if err != nil || !cfg.AWSEnabled() || len(cfg.AWSSecurityGroupIDs) != 2 || cfg.AWSGPUCount != 1 || cfg.AWSRootVolumeGiB != 200 || cfg.AWSImageCachePolicy != "prefer" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_AWS_GPU_COUNT", "4")
	if cfg, err = Load(); err != nil || cfg.AWSGPUCount != 4 {
		t.Fatalf("multi-GPU AWS topology rejected: cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_AWS_GPU_COUNT", "0")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "AWS_GPU_COUNT") {
		t.Fatalf("invalid AWS GPU count accepted: %v", err)
	}
	t.Setenv("INFERCRANE_AWS_GPU_COUNT", "1")
	t.Setenv("INFERCRANE_AWS_SUBNET_ID", "")
	t.Setenv("INFERCRANE_AWS_SUBNET_IDS", "subnet-private-a, subnet-private-b,subnet-private-a")
	cfg, err = Load()
	if err != nil || len(cfg.AWSSubnetIDs) != 3 || cfg.AWSSubnetIDs[0] != "subnet-private-a" || cfg.AWSSubnetIDs[1] != "subnet-private-b" {
		t.Fatalf("multi-subnet cfg=%#v err=%v", cfg.AWSSubnetIDs, err)
	}
	t.Setenv("INFERCRANE_AWS_IMAGE_CACHE_POLICY", "fastest")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "IMAGE_CACHE_POLICY") {
		t.Fatalf("expected invalid cache policy failure, got %v", err)
	}
	t.Setenv("INFERCRANE_AWS_IMAGE_CACHE_POLICY", "required")
	t.Setenv("INFERCRANE_AWS_ROOT_VOLUME_GIB", "30")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "ROOT_VOLUME_GIB") {
		t.Fatalf("expected unsafe AWS root volume failure, got %v", err)
	}
}

func TestAWSArtifactSnapshotConfigurationIsExactAndBounded(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	for key, value := range map[string]string{
		"INFERCRANE_AWS_ROLE_ARN":                                  "arn:aws:iam::123456789012:role/infercrane",
		"INFERCRANE_AWS_REGION":                                    "eu-central-1",
		"INFERCRANE_AWS_SUBNET_ID":                                 "subnet-private",
		"INFERCRANE_AWS_SECURITY_GROUP_IDS":                        "sg-runtime",
		"INFERCRANE_AWS_AMI_ID":                                    "ami-gpu",
		"INFERCRANE_AWS_INSTANCE_TYPE":                             "g6e.xlarge",
		"INFERCRANE_AWS_GPU":                                       "L40S",
		"INFERCRANE_AWS_INSTANCE_PROFILE_ARN":                      "arn:aws:iam::123456789012:instance-profile/infercrane-worker",
		"INFERCRANE_AWS_WORKER_SECRET_ARN":                         "arn:aws:secretsmanager:eu-central-1:123456789012:secret:worker",
		"INFERCRANE_AWS_IMAGE_DIGEST":                              "ghcr.io/infercrane/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"INFERCRANE_AWS_ARTIFACT_CACHE_POLICY":                     "required",
		"INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON":                   `{"mistralai/Mistral-7B-Instruct-v0.3@0123456789abcdef0123456789abcdef01234567":"snap-0123456789abcdef0"}`,
		"INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS": "200",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil || cfg.AWSArtifactCachePolicy != "required" || cfg.AWSArtifactSnapshots["mistralai/Mistral-7B-Instruct-v0.3@0123456789abcdef0123456789abcdef01234567"] != "snap-0123456789abcdef0" || cfg.AWSArtifactVolumeInitializationRate != 200 {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}

	t.Setenv("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON", `{"mutable-model":"snap-0123456789abcdef0"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "model identities") {
		t.Fatalf("mutable identity accepted: %v", err)
	}
	t.Setenv("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON", `{"model@0123456789abcdef0123456789abcdef01234567":"snapshot-latest"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "snapshot IDs") {
		t.Fatalf("invalid snapshot accepted: %v", err)
	}
	t.Setenv("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON", `{"model@0123456789abcdef0123456789abcdef01234567":"snap-0123456789abcdef0"} trailing`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "exactly one JSON object") {
		t.Fatalf("trailing JSON accepted: %v", err)
	}
	t.Setenv("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON", `{"model@0123456789abcdef0123456789abcdef01234567":"snap-0123456789abcdef0"}`)
	t.Setenv("INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS", "99")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "INITIALIZATION_RATE") {
		t.Fatalf("unsafe initialization rate accepted: %v", err)
	}
	t.Setenv("INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS", "0")
	t.Setenv("INFERCRANE_AWS_ARTIFACT_CACHE_POLICY", "disabled")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "cannot be set") {
		t.Fatalf("disabled policy accepted snapshot mapping: %v", err)
	}
}

func TestKubernetesConfigurationIsExplicitCompleteAndImmutable(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_KUBERNETES_CONTEXT", "production")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("expected partial Kubernetes configuration failure, got %v", err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_IMAGE_DIGEST", "ghcr.io/infercrane/runtime:latest")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected mutable Kubernetes image failure, got %v", err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_IMAGE_DIGEST", "ghcr.io/infercrane/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cfg, err := Load()
	if err != nil || !cfg.KubernetesEnabled() || cfg.KubernetesNamespace != "infercrane-system" || cfg.KubernetesWorkloadAPI != "deployment" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_WORKLOAD_API", "operator")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "deployment or kserve") {
		t.Fatalf("expected bounded workload API failure, got %v", err)
	}
}

func TestKubernetesArtifactPVCConfigurationIsExactAndBounded(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_KUBERNETES_CONTEXT", "production")
	t.Setenv("INFERCRANE_KUBERNETES_IMAGE_DIGEST", "ghcr.io/infercrane/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	identity := "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567"
	t.Setenv("INFERCRANE_KUBERNETES_ARTIFACT_CACHE_POLICY", "required")
	t.Setenv("INFERCRANE_KUBERNETES_ARTIFACT_PVCS_JSON", `{"`+identity+`":"qwen3-immutable-cache"}`)
	cfg, err := Load()
	if err != nil || cfg.KubernetesArtifactCachePolicy != "required" || cfg.KubernetesArtifactPVCs[identity] != "qwen3-immutable-cache" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_ARTIFACT_PVCS_JSON", `{"Qwen/Qwen3-8B":"qwen3-cache"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "immutable model identities") {
		t.Fatalf("mutable model cache identity accepted: %v", err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_ARTIFACT_PVCS_JSON", `{"`+identity+`":"Bad_Claim"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "claim names") {
		t.Fatalf("invalid PVC name accepted: %v", err)
	}
	t.Setenv("INFERCRANE_KUBERNETES_ARTIFACT_PVCS_JSON", `{"`+identity+`":"qwen3-cache"}`)
	t.Setenv("INFERCRANE_KUBERNETES_ARTIFACT_CACHE_POLICY", "disabled")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "cannot configure") {
		t.Fatalf("disabled cache policy accepted PVC mapping: %v", err)
	}
}

func TestDynamoConfigurationIsOptionalCompleteAndImmutable(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST", "nvcr.io/nvidia/ai-dynamo/vllm-runtime:latest")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "Kubernetes") {
		t.Fatalf("expected missing Kubernetes boundary, got %v", err)
	}
	for key, value := range map[string]string{
		"INFERCRANE_KUBERNETES_CONTEXT":          "production",
		"INFERCRANE_KUBERNETES_IMAGE_DIGEST":     "ghcr.io/infercrane/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST":    "nvcr.io/nvidia/ai-dynamo/vllm-runtime@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION": "1.4.0",
		"INFERCRANE_DYNAMO_MODEL_SECRET_NAME":    "huggingface-models",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil || !cfg.KubernetesEnabled() || !cfg.DynamoEnabled() || cfg.DynamoModelSecretName != "huggingface-models" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_DYNAMO_MODEL_SECRET_NAME", "Bad_Secret")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "DNS label") {
		t.Fatalf("invalid secret name accepted: %v", err)
	}
	t.Setenv("INFERCRANE_DYNAMO_MODEL_SECRET_NAME", "huggingface-models")
	t.Setenv("INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION", "latest")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "MAJOR.MINOR.PATCH") {
		t.Fatalf("unversioned Dynamo runtime accepted: %v", err)
	}
}

func TestGCPBYOCConfigurationIsAllOrNothingAndImmutable(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_GCP_PROJECT", "acme-prod")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("expected partial GCP configuration failure, got %v", err)
	}
	for key, value := range map[string]string{
		"INFERCRANE_GCP_ZONE": "europe-west4-a", "INFERCRANE_GCP_SUBNET": "private-runtime",
		"INFERCRANE_GCP_MACHINE_TYPE": "g2-standard-4", "INFERCRANE_GCP_GPU": "nvidia-l4",
		"INFERCRANE_GCP_SERVICE_ACCOUNT": "runtime@acme-prod.iam.gserviceaccount.com",
		"INFERCRANE_GCP_VM_IMAGE":        "projects/cos-cloud/global/images/cos-stable-20260801",
		"INFERCRANE_GCP_WORKER_SECRET":   "infercrane-worker-key",
		"INFERCRANE_GCP_CONTAINER_IMAGE": "europe-docker.pkg.dev/acme/runtime:v1",
	} {
		t.Setenv(key, value)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected mutable GCP image failure, got %v", err)
	}
	t.Setenv("INFERCRANE_GCP_CONTAINER_IMAGE", "europe-docker.pkg.dev/acme/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	cfg, err := Load()
	if err != nil || !cfg.GCPEnabled() || cfg.GCPZone != "europe-west4-a" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	if cfg.GCPBootDiskGiB != 200 {
		t.Fatalf("unexpected safe GCP boot disk default: %d", cfg.GCPBootDiskGiB)
	}
	t.Setenv("INFERCRANE_GCP_BOOT_DISK_GIB", "10")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "BOOT_DISK_GIB") {
		t.Fatalf("undersized GCP boot disk accepted: %v", err)
	}
	t.Setenv("INFERCRANE_GCP_BOOT_DISK_GIB", "200")
	t.Setenv("INFERCRANE_GCP_VM_IMAGE", "projects/cos-cloud/global/images/family/cos-stable")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "image family") {
		t.Fatalf("expected mutable VM image family failure, got %v", err)
	}
}

func TestGCPArtifactDiskConfigurationIsExactAndBounded(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	for key, value := range map[string]string{
		"INFERCRANE_GCP_PROJECT":               "acme-prod",
		"INFERCRANE_GCP_ZONE":                  "europe-west4-a",
		"INFERCRANE_GCP_SUBNET":                "private-runtime",
		"INFERCRANE_GCP_MACHINE_TYPE":          "g2-standard-4",
		"INFERCRANE_GCP_GPU":                   "nvidia-l4",
		"INFERCRANE_GCP_SERVICE_ACCOUNT":       "runtime@acme-prod.iam.gserviceaccount.com",
		"INFERCRANE_GCP_VM_IMAGE":              "projects/cos-cloud/global/images/cos-stable-20260801",
		"INFERCRANE_GCP_WORKER_SECRET":         "infercrane-worker-key",
		"INFERCRANE_GCP_CONTAINER_IMAGE":       "europe-docker.pkg.dev/acme/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"INFERCRANE_GCP_ARTIFACT_CACHE_POLICY": "required",
		"INFERCRANE_GCP_ARTIFACT_DISKS_JSON":   `{"Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567":"qwen3-8b-cache"}`,
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil || cfg.GCPArtifactDisks["Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567"] != "qwen3-8b-cache" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	t.Setenv("INFERCRANE_GCP_ARTIFACT_DISKS_JSON", `{"Qwen/Qwen3-8B":"qwen-cache"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "immutable model identities") {
		t.Fatalf("mutable model identity accepted: %v", err)
	}
	t.Setenv("INFERCRANE_GCP_ARTIFACT_DISKS_JSON", `{"Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567":"Bad_Disk"}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "disk names") {
		t.Fatalf("invalid disk name accepted: %v", err)
	}
	t.Setenv("INFERCRANE_GCP_ARTIFACT_DISKS_JSON", `{}`)
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "needs INFERCRANE_GCP_ARTIFACT_DISKS_JSON") {
		t.Fatalf("required empty mapping accepted: %v", err)
	}
}

func TestInitializeClientWritesPrivateConfigAndLoadClientUsesIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_URL", "")
	path, err := InitializeClient("https://control.example", "issued-control-plane-credential")
	if err != nil || path != filepath.Join(root, "infercrane", "config.json") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%v", info.Mode().Perm())
	}
	config, err := LoadClient()
	if err != nil || config.ControlURL != "https://control.example" || config.APIKey != "issued-control-plane-credential" {
		t.Fatalf("config=%#v err=%v", config, err)
	}
}

func TestInitializeClientRejectsUnregisteredLocalCredentialGeneration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := InitializeClient("https://control.example", ""); err == nil || !strings.Contains(err.Error(), "existing control-plane credential") {
		t.Fatalf("err=%v", err)
	}
}

func TestClientContextsCanBeSelectedWithoutExposingCredentials(t *testing.T) {
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_URL", "")
	if _, err := InitializeClientContext("staging", "https://staging.example", "staging-secret", true); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeClientContext("production", "https://production.example", "production-secret", false); err != nil {
		t.Fatal(err)
	}
	settings, err := ClientConfiguration()
	if err != nil || settings.Current != "staging" || len(settings.Contexts) != 2 {
		t.Fatalf("settings=%#v err=%v", settings, err)
	}
	if err = SelectClientContext("production"); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadClient()
	if err != nil || loaded.ControlURL != "https://production.example" || loaded.APIKey != "production-secret" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
}

func TestLoadClientReadsLegacySingleContextConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("INFERCRANE_CONFIG", path)
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_URL", "")
	if err := os.WriteFile(path, []byte(`{"url":"https://legacy.example","api_key":"legacy-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadClient()
	if err != nil || loaded.ControlURL != "https://legacy.example" || loaded.APIKey != "legacy-secret" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
}
