package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSecretOrEnvironmentUsesEnvironmentAndClearsIt(t *testing.T) {
	const name = "INFERCRANE_TEST_OPENROUTER_SECRET"
	t.Setenv(name, "  provider-token  ")

	got, err := readSecretOrEnvironment("", name)
	if err != nil {
		t.Fatalf("read environment secret: %v", err)
	}
	if got != "provider-token" {
		t.Fatalf("secret = %q, want provider-token", got)
	}
	if _, ok := os.LookupEnv(name); ok {
		t.Fatalf("%s remained in the process environment", name)
	}
}

func TestReadSecretOrEnvironmentPrefersOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider-token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const name = "INFERCRANE_TEST_OPENROUTER_SECRET_FILE"
	t.Setenv(name, "environment-token")

	got, err := readSecretOrEnvironment(path, name)
	if err != nil {
		t.Fatalf("read file secret: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("secret = %q, want file-token", got)
	}
}
