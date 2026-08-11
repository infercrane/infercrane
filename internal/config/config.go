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
)

type Config struct {
	DatabaseURL, ControlURL, Host, APIKey, RouterBinary, AIPerfBinary, PassportSigningKeyFile, InstanceID, Environment string
	RunPodAPIKey, RunPodServerlessTemplateID, RunPodRESTURL                                                            string
	AWSRoleARN, AWSExternalID, AWSRegion, AWSSubnetID, AWSAMIID, AWSInstanceType, AWSGPU                               string
	AWSInstanceProfileARN, AWSWorkerSecretARN, AWSImageDigest                                                          string
	AWSSecurityGroupIDs                                                                                                []string
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
	return Config{ControlURL: controlURL, APIKey: apiKey}, nil
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
	config := Config{
		DatabaseURL:                env("INFERCRANE_DATABASE_URL", "postgres://infercrane:infercrane@127.0.0.1:5432/infercrane?sslmode=disable"),
		ControlURL:                 env("INFERCRANE_URL", "http://127.0.0.1:8080"),
		Host:                       env("INFERCRANE_HOST", "127.0.0.1"),
		APIKey:                     env("INFERCRANE_API_KEY", ""),
		RouterBinary:               env("INFERCRANE_ROUTER_BINARY", "vllm-router"),
		AIPerfBinary:               env("INFERCRANE_AIPERF_BINARY", "aiperf"),
		PassportSigningKeyFile:     env("INFERCRANE_PASSPORT_SIGNING_KEY_FILE", ""),
		InstanceID:                 env("INFERCRANE_INSTANCE_ID", hostname),
		Environment:                env("INFERCRANE_ENV", "development"),
		RunPodAPIKey:               env("RUNPOD_API_KEY", ""),
		RunPodServerlessTemplateID: env("INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID", ""),
		RunPodRESTURL:              env("INFERCRANE_RUNPOD_REST_URL", "https://rest.runpod.io/v1"),
		AWSRoleARN:                 env("INFERCRANE_AWS_ROLE_ARN", ""),
		AWSExternalID:              env("INFERCRANE_AWS_EXTERNAL_ID", ""),
		AWSRegion:                  env("INFERCRANE_AWS_REGION", ""),
		AWSSubnetID:                env("INFERCRANE_AWS_SUBNET_ID", ""),
		AWSSecurityGroupIDs:        splitCSV(env("INFERCRANE_AWS_SECURITY_GROUP_IDS", "")),
		AWSAMIID:                   env("INFERCRANE_AWS_AMI_ID", ""),
		AWSInstanceType:            env("INFERCRANE_AWS_INSTANCE_TYPE", ""),
		AWSGPU:                     env("INFERCRANE_AWS_GPU", ""),
		AWSInstanceProfileARN:      env("INFERCRANE_AWS_INSTANCE_PROFILE_ARN", ""),
		AWSWorkerSecretARN:         env("INFERCRANE_AWS_WORKER_SECRET_ARN", ""),
		AWSImageDigest:             env("INFERCRANE_AWS_IMAGE_DIGEST", ""),
		Port:                       port, RouterStartPort: routerPort, DatabaseMaxOpen: maxOpen, DatabaseMaxIdle: maxIdle,
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
	return config, nil
}

func (c Config) AWSEnabled() bool { return c.AWSRoleARN != "" }

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
	if !strings.Contains(config.AWSImageDigest, "@sha256:") {
		return errors.New("INFERCRANE_AWS_IMAGE_DIGEST must be pinned by sha256 digest")
	}
	return nil
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
