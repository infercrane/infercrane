package checkpointcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const PolicySchemaVersion = "infercrane.checkpoint-catalog-policy/v1"

// Policy holds the human review decisions the sync tool applies. Approving a
// publisher or a format is a reviewed policy change; everything else is
// derived from the recorded snapshot.
type Policy struct {
	SchemaVersion      string              `json:"schema_version"`
	ReviewedAt         string              `json:"reviewed_at"`
	ApprovedLicenses   []LicenseRule       `json:"approved_licenses"`
	RunnableHardware   []HardwareTarget    `json:"runnable_hardware"`
	Publishers         []PublisherPolicy   `json:"publishers"`
	LicenseResolutions []LicenseResolution `json:"license_resolutions,omitempty"`
	EngineResolutions  []EngineResolution  `json:"engine_resolutions,omitempty"`
	Exclusions         []Exclusion         `json:"exclusions,omitempty"`
}

type LicenseRule struct {
	Tag  string `json:"tag"`
	SPDX string `json:"spdx"`
}

type HardwareTarget struct {
	Vendor        string   `json:"vendor"`
	Architectures []string `json:"architectures"`
}

type PublisherPolicy struct {
	Organization   string         `json:"organization"`
	Name           string         `json:"name"`
	HardwareVendor string         `json:"hardware_vendor"`
	ReviewState    string         `json:"review_state"`
	CreatedAfter   string         `json:"created_after"`
	Formats        []FormatPolicy `json:"formats"`
}

// FormatPolicy is a reviewed decision about one publisher's format. For a
// quantized checkpoint Scheme is the quantization scheme; for a speculator it
// is the speculative-decoding method.
type FormatPolicy struct {
	Kind        string         `json:"kind"`
	Tool        string         `json:"tool,omitempty"`
	Scheme      string         `json:"scheme"`
	ReviewState string         `json:"review_state"`
	Hardware    HardwareTarget `json:"hardware"`
	Runtimes    []string       `json:"runtimes"`
}

// LicenseResolution records a reviewer's resolution of a non-standard
// license tag (for example "other") for one repository.
type LicenseResolution struct {
	Repository string `json:"repository"`
	SPDX       string `json:"spdx"`
	Note       string `json:"note"`
}

// EngineResolution records the first engine release that contains a pull
// request a model card requires.
type EngineResolution struct {
	PullRequest    string `json:"pull_request"`
	MinimumVersion string `json:"minimum_version"`
	Note           string `json:"note"`
}

// Exclusion removes a repository, or one revision of it, from the catalog.
// With Engines set it instead blocks those engines for that revision, for
// card statements the parser cannot read safely.
type Exclusion struct {
	Repository string   `json:"repository"`
	Revision   string   `json:"revision,omitempty"`
	Engines    []string `json:"engines,omitempty"`
	Reason     string   `json:"reason"`
}

func DecodePolicy(data []byte) (Policy, string, error) {
	var policy Policy
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, "", fmt.Errorf("decode checkpoint catalog policy: %w", err)
	}
	if err := ValidatePolicy(policy); err != nil {
		return Policy{}, "", err
	}
	sum := sha256.Sum256(data)
	return policy, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func ValidatePolicy(policy Policy) error {
	if policy.SchemaVersion != PolicySchemaVersion || !datePattern.MatchString(policy.ReviewedAt) || len(policy.Publishers) == 0 || len(policy.RunnableHardware) == 0 {
		return errors.New("checkpoint catalog policy requires a schema, review date, runnable hardware, and publishers")
	}
	for _, target := range policy.RunnableHardware {
		if target.Vendor != "nvidia" || len(target.Architectures) == 0 {
			return errors.New("the product can run only explicit NVIDIA architectures today")
		}
	}
	for _, rule := range policy.ApprovedLicenses {
		if rule.Tag == "" || rule.SPDX == "" {
			return errors.New("approved licenses require a tag and an SPDX identifier")
		}
	}
	seen := map[string]struct{}{}
	for _, publisher := range policy.Publishers {
		if publisher.Organization == "" || publisher.Name == "" || publisher.HardwareVendor == "" || !datePattern.MatchString(publisher.CreatedAfter) || publisher.ReviewState != ReviewApproved && publisher.ReviewState != ReviewPending {
			return fmt.Errorf("publisher policy %q is incomplete", publisher.Organization)
		}
		if _, duplicate := seen[strings.ToLower(publisher.Organization)]; duplicate {
			return fmt.Errorf("duplicate publisher policy %q", publisher.Organization)
		}
		seen[strings.ToLower(publisher.Organization)] = struct{}{}
		formats := map[string]struct{}{}
		for _, format := range publisher.Formats {
			if format.Kind != KindQuantized && format.Kind != KindSpeculator || format.Scheme == "" || format.Kind == KindQuantized && format.Tool == "" || format.ReviewState != ReviewApproved && format.ReviewState != ReviewPending || format.Hardware.Vendor == "" {
				return fmt.Errorf("format policy %s/%s is incomplete", publisher.Organization, format.Scheme)
			}
			key := format.Kind + "/" + format.Tool + "/" + format.Scheme
			if _, duplicate := formats[key]; duplicate {
				return fmt.Errorf("duplicate format policy %s %s", publisher.Organization, key)
			}
			formats[key] = struct{}{}
		}
	}
	for _, resolution := range policy.LicenseResolutions {
		if resolution.Repository == "" || resolution.SPDX == "" || resolution.Note == "" {
			return errors.New("license resolutions require a repository, SPDX identifier, and note")
		}
	}
	for _, resolution := range policy.EngineResolutions {
		if !slices.Contains([]string{"vllm-project/vllm", "sgl-project/sglang"}, strings.Split(resolution.PullRequest, "#")[0]) || resolution.MinimumVersion == "" || resolution.Note == "" {
			return errors.New("engine resolutions require a known engine pull request, release, and note")
		}
	}
	for _, exclusion := range policy.Exclusions {
		if exclusion.Repository == "" || exclusion.Reason == "" || exclusion.Revision != "" && !ImmutableRevision(exclusion.Revision) || len(exclusion.Engines) > 0 && exclusion.Revision == "" {
			return errors.New("exclusions require a repository and reason; engine blocks require an immutable revision")
		}
	}
	return nil
}

// Publisher returns the policy for one organization.
func (policy Policy) Publisher(organization string) (PublisherPolicy, bool) {
	for _, publisher := range policy.Publishers {
		if strings.EqualFold(publisher.Organization, organization) {
			return publisher, true
		}
	}
	return PublisherPolicy{}, false
}

// Format returns the reviewed decision for one publisher format.
func (publisher PublisherPolicy) Format(kind, tool, scheme string) (FormatPolicy, bool) {
	for _, format := range publisher.Formats {
		if format.Kind == kind && format.Scheme == scheme && (kind == KindSpeculator || format.Tool == tool) {
			return format, true
		}
	}
	return FormatPolicy{}, false
}

// Runnable reports whether the product can run this hardware target today.
func (policy Policy) Runnable(target HardwareTarget) bool {
	if len(target.Architectures) == 0 {
		return false
	}
	for _, runnable := range policy.RunnableHardware {
		if runnable.Vendor != target.Vendor {
			continue
		}
		for _, architecture := range target.Architectures {
			if !slices.Contains(runnable.Architectures, architecture) {
				return false
			}
		}
		return true
	}
	return false
}

// License resolves an upstream license tag for a repository.
func (policy Policy) License(repository, tag string) (string, string) {
	for _, resolution := range policy.LicenseResolutions {
		if strings.EqualFold(resolution.Repository, repository) {
			return resolution.SPDX, LicenseApproved
		}
	}
	for _, rule := range policy.ApprovedLicenses {
		if strings.EqualFold(rule.Tag, tag) {
			return rule.SPDX, LicenseApproved
		}
	}
	return "", LicenseUnresolved
}

// EngineRelease returns the reviewed release that contains a pull request.
func (policy Policy) EngineRelease(pullRequest string) (string, bool) {
	for _, resolution := range policy.EngineResolutions {
		if strings.EqualFold(resolution.PullRequest, pullRequest) {
			return resolution.MinimumVersion, true
		}
	}
	return "", false
}

// Excluded returns the reviewer's reason for excluding a repository revision.
// An empty revision asks whether the whole repository is excluded.
func (policy Policy) Excluded(repository, revision string) (string, bool) {
	for _, exclusion := range policy.Exclusions {
		if len(exclusion.Engines) == 0 && strings.EqualFold(exclusion.Repository, repository) && (exclusion.Revision == "" || strings.EqualFold(exclusion.Revision, revision)) {
			return exclusion.Reason, true
		}
	}
	return "", false
}

// EngineBlocks returns reviewer-recorded engine blocks for one revision.
func (policy Policy) EngineBlocks(repository, revision string) []EngineRequirement {
	var blocks []EngineRequirement
	for _, exclusion := range policy.Exclusions {
		if len(exclusion.Engines) == 0 || !strings.EqualFold(exclusion.Repository, repository) || !strings.EqualFold(exclusion.Revision, revision) {
			continue
		}
		for _, engine := range exclusion.Engines {
			blocks = append(blocks, EngineRequirement{Engine: engine, State: EngineBlockedByReview, Note: exclusion.Reason})
		}
	}
	return blocks
}
