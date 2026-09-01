package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type Config struct {
	DatabaseURL, ControlURL, Host, APIKey, RouterBinary, AIPerfBinary, PassportSigningKeyFile, InstanceID, Environment string
	TLSCertFile, TLSKeyFile, TLSClientCAFile, ClientTLSCertFile, ClientTLSKeyFile, ClientTLSCAFile                     string
	AsyncEncryptionKey, AsyncEncryptionKeyReference                                                                    string
	HostedAuthIssuer, HostedAuthAudience, HostedAuthJWTKey, HostedAuthJWTKeyFile                                       string
	HostedAuthAuthorizedParties                                                                                        []string
	HostedAuthAutoProvision                                                                                            bool
	StripeSecretKey, StripeWebhookSecret, StripeBillingReturnURL                                                       string
	ModelAPICatalogFile                                                                                                string
	StripePriceIDs                                                                                                     map[int64]string
	StripeLivemode                                                                                                     bool
	RunPodAPIKey, RunPodServerlessTemplateID, RunPodRESTURL, RunPodArtifactCachePolicy, RunPodHFTokenSecret            string
	SkyPilotAPI                                                                                                        string
	SkyPilotProviders                                                                                                  []SkyPilotProvider
	RunPodContainerDiskGiB                                                                                             int
	RunPodNetworkVolumes                                                                                               map[string]string
	AWSRoleARN, AWSExternalID, AWSRegion, AWSSubnetID, AWSAMIID, AWSInstanceType, AWSGPU                               string
	AWSInstanceProfileARN, AWSWorkerSecretARN, AWSImageDigest                                                          string
	AWSImageCachePolicy, AWSArtifactCachePolicy                                                                        string
	AWSSubnetIDs, AWSSecurityGroupIDs                                                                                  []string
	AWSArtifactSnapshots                                                                                               map[string]string
	AWSGPUCount, AWSRootVolumeGiB, AWSGP3IOPS, AWSGP3Throughput, AWSArtifactVolumeInitializationRate                   int
	GCPProject, GCPZone, GCPSubnet, GCPMachineType, GCPGPU, GCPServiceAccount                                          string
	GCPVMImage, GCPContainerImage, GCPWorkerSecret, GCPArtifactCachePolicy                                             string
	GCPArtifactDisks                                                                                                   map[string]string
	GCPBootDiskGiB                                                                                                     int
	KubernetesContext, KubernetesNamespace, KubernetesWorkloadAPI, KubernetesServiceAccount                            string
	KubernetesWorkerSecretName, KubernetesWorkerSecretKey, KubernetesImageDigest                                       string
	KubernetesGPUResource, KubernetesGPUProductLabel, KubernetesImageCachePolicy, KubernetesArtifactCachePolicy        string
	KubernetesArtifactPVCs                                                                                             map[string]string
	DynamoVLLMImageDigest, DynamoVLLMRuntimeVersion, DynamoSGLangImageDigest, DynamoSGLangRuntimeVersion               string
	DynamoModelSecretName                                                                                              string
	Port, RouterStartPort, DatabaseMaxOpen, DatabaseMaxIdle                                                            int
	HealthInterval, UpstreamTimeout, ShutdownTimeout, RequestRetention, GPUPriceSyncInterval                           time.Duration
	OptimizationPrices                                                                                                 []OptimizationPrice
}

type OptimizationPrice struct {
	Cloud, Region, GPU, Currency, Source string
	GPUCount, Replicas                   int
	HourlyUSD                            float64
	ObservedAt, ValidUntil               time.Time
}

// SkyPilotProvider is a declarative execution manifest. It contains only
// provider identity, supported portable runtimes, and names of environment
// variables that prove credentials are configured. Credential values never
// enter configuration snapshots or API responses.
type SkyPilotProvider struct {
	Cloud         string   `json:"cloud"`
	Label         string   `json:"label,omitempty"`
	Runtimes      []string `json:"runtimes"`
	CredentialEnv []string `json:"credential_env"`
}

type ClientContext struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key,omitempty"`
}

type ClientSettings struct {
	Current  string                   `json:"current_context"`
	Contexts map[string]ClientContext `json:"contexts"`
}

type clientFile struct {
	// URL and APIKey retain read compatibility with the pre-context format.
	URL      string                   `json:"url,omitempty"`
	APIKey   string                   `json:"api_key,omitempty"`
	Current  string                   `json:"current_context,omitempty"`
	Contexts map[string]ClientContext `json:"contexts,omitempty"`
}

func Load() (Config, error) {
	return load(true)
}

func LoadClient() (Config, error) {
	return LoadClientContext(os.Getenv("INFERCRANE_CONTEXT"))
}

func LoadClientContext(contextName string) (Config, error) {
	stored, err := readClientFile()
	if err != nil {
		return Config{}, err
	}
	selected := ClientContext{URL: stored.URL, APIKey: stored.APIKey}
	if contextName == "" {
		contextName = stored.Current
	}
	if contextName != "" {
		var ok bool
		selected, ok = stored.Contexts[contextName]
		if !ok {
			return Config{}, fmt.Errorf("InferCrane context %q was not found", contextName)
		}
	}
	controlURL := selected.URL
	if value := os.Getenv("INFERCRANE_URL"); value != "" {
		controlURL = value
	}
	if controlURL == "" {
		controlURL = "http://127.0.0.1:8080"
	}
	apiKey := selected.APIKey
	if value := os.Getenv("INFERCRANE_API_KEY"); value != "" {
		apiKey = value
	}
	if apiKey == "" {
		return Config{}, fmt.Errorf("INFERCRANE_API_KEY is required; run infercrane init or set the environment variable")
	}
	parsed, parseErr := url.Parse(controlURL)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Config{}, fmt.Errorf("InferCrane URL must be absolute HTTP(S) without credentials, query, or fragment")
	}
	clientCert, clientKey := env("INFERCRANE_CLIENT_TLS_CERT_FILE", ""), env("INFERCRANE_CLIENT_TLS_KEY_FILE", "")
	if (clientCert == "") != (clientKey == "") {
		return Config{}, errors.New("INFERCRANE_CLIENT_TLS_CERT_FILE and INFERCRANE_CLIENT_TLS_KEY_FILE must be configured together")
	}
	return Config{ControlURL: controlURL, APIKey: apiKey, ClientTLSCertFile: clientCert, ClientTLSKeyFile: clientKey, ClientTLSCAFile: env("INFERCRANE_CLIENT_TLS_CA_FILE", "")}, nil
}

func InitializeClient(controlURL, apiKey string) (string, error) {
	return InitializeClientContext("default", controlURL, apiKey, true)
}

func InitializeClientContext(name, controlURL, apiKey string, selectContext bool) (string, error) {
	if name == "" {
		name = "default"
	}
	if controlURL == "" {
		controlURL = "http://127.0.0.1:8080"
	}
	parsed, err := url.Parse(controlURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("InferCrane URL must be absolute HTTP(S) without credentials, query, or fragment")
	}
	if apiKey == "" {
		return "", fmt.Errorf("an existing control-plane credential is required; pass --api-key or set INFERCRANE_API_KEY")
	}
	path, err := clientConfigPath()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	stored, readErr := readClientFile()
	if readErr != nil {
		return "", readErr
	}
	contexts := stored.Contexts
	if contexts == nil {
		contexts = map[string]ClientContext{}
		if stored.URL != "" && stored.APIKey != "" {
			contexts["default"] = ClientContext{URL: stored.URL, APIKey: stored.APIKey}
		}
	}
	contexts[name] = ClientContext{URL: controlURL, APIKey: apiKey}
	current := stored.Current
	if current == "" || selectContext {
		current = name
	}
	encoded, _ := json.MarshalIndent(clientFile{Current: current, Contexts: contexts}, "", "  ")
	encoded = append(encoded, '\n')
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		return "", fmt.Errorf("write client config: %w", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("secure client config: %w", err)
	}
	return path, nil
}

func ClientConfiguration() (ClientSettings, error) {
	stored, err := readClientFile()
	if err != nil {
		return ClientSettings{}, err
	}
	contexts := stored.Contexts
	current := stored.Current
	if contexts == nil {
		contexts = map[string]ClientContext{}
		if stored.URL != "" {
			contexts["default"] = ClientContext{URL: stored.URL, APIKey: stored.APIKey}
			current = "default"
		}
	}
	return ClientSettings{Current: current, Contexts: contexts}, nil
}

func SelectClientContext(name string) error {
	settings, err := ClientConfiguration()
	if err != nil {
		return err
	}
	if _, ok := settings.Contexts[name]; !ok {
		return fmt.Errorf("InferCrane context %q was not found", name)
	}
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(clientFile{Current: name, Contexts: settings.Contexts}, "", "  ")
	encoded = append(encoded, '\n')
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write client config: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func clientConfigPath() (string, error) {
	if path := os.Getenv("INFERCRANE_CONFIG"); path != "" {
		return path, nil
	}
	if root := os.Getenv("XDG_CONFIG_HOME"); root != "" {
		return filepath.Join(root, "infercrane", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "infercrane", "config.json"), nil
}

func readClientFile() (clientFile, error) {
	path, err := clientConfigPath()
	if err != nil {
		return clientFile{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return clientFile{}, nil
	}
	if err != nil {
		return clientFile{}, fmt.Errorf("read client config: %w", err)
	}
	var stored clientFile
	if err = json.Unmarshal(data, &stored); err != nil {
		return clientFile{}, fmt.Errorf("parse client config: %w", err)
	}
	return stored, nil
}

func load(requireAPIKey bool) (Config, error) {
	hostname, _ := os.Hostname()
	port, err := envInt("INFERCRANE_PORT", 8080)
	if err != nil {
		return Config{}, err
	}
	routerPort, err := envInt("INFERCRANE_ROUTER_START_PORT", 18080)
	if err != nil {
		return Config{}, err
	}
	maxOpen, err := envInt("INFERCRANE_DATABASE_MAX_OPEN", 32)
	if err != nil {
		return Config{}, err
	}
	maxIdle, err := envInt("INFERCRANE_DATABASE_MAX_IDLE", 8)
	if err != nil {
		return Config{}, err
	}
	healthSeconds, err := envInt("INFERCRANE_HEALTH_INTERVAL_SECONDS", 10)
	if err != nil {
		return Config{}, err
	}
	upstreamSeconds, err := envInt("INFERCRANE_UPSTREAM_TIMEOUT_SECONDS", 300)
	if err != nil {
		return Config{}, err
	}
	shutdownSeconds, err := envInt("INFERCRANE_SHUTDOWN_TIMEOUT_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	retentionHours, err := envInt("INFERCRANE_REQUEST_RETENTION_HOURS", 24)
	if err != nil {
		return Config{}, err
	}
	gpuPriceSyncSeconds, err := envInt("INFERCRANE_GPU_PRICE_SYNC_SECONDS", 0)
	if err != nil {
		return Config{}, err
	}
	runPodContainerDiskGiB, err := envInt("INFERCRANE_RUNPOD_CONTAINER_DISK_GIB", 100)
	if err != nil {
		return Config{}, err
	}
	runPodNetworkVolumes, err := envStringMap("INFERCRANE_RUNPOD_NETWORK_VOLUMES_JSON")
	if err != nil {
		return Config{}, err
	}
	awsRootVolumeGiB, err := envInt("INFERCRANE_AWS_ROOT_VOLUME_GIB", 200)
	if err != nil {
		return Config{}, err
	}
	awsGPUCount, err := envInt("INFERCRANE_AWS_GPU_COUNT", 1)
	if err != nil {
		return Config{}, err
	}
	awsGP3IOPS, err := envInt("INFERCRANE_AWS_GP3_IOPS", 3000)
	if err != nil {
		return Config{}, err
	}
	awsGP3Throughput, err := envInt("INFERCRANE_AWS_GP3_THROUGHPUT_MIBPS", 125)
	if err != nil {
		return Config{}, err
	}
	awsArtifactVolumeInitializationRate, err := envInt("INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS", 0)
	if err != nil {
		return Config{}, err
	}
	gcpBootDiskGiB, err := envInt("INFERCRANE_GCP_BOOT_DISK_GIB", 200)
	if err != nil {
		return Config{}, err
	}
	awsArtifactSnapshots, err := envStringMap("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON")
	if err != nil {
		return Config{}, err
	}
	gcpArtifactDisks, err := envStringMap("INFERCRANE_GCP_ARTIFACT_DISKS_JSON")
	if err != nil {
		return Config{}, err
	}
	kubernetesArtifactPVCs, err := envStringMap("INFERCRANE_KUBERNETES_ARTIFACT_PVCS_JSON")
	if err != nil {
		return Config{}, err
	}
	optimizationPrices, err := envOptimizationPrices("INFERCRANE_OPTIMIZATION_PRICES_JSON")
	if err != nil {
		return Config{}, err
	}
	skyPilotProviders, err := envSkyPilotProviders("INFERCRANE_SKYPILOT_PROVIDERS_JSON")
	if err != nil {
		return Config{}, err
	}
	stripePriceIDs, err := envStripePriceIDs("INFERCRANE_STRIPE_PRICE_IDS_JSON")
	if err != nil {
		return Config{}, err
	}
	stripeLivemode, err := envBool("INFERCRANE_STRIPE_LIVEMODE", false)
	if err != nil {
		return Config{}, err
	}
	hostedAuthAutoProvision, err := envBool("INFERCRANE_HOSTED_AUTH_AUTO_PROVISION", false)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		DatabaseURL:                         env("INFERCRANE_DATABASE_URL", "postgres://infercrane:infercrane@127.0.0.1:5432/infercrane?sslmode=disable"),
		ControlURL:                          env("INFERCRANE_URL", "http://127.0.0.1:8080"),
		Host:                                env("INFERCRANE_HOST", "127.0.0.1"),
		APIKey:                              env("INFERCRANE_API_KEY", ""),
		RouterBinary:                        env("INFERCRANE_ROUTER_BINARY", "vllm-router"),
		AIPerfBinary:                        env("INFERCRANE_AIPERF_BINARY", "aiperf"),
		PassportSigningKeyFile:              env("INFERCRANE_PASSPORT_SIGNING_KEY_FILE", ""),
		InstanceID:                          env("INFERCRANE_INSTANCE_ID", hostname),
		Environment:                         env("INFERCRANE_ENV", "development"),
		TLSCertFile:                         env("INFERCRANE_TLS_CERT_FILE", ""),
		TLSKeyFile:                          env("INFERCRANE_TLS_KEY_FILE", ""),
		TLSClientCAFile:                     env("INFERCRANE_TLS_CLIENT_CA_FILE", ""),
		AsyncEncryptionKey:                  env("INFERCRANE_ASYNC_ENCRYPTION_KEY", ""),
		AsyncEncryptionKeyReference:         env("INFERCRANE_ASYNC_ENCRYPTION_KEY_REFERENCE", "environment:INFERCRANE_ASYNC_ENCRYPTION_KEY"),
		HostedAuthIssuer:                    env("INFERCRANE_HOSTED_AUTH_ISSUER", ""),
		HostedAuthAudience:                  env("INFERCRANE_HOSTED_AUTH_AUDIENCE", ""),
		HostedAuthJWTKey:                    env("INFERCRANE_HOSTED_AUTH_JWT_KEY", ""),
		HostedAuthJWTKeyFile:                env("INFERCRANE_HOSTED_AUTH_JWT_KEY_FILE", ""),
		HostedAuthAuthorizedParties:         splitCSV(env("INFERCRANE_HOSTED_AUTH_AUTHORIZED_PARTIES", "")),
		HostedAuthAutoProvision:             hostedAuthAutoProvision,
		StripeSecretKey:                     env("INFERCRANE_STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret:                 env("INFERCRANE_STRIPE_WEBHOOK_SECRET", ""),
		StripeBillingReturnURL:              env("INFERCRANE_BILLING_RETURN_URL", ""),
		StripePriceIDs:                      stripePriceIDs,
		StripeLivemode:                      stripeLivemode,
		ModelAPICatalogFile:                 env("INFERCRANE_MODEL_API_CATALOG_FILE", ""),
		RunPodAPIKey:                        env("RUNPOD_API_KEY", ""),
		RunPodServerlessTemplateID:          env("INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID", ""),
		RunPodRESTURL:                       env("INFERCRANE_RUNPOD_REST_URL", "https://rest.runpod.io/v1"),
		RunPodContainerDiskGiB:              runPodContainerDiskGiB,
		RunPodArtifactCachePolicy:           env("INFERCRANE_RUNPOD_ARTIFACT_CACHE_POLICY", "prefer"),
		RunPodNetworkVolumes:                runPodNetworkVolumes,
		RunPodHFTokenSecret:                 env("INFERCRANE_RUNPOD_HF_TOKEN_SECRET", ""),
		SkyPilotAPI:                         env("INFERCRANE_SKYPILOT_API", "auto"),
		SkyPilotProviders:                   skyPilotProviders,
		AWSRoleARN:                          env("INFERCRANE_AWS_ROLE_ARN", ""),
		AWSExternalID:                       env("INFERCRANE_AWS_EXTERNAL_ID", ""),
		AWSRegion:                           env("INFERCRANE_AWS_REGION", ""),
		AWSSubnetID:                         env("INFERCRANE_AWS_SUBNET_ID", ""),
		AWSSubnetIDs:                        splitCSV(env("INFERCRANE_AWS_SUBNET_IDS", "")),
		AWSSecurityGroupIDs:                 splitCSV(env("INFERCRANE_AWS_SECURITY_GROUP_IDS", "")),
		AWSAMIID:                            env("INFERCRANE_AWS_AMI_ID", ""),
		AWSInstanceType:                     env("INFERCRANE_AWS_INSTANCE_TYPE", ""),
		AWSGPU:                              env("INFERCRANE_AWS_GPU", ""),
		AWSGPUCount:                         awsGPUCount,
		AWSInstanceProfileARN:               env("INFERCRANE_AWS_INSTANCE_PROFILE_ARN", ""),
		AWSWorkerSecretARN:                  env("INFERCRANE_AWS_WORKER_SECRET_ARN", ""),
		AWSImageDigest:                      env("INFERCRANE_AWS_IMAGE_DIGEST", ""),
		AWSImageCachePolicy:                 env("INFERCRANE_AWS_IMAGE_CACHE_POLICY", "prefer"),
		AWSArtifactCachePolicy:              env("INFERCRANE_AWS_ARTIFACT_CACHE_POLICY", "prefer"),
		AWSArtifactSnapshots:                awsArtifactSnapshots,
		AWSArtifactVolumeInitializationRate: awsArtifactVolumeInitializationRate,
		AWSRootVolumeGiB:                    awsRootVolumeGiB,
		AWSGP3IOPS:                          awsGP3IOPS,
		AWSGP3Throughput:                    awsGP3Throughput,
		GCPProject:                          env("INFERCRANE_GCP_PROJECT", ""),
		GCPZone:                             env("INFERCRANE_GCP_ZONE", ""),
		GCPSubnet:                           env("INFERCRANE_GCP_SUBNET", ""),
		GCPMachineType:                      env("INFERCRANE_GCP_MACHINE_TYPE", ""),
		GCPGPU:                              env("INFERCRANE_GCP_GPU", ""),
		GCPServiceAccount:                   env("INFERCRANE_GCP_SERVICE_ACCOUNT", ""),
		GCPVMImage:                          env("INFERCRANE_GCP_VM_IMAGE", ""),
		GCPContainerImage:                   env("INFERCRANE_GCP_CONTAINER_IMAGE", ""),
		GCPWorkerSecret:                     env("INFERCRANE_GCP_WORKER_SECRET", ""),
		GCPArtifactCachePolicy:              env("INFERCRANE_GCP_ARTIFACT_CACHE_POLICY", "prefer"),
		GCPArtifactDisks:                    gcpArtifactDisks,
		GCPBootDiskGiB:                      gcpBootDiskGiB,
		KubernetesContext:                   env("INFERCRANE_KUBERNETES_CONTEXT", ""),
		KubernetesNamespace:                 env("INFERCRANE_KUBERNETES_NAMESPACE", "infercrane-system"),
		KubernetesWorkloadAPI:               env("INFERCRANE_KUBERNETES_WORKLOAD_API", "deployment"),
		KubernetesServiceAccount:            env("INFERCRANE_KUBERNETES_SERVICE_ACCOUNT", "infercrane-runtime"),
		KubernetesWorkerSecretName:          env("INFERCRANE_KUBERNETES_WORKER_SECRET_NAME", "infercrane-worker"),
		KubernetesWorkerSecretKey:           env("INFERCRANE_KUBERNETES_WORKER_SECRET_KEY", "api-key"),
		KubernetesImageDigest:               env("INFERCRANE_KUBERNETES_IMAGE_DIGEST", ""),
		KubernetesGPUResource:               env("INFERCRANE_KUBERNETES_GPU_RESOURCE", "nvidia.com/gpu"),
		KubernetesGPUProductLabel:           env("INFERCRANE_KUBERNETES_GPU_PRODUCT_LABEL", "nvidia.com/gpu.product"),
		KubernetesImageCachePolicy:          env("INFERCRANE_KUBERNETES_IMAGE_CACHE_POLICY", "prefer"),
		KubernetesArtifactCachePolicy:       env("INFERCRANE_KUBERNETES_ARTIFACT_CACHE_POLICY", "prefer"),
		KubernetesArtifactPVCs:              kubernetesArtifactPVCs,
		DynamoVLLMImageDigest:               env("INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST", ""),
		DynamoVLLMRuntimeVersion:            env("INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION", ""),
		DynamoSGLangImageDigest:             env("INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST", ""),
		DynamoSGLangRuntimeVersion:          env("INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION", ""),
		DynamoModelSecretName:               env("INFERCRANE_DYNAMO_MODEL_SECRET_NAME", ""),
		OptimizationPrices:                  optimizationPrices,
		Port:                                port, RouterStartPort: routerPort, DatabaseMaxOpen: maxOpen, DatabaseMaxIdle: maxIdle,
		HealthInterval: time.Duration(healthSeconds) * time.Second, UpstreamTimeout: time.Duration(upstreamSeconds) * time.Second,
		ShutdownTimeout: time.Duration(shutdownSeconds) * time.Second, RequestRetention: time.Duration(retentionHours) * time.Hour,
		GPUPriceSyncInterval: time.Duration(gpuPriceSyncSeconds) * time.Second,
	}
	// Preserve the existing RunPod default while moving the execution boundary
	// to provider-neutral manifests. Other clouds must be declared explicitly.
	if len(config.SkyPilotProviders) == 0 && config.RunPodAPIKey != "" {
		config.SkyPilotProviders = []SkyPilotProvider{{
			Cloud:         "runpod",
			Label:         "RunPod",
			Runtimes:      []string{"vllm", "sglang", "custom-oci"},
			CredentialEnv: []string{"RUNPOD_API_KEY"},
		}}
	}
	if config.Port < 1 || config.Port > 65535 {
		return Config{}, fmt.Errorf("INFERCRANE_PORT must be between 1 and 65535")
	}
	if config.RouterStartPort < 1024 || config.RouterStartPort > 55000 {
		return Config{}, fmt.Errorf("INFERCRANE_ROUTER_START_PORT must be between 1024 and 55000")
	}
	if config.DatabaseURL == "" || config.InstanceID == "" {
		return Config{}, fmt.Errorf("database URL and instance ID are required")
	}
	controlURL, controlErr := url.Parse(config.ControlURL)
	if controlErr != nil || (controlURL.Scheme != "http" && controlURL.Scheme != "https") || controlURL.Host == "" || controlURL.User != nil || controlURL.RawQuery != "" || controlURL.Fragment != "" {
		return Config{}, fmt.Errorf("INFERCRANE_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if requireAPIKey && config.APIKey == "" {
		return Config{}, fmt.Errorf("INFERCRANE_API_KEY is required")
	}
	if config.Environment != "development" && config.Environment != "test" && config.Environment != "production" {
		return Config{}, fmt.Errorf("INFERCRANE_ENV must be development, test, or production")
	}
	if config.Environment == "production" {
		if requireAPIKey && len(config.APIKey) < 32 {
			return Config{}, fmt.Errorf("INFERCRANE_API_KEY must be at least 32 characters in production")
		}
		parsed, parseErr := url.Parse(config.DatabaseURL)
		if parseErr != nil {
			return Config{}, fmt.Errorf("parse production database URL: %w", parseErr)
		}
		sslMode := parsed.Query().Get("sslmode")
		if sslMode != "require" && sslMode != "verify-ca" && sslMode != "verify-full" {
			return Config{}, fmt.Errorf("production PostgreSQL requires sslmode=require, verify-ca, or verify-full")
		}
	}
	if config.DatabaseMaxOpen < 1 || config.DatabaseMaxIdle < 0 || config.DatabaseMaxIdle > config.DatabaseMaxOpen {
		return Config{}, fmt.Errorf("invalid database pool limits")
	}
	if config.HealthInterval <= 0 || config.UpstreamTimeout <= 0 || config.ShutdownTimeout <= 0 || config.RequestRetention <= 0 {
		return Config{}, fmt.Errorf("timeouts must be positive")
	}
	if config.GPUPriceSyncInterval != 0 && (config.GPUPriceSyncInterval < 5*time.Minute || config.GPUPriceSyncInterval > 24*time.Hour) {
		return Config{}, fmt.Errorf("INFERCRANE_GPU_PRICE_SYNC_SECONDS must be 0 or between 300 and 86400")
	}
	if config.RunPodContainerDiskGiB < 50 || config.RunPodContainerDiskGiB > 2048 {
		return Config{}, fmt.Errorf("INFERCRANE_RUNPOD_CONTAINER_DISK_GIB must be between 50 and 2048")
	}
	if err := validateRunPod(config); err != nil {
		return Config{}, err
	}
	if err := validateSkyPilot(config); err != nil {
		return Config{}, err
	}
	if err := validateAWS(config); err != nil {
		return Config{}, err
	}
	if err := validateGCP(config); err != nil {
		return Config{}, err
	}
	if err := validateKubernetes(config); err != nil {
		return Config{}, err
	}
	if err := validateDynamo(config); err != nil {
		return Config{}, err
	}
	if err := validateServerTLS(config); err != nil {
		return Config{}, err
	}
	if err := validateHostedAuth(config); err != nil {
		return Config{}, err
	}
	if err := validateStripe(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func validateRunPod(config Config) error {
	policy := config.RunPodArtifactCachePolicy
	if policy != "disabled" && policy != "prefer" && policy != "required" {
		return errors.New("INFERCRANE_RUNPOD_ARTIFACT_CACHE_POLICY must be disabled, prefer, or required")
	}
	if policy == "required" && len(config.RunPodNetworkVolumes) == 0 {
		return errors.New("required RunPod artifact caching needs INFERCRANE_RUNPOD_NETWORK_VOLUMES_JSON")
	}
	if policy == "disabled" && len(config.RunPodNetworkVolumes) > 0 {
		return errors.New("disabled RunPod artifact caching cannot configure network volume mappings")
	}
	for modelIdentity, volumeID := range config.RunPodNetworkVolumes {
		if !validImmutableModelIdentity(modelIdentity) || !validRunPodResourceID(volumeID) {
			return errors.New("RunPod network volume mappings require immutable model identities and valid volume IDs")
		}
	}
	if config.RunPodHFTokenSecret != "" && !validRunPodResourceID(config.RunPodHFTokenSecret) {
		return errors.New("INFERCRANE_RUNPOD_HF_TOKEN_SECRET must be a valid RunPod secret name")
	}
	return nil
}

func validateSkyPilot(config Config) error {
	switch config.SkyPilotAPI {
	case "auto", "enabled", "disabled":
	default:
		return errors.New("INFERCRANE_SKYPILOT_API must be auto, enabled, or disabled")
	}
	seen := map[string]struct{}{}
	for _, provider := range config.SkyPilotProviders {
		if !validManifestID(provider.Cloud) || len(provider.Runtimes) == 0 || len(provider.CredentialEnv) == 0 {
			return errors.New("SkyPilot provider manifests require a safe cloud ID, at least one runtime, and credential_env references")
		}
		if _, duplicate := seen[provider.Cloud]; duplicate {
			return fmt.Errorf("SkyPilot provider manifest %q is duplicated", provider.Cloud)
		}
		seen[provider.Cloud] = struct{}{}
		runtimes := map[string]struct{}{}
		for _, runtime := range provider.Runtimes {
			if runtime != "vllm" && runtime != "sglang" && runtime != "custom-oci" {
				return fmt.Errorf("SkyPilot provider %q runtime must be vllm, sglang, or custom-oci", provider.Cloud)
			}
			if _, duplicate := runtimes[runtime]; duplicate {
				return fmt.Errorf("SkyPilot provider %q runtime %q is duplicated", provider.Cloud, runtime)
			}
			runtimes[runtime] = struct{}{}
		}
		for _, name := range provider.CredentialEnv {
			if !validEnvironmentName(name) {
				return fmt.Errorf("SkyPilot provider %q has an invalid credential environment reference", provider.Cloud)
			}
		}
	}
	if config.SkyPilotAPI == "enabled" && len(config.ConfiguredSkyPilotProviders()) == 0 {
		return errors.New("enabled SkyPilot API requires at least one manifest whose credential_env references are configured")
	}
	return nil
}

func validManifestID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	isAlphaNumeric := func(char byte) bool { return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' }
	if !isAlphaNumeric(value[0]) || !isAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func validEnvironmentName(value string) bool {
	if value == "" || len(value) > 128 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, char := range value {
		if char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validRunPodResourceID(value string) bool {
	if len(value) < 4 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func (c Config) AWSEnabled() bool { return c.AWSRoleARN != "" }

// SkyPilotEnabled is the single execution boundary for both the optional
// local API process and the provider adapter. Keeping composition behind the
// same switch prevents an innocuous status observation from auto-starting
// SkyPilot's local multiprocessing server on control-plane replicas.
func (c Config) SkyPilotEnabled() bool {
	return c.SkyPilotAPI != "disabled" && len(c.ConfiguredSkyPilotProviders()) > 0
}

// ConfiguredSkyPilotProviders returns only manifests whose named credential
// evidence exists in this process. A catalog declaration by itself can never
// make a provider executable.
func (c Config) ConfiguredSkyPilotProviders() []SkyPilotProvider {
	configured := make([]SkyPilotProvider, 0, len(c.SkyPilotProviders))
	for _, provider := range c.SkyPilotProviders {
		ready := len(provider.CredentialEnv) > 0
		for _, name := range provider.CredentialEnv {
			if strings.TrimSpace(os.Getenv(name)) == "" {
				ready = false
				break
			}
		}
		if ready {
			configured = append(configured, provider)
		}
	}
	return configured
}

func (c Config) GCPEnabled() bool { return c.GCPProject != "" }

func (c Config) KubernetesEnabled() bool { return c.KubernetesContext != "" }

func (c Config) DynamoEnabled() bool {
	return c.DynamoRuntimeEnabled("vllm") || c.DynamoRuntimeEnabled("sglang")
}
func (c Config) DynamoRuntimeEnabled(runtime string) bool {
	switch runtime {
	case "vllm":
		return c.DynamoVLLMImageDigest != "" && c.DynamoVLLMRuntimeVersion != ""
	case "sglang":
		return c.DynamoSGLangImageDigest != "" && c.DynamoSGLangRuntimeVersion != ""
	default:
		return false
	}
}

func (c Config) HostedAuthEnabled() bool {
	return c.HostedAuthJWTKey != "" || c.HostedAuthJWTKeyFile != ""
}

func (c Config) StripeEnabled() bool { return c.StripeSecretKey != "" }

func validateStripe(config Config) error {
	configured := config.StripeSecretKey != "" || config.StripeWebhookSecret != "" || config.StripeBillingReturnURL != "" || len(config.StripePriceIDs) > 0 || config.StripeLivemode
	if !configured {
		return nil
	}
	if config.StripeSecretKey == "" || config.StripeWebhookSecret == "" || config.StripeBillingReturnURL == "" || len(config.StripePriceIDs) != 5 {
		return errors.New("Stripe prepaid funding configuration is partial; secret key, webhook secret, return URL, and all five fixed Price IDs are required")
	}
	if config.StripeLivemode && !strings.HasPrefix(config.StripeSecretKey, "sk_live_") {
		return errors.New("INFERCRANE_STRIPE_LIVEMODE=true requires a Stripe live-mode secret key")
	}
	if !config.StripeLivemode && !strings.HasPrefix(config.StripeSecretKey, "sk_test_") {
		return errors.New("Stripe sandbox funding requires an sk_test_ secret key")
	}
	if !strings.HasPrefix(config.StripeWebhookSecret, "whsec_") {
		return errors.New("INFERCRANE_STRIPE_WEBHOOK_SECRET must be a Stripe webhook signing secret")
	}
	parsed, err := url.Parse(config.StripeBillingReturnURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("INFERCRANE_BILLING_RETURN_URL must be an absolute HTTP(S) URL without credentials or fragment")
	}
	return nil
}

func validateHostedAuth(config Config) error {
	configured := config.HostedAuthIssuer != "" || config.HostedAuthAudience != "" || config.HostedAuthJWTKey != "" || config.HostedAuthJWTKeyFile != "" || len(config.HostedAuthAuthorizedParties) > 0 || config.HostedAuthAutoProvision
	if !configured {
		return nil
	}
	if config.HostedAuthIssuer == "" || (config.HostedAuthJWTKey == "" && config.HostedAuthJWTKeyFile == "") || len(config.HostedAuthAuthorizedParties) == 0 {
		return errors.New("hosted auth configuration is partial; issuer, JWT public key, and authorized parties are required")
	}
	if config.HostedAuthJWTKey != "" && config.HostedAuthJWTKeyFile != "" {
		return errors.New("configure exactly one of INFERCRANE_HOSTED_AUTH_JWT_KEY or INFERCRANE_HOSTED_AUTH_JWT_KEY_FILE")
	}
	issuer, err := url.Parse(config.HostedAuthIssuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("INFERCRANE_HOSTED_AUTH_ISSUER must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	for _, party := range config.HostedAuthAuthorizedParties {
		parsed, parseErr := url.Parse(party)
		if parseErr != nil || (parsed.Scheme != "https" && !(config.Environment != "production" && parsed.Scheme == "http" && parsed.Hostname() == "localhost")) || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("invalid hosted auth authorized party %q", party)
		}
	}
	return nil
}

func validateAWS(config Config) error {
	subnets := append([]string(nil), config.AWSSubnetIDs...)
	if len(subnets) == 0 && config.AWSSubnetID != "" {
		subnets = append(subnets, config.AWSSubnetID)
	}
	values := []string{config.AWSRoleARN, config.AWSRegion, config.AWSAMIID, config.AWSInstanceType, config.AWSGPU, config.AWSInstanceProfileARN, config.AWSWorkerSecretARN, config.AWSImageDigest}
	configured := len(config.AWSSecurityGroupIDs) > 0 || len(config.AWSArtifactSnapshots) > 0 || config.AWSArtifactCachePolicy == "required" || config.AWSArtifactVolumeInitializationRate != 0
	configured = configured || len(subnets) > 0
	for _, value := range values {
		configured = configured || value != ""
	}
	if !configured {
		return nil
	}
	for _, value := range values {
		if value == "" {
			return errors.New("AWS BYOC configuration is partial; role ARN, region, at least one subnet, security groups, AMI, instance type, GPU, instance profile, worker secret ARN, and immutable image digest are required")
		}
	}
	if len(subnets) == 0 {
		return errors.New("AWS BYOC configuration requires at least one subnet through INFERCRANE_AWS_SUBNET_IDS or INFERCRANE_AWS_SUBNET_ID")
	}
	if len(config.AWSSecurityGroupIDs) == 0 {
		return errors.New("AWS BYOC configuration requires at least one security group")
	}
	if runtimecontract.ValidateImage(config.AWSImageDigest) != nil {
		return errors.New("INFERCRANE_AWS_IMAGE_DIGEST must be pinned by sha256 digest")
	}
	if config.AWSGPUCount < 1 || config.AWSGPUCount > 1024 {
		return errors.New("INFERCRANE_AWS_GPU_COUNT must be between 1 and 1024")
	}
	if config.AWSRootVolumeGiB < 50 || config.AWSRootVolumeGiB > 16384 {
		return errors.New("INFERCRANE_AWS_ROOT_VOLUME_GIB must be between 50 and 16384")
	}
	if config.AWSGP3IOPS < 3000 || config.AWSGP3IOPS > 80000 {
		return errors.New("INFERCRANE_AWS_GP3_IOPS must be between 3000 and 80000")
	}
	if config.AWSGP3Throughput < 125 || config.AWSGP3Throughput > 2000 || config.AWSGP3Throughput*4 > config.AWSGP3IOPS {
		return errors.New("INFERCRANE_AWS_GP3_THROUGHPUT_MIBPS must be between 125 and 2000 and no more than one quarter of configured IOPS")
	}
	if config.AWSImageCachePolicy != "prefer" && config.AWSImageCachePolicy != "required" {
		return errors.New("INFERCRANE_AWS_IMAGE_CACHE_POLICY must be prefer or required")
	}
	if config.AWSArtifactCachePolicy != "disabled" && config.AWSArtifactCachePolicy != "prefer" && config.AWSArtifactCachePolicy != "required" {
		return errors.New("INFERCRANE_AWS_ARTIFACT_CACHE_POLICY must be disabled, prefer, or required")
	}
	if config.AWSArtifactCachePolicy == "required" && len(config.AWSArtifactSnapshots) == 0 {
		return errors.New("INFERCRANE_AWS_ARTIFACT_CACHE_POLICY=required needs at least one immutable model-to-snapshot mapping")
	}
	if config.AWSArtifactCachePolicy == "disabled" && len(config.AWSArtifactSnapshots) > 0 {
		return errors.New("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON cannot be set while artifact caching is disabled")
	}
	for modelIdentity, snapshotID := range config.AWSArtifactSnapshots {
		if !validImmutableModelIdentity(modelIdentity) || !validAWSSnapshotID(snapshotID) {
			return errors.New("INFERCRANE_AWS_ARTIFACT_SNAPSHOTS_JSON must map non-empty immutable model identities to AWS snapshot IDs")
		}
	}
	if rate := config.AWSArtifactVolumeInitializationRate; rate != 0 && (rate < 100 || rate > 300) {
		return errors.New("INFERCRANE_AWS_ARTIFACT_VOLUME_INITIALIZATION_RATE_MIBPS must be 0 or between 100 and 300")
	}
	return nil
}

func validImmutableModelIdentity(value string) bool {
	separator := strings.LastIndex(value, "@")
	if separator <= 0 || strings.TrimSpace(value) != value {
		return false
	}
	revision := value[separator+1:]
	if len(revision) != 40 {
		return false
	}
	for _, char := range revision {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validAWSSnapshotID(value string) bool {
	if !strings.HasPrefix(value, "snap-") || len(value) < len("snap-")+8 {
		return false
	}
	for _, char := range value[len("snap-"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateGCP(config Config) error {
	values := []string{config.GCPProject, config.GCPZone, config.GCPSubnet, config.GCPMachineType, config.GCPGPU, config.GCPServiceAccount, config.GCPVMImage, config.GCPContainerImage, config.GCPWorkerSecret}
	configured := len(config.GCPArtifactDisks) > 0 || config.GCPArtifactCachePolicy == "required"
	for _, value := range values {
		configured = configured || value != ""
	}
	if !configured {
		return nil
	}
	for _, value := range values {
		if value == "" {
			return errors.New("GCP BYOC configuration is partial; project, zone, subnet, machine type, GPU, service account, VM image, worker secret, and immutable container image are required")
		}
	}
	if runtimecontract.ValidateImage(config.GCPContainerImage) != nil {
		return errors.New("INFERCRANE_GCP_CONTAINER_IMAGE must be pinned by sha256 digest")
	}
	if strings.Contains(config.GCPVMImage, "/family/") {
		return errors.New("INFERCRANE_GCP_VM_IMAGE must identify an immutable image, not an image family")
	}
	if config.GCPBootDiskGiB < 50 || config.GCPBootDiskGiB > 65536 {
		return errors.New("INFERCRANE_GCP_BOOT_DISK_GIB must be between 50 and 65536")
	}
	if config.GCPArtifactCachePolicy != "disabled" && config.GCPArtifactCachePolicy != "prefer" && config.GCPArtifactCachePolicy != "required" {
		return errors.New("INFERCRANE_GCP_ARTIFACT_CACHE_POLICY must be disabled, prefer, or required")
	}
	if config.GCPArtifactCachePolicy == "required" && len(config.GCPArtifactDisks) == 0 {
		return errors.New("required GCP artifact caching needs INFERCRANE_GCP_ARTIFACT_DISKS_JSON")
	}
	if config.GCPArtifactCachePolicy == "disabled" && len(config.GCPArtifactDisks) > 0 {
		return errors.New("disabled GCP artifact caching cannot configure persistent disk mappings")
	}
	for modelIdentity, disk := range config.GCPArtifactDisks {
		if !validImmutableModelIdentity(modelIdentity) || !validKubernetesConfigName(disk) {
			return errors.New("GCP artifact disk mappings require immutable model identities and valid zonal disk names")
		}
	}
	return nil
}

func validateServerTLS(config Config) error {
	if (config.TLSCertFile == "") != (config.TLSKeyFile == "") {
		return errors.New("INFERCRANE_TLS_CERT_FILE and INFERCRANE_TLS_KEY_FILE must be configured together")
	}
	if config.TLSClientCAFile != "" && config.TLSCertFile == "" {
		return errors.New("INFERCRANE_TLS_CLIENT_CA_FILE requires the TLS certificate and key")
	}
	return nil
}

func validateKubernetes(config Config) error {
	configured := config.KubernetesContext != "" || config.KubernetesImageDigest != "" || len(config.KubernetesArtifactPVCs) > 0 || config.KubernetesArtifactCachePolicy == "required"
	if !configured {
		return nil
	}
	values := []string{config.KubernetesContext, config.KubernetesNamespace, config.KubernetesWorkloadAPI, config.KubernetesServiceAccount, config.KubernetesWorkerSecretName, config.KubernetesWorkerSecretKey, config.KubernetesImageDigest, config.KubernetesGPUResource, config.KubernetesGPUProductLabel}
	for _, value := range values {
		if value == "" {
			return errors.New("Kubernetes configuration is partial; context, namespace, workload API, service account, worker Secret name/key, immutable image digest, GPU resource, and GPU product label are required")
		}
	}
	if config.KubernetesWorkloadAPI != "deployment" && config.KubernetesWorkloadAPI != "kserve" {
		return errors.New("INFERCRANE_KUBERNETES_WORKLOAD_API must be deployment or kserve")
	}
	if runtimecontract.ValidateImage(config.KubernetesImageDigest) != nil {
		return errors.New("INFERCRANE_KUBERNETES_IMAGE_DIGEST must be pinned by sha256 digest")
	}
	if config.KubernetesImageCachePolicy != "prefer" && config.KubernetesImageCachePolicy != "required" {
		return errors.New("INFERCRANE_KUBERNETES_IMAGE_CACHE_POLICY must be prefer or required")
	}
	if config.KubernetesArtifactCachePolicy != "disabled" && config.KubernetesArtifactCachePolicy != "prefer" && config.KubernetesArtifactCachePolicy != "required" {
		return errors.New("INFERCRANE_KUBERNETES_ARTIFACT_CACHE_POLICY must be disabled, prefer, or required")
	}
	if config.KubernetesArtifactCachePolicy == "required" && len(config.KubernetesArtifactPVCs) == 0 {
		return errors.New("required Kubernetes artifact caching needs INFERCRANE_KUBERNETES_ARTIFACT_PVCS_JSON")
	}
	if config.KubernetesArtifactCachePolicy == "disabled" && len(config.KubernetesArtifactPVCs) > 0 {
		return errors.New("disabled Kubernetes artifact caching cannot configure PVC mappings")
	}
	for modelIdentity, claim := range config.KubernetesArtifactPVCs {
		if !validImmutableModelIdentity(modelIdentity) || !validKubernetesConfigName(claim) {
			return errors.New("Kubernetes artifact PVC mappings require immutable model identities and DNS-label claim names")
		}
	}
	return nil
}

func validKubernetesConfigName(value string) bool {
	if value == "" || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (char == '-' && index > 0 && index < len(value)-1) {
			continue
		}
		return false
	}
	return true
}

func validateDynamo(config Config) error {
	configured := config.DynamoVLLMImageDigest != "" || config.DynamoVLLMRuntimeVersion != "" || config.DynamoSGLangImageDigest != "" || config.DynamoSGLangRuntimeVersion != "" || config.DynamoModelSecretName != ""
	if !configured {
		return nil
	}
	if !config.KubernetesEnabled() {
		return errors.New("Dynamo configuration requires the complete Kubernetes configuration")
	}
	if !config.DynamoEnabled() {
		return errors.New("Dynamo configuration requires at least one complete vLLM or SGLang image digest and matching runtime version pair")
	}
	for _, runtime := range []struct{ name, image, version string }{
		{"vLLM", config.DynamoVLLMImageDigest, config.DynamoVLLMRuntimeVersion},
		{"SGLang", config.DynamoSGLangImageDigest, config.DynamoSGLangRuntimeVersion},
	} {
		if (runtime.image == "") != (runtime.version == "") {
			return fmt.Errorf("Dynamo %s configuration requires both image digest and runtime version", runtime.name)
		}
		if runtime.image == "" {
			continue
		}
		if runtimecontract.ValidateImage(runtime.image) != nil {
			return fmt.Errorf("Dynamo %s image must be pinned by sha256 digest", runtime.name)
		}
		if !validDynamoRuntimeVersion(runtime.version) {
			return fmt.Errorf("Dynamo %s runtime version must be numeric MAJOR.MINOR.PATCH and match its image", runtime.name)
		}
	}
	if config.DynamoModelSecretName != "" && !validDNSLabel(config.DynamoModelSecretName) {
		return errors.New("INFERCRANE_DYNAMO_MODEL_SECRET_NAME must be a valid Kubernetes DNS label")
	}
	return nil
}

func validDynamoRuntimeVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 16); err != nil {
			return false
		}
	}
	return true
}

func validDNSLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func splitCSV(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return value, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", key, err)
	}
	return value, nil
}

func envStripePriceIDs(key string) (map[int64]string, error) {
	encoded, err := envStringMap(key)
	if err != nil || len(encoded) == 0 {
		return nil, err
	}
	allowed := map[int64]struct{}{25: {}, 50: {}, 100: {}, 250: {}, 500: {}}
	prices := make(map[int64]string, len(encoded))
	for dollarAmount, priceID := range encoded {
		amount, parseErr := strconv.ParseInt(dollarAmount, 10, 64)
		_, supported := allowed[amount]
		if parseErr != nil || !supported || !strings.HasPrefix(priceID, "price_") || strings.TrimSpace(priceID) != priceID || len(priceID) < len("price_")+1 {
			return nil, fmt.Errorf("%s must map only 25, 50, 100, 250, and 500 USD to Stripe price_ IDs", key)
		}
		prices[amount*1_000_000] = priceID
	}
	return prices, nil
}

func envStringMap(key string) (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	values := map[string]string{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object of string keys and values: %w", key, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one JSON object", key)
	}
	return values, nil
}

func envSkyPilotProviders(key string) ([]SkyPilotProvider, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	var providers []SkyPilotProvider
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&providers); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of SkyPilot provider manifests: %w", key, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one JSON array", key)
	}
	if len(providers) > 32 {
		return nil, fmt.Errorf("%s supports at most 32 provider manifests", key)
	}
	return providers, nil
}

func envOptimizationPrices(key string) ([]OptimizationPrice, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	type encodedPrice struct {
		Cloud, Region, GPU, Currency, Source string
		GPUCount                             int `json:"gpu_count"`
		Replicas                             int
		HourlyUSD                            float64 `json:"hourly_usd"`
		ObservedAt                           string  `json:"observed_at"`
		ValidUntil                           string  `json:"valid_until"`
	}
	var encoded []encodedPrice
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoded); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of exact sourced prices: %w", key, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one JSON array", key)
	}
	if len(encoded) > 1000 {
		return nil, fmt.Errorf("%s supports at most 1000 exact price tuples", key)
	}
	prices := make([]OptimizationPrice, 0, len(encoded))
	seen := map[string]struct{}{}
	for index, row := range encoded {
		if row.GPUCount == 0 {
			row.GPUCount = 1
		}
		observedAt, observedErr := time.Parse(time.RFC3339, row.ObservedAt)
		validUntil, validErr := time.Parse(time.RFC3339, row.ValidUntil)
		identity := strings.Join([]string{row.Cloud, row.Region, row.GPU, strconv.Itoa(row.GPUCount), strconv.Itoa(row.Replicas)}, "\x00")
		_, duplicate := seen[identity]
		if strings.TrimSpace(row.Cloud) == "" || strings.TrimSpace(row.GPU) == "" || strings.TrimSpace(row.Source) == "" || row.Currency != "USD" || row.GPUCount < 1 || row.GPUCount > 1024 || row.Replicas < 1 || row.Replicas > 100 || row.HourlyUSD <= 0 || math.IsNaN(row.HourlyUSD) || math.IsInf(row.HourlyUSD, 0) || observedErr != nil || validErr != nil || !validUntil.After(observedAt) || duplicate {
			return nil, fmt.Errorf("%s[%d] must contain a unique cloud/region/GPU/gpu_count/replicas tuple, positive finite USD rate, source, and increasing RFC3339 evidence window", key, index)
		}
		seen[identity] = struct{}{}
		prices = append(prices, OptimizationPrice{Cloud: row.Cloud, Region: row.Region, GPU: row.GPU, GPUCount: row.GPUCount, Replicas: row.Replicas, HourlyUSD: row.HourlyUSD, Currency: row.Currency, Source: row.Source, ObservedAt: observedAt.UTC(), ValidUntil: validUntil.UTC()})
	}
	return prices, nil
}
