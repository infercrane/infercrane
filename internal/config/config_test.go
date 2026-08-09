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
