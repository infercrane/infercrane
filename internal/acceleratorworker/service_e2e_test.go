package acceleratorworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/brezelexecutor"
)

func TestWorkerClientEndToEndWithGenericModelAndArtifactBroker(t *testing.T) {
	artifacts, err := NewLocalArtifactStore(filepath.Join(t.TempDir(), "artifacts"), "https://worker.example", 1024)
	if err != nil {
		t.Fatal(err)
	}
	handler := fixtureHandler(profilerFunc(func(_ context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
		return acceleratorlab.ProfileEvidence{
			InputDigest: input.InputDigest, Tool: "nsys", ToolVersion: "1",
			Hardware: input.Hardware, RuntimeImageDigest: input.Runtime.ImageDigest,
			Topology: input.Topology, WorkloadDigest: input.Workload.Digest,
			ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		}, nil
	}))
	handler.Artifacts = artifacts
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	artifacts.baseURL = server.URL
	client, err := acceleratorlab.NewWorkerClient(acceleratorlab.WorkerConfig{
		BaseURL: server.URL, Token: "secret", Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Capabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	intent := profileIntent()
	if !strings.Contains(intent.Model.Repository, "previously-unseen") {
		t.Fatal("fixture stopped exercising the generic model boundary")
	}
	evidence, err := client.Capture(context.Background(), intent)
	if err != nil || evidence.InputDigest != intent.InputDigest {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	payload := "portable-kernel-bundle"
	uri, err := client.Publish(context.Background(), "job-1", "source", strings.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(payload))
	reader, err := client.Open(context.Background(), brezelexecutor.ArtifactRef{
		Name: "source", URI: uri, SHA256: hex.EncodeToString(digest[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != payload {
		t.Fatalf("artifact=%q", data)
	}
}
