package acceleratorworker

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalArtifactStorePublishesImmutableContentAddress(t *testing.T) {
	store, err := NewLocalArtifactStore(filepath.Join(t.TempDir(), "artifacts"), "https://worker.example", 1024)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(context.Background(), "kernel", strings.NewReader("payload"), 7)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.SHA256 != "sha256:239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5" || !strings.HasSuffix(artifact.URI, strings.TrimPrefix(artifact.SHA256, "sha256:")) {
		t.Fatalf("artifact=%+v", artifact)
	}
	reader, size, err := store.Open(context.Background(), strings.TrimPrefix(artifact.SHA256, "sha256:"))
	if err != nil || size != 7 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "payload" {
		t.Fatalf("payload=%q", data)
	}
}

func TestLocalArtifactStoreRejectsSizeMismatch(t *testing.T) {
	store, err := NewLocalArtifactStore(filepath.Join(t.TempDir(), "artifacts"), "http://127.0.0.1:8091", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Put(context.Background(), "kernel", strings.NewReader("payload"), 8); err == nil {
		t.Fatal("size mismatch accepted")
	}
}
