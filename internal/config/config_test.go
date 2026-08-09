package config

import (
	"os"
	"path/filepath"
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
	path, generated, err := InitializeClient("https://control.example", "")
	if err != nil || !generated || path != filepath.Join(root, "infercrane", "config.json") {
		t.Fatalf("path=%q generated=%t err=%v", path, generated, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%v", info.Mode().Perm())
	}
	config, err := LoadClient()
	if err != nil || config.ControlURL != "https://control.example" || len(config.APIKey) != 64 {
		t.Fatalf("config=%#v err=%v", config, err)
	}
}
