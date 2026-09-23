package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSecretRequiresAbsoluteOwnerOnlyRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "provider-key")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := readSecret(path)
	if err != nil || value != "secret" {
		t.Fatalf("readSecret() = %q, %v", value, err)
	}
	if _, err = readSecret("provider-key"); err == nil {
		t.Fatal("expected relative secret path to fail")
	}
	if err = os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err = readSecret(path); err == nil {
		t.Fatal("expected group-readable secret to fail")
	}
}
