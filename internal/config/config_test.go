package config

import "testing"

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

func TestLoadForDiagnosticsAllowsMissingAPIKey(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "")
	config, err := LoadForDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if config.APIKey != "" {
		t.Fatal("diagnostic config must not invent an API key")
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
