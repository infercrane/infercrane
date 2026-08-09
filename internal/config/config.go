package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL, ControlURL, Host, APIKey, RouterBinary, InstanceID, Environment string
	Port, RouterStartPort, DatabaseMaxOpen, DatabaseMaxIdle                      int
	HealthInterval, UpstreamTimeout, ShutdownTimeout, RequestRetention           time.Duration
}

func Load() (Config, error) {
	return load(true)
}

// LoadForDiagnostics loads and validates non-secret configuration. It allows
// doctor to report a missing API key alongside the rest of the environment.
func LoadForDiagnostics() (Config, error) {
	return load(false)
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
		DatabaseURL:  env("INFERCRANE_DATABASE_URL", "postgres://infercrane:infercrane@127.0.0.1:5432/infercrane?sslmode=disable"),
		ControlURL:   env("INFERCRANE_URL", "http://127.0.0.1:8080"),
		Host:         env("INFERCRANE_HOST", "127.0.0.1"),
		APIKey:       env("INFERCRANE_API_KEY", ""),
		RouterBinary: env("INFERCRANE_ROUTER_BINARY", "vllm-router"),
		InstanceID:   env("INFERCRANE_INSTANCE_ID", hostname),
		Environment:  env("INFERCRANE_ENV", "development"),
		Port:         port, RouterStartPort: routerPort, DatabaseMaxOpen: maxOpen, DatabaseMaxIdle: maxIdle,
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
	return config, nil
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
