// Package modelcheck checks an open Hugging Face model that InferCrane has not
// reviewed: can the pinned runtimes load its architecture, how much GPU memory
// does it need, and what do its license and access settings say. It reads the
// Hub only from the control plane, never on the inference request path, and
// never reads or writes PostgreSQL. Every fact is what Hugging Face or the
// pinned runtime registries say; anything it cannot read stays unknown.
package modelcheck

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// RegistrySchemaVersion identifies runtime_model_registries.json.
const RegistrySchemaVersion = "infercrane.runtime-model-registries/v1"

//go:embed runtime_model_registries.json
var registryDocumentJSON []byte

// RuntimeRegistry is one pinned runtime release's model registry, read from
// its source at an exact commit by scripts/snapshot-runtime-model-registries.py.
type RuntimeRegistry struct {
	Runtime               string              `json:"runtime"`
	Version               string              `json:"version"`
	Tag                   string              `json:"tag"`
	Commit                string              `json:"commit"`
	SourceURL             string              `json:"source_url"`
	QuantizationSourceURL string              `json:"quantization_source_url,omitempty"`
	QuantizationMethods   []string            `json:"quantization_methods,omitempty"`
	Architectures         map[string][]string `json:"architectures"`
	PreviouslySupported   map[string]string   `json:"previously_supported,omitempty"`
	OutOfTree             map[string]string   `json:"out_of_tree,omitempty"`
}

// RegistrySnapshot is the committed set of runtime registries.
type RegistrySnapshot struct {
	SchemaVersion string            `json:"schema_version"`
	RetrievedAt   string            `json:"retrieved_at"`
	Runtimes      []RuntimeRegistry `json:"runtimes"`
}

// Runtime returns the registry for one runtime name.
func (s RegistrySnapshot) Runtime(name string) (RuntimeRegistry, bool) {
	for _, registry := range s.Runtimes {
		if registry.Runtime == name {
			return registry, true
		}
	}
	return RuntimeRegistry{}, false
}

var (
	registryOnce     sync.Once
	registrySnapshot RegistrySnapshot
	registryErr      error
	gitCommit        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Registries returns the embedded registry snapshot. It fails only when the
// committed file is malformed, which a unit test prevents.
func Registries() (RegistrySnapshot, error) {
	registryOnce.Do(func() {
		registrySnapshot, registryErr = parseRegistries(registryDocumentJSON)
	})
	return registrySnapshot, registryErr
}

func parseRegistries(raw []byte) (RegistrySnapshot, error) {
	var snapshot RegistrySnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return RegistrySnapshot{}, fmt.Errorf("decode runtime model registries: %w", err)
	}
	if snapshot.SchemaVersion != RegistrySchemaVersion {
		return RegistrySnapshot{}, fmt.Errorf("runtime model registries schema %q is not %q", snapshot.SchemaVersion, RegistrySchemaVersion)
	}
	seen := map[string]bool{}
	for _, registry := range snapshot.Runtimes {
		if registry.Runtime == "" || registry.Version == "" || !gitCommit.MatchString(registry.Commit) || registry.SourceURL == "" || len(registry.Architectures) == 0 {
			return RegistrySnapshot{}, errors.New("runtime model registry is missing its runtime, version, commit, source, or architectures")
		}
		if seen[registry.Runtime] {
			return RegistrySnapshot{}, fmt.Errorf("runtime model registry %q is listed twice", registry.Runtime)
		}
		seen[registry.Runtime] = true
	}
	return snapshot, nil
}
