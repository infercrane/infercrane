// Package kernelgen writes GPU kernel candidates for a measured hotspot. A
// language model only returns source text and has no tools. Every draft is
// untrusted, runs only inside the configured isolated evaluator, and is kept
// or rejected by internal/kernelresearch rather than by the model.
package kernelgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/kernelplanner"
)

const (
	PackSchemaV1 = "infercrane.kernel-pack/v1"

	maxPackBytes         = 1 << 20
	defaultSourceBytes   = 128 << 10
	defaultPlausibleGain = 3.0
)

// Pack is the human-reviewed contract for one operator family. It is the only
// description of correctness a draft sees; hidden test distributions live in
// the evaluator and are never included in a prompt.
type Pack struct {
	SchemaVersion       string                       `json:"schema_version"`
	Family              kernelplanner.OperatorFamily `json:"family"`
	Backend             string                       `json:"backend"`
	License             string                       `json:"license"`
	Filename            string                       `json:"filename"`
	Contract            string                       `json:"contract"`
	RequiredSubstrings  []string                     `json:"required_substrings"`
	ForbiddenSubstrings []string                     `json:"forbidden_substrings"`
	MaxSourceBytes      int                          `json:"max_source_bytes,omitempty"`
	MaxPlausibleSpeedup float64                      `json:"max_plausible_speedup,omitempty"`
	// Integration is a directory that routes a serving runtime to the kernel.
	Integration string   `json:"integration,omitempty"`
	Runtimes    []string `json:"runtimes,omitempty"`
	// CandidateArgs select the kernel at launch. Control arms never receive them.
	CandidateArgs []string `json:"candidate_args,omitempty"`
}

var safeFamily = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// LoadPack reads <directory>/<family>/pack.json.
func LoadPack(directory string, family kernelplanner.OperatorFamily) (Pack, error) {
	if !safeFamily.MatchString(string(family)) {
		return Pack{}, fmt.Errorf("invalid operator family %q", family)
	}
	path := filepath.Join(directory, string(family), "pack.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return Pack{}, fmt.Errorf("no reviewed kernel pack for %s: %w", family, err)
	}
	if len(body) > maxPackBytes {
		return Pack{}, errors.New("kernel pack exceeds the size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var pack Pack
	if err = decoder.Decode(&pack); err != nil {
		return Pack{}, fmt.Errorf("decode kernel pack: %w", err)
	}
	if pack.Family != family {
		return Pack{}, fmt.Errorf("kernel pack declares family %q, expected %q", pack.Family, family)
	}
	return pack.normalized()
}

func (p Pack) normalized() (Pack, error) {
	if p.SchemaVersion != PackSchemaV1 || p.Backend == "" || p.License == "" || p.Filename == "" || filepath.Base(p.Filename) != p.Filename || strings.TrimSpace(p.Contract) == "" {
		return Pack{}, errors.New("kernel pack requires the v1 schema, backend, license, plain filename, and contract")
	}
	if p.MaxSourceBytes <= 0 {
		p.MaxSourceBytes = defaultSourceBytes
	}
	if p.MaxPlausibleSpeedup <= 0 {
		p.MaxPlausibleSpeedup = defaultPlausibleGain
	}
	return p, nil
}

// Screen is a cheap filter that saves GPU spend on drafts which obviously
// avoid writing a kernel. It is not a security boundary; isolation and the
// hidden correctness suite are.
func (p Pack) Screen(source string) error {
	if strings.TrimSpace(source) == "" {
		return errors.New("draft is empty")
	}
	if len(source) > p.MaxSourceBytes {
		return fmt.Errorf("draft exceeds %d bytes", p.MaxSourceBytes)
	}
	for _, required := range p.RequiredSubstrings {
		if !strings.Contains(source, required) {
			return fmt.Errorf("draft must contain %q", required)
		}
	}
	for _, forbidden := range p.ForbiddenSubstrings {
		if strings.Contains(source, forbidden) {
			return fmt.Errorf("draft must not contain %q", forbidden)
		}
	}
	return nil
}
