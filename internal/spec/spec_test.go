package spec

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "deployment.yaml")
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestLoadServerlessArtifactAndRuntimeIdentity(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: qwen-serverless
model:
  id: Qwen/Qwen3-8B
  revision: 0123456789abcdef0123456789abcdef01234567
runtime:
  engine: vllm
  version: 0.10.2
  args: [--enable-prefix-caching]
compute:
  mode: serverless
resources:
  gpu: L40S
provider:
  cloud: runpod
scaling:
  min_replicas: 0
  max_replicas: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Compute.Mode != "serverless" || loaded.Scaling.MinReplicas != 0 || loaded.Scaling.MaxReplicas != 4 || loaded.Model.Revision == "" || loaded.Runtime.Version != "0.10.2" {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestLoadElasticDefaults(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: qwen
model: {id: Qwen/Qwen3-8B}
resources: {gpu: L40S}
provider: {cloud: runpod}
`))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Compute.Mode != "elastic" || loaded.Runtime.Engine != "vllm" || loaded.Scaling.MinReplicas != 1 || loaded.Scaling.MaxReplicas != 1 || loaded.Routing.Strategy != "round-robin" {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestLoadRejectsNonzeroServerlessMinimum(t *testing.T) {
	_, err := Load(writeSpec(t, `
name: qwen
model: {id: Qwen/Qwen3-8B}
compute: {mode: serverless}
resources: {gpu: L40S}
provider: {cloud: runpod}
scaling: {min_replicas: 1, max_replicas: 4}
`))
	if err == nil {
		t.Fatal("expected serverless minimum validation error")
	}
}

func TestLoadRejectsExcludedRuntimeAndCloud(t *testing.T) {
	for name, override := range map[string]string{
		"runtime": "runtime: {engine: sglang}\nprovider: {cloud: runpod}",
		"cloud":   "runtime: {engine: vllm}\nprovider: {cloud: aws}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeSpec(t, "name: qwen\nmodel: {id: Qwen/Qwen3-8B}\nresources: {gpu: L40S}\n"+override+"\n"))
			if err == nil {
				t.Fatal("expected v0.1 exclusion error")
			}
		})
	}
}
