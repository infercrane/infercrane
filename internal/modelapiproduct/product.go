// Package modelapiproduct defines the supplier-neutral public Model API
// product contract and the operator-private publication that can make a
// product callable. Discovery metadata alone never makes a product callable.
package modelapiproduct

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const (
	ProductSchemaVersion             = "model-api-product/v1"
	PublicProjectionSchemaVersion    = "model-api-public-projection/v1"
	OperatorProjectionSchemaVersion  = "model-api-operator-projection/v1"
	EntitlementSchemaVersion         = "model-api-product-entitlement/v1"
	CustomerEntitlementSchemaVersion = "model-api-customer-entitlement/v1"
)

type Availability string

const (
	AvailabilityCatalogOnly    Availability = "catalog_only"
	AvailabilityPrivatePreview Availability = "private_preview"
	AvailabilityAvailable      Availability = "available"
	AvailabilityDegraded       Availability = "degraded"
	AvailabilityUnavailable    Availability = "unavailable"
)

func (a Availability) Valid() bool {
	switch a {
	case AvailabilityCatalogOnly, AvailabilityPrivatePreview, AvailabilityAvailable, AvailabilityDegraded, AvailabilityUnavailable:
		return true
	default:
		return false
	}
}

type ClaimState string

const (
	ClaimCataloged        ClaimState = "cataloged"
	ClaimSupplierReported ClaimState = "supplier_reported"
	ClaimQualified        ClaimState = "qualified"
)

func (s ClaimState) Valid() bool {
	switch s {
	case ClaimCataloged, ClaimSupplierReported, ClaimQualified:
		return true
	default:
		return false
	}
}

type SelfHostEligibility string

const (
	SelfHostUnknown    SelfHostEligibility = "unknown"
	SelfHostEligible   SelfHostEligibility = "eligible"
	SelfHostIneligible SelfHostEligibility = "ineligible"
)

func (e SelfHostEligibility) Valid() bool {
	switch e {
	case SelfHostUnknown, SelfHostEligible, SelfHostIneligible:
		return true
	default:
		return false
	}
}

// CapabilityClaim keeps catalog metadata distinct from current qualification.
// Qualified claims need time-bounded evidence before they may be used to admit
// traffic or make a public support claim.
type CapabilityClaim struct {
	Name          string     `json:"name"`
	State         ClaimState `json:"state"`
	EvidenceID    string     `json:"evidence_id,omitempty"`
	EvidenceUntil *time.Time `json:"evidence_until,omitempty"`
}

func (c CapabilityClaim) Validate() error {
	if !validName(c.Name) || !c.State.Valid() {
		return errors.New("capability claim needs a valid name and state")
	}
	if c.State == ClaimQualified {
		if c.EvidenceID == "" || c.EvidenceUntil == nil || c.EvidenceUntil.IsZero() {
			return errors.New("qualified capability claim needs evidence and an expiry")
		}
	} else if c.EvidenceID != "" || c.EvidenceUntil != nil {
		return errors.New("unqualified capability claim cannot carry qualification evidence")
	}
	return nil
}

func (c CapabilityClaim) CurrentAt(at time.Time) bool {
	return c.State == ClaimQualified && c.EvidenceID != "" && c.EvidenceUntil != nil && c.EvidenceUntil.After(at.UTC())
}

// CallableOperation reports whether the claim is evidence for a request-path
// operation. Streaming is a modifier of several operations, not an operation
// on its own, and is checked separately when launchability is evaluated.
func (c CapabilityClaim) CallableOperation() (string, bool) {
	operation, ok := map[string]string{
		"chat-completions": "chat",
		"completions":      "completions",
		"responses":        "responses",
		"embeddings":       "embeddings",
	}[c.Name]
	return operation, ok
}

func (p Product) HasCurrentCallableCapabilityAt(at time.Time) bool {
	hasCallable, requiresStreaming, hasStreaming := false, false, false
	for _, claim := range p.Capabilities {
		if !claim.CurrentAt(at) {
			continue
		}
		if claim.Name == "streaming" {
			hasStreaming = true
			continue
		}
		operation, callable := claim.CallableOperation()
		hasCallable = hasCallable || callable
		requiresStreaming = requiresStreaming || callable && (operation == "chat" || operation == "completions" || operation == "responses")
	}
	return hasCallable && (!requiresStreaming || hasStreaming)
}

// Product is the stable, supplier-neutral identity shown to customers. Unknown
// context and support data remain absent rather than being represented as zero.
type Product struct {
	SchemaVersion       string              `json:"schema_version"`
	ID                  string              `json:"id"`
	DisplayName         string              `json:"display_name"`
	Publisher           string              `json:"publisher"`
	Description         string              `json:"description"`
	Protocol            string              `json:"protocol"`
	Tasks               []string            `json:"tasks"`
	Capabilities        []CapabilityClaim   `json:"capabilities"`
	InputModalities     []string            `json:"input_modalities"`
	OutputModalities    []string            `json:"output_modalities"`
	ContextWindowTokens *int64              `json:"context_window_tokens,omitempty"`
	Availability        Availability        `json:"availability"`
	SelfHostEligibility SelfHostEligibility `json:"self_host_eligibility"`
}

func (p Product) Validate() error {
	if p.SchemaVersion != ProductSchemaVersion {
		return fmt.Errorf("product schema_version must be %q", ProductSchemaVersion)
	}
	if !validID(p.ID) || p.DisplayName == "" || p.Publisher == "" || p.Description == "" || p.Protocol != "openai" {
		return errors.New("product identity and OpenAI-compatible protocol are required")
	}
	if !p.Availability.Valid() || !p.SelfHostEligibility.Valid() {
		return errors.New("product availability and self-host eligibility must be explicit")
	}
	if len(p.Tasks) == 0 || len(p.Capabilities) == 0 || len(p.InputModalities) == 0 || len(p.OutputModalities) == 0 {
		return errors.New("product tasks, capabilities, and modalities are required")
	}
	if err := uniqueNames(p.Tasks, "task"); err != nil {
		return err
	}
	if err := uniqueNames(p.InputModalities, "input modality"); err != nil {
		return err
	}
	if err := uniqueNames(p.OutputModalities, "output modality"); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(p.Capabilities))
	for _, capability := range p.Capabilities {
		if err := capability.Validate(); err != nil {
			return fmt.Errorf("product %q capability %q: %w", p.ID, capability.Name, err)
		}
		if _, exists := seen[capability.Name]; exists {
			return fmt.Errorf("product %q capability %q is duplicated", p.ID, capability.Name)
		}
		seen[capability.Name] = struct{}{}
	}
	if p.ContextWindowTokens != nil && *p.ContextWindowTokens <= 0 {
		return errors.New("known context window must be positive")
	}
	return nil
}

// DefaultCatalog returns the fixed public launch shelf. Every entry starts as
// catalog-only with cataloged (not qualified) capability claims and no invented
// context, price, availability, or performance evidence.
func DefaultCatalog() []Product {
	products := []Product{
		newCatalogProduct("glm-5.2", "GLM-5.2", "Z.ai", "Planned for coding, reasoning, and bilingual chat workloads.", []string{"coding", "reasoning", "chat"}),
		newCatalogProduct("glm-5.3", "GLM-5.3", "Z.ai", "Planned for reasoning and long-context workloads.", []string{"reasoning", "long-context", "chat"}),
		newCatalogProduct("glm-5.3-flash", "GLM-5.3-Flash", "Z.ai", "Planned for cost-sensitive and latency-sensitive workloads.", []string{"chat", "coding"}),
		newCatalogProduct("kimi-k3", "Kimi-K3", "Moonshot AI", "Planned for coding and agentic workflows.", []string{"coding", "agents", "chat"}),
		newCatalogProduct("kimi-k2.6", "Kimi-K2.6", "Moonshot AI", "Planned for coding, agentic, and long-context workloads.", []string{"coding", "agents", "long-context", "chat"}),
		newCatalogProduct("deepseek-v4-flash", "DeepSeek-V4-Flash", "DeepSeek", "Planned for high-throughput workloads after route qualification.", []string{"chat", "coding", "throughput"}),
	}
	sort.Slice(products, func(i, j int) bool { return products[i].ID < products[j].ID })
	return products
}

func ValidateCatalog(products []Product) error {
	if len(products) == 0 {
		return errors.New("product catalog cannot be empty")
	}
	seen := make(map[string]struct{}, len(products))
	for _, product := range products {
		if err := product.Validate(); err != nil {
			return fmt.Errorf("product %q: %w", product.ID, err)
		}
		if _, exists := seen[product.ID]; exists {
			return fmt.Errorf("product id %q is duplicated", product.ID)
		}
		seen[product.ID] = struct{}{}
	}
	return nil
}

func newCatalogProduct(id, name, publisher, description string, tasks []string) Product {
	return Product{
		SchemaVersion:       ProductSchemaVersion,
		ID:                  id,
		DisplayName:         name,
		Publisher:           publisher,
		Description:         description,
		Protocol:            "openai",
		Tasks:               append([]string(nil), tasks...),
		Capabilities:        []CapabilityClaim{{Name: "chat-completions", State: ClaimCataloged}, {Name: "streaming", State: ClaimCataloged}},
		InputModalities:     []string{"text"},
		OutputModalities:    []string{"text"},
		Availability:        AvailabilityCatalogOnly,
		SelfHostEligibility: SelfHostUnknown,
	}
}

var idPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*$`)
var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:[-_.][a-z0-9]+)*$`)

func validID(value string) bool   { return len(value) <= 128 && idPattern.MatchString(value) }
func validName(value string) bool { return len(value) <= 128 && namePattern.MatchString(value) }

func uniqueNames(values []string, field string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validName(value) {
			return fmt.Errorf("%s %q is invalid", field, value)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s %q is duplicated", field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}
