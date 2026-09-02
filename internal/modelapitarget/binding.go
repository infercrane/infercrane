// Package modelapitarget defines the immutable contract that binds a stable
// Model API product to one exact private execution target. Bindings contain
// references and digests only; credentials and mutable endpoint configuration
// remain outside this package.
package modelapitarget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = "model-api-target-binding/v1"

// Kind describes who operates the capacity behind a target. The customer
// product identity is deliberately independent from this private choice.
type Kind string

const (
	KindUpstream      Kind = "upstream"
	KindServerlessGPU Kind = "serverless_gpu"
	KindDedicated     Kind = "dedicated"
	KindBYOC          Kind = "byoc"
)

func (kind Kind) Valid() bool {
	switch kind {
	case KindUpstream, KindServerlessGPU, KindDedicated, KindBYOC:
		return true
	default:
		return false
	}
}

// Binding pins every execution-relevant identity needed to reconstruct a
// target choice. EndpointReference is an opaque, secret-free registry key;
// EndpointConfigDigest proves which immutable adapter configuration it names.
type Binding struct {
	SchemaVersion        string    `json:"schema_version"`
	ID                   string    `json:"id"`
	OperatorTenantID     string    `json:"operator_tenant_id"`
	ProductID            string    `json:"product_id"`
	Kind                 Kind      `json:"kind"`
	OfferID              string    `json:"offer_id"`
	OfferVersion         int64     `json:"offer_version"`
	Adapter              string    `json:"adapter"`
	SupplierModelID      string    `json:"supplier_model_id"`
	EndpointReference    string    `json:"endpoint_reference"`
	EndpointConfigDigest string    `json:"endpoint_config_digest"`
	Region               string    `json:"region"`
	ValidFrom            time.Time `json:"valid_from"`
	ValidUntil           time.Time `json:"valid_until"`
	CreatedAt            time.Time `json:"created_at"`
	ContractDigest       string    `json:"contract_digest"`
}

// Draft is accepted only at the creation boundary. NewBinding establishes the
// schema version and digest so callers cannot accidentally publish a partial
// or mutable target contract.
type Draft struct {
	ID                   string
	OperatorTenantID     string
	ProductID            string
	Kind                 Kind
	OfferID              string
	OfferVersion         int64
	Adapter              string
	SupplierModelID      string
	EndpointReference    string
	EndpointConfigDigest string
	Region               string
	ValidFrom            time.Time
	ValidUntil           time.Time
	CreatedAt            time.Time
}

func NewBinding(draft Draft) (Binding, error) {
	binding := Binding{
		SchemaVersion:        SchemaVersion,
		ID:                   draft.ID,
		OperatorTenantID:     draft.OperatorTenantID,
		ProductID:            draft.ProductID,
		Kind:                 draft.Kind,
		OfferID:              draft.OfferID,
		OfferVersion:         draft.OfferVersion,
		Adapter:              draft.Adapter,
		SupplierModelID:      draft.SupplierModelID,
		EndpointReference:    draft.EndpointReference,
		EndpointConfigDigest: draft.EndpointConfigDigest,
		Region:               draft.Region,
		ValidFrom:            draft.ValidFrom.UTC(),
		ValidUntil:           draft.ValidUntil.UTC(),
		CreatedAt:            draft.CreatedAt.UTC(),
	}
	if err := binding.validateFields(); err != nil {
		return Binding{}, err
	}
	digest, err := binding.canonicalDigest()
	if err != nil {
		return Binding{}, err
	}
	binding.ContractDigest = digest
	return binding, nil
}

func (binding Binding) Validate() error {
	if err := binding.validateFields(); err != nil {
		return err
	}
	digest, err := binding.canonicalDigest()
	if err != nil {
		return err
	}
	if binding.ContractDigest != digest {
		return errors.New("target binding contract digest does not match its immutable fields")
	}
	return nil
}

// CurrentAt uses a half-open interval so adjacent binding windows cannot both
// be current at their shared boundary.
func (binding Binding) CurrentAt(at time.Time) bool {
	return binding.Validate() == nil && !at.UTC().Before(binding.ValidFrom) && at.UTC().Before(binding.ValidUntil)
}

func (binding Binding) HasCanonicalDigest() bool { return binding.Validate() == nil }

var sha256Pattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (binding Binding) validateFields() error {
	if binding.SchemaVersion != SchemaVersion {
		return fmt.Errorf("target binding schema_version must be %q", SchemaVersion)
	}
	for name, value := range map[string]string{
		"id": binding.ID, "operator tenant id": binding.OperatorTenantID, "product id": binding.ProductID,
		"offer id": binding.OfferID, "adapter": binding.Adapter, "supplier model id": binding.SupplierModelID,
		"endpoint reference": binding.EndpointReference, "region": binding.Region,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("target binding %s is required and must be canonical", name)
		}
	}
	if !binding.Kind.Valid() {
		return fmt.Errorf("unknown target kind %q", binding.Kind)
	}
	if binding.OfferVersion <= 0 {
		return errors.New("target binding offer version must be positive")
	}
	if !sha256Pattern.MatchString(binding.EndpointConfigDigest) {
		return errors.New("target binding endpoint config digest must be a lowercase sha256 digest")
	}
	if binding.CreatedAt.IsZero() || binding.ValidFrom.IsZero() || binding.ValidUntil.IsZero() || binding.CreatedAt.After(binding.ValidFrom) || !binding.ValidUntil.After(binding.ValidFrom) {
		return errors.New("target binding needs ordered created_at, valid_from, and valid_until timestamps")
	}
	return nil
}

func (binding Binding) canonicalDigest() (string, error) {
	contract := struct {
		SchemaVersion        string    `json:"schema_version"`
		ID                   string    `json:"id"`
		OperatorTenantID     string    `json:"operator_tenant_id"`
		ProductID            string    `json:"product_id"`
		Kind                 Kind      `json:"kind"`
		OfferID              string    `json:"offer_id"`
		OfferVersion         int64     `json:"offer_version"`
		Adapter              string    `json:"adapter"`
		SupplierModelID      string    `json:"supplier_model_id"`
		EndpointReference    string    `json:"endpoint_reference"`
		EndpointConfigDigest string    `json:"endpoint_config_digest"`
		Region               string    `json:"region"`
		ValidFrom            time.Time `json:"valid_from"`
		ValidUntil           time.Time `json:"valid_until"`
		CreatedAt            time.Time `json:"created_at"`
	}{
		SchemaVersion: binding.SchemaVersion, ID: binding.ID, OperatorTenantID: binding.OperatorTenantID,
		ProductID: binding.ProductID, Kind: binding.Kind, OfferID: binding.OfferID, OfferVersion: binding.OfferVersion,
		Adapter: binding.Adapter, SupplierModelID: binding.SupplierModelID, EndpointReference: binding.EndpointReference,
		EndpointConfigDigest: binding.EndpointConfigDigest, Region: binding.Region, ValidFrom: binding.ValidFrom.UTC(),
		ValidUntil: binding.ValidUntil.UTC(), CreatedAt: binding.CreatedAt.UTC(),
	}
	body, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("encode target binding contract: %w", err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
