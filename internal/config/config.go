package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
	HostedAuthIssuer, HostedAuthAudience, HostedAuthJWTKeyFile                                                         string
	HostedAuthAuthorizedParties                                                                                        []string
	RunPodAPIKey, RunPodServerlessTemplateID, RunPodRESTURL                                                            string
	AWSRoleARN, AWSExternalID, AWSRegion, AWSSubnetID, AWSAMIID, AWSInstanceType, AWSGPU                               string
	AWSInstanceProfileARN, AWSWorkerSecretARN, AWSImageDigest                                                          string
	AWSImageCachePolicy                                                                                                string
	AWSSecurityGroupIDs                                                                                                []string
	AWSRootVolumeGiB                                                                                                   int
	GCPProject, GCPZone, GCPSubnet, GCPMachineType, GCPGPU, GCPServiceAccount                                          string
	GCPVMImage, GCPContainerImage, GCPWorkerSecret                                                                     string
	KubernetesContext, KubernetesNamespace, KubernetesWorkloadAPI, KubernetesServiceAccount                            string
	KubernetesWorkerSecretName, KubernetesWorkerSecretKey, KubernetesImageDigest                                       string
	KubernetesGPUResource, KubernetesGPUProductLabel                                                                   string
	DynamoVLLMImageDigest, DynamoVLLMRuntimeVersion, DynamoSGLangImageDigest, DynamoSGLangRuntimeVersion               string
	DynamoModelSecretName                                                                                              string
	Port, RouterStartPort, DatabaseMaxOpen, DatabaseMaxIdle                                                            int
	HealthInterval, UpstreamTimeout, ShutdownTimeout, RequestRetention                                                 time.Duration
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
	awsRootVolumeGiB, err := envInt("INFERCRANE_AWS_ROOT_VOLUME_GIB", 100)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		DatabaseURL:                 env("INFERCRANE_DATABASE_URL", "postgres://infercrane:infercrane@127.0.0.1:5432/infercrane?sslmode=disable"),
		ControlURL:                  env("INFERCRANE_URL", "http://127.0.0.1:8080"),
		Host:                        env("INFERCRANE_HOST", "127.0.0.1"),
		APIKey:                      env("INFERCRANE_API_KEY", ""),
		RouterBinary:                env("INFERCRANE_ROUTER_BINARY", "vllm-router"),
		AIPerfBinary:                env("INFERCRANE_AIPERF_BINARY", "aiperf"),
		PassportSigningKeyFile:      env("INFERCRANE_PASSPORT_SIGNING_KEY_FILE", ""),
		InstanceID:                  env("INFERCRANE_INSTANCE_ID", hostname),
		Environment:                 env("INFERCRANE_ENV", "development"),
		TLSCertFile:                 env("INFERCRANE_TLS_CERT_FILE", ""),
		TLSKeyFile:                  env("INFERCRANE_TLS_KEY_FILE", ""),
		TLSClientCAFile:             env("INFERCRANE_TLS_CLIENT_CA_FILE", ""),
		AsyncEncryptionKey:          env("INFERCRANE_ASYNC_ENCRYPTION_KEY", ""),
		AsyncEncryptionKeyReference: env("INFERCRANE_ASYNC_ENCRYPTION_KEY_REFERENCE", "environment:INFERCRANE_ASYNC_ENCRYPTION_KEY"),
		HostedAuthIssuer:            env("INFERCRANE_HOSTED_AUTH_ISSUER", ""),
		HostedAuthAudience:          env("INFERCRANE_HOSTED_AUTH_AUDIENCE", ""),
		HostedAuthJWTKeyFile:        env("INFERCRANE_HOSTED_AUTH_JWT_KEY_FILE", ""),
		HostedAuthAuthorizedParties: splitCSV(env("INFERCRANE_HOSTED_AUTH_AUTHORIZED_PARTIES", "")),
		RunPodAPIKey:                env("RUNPOD_API_KEY", ""),
		RunPodServerlessTemplateID:  env("INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID", ""),
		RunPodRESTURL:               env("INFERCRANE_RUNPOD_REST_URL", "https://rest.runpod.io/v1"),
		AWSRoleARN:                  env("INFERCRANE_AWS_ROLE_ARN", ""),
		AWSExternalID:               env("INFERCRANE_AWS_EXTERNAL_ID", ""),
		AWSRegion:                   env("INFERCRANE_AWS_REGION", ""),
		AWSSubnetID:                 env("INFERCRANE_AWS_SUBNET_ID", ""),
		AWSSecurityGroupIDs:         splitCSV(env("INFERCRANE_AWS_SECURITY_GROUP_IDS", "")),
		AWSAMIID:                    env("INFERCRANE_AWS_AMI_ID", ""),
		AWSInstanceType:             env("INFERCRANE_AWS_INSTANCE_TYPE", ""),
		AWSGPU:                      env("INFERCRANE_AWS_GPU", ""),
		AWSInstanceProfileARN:       env("INFERCRANE_AWS_INSTANCE_PROFILE_ARN", ""),
		AWSWorkerSecretARN:          env("INFERCRANE_AWS_WORKER_SECRET_ARN", ""),
		AWSImageDigest:              env("INFERCRANE_AWS_IMAGE_DIGEST", ""),
		AWSImageCachePolicy:         env("INFERCRANE_AWS_IMAGE_CACHE_POLICY", "prefer"),
		AWSRootVolumeGiB:            awsRootVolumeGiB,
		GCPProject:                  env("INFERCRANE_GCP_PROJECT", ""),
		GCPZone:                     env("INFERCRANE_GCP_ZONE", ""),
		GCPSubnet:                   env("INFERCRANE_GCP_SUBNET", ""),
		GCPMachineType:              env("INFERCRANE_GCP_MACHINE_TYPE", ""),
		GCPGPU:                      env("INFERCRANE_GCP_GPU", ""),
		GCPServiceAccount:           env("INFERCRANE_GCP_SERVICE_ACCOUNT", ""),
		GCPVMImage:                  env("INFERCRANE_GCP_VM_IMAGE", ""),
		GCPContainerImage:           env("INFERCRANE_GCP_CONTAINER_IMAGE", ""),
		GCPWorkerSecret:             env("INFERCRANE_GCP_WORKER_SECRET", ""),
		KubernetesContext:           env("INFERCRANE_KUBERNETES_CONTEXT", ""),
		KubernetesNamespace:         env("INFERCRANE_KUBERNETES_NAMESPACE", "infercrane-system"),
		KubernetesWorkloadAPI:       env("INFERCRANE_KUBERNETES_WORKLOAD_API", "deployment"),
		KubernetesServiceAccount:    env("INFERCRANE_KUBERNETES_SERVICE_ACCOUNT", "infercrane-runtime"),
		KubernetesWorkerSecretName:  env("INFERCRANE_KUBERNETES_WORKER_SECRET_NAME", "infercrane-worker"),
		KubernetesWorkerSecretKey:   env("INFERCRANE_KUBERNETES_WORKER_SECRET_KEY", "api-key"),
		KubernetesImageDigest:       env("INFERCRANE_KUBERNETES_IMAGE_DIGEST", ""),
		KubernetesGPUResource:       env("INFERCRANE_KUBERNETES_GPU_RESOURCE", "nvidia.com/gpu"),
		KubernetesGPUProductLabel:   env("INFERCRANE_KUBERNETES_GPU_PRODUCT_LABEL", "nvidia.com/gpu.product"),
		DynamoVLLMImageDigest:       env("INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST", ""),
		DynamoVLLMRuntimeVersion:    env("INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION", ""),
		DynamoSGLangImageDigest:     env("INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST", ""),
		DynamoSGLangRuntimeVersion:  env("INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION", ""),
		DynamoModelSecretName:       env("INFERCRANE_DYNAMO_MODEL_SECRET_NAME", ""),
		Port:                        port, RouterStartPort: routerPort, DatabaseMaxOpen: maxOpen, DatabaseMaxIdle: maxIdle,
		HealthInterval: time.Duration(healthSeconds) * time.Second, UpstreamTimeout: time.Duration(upstreamSeconds) * time.Second,
		ShutdownTimeout: time.Duration(shutdownSeconds) * time.Second, RequestRetention: time.Duration(retentionHours) * time.Hour,
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
	return config, nil
}

func (c Config) AWSEnabled() bool { return c.AWSRoleARN != "" }

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

func (c Config) HostedAuthEnabled() bool { return c.HostedAuthJWTKeyFile != "" }

func validateHostedAuth(config Config) error {
	configured := config.HostedAuthIssuer != "" || config.HostedAuthAudience != "" || config.HostedAuthJWTKeyFile != "" || len(config.HostedAuthAuthorizedParties) > 0
	if !configured {
		return nil
	}
	if config.HostedAuthIssuer == "" || config.HostedAuthJWTKeyFile == "" || len(config.HostedAuthAuthorizedParties) == 0 {
		return errors.New("hosted auth configuration is partial; issuer, JWT public-key file, and authorized parties are required")
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
	values := []string{config.AWSRoleARN, config.AWSRegion, config.AWSSubnetID, config.AWSAMIID, config.AWSInstanceType, config.AWSGPU, config.AWSInstanceProfileARN, config.AWSWorkerSecretARN, config.AWSImageDigest}
	configured := len(config.AWSSecurityGroupIDs) > 0
	for _, value := range values {
		configured = configured || value != ""
	}
	if !configured {
		return nil
	}
	for _, value := range values {
		if value == "" {
			return errors.New("AWS BYOC configuration is partial; role ARN, region, subnet, security groups, AMI, instance type, GPU, instance profile, worker secret ARN, and immutable image digest are required")
		}
	}
	if len(config.AWSSecurityGroupIDs) == 0 {
		return errors.New("AWS BYOC configuration requires at least one security group")
	}
	if runtimecontract.ValidateImage(config.AWSImageDigest) != nil {
		return errors.New("INFERCRANE_AWS_IMAGE_DIGEST must be pinned by sha256 digest")
	}
	if config.AWSRootVolumeGiB < 50 || config.AWSRootVolumeGiB > 16384 {
		return errors.New("INFERCRANE_AWS_ROOT_VOLUME_GIB must be between 50 and 16384")
	}
	if config.AWSImageCachePolicy != "prefer" && config.AWSImageCachePolicy != "required" {
		return errors.New("INFERCRANE_AWS_IMAGE_CACHE_POLICY must be prefer or required")
	}
	return nil
}

func validateGCP(config Config) error {
	values := []string{config.GCPProject, config.GCPZone, config.GCPSubnet, config.GCPMachineType, config.GCPGPU, config.GCPServiceAccount, config.GCPVMImage, config.GCPContainerImage, config.GCPWorkerSecret}
	configured := false
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
	configured := config.KubernetesContext != "" || config.KubernetesImageDigest != ""
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
	return nil
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
