package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsInvalidInteger(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_PORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid port to fail")
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
	if err != nil || !cfg.AWSEnabled() || len(cfg.AWSSecurityGroupIDs) != 2 || cfg.AWSRootVolumeGiB != 100 || cfg.AWSImageCachePolicy != "prefer" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
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
	t.Setenv("INFERCRANE_GCP_VM_IMAGE", "projects/cos-cloud/global/images/family/cos-stable")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "image family") {
		t.Fatalf("expected mutable VM image family failure, got %v", err)
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
