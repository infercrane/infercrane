package modelapisupply

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapiqualification"
)

const (
	OfferActive   = "active"
	OfferDisabled = "disabled"

	CommercialReady   = "ready"
	CommercialPending = "pending"
	CommercialExpired = "expired"

	QualificationQualified = "qualified"
	QualificationPending   = "pending"
	QualificationRejected  = "rejected"
	QualificationExpired   = "expired"
)

// CostRate is private supplier cost. It is deliberately separate from the
// supplier-neutral customer retail rate supplied during materialization.
type CostRate struct {
	Currency                   string    `json:"currency"`
	InputMicrousdPerMTok       *int64    `json:"input_microusd_per_million,omitempty"`
	OutputMicrousdPerMTok      *int64    `json:"output_microusd_per_million,omitempty"`
	CachedInputMicrousdPerMTok *int64    `json:"cached_input_microusd_per_million,omitempty"`
	Provenance                 string    `json:"provenance,omitempty"`
	ValidFrom                  time.Time `json:"valid_from,omitempty"`
	ValidUntil                 time.Time `json:"valid_until,omitempty"`
}

type CommercialAuthorization struct {
	State      string    `json:"state"`
	TermsRef   string    `json:"terms_ref,omitempty"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
}

// QualificationEvidence is valid only for its provider-bound offer tuple,
// stated protocol, region, capabilities, and observation window. It does not
// claim an upstream runtime or model revision that the supplier cannot prove.
type QualificationEvidence struct {
	ID             string    `json:"id"`
	State          string    `json:"state"`
	TupleKey       string    `json:"tuple_key"`
	Protocol       string    `json:"protocol"`
	Region         string    `json:"region"`
	Capabilities   []string  `json:"capabilities"`
	Scope          string    `json:"scope,omitempty"`
	EvidenceRef    string    `json:"evidence_ref,omitempty"`
	EvidenceDigest string    `json:"evidence_digest,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	ObservedAt     time.Time `json:"observed_at,omitempty"`
	ValidUntil     time.Time `json:"valid_until,omitempty"`
	SampleCount    int       `json:"sample_count"`
	TTFTP95MS      *float64  `json:"ttft_p95_ms,omitempty"`
	OutputTokensP5 *float64  `json:"output_tokens_p5,omitempty"`
}

// Validate checks that qualification evidence is complete and internally
// consistent before it crosses a persistence or publication boundary.
func (e QualificationEvidence) Validate() error { return validateQualification(e) }

// HuggingFaceProvenance is discovery metadata only. MaterializeCandidate does
// not use these fields to establish availability, capabilities, price,
// qualification, or performance truth.
type HuggingFaceProvenance struct {
	RepositoryID string    `json:"repository_id,omitempty"`
	Revision     string    `json:"revision,omitempty"`
	License      string    `json:"license,omitempty"`
	SourceURL    string    `json:"source_url,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
}

// Offer is an immutable revision of an operator-owned private supplier offer.
// CredentialReference points at a secret manager; credentials are never part
// of this record or a compiled supply plan.
type Offer struct {
	ID                  string                  `json:"id"`
	Version             int64                   `json:"version"`
	OperatorTenantID    string                  `json:"operator_tenant_id"`
	ProductID           string                  `json:"product_id"`
	Supplier            string                  `json:"supplier"`
	Adapter             string                  `json:"adapter"`
	SupplierModelID     string                  `json:"supplier_model_id"`
	Protocol            string                  `json:"protocol"`
	TupleKey            string                  `json:"tuple_key"`
	Region              string                  `json:"region"`
	CredentialReference string                  `json:"credential_reference"`
	State               string                  `json:"state"`
	Capabilities        []string                `json:"capabilities"`
	Access              string                  `json:"access"`
	Availability        string                  `json:"availability"`
	Health              string                  `json:"health"`
	ObservedAt          time.Time               `json:"observed_at"`
	CostRate            CostRate                `json:"cost_rate"`
	Commercial          CommercialAuthorization `json:"commercial"`
	Qualification       *QualificationEvidence  `json:"qualification,omitempty"`
	HuggingFace         *HuggingFaceProvenance  `json:"hugging_face,omitempty"`
}

type CandidateMaterialization struct {
	CandidateID string
	Offer       Offer
	RetailRate  modelapiproduct.RetailRate
}

func (o Offer) Validate() error {
	required := map[string]string{
		"id": o.ID, "operator tenant id": o.OperatorTenantID, "product id": o.ProductID,
		"supplier": o.Supplier, "adapter": o.Adapter, "supplier model id": o.SupplierModelID,
		"protocol": o.Protocol, "tuple key": o.TupleKey, "region": o.Region,
		"credential reference": o.CredentialReference,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("offer %s is required", name)
		}
	}
	if o.Version <= 0 {
		return errors.New("offer version must be positive")
	}
	if o.State != OfferActive && o.State != OfferDisabled {
		return fmt.Errorf("unknown offer state %q", o.State)
	}
	if len(normalizeStrings(o.Capabilities)) == 0 {
		return errors.New("offer requires at least one capability")
	}
	if err := validateCostRate(o.CostRate); err != nil {
		return err
	}
	if o.Commercial.State != CommercialReady && o.Commercial.State != CommercialPending && o.Commercial.State != CommercialExpired {
		return fmt.Errorf("unknown commercial authorization state %q", o.Commercial.State)
	}
	if o.Commercial.State == CommercialReady && (strings.TrimSpace(o.Commercial.TermsRef) == "" || o.Commercial.ValidUntil.IsZero()) {
		return errors.New("ready commercial authorization requires terms reference and expiry")
	}
	if o.Qualification != nil {
		if err := validateQualification(*o.Qualification); err != nil {
			return err
		}
	}
	if o.HuggingFace != nil && o.HuggingFace.ObservedAt.IsZero() {
		return errors.New("Hugging Face metadata requires an observation timestamp")
	}
	return nil
}

// Validate checks whether a supplier cost schedule is complete enough to be
// compared with an immutable customer retail rate.
func (r CostRate) Validate() error { return validateCostRate(r) }

func validateCostRate(rate CostRate) error {
	if rate.Currency != "USD" {
		return fmt.Errorf("supplier cost currency %q is unsupported", rate.Currency)
	}
	present := rate.InputMicrousdPerMTok != nil || rate.OutputMicrousdPerMTok != nil || rate.CachedInputMicrousdPerMTok != nil || strings.TrimSpace(rate.Provenance) != "" || !rate.ValidFrom.IsZero() || !rate.ValidUntil.IsZero()
	if !present {
		return nil
	}
	if rate.InputMicrousdPerMTok == nil || rate.OutputMicrousdPerMTok == nil || strings.TrimSpace(rate.Provenance) == "" {
		return errors.New("partial supplier cost requires input, output, and provenance")
	}
	for _, value := range []*int64{rate.InputMicrousdPerMTok, rate.OutputMicrousdPerMTok, rate.CachedInputMicrousdPerMTok} {
		if value != nil && *value < 0 {
			return errors.New("supplier cost cannot be negative")
		}
	}
	if rate.ValidFrom.IsZero() || rate.ValidUntil.IsZero() || !rate.ValidUntil.After(rate.ValidFrom) {
		return errors.New("supplier cost requires an increasing validity window")
	}
	return nil
}

func validateQualification(evidence QualificationEvidence) error {
	if strings.TrimSpace(evidence.ID) == "" || strings.TrimSpace(evidence.TupleKey) == "" || strings.TrimSpace(evidence.Protocol) == "" || strings.TrimSpace(evidence.Region) == "" {
		return errors.New("qualification requires id, tuple key, protocol, and region")
	}
	if evidence.State != QualificationQualified && evidence.State != QualificationPending && evidence.State != QualificationRejected && evidence.State != QualificationExpired {
		return fmt.Errorf("unknown qualification state %q", evidence.State)
	}
	if evidence.State == QualificationQualified && (strings.TrimSpace(evidence.Scope) == "" || strings.TrimSpace(evidence.EvidenceRef) == "" || strings.TrimSpace(evidence.EvidenceDigest) == "") {
		return errors.New("qualified evidence requires scope, evidence reference, and digest")
	}
	if evidence.State == QualificationQualified && (evidence.ObservedAt.IsZero() || evidence.ValidUntil.IsZero() || !evidence.ValidUntil.After(evidence.ObservedAt)) {
		return errors.New("qualified evidence requires an increasing validity window")
	}
	if evidence.State == QualificationQualified && evidence.ValidUntil.Sub(evidence.ObservedAt) > modelapiqualification.MaximumValidity {
		return fmt.Errorf("qualified evidence validity cannot exceed %s", modelapiqualification.MaximumValidity)
	}
	if !evidence.ObservedAt.IsZero() && !evidence.ValidUntil.IsZero() && !evidence.ValidUntil.After(evidence.ObservedAt) {
		return errors.New("qualification evidence validity window must increase")
	}
	if evidence.SampleCount < 0 {
		return errors.New("qualification sample count cannot be negative")
	}
	return nil
}

// MaterializeCandidate combines one immutable private offer revision with one
// immutable public retail rate version. Missing or non-current business
// evidence remains on the Candidate so Compile can persist explicit rejection
// reasons instead of turning an incomplete offer into an input error.
func MaterializeCandidate(input CandidateMaterialization) (Candidate, error) {
	if strings.TrimSpace(input.CandidateID) == "" {
		return Candidate{}, errors.New("candidate id is required")
	}
	if err := input.Offer.Validate(); err != nil {
		return Candidate{}, err
	}
	if err := input.RetailRate.Validate(); err != nil {
		return Candidate{}, err
	}
	if input.RetailRate.ProductID != input.Offer.ProductID {
		return Candidate{}, errors.New("retail rate and supplier offer product ids must match")
	}
	if input.Offer.CostRate.Currency != input.RetailRate.Currency {
		return Candidate{}, errors.New("supplier cost and retail rate currencies must match")
	}

	offer := input.Offer
	validUntil := earlierTime(offer.CostRate.ValidUntil, input.RetailRate.ValidUntil)
	retailInput, retailOutput := input.RetailRate.InputMicrousdPerMillion, input.RetailRate.OutputMicrousdPerMillion
	candidate := Candidate{
		ID: input.CandidateID, OfferID: offer.ID, OfferVersion: offer.Version,
		RetailRateID: input.RetailRate.ID, RetailRateVersion: input.RetailRate.Version, Supplier: offer.Supplier,
		ModelID: offer.ProductID, SupplierModelID: offer.SupplierModelID, TupleKey: offer.TupleKey,
		Protocol: offer.Protocol, Capabilities: normalizeStrings(offer.Capabilities), Regions: []string{offer.Region},
		OfferState: offer.State, Access: offer.Access, Availability: offer.Availability, Health: offer.Health,
		ObservedAt: offer.ObservedAt, RateValidUntil: validUntil,
		RetailInputMicrousdPerMTok:  &retailInput,
		RetailOutputMicrousdPerMTok: &retailOutput,
		CostInputMicrousdPerMTok:    offer.CostRate.InputMicrousdPerMTok,
		CostOutputMicrousdPerMTok:   offer.CostRate.OutputMicrousdPerMTok,
		CostBasisProvenance:         offer.CostRate.Provenance, CommercialState: offer.Commercial.State,
		CommercialValidUntil: offer.Commercial.ValidUntil,
	}
	if offer.Qualification != nil {
		copy := *offer.Qualification
		copy.Capabilities = normalizeStrings(copy.Capabilities)
		candidate.Qualification = &copy
		candidate.Evidence = &CapacityEvidence{
			TupleKey: copy.TupleKey, ObservedAt: copy.ObservedAt, SampleCount: copy.SampleCount,
			TTFTP95MS: copy.TTFTP95MS, OutputTokensP5: copy.OutputTokensP5,
		}
	}
	return candidate, nil
}

func normalizeStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func earlierTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
