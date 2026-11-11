// Package checkpointcatalog holds InferCrane's reviewed catalog of
// vendor-published optimized checkpoints and speculators. The catalog is
// generated offline from a recorded Hugging Face snapshot by
// tools/checkpoint-catalog-sync, reviewed as a diff, and committed. Runtime
// code only reads the embedded copy: it never fetches anything. A catalog
// entry is a candidate seed, never InferCrane performance or quality evidence.
package checkpointcatalog

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const SchemaVersion = "infercrane.checkpoint-catalog/v2"

// Entry kinds.
const (
	KindQuantized  = "quantized_checkpoint"
	KindSpeculator = "speculator"
)

// Lineage states for the link between a build and its base model.
const (
	LineageExact             = "exact"
	LineagePublisherDeclared = "publisher_declared"
)

// Review, hardware, license, eligibility, and revision states.
const (
	ReviewApproved = "approved"
	ReviewPending  = "pending"

	SupportRunnable    = "runnable"
	SupportUnsupported = "unsupported_hardware"
	SupportUnknown     = "unknown"

	LicenseApproved   = "approved"
	LicenseUnresolved = "unresolved"

	EligibilityCandidate   = "candidate"
	EligibilityCatalogOnly = "catalog_only"

	RevisionCurrent    = "current"
	RevisionSuperseded = "superseded"

	EvidenceUnmeasured = "unmeasured"

	EngineMinimumVersion   = "minimum_version"
	EngineUnreleasedChange = "requires_unreleased_change"
	EngineBlockedByReview  = "blocked_by_review"
)

//go:embed catalog.json
var embedded []byte

type Catalog struct {
	SchemaVersion string      `json:"schema_version"`
	ReviewedAt    string      `json:"reviewed_at"`
	Snapshot      Snapshot    `json:"snapshot"`
	Publishers    []Publisher `json:"publishers"`
	Entries       []Entry     `json:"entries"`
}

// Snapshot identifies the recorded upstream observation and the reviewed
// policy that produced the catalog.
type Snapshot struct {
	Source       string `json:"source"`
	FetchedAt    string `json:"fetched_at"`
	Digest       string `json:"digest"`
	PolicyDigest string `json:"policy_digest"`
}

type Publisher struct {
	Organization   string `json:"organization"`
	Name           string `json:"name"`
	HardwareVendor string `json:"hardware_vendor"`
	ReviewState    string `json:"review_state"`
}

type Entry struct {
	ID                 string              `json:"id"`
	Kind               string              `json:"kind"`
	Publisher          string              `json:"publisher"`
	Repository         string              `json:"repository"`
	Revision           string              `json:"revision"`
	RevisionState      string              `json:"revision_state"`
	SupersededBy       string              `json:"superseded_by,omitempty"`
	CreatedAt          string              `json:"created_at"`
	LastModified       string              `json:"last_modified"`
	Base               Base                `json:"base"`
	Quantization       *Quantization       `json:"quantization,omitempty"`
	Speculator         *Speculator         `json:"speculator,omitempty"`
	Hardware           Hardware            `json:"hardware"`
	Runtimes           []string            `json:"runtimes"`
	EngineRequirements []EngineRequirement `json:"engine_requirements,omitempty"`
	License            License             `json:"license"`
	Artifact           Artifact            `json:"artifact"`
	PublisherPriors    []PriorTable        `json:"publisher_priors,omitempty"`
	Review             Review              `json:"review"`
	Eligibility        Eligibility         `json:"eligibility"`
	SourceURL          string              `json:"source_url"`
	Limitations        []string            `json:"limitations"`
}

// Base links a build to the model it was derived from. DeclaredRevision is set
// only when the publisher states the exact source commit; ScreeningRevision is
// the base revision a campaign should pin when it screens this build.
type Base struct {
	Repository              string `json:"repository"`
	DeclaredRevision        string `json:"declared_revision,omitempty"`
	Lineage                 string `json:"lineage"`
	ScreeningRevision       string `json:"screening_revision,omitempty"`
	ScreeningRevisionSource string `json:"screening_revision_source,omitempty"`
}

type Quantization struct {
	Tool        string `json:"tool"`
	ToolVersion string `json:"tool_version"`
	Method      string `json:"method"`
	Scheme      string `json:"scheme"`
}

type Speculator struct {
	Method  string   `json:"method"`
	Targets []string `json:"targets"`
}

type Hardware struct {
	Vendor         string   `json:"vendor"`
	Architectures  []string `json:"architectures"`
	ProductSupport string   `json:"product_support"`
}

// EngineRequirement is parsed from the model card. A pull-request requirement
// keeps the entry off that engine until a reviewer records the release that
// contains it.
type EngineRequirement struct {
	Engine         string `json:"engine"`
	State          string `json:"state"`
	MinimumVersion string `json:"minimum_version,omitempty"`
	PullRequest    string `json:"pull_request,omitempty"`
	Note           string `json:"note,omitempty"`
}

type License struct {
	Tag    string `json:"tag"`
	SPDX   string `json:"spdx,omitempty"`
	State  string `json:"state"`
	Digest string `json:"digest,omitempty"`
}

type Artifact struct {
	Format          string `json:"format"`
	ManifestDigest  string `json:"manifest_digest"`
	ModelCardDigest string `json:"model_card_digest,omitempty"`
	FileCount       int    `json:"file_count"`
	SizeBytes       int64  `json:"size_bytes"`
}

// PriorTable is a publisher-reported table copied from the model card at the
// pinned revision. It is discovery context only and never qualification
// evidence.
type PriorTable struct {
	Source        string     `json:"source"`
	EvidenceState string     `json:"evidence_state"`
	Columns       []string   `json:"columns"`
	Rows          [][]string `json:"rows"`
}

type Review struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons,omitempty"`
}

type Eligibility struct {
	State    string   `json:"state"`
	Blockers []string `json:"blockers,omitempty"`
}

// QuantizationTools lists the tools whose checkpoints can become proposal
// candidates. Other tools may appear in the catalog, but stay catalog-only.
func QuantizationTools() []string {
	return []string{"llm-compressor", "modelopt"}
}

// loadDefault decodes the embedded catalog once per process. Proposals and
// model discovery call Default on every request.
var loadDefault = sync.OnceValues(func() (Catalog, error) { return Decode(embedded) })

// Default returns the embedded, reviewed catalog. Callers share it and must
// treat it as read-only.
func Default() (Catalog, error) {
	return loadDefault()
}

func MustDefault() Catalog {
	catalog, err := Default()
	if err != nil {
		panic(err)
	}
	return catalog
}

func Decode(data []byte) (Catalog, error) {
	var catalog Catalog
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode checkpoint catalog: %w", err)
	}
	if err := Validate(catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func Validate(catalog Catalog) error {
	if catalog.SchemaVersion != SchemaVersion || !datePattern.MatchString(catalog.ReviewedAt) {
		return errors.New("checkpoint catalog requires a supported schema and a review date")
	}
	if !strings.HasPrefix(catalog.Snapshot.Source, "https://") || catalog.Snapshot.FetchedAt == "" || !Digest(catalog.Snapshot.Digest) || !Digest(catalog.Snapshot.PolicyDigest) {
		return errors.New("checkpoint catalog requires snapshot and policy provenance")
	}
	publishers := map[string]Publisher{}
	for _, publisher := range catalog.Publishers {
		if publisher.Organization == "" || publisher.Name == "" || publisher.HardwareVendor == "" || publisher.ReviewState != ReviewApproved && publisher.ReviewState != ReviewPending {
			return fmt.Errorf("publisher %q is incomplete", publisher.Organization)
		}
		if _, duplicate := publishers[publisher.Organization]; duplicate {
			return fmt.Errorf("duplicate publisher %q", publisher.Organization)
		}
		publishers[publisher.Organization] = publisher
	}
	ids := map[string]struct{}{}
	pins := map[string]struct{}{}
	for _, entry := range catalog.Entries {
		publisher, known := publishers[entry.Publisher]
		if !known {
			return fmt.Errorf("entry %q has an unknown publisher", entry.ID)
		}
		if err := ValidateEntry(entry); err != nil {
			return fmt.Errorf("entry %q: %w", entry.ID, err)
		}
		if publisher.ReviewState != ReviewApproved && entry.Review.State == ReviewApproved {
			return fmt.Errorf("entry %q is approved but its publisher is pending review", entry.ID)
		}
		if _, duplicate := ids[entry.ID]; duplicate {
			return fmt.Errorf("duplicate checkpoint entry %q", entry.ID)
		}
		ids[entry.ID] = struct{}{}
		pin := strings.ToLower(entry.Repository + "@" + entry.Revision)
		if _, duplicate := pins[pin]; duplicate {
			return fmt.Errorf("duplicate checkpoint revision %q", pin)
		}
		pins[pin] = struct{}{}
	}
	return nil
}

func ValidateEntry(entry Entry) error {
	if entry.ID == "" || entry.Repository == "" || !strings.HasPrefix(strings.ToLower(entry.Repository), strings.ToLower(entry.Publisher)+"/") || entry.CreatedAt == "" || entry.LastModified == "" || len(entry.Limitations) == 0 {
		return errors.New("identity, publisher repository, timestamps, and limitations are required")
	}
	if !ImmutableRevision(entry.Revision) || !Digest(entry.Artifact.ManifestDigest) || entry.Artifact.ModelCardDigest != "" && !Digest(entry.Artifact.ModelCardDigest) || entry.License.Digest != "" && !Digest(entry.License.Digest) || entry.Artifact.Format == "" || entry.Artifact.FileCount < 1 {
		return errors.New("revision and artifact manifests must be immutable")
	}
	if entry.SourceURL != "https://huggingface.co/"+entry.Repository {
		return errors.New("source must be the HTTPS Hugging Face repository")
	}
	switch entry.RevisionState {
	case RevisionCurrent:
		if entry.SupersededBy != "" {
			return errors.New("a current revision cannot be superseded")
		}
	case RevisionSuperseded:
		if !ImmutableRevision(entry.SupersededBy) || entry.SupersededBy == entry.Revision {
			return errors.New("a superseded revision must name its immutable successor")
		}
	default:
		return errors.New("unsupported revision state")
	}
	switch entry.Kind {
	case KindQuantized:
		if entry.Quantization == nil || entry.Speculator != nil || entry.Quantization.Tool == "" || entry.Quantization.ToolVersion == "" || entry.Quantization.Scheme == "" {
			return errors.New("a quantized checkpoint requires its tool, version, and scheme")
		}
	case KindSpeculator:
		if entry.Speculator == nil || entry.Quantization != nil || entry.Speculator.Method == "" || len(entry.Speculator.Targets) == 0 {
			return errors.New("a speculator requires its method and target models")
		}
	default:
		return errors.New("unsupported entry kind")
	}
	if entry.Base.Repository == "" || entry.Base.Lineage != LineageExact && entry.Base.Lineage != LineagePublisherDeclared {
		return errors.New("a build must declare its base model and lineage")
	}
	if entry.Base.Lineage == LineageExact && (!ImmutableRevision(entry.Base.DeclaredRevision) || entry.Base.ScreeningRevision != entry.Base.DeclaredRevision) {
		return errors.New("exact lineage requires the declared immutable base revision as the screening revision")
	}
	if entry.Base.ScreeningRevision != "" && (!ImmutableRevision(entry.Base.ScreeningRevision) || !strings.HasPrefix(entry.Base.ScreeningRevisionSource, "https://huggingface.co/")) {
		return errors.New("a screening base revision must be immutable and sourced")
	}
	if entry.Base.ScreeningRevision == "" && entry.Base.ScreeningRevisionSource != "" {
		return errors.New("a screening source requires a screening revision")
	}
	if entry.Hardware.Vendor == "" || !slices.Contains([]string{SupportRunnable, SupportUnsupported, SupportUnknown}, entry.Hardware.ProductSupport) {
		return errors.New("hardware vendor and product support are required")
	}
	if entry.Hardware.Vendor != "nvidia" && entry.Hardware.ProductSupport == SupportRunnable {
		return errors.New("only NVIDIA hardware is runnable by the product today")
	}
	if entry.Hardware.ProductSupport == SupportRunnable && len(entry.Hardware.Architectures) == 0 {
		return errors.New("runnable hardware requires explicit architectures")
	}
	if entry.License.Tag == "" || entry.License.State != LicenseApproved && entry.License.State != LicenseUnresolved || entry.License.State == LicenseApproved && entry.License.SPDX == "" {
		return errors.New("license tag and resolution state are required")
	}
	if entry.Review.State != ReviewApproved && entry.Review.State != ReviewPending || entry.Review.State == ReviewPending && len(entry.Review.Reasons) == 0 {
		return errors.New("review state and pending reasons are required")
	}
	for _, requirement := range entry.EngineRequirements {
		switch {
		case requirement.Engine == "":
			return errors.New("engine requirement requires an engine")
		case requirement.State == EngineMinimumVersion && requirement.MinimumVersion == "":
			return errors.New("minimum-version requirement requires a version")
		case requirement.State == EngineUnreleasedChange && requirement.PullRequest == "":
			return errors.New("unreleased-change requirement requires a pull request")
		case requirement.State == EngineBlockedByReview && requirement.Note == "":
			return errors.New("a reviewer engine block requires a note")
		case requirement.State != EngineMinimumVersion && requirement.State != EngineUnreleasedChange && requirement.State != EngineBlockedByReview:
			return errors.New("unsupported engine requirement state")
		}
	}
	for _, prior := range entry.PublisherPriors {
		if prior.EvidenceState != EvidenceUnmeasured || prior.Source == "" || len(prior.Columns) == 0 || len(prior.Rows) == 0 {
			return errors.New("publisher priors must be sourced and remain unmeasured")
		}
	}
	switch entry.Eligibility.State {
	case EligibilityCandidate:
		if len(entry.Eligibility.Blockers) != 0 || entry.Kind != KindQuantized || entry.Review.State != ReviewApproved || entry.Hardware.ProductSupport != SupportRunnable || entry.License.State != LicenseApproved || len(entry.Runtimes) == 0 || entry.Artifact.ModelCardDigest == "" || !slices.Contains(QuantizationTools(), entry.Quantization.Tool) {
			return errors.New("a candidate must be an approved, licensed, runnable quantized checkpoint from a supported tool with a pinned model card")
		}
	case EligibilityCatalogOnly:
		if len(entry.Eligibility.Blockers) == 0 {
			return errors.New("a catalog-only entry must name its blockers")
		}
	default:
		return errors.New("unsupported eligibility state")
	}
	return nil
}

// Candidates returns the entries that may seed proposal candidates.
func (catalog Catalog) Candidates() []Entry {
	var out []Entry
	for _, entry := range catalog.Entries {
		if entry.Eligibility.State == EligibilityCandidate {
			out = append(out, entry)
		}
	}
	return out
}

// PublisherName returns the reviewed display name for an organization.
func (catalog Catalog) PublisherName(organization string) string {
	for _, publisher := range catalog.Publishers {
		if publisher.Organization == organization {
			return publisher.Name
		}
	}
	return organization
}

// Match returns candidate entries that can be screened for one exact serving
// tuple. It rejects accelerators outside the declared architectures, exact
// lineage entries whose base revision differs, and engines whose pinned
// version does not satisfy the publisher's requirement.
func (catalog Catalog) Match(baseRepository, baseRevision, runtimeName, runtimeVersion, gpu string) []Entry {
	architecture := AcceleratorArchitecture(gpu)
	if architecture == "" {
		return nil
	}
	var matches []Entry
	for _, entry := range catalog.Candidates() {
		if !strings.EqualFold(strings.TrimSpace(entry.Base.Repository), strings.TrimSpace(baseRepository)) || !containsFold(entry.Runtimes, runtimeName) || !containsFold(entry.Hardware.Architectures, architecture) {
			continue
		}
		if entry.Base.Lineage == LineageExact && !strings.EqualFold(entry.Base.DeclaredRevision, baseRevision) {
			continue
		}
		if !EngineSatisfied(entry.EngineRequirements, runtimeName, runtimeVersion) {
			continue
		}
		matches = append(matches, entry)
	}
	return matches
}

// EngineSatisfied reports whether a pinned engine version meets every
// requirement the publisher declared for that engine.
func EngineSatisfied(requirements []EngineRequirement, engine, version string) bool {
	for _, requirement := range requirements {
		if !strings.EqualFold(requirement.Engine, engine) {
			continue
		}
		if requirement.State != EngineMinimumVersion {
			return false
		}
		if version == "" || CompareVersions(version, requirement.MinimumVersion) < 0 {
			return false
		}
	}
	return true
}

// CompareVersions compares dotted engine versions such as 0.5.10.post1.
func CompareVersions(a, b string) int {
	left, right := versionParts(a), versionParts(b)
	for index := 0; index < max(len(left), len(right)); index++ {
		var l, r int
		if index < len(left) {
			l = left[index]
		}
		if index < len(right) {
			r = right[index]
		}
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(version string) []int {
	version = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(version)), "v")
	var parts []int
	for _, field := range strings.FieldsFunc(version, func(r rune) bool { return r == '.' || r == '-' || r == '+' }) {
		field = strings.TrimPrefix(field, "post")
		value, err := strconv.Atoi(field)
		if err != nil {
			break
		}
		parts = append(parts, value)
	}
	return parts
}

// AcceleratorArchitecture maps a concrete NVIDIA GPU name to the architecture
// family the catalog uses. Unknown or non-NVIDIA names return "".
func AcceleratorArchitecture(gpu string) string {
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(gpu))
	for _, marker := range []string{"gb200", "gb300", "b200", "b300", "rtxpro6000", "rtx5090"} {
		if strings.Contains(normalized, marker) {
			return "blackwell"
		}
	}
	for _, marker := range []string{"gh200", "h100", "h200", "h800", "h20"} {
		if strings.Contains(normalized, marker) {
			return "hopper"
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

func ImmutableRevision(value string) bool {
	if len(value) < 40 || len(value) > 64 || len(value)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
