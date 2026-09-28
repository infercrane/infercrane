// Package modeloptcatalog contains a reviewed, immutable catalog of
// publisher-provided NVIDIA Model Optimizer checkpoints. A catalog match is a
// candidate seed, never InferCrane performance or quality evidence.
package modeloptcatalog

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const SchemaVersion = "infercrane.modelopt.checkpoint-catalog/v1"

//go:embed catalog.json
var embedded []byte

type Seed struct {
	ID                       string   `json:"id"`
	Publisher                string   `json:"publisher"`
	BaseRepository           string   `json:"base_repository"`
	BaseRevision             string   `json:"base_revision,omitempty"`
	LineageState             string   `json:"lineage_state"`
	OutputRepository         string   `json:"output_repository"`
	OutputRevision           string   `json:"output_revision"`
	ManifestDigest           string   `json:"manifest_digest"`
	ModelCardDigest          string   `json:"model_card_digest"`
	LicenseSPDX              string   `json:"license_spdx"`
	LicenseDigest            string   `json:"license_digest,omitempty"`
	Tool                     string   `json:"tool"`
	ToolVersion              string   `json:"tool_version"`
	Algorithm                string   `json:"algorithm"`
	Format                   string   `json:"format"`
	FileCount                int      `json:"file_count"`
	SizeBytes                int64    `json:"size_bytes"`
	Runtimes                 []string `json:"runtimes"`
	AcceleratorArchitectures []string `json:"accelerator_architectures"`
	InputModalities          []string `json:"input_modalities"`
	ContextWindowTokens      int      `json:"context_window_tokens"`
	SourceURL                string   `json:"source_url"`
	Limitations              []string `json:"limitations"`
}

type Catalog struct {
	SchemaVersion string `json:"schema_version"`
	ReviewedAt    string `json:"reviewed_at"`
	Source        string `json:"source"`
	Seeds         []Seed `json:"seeds"`
}

func Default() (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(embedded, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode embedded ModelOpt catalog: %w", err)
	}
	if err := Validate(catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func MustDefault() Catalog {
	catalog, err := Default()
	if err != nil {
		panic(err)
	}
	return catalog
}

func Validate(catalog Catalog) error {
	if catalog.SchemaVersion != SchemaVersion || strings.TrimSpace(catalog.ReviewedAt) == "" || !strings.HasPrefix(catalog.Source, "https://") || len(catalog.Seeds) == 0 {
		return errors.New("ModelOpt catalog requires a supported schema, review date, HTTPS source, and seeds")
	}
	seen := map[string]struct{}{}
	for _, seed := range catalog.Seeds {
		if err := ValidateSeed(seed); err != nil {
			return fmt.Errorf("seed %q: %w", seed.ID, err)
		}
		if _, duplicate := seen[seed.ID]; duplicate {
			return fmt.Errorf("duplicate ModelOpt seed %q", seed.ID)
		}
		seen[seed.ID] = struct{}{}
	}
	return nil
}

func ValidateSeed(seed Seed) error {
	if seed.ID == "" || seed.Publisher == "" || seed.BaseRepository == "" || seed.OutputRepository == "" || seed.Tool != "modelopt" || seed.ToolVersion == "" || seed.Algorithm == "" || seed.Format == "" || seed.LicenseSPDX == "" || seed.FileCount < 1 || seed.SizeBytes < 1 || len(seed.Runtimes) == 0 || len(seed.AcceleratorArchitectures) == 0 || len(seed.Limitations) == 0 {
		return errors.New("checkpoint identity, provenance, compatibility, license, and limitations are required")
	}
	if seed.LineageState != "exact" && seed.LineageState != "publisher_declared" {
		return errors.New("lineage_state must be exact or publisher_declared")
	}
	if seed.LineageState == "exact" && !immutableRevision(seed.BaseRevision) {
		return errors.New("exact lineage requires an immutable base revision")
	}
	if !immutableRevision(seed.OutputRevision) || !digest(seed.ManifestDigest) || !digest(seed.ModelCardDigest) || seed.LicenseDigest != "" && !digest(seed.LicenseDigest) {
		return errors.New("output revision and publisher manifests must be immutable")
	}
	if !strings.HasPrefix(seed.SourceURL, "https://huggingface.co/") {
		return errors.New("checkpoint source must be an HTTPS Hugging Face repository")
	}
	return nil
}

// Match returns seeds that can be screened for one exact serving tuple. It
// deliberately rejects Hopper requests for Blackwell-only checkpoints and
// exact-lineage seeds when the requested base revision differs.
func (catalog Catalog) Match(baseRepository, baseRevision, runtimeName, gpu string) []Seed {
	architecture := acceleratorArchitecture(gpu)
	var matches []Seed
	for _, seed := range catalog.Seeds {
		if !strings.EqualFold(strings.TrimSpace(seed.BaseRepository), strings.TrimSpace(baseRepository)) || !containsFold(seed.Runtimes, runtimeName) || !containsFold(seed.AcceleratorArchitectures, architecture) {
			continue
		}
		if seed.LineageState == "exact" && !strings.EqualFold(seed.BaseRevision, baseRevision) {
			continue
		}
		matches = append(matches, seed)
	}
	return matches
}

func acceleratorArchitecture(gpu string) string {
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(gpu))
	for _, marker := range []string{"gb200", "gb300", "b200", "b300", "rtxpro6000", "rtx5090"} {
		if strings.Contains(normalized, marker) {
			return "blackwell"
		}
	}
	return ""
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

func immutableRevision(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func digest(value string) bool {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
