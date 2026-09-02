package modelapiproduct

import (
	"errors"
	"fmt"
	"time"
)

type QualificationState string

const (
	QualificationPending   QualificationState = "pending"
	QualificationQualified QualificationState = "qualified"
	QualificationStale     QualificationState = "stale"
)

func (s QualificationState) Valid() bool {
	switch s {
	case QualificationPending, QualificationQualified, QualificationStale:
		return true
	default:
		return false
	}
}

type RouteQualification struct {
	State         QualificationState `json:"state"`
	EvidenceID    string             `json:"evidence_id,omitempty"`
	EvidenceUntil *time.Time         `json:"evidence_until,omitempty"`
}

func (q RouteQualification) Validate() error {
	if !q.State.Valid() {
		return errors.New("route qualification state is invalid")
	}
	if q.State == QualificationQualified {
		if q.EvidenceID == "" || q.EvidenceUntil == nil || q.EvidenceUntil.IsZero() {
			return errors.New("qualified route needs evidence and an expiry")
		}
	} else if q.EvidenceID != "" || q.EvidenceUntil != nil {
		return errors.New("non-qualified route cannot carry active evidence")
	}
	return nil
}

func (q RouteQualification) CurrentAt(at time.Time) bool {
	return q.State == QualificationQualified && q.EvidenceID != "" && q.EvidenceUntil != nil && q.EvidenceUntil.After(at.UTC())
}

// OperatorPublication contains trusted control-plane references. It must never
// be serialized through the anonymous or customer projection.
type OperatorPublication struct {
	SchemaVersion       string             `json:"schema_version"`
	ProductID           string             `json:"product_id"`
	OperatorWorkspaceID string             `json:"operator_workspace_id"`
	ServingPlanID       string             `json:"serving_plan_id"`
	SupplyPlanID        string             `json:"supply_plan_id"`
	Qualification       RouteQualification `json:"qualification"`
	RetailRate          *RetailRate        `json:"retail_rate,omitempty"`
	UpdatedAt           time.Time          `json:"updated_at"`
}

func (p OperatorPublication) validateStructure(product Product) error {
	if p.SchemaVersion != OperatorProjectionSchemaVersion {
		return fmt.Errorf("operator publication schema_version must be %q", OperatorProjectionSchemaVersion)
	}
	if err := product.Validate(); err != nil {
		return err
	}
	if p.ProductID != product.ID {
		return errors.New("publication product does not match public product")
	}
	if err := p.Qualification.Validate(); err != nil {
		return err
	}
	if p.OperatorWorkspaceID == "" || p.ServingPlanID == "" || p.SupplyPlanID == "" || p.UpdatedAt.IsZero() {
		return errors.New("operator publication needs workspace, serving plan, supply plan, and update time")
	}
	if p.RetailRate != nil {
		if err := p.RetailRate.Validate(); err != nil {
			return err
		}
		if p.RetailRate.ProductID != product.ID {
			return errors.New("retail rate product does not match publication product")
		}
	}
	return nil
}

func (p OperatorPublication) ValidateAt(product Product, at time.Time) error {
	if err := p.validateStructure(product); err != nil {
		return err
	}
	if product.Availability == AvailabilityAvailable || product.Availability == AvailabilityDegraded {
		if p.RetailRate == nil || !p.RetailRate.CurrentAt(at) || !p.Qualification.CurrentAt(at) || !product.HasCurrentCallableCapabilityAt(at) {
			return errors.New("callable availability needs a current retail rate, route qualification, and per-operation capability evidence")
		}
	}
	return nil
}

type PublicCapabilityClaim struct {
	Name               string     `json:"name"`
	State              ClaimState `json:"state"`
	EvidenceValidUntil *time.Time `json:"evidence_valid_until,omitempty"`
}

type PublicProjection struct {
	SchemaVersion       string                  `json:"schema_version"`
	ID                  string                  `json:"id"`
	DisplayName         string                  `json:"display_name"`
	Publisher           string                  `json:"publisher"`
	Description         string                  `json:"description"`
	Protocol            string                  `json:"protocol"`
	Tasks               []string                `json:"tasks"`
	Capabilities        []PublicCapabilityClaim `json:"capabilities"`
	InputModalities     []string                `json:"input_modalities"`
	OutputModalities    []string                `json:"output_modalities"`
	ContextWindowTokens *int64                  `json:"context_window_tokens,omitempty"`
	Availability        Availability            `json:"availability"`
	SelfHostEligibility SelfHostEligibility     `json:"self_host_eligibility"`
	RetailRate          *RetailRate             `json:"retail_rate,omitempty"`
	Qualification       QualificationState      `json:"qualification"`
	EvidenceValidUntil  *time.Time              `json:"evidence_valid_until,omitempty"`
	Callable            bool                    `json:"callable"`
}

func (p PublicProjection) HasCurrentCallableCapabilityAt(at time.Time) bool {
	hasCallable, requiresStreaming, hasStreaming := false, false, false
	for _, claim := range p.Capabilities {
		if claim.State != ClaimQualified || claim.EvidenceValidUntil == nil || !claim.EvidenceValidUntil.After(at.UTC()) {
			continue
		}
		if claim.Name == "streaming" {
			hasStreaming = true
			continue
		}
		operation, callable := (CapabilityClaim{Name: claim.Name}).CallableOperation()
		hasCallable = hasCallable || callable
		requiresStreaming = requiresStreaming || callable && (operation == "chat" || operation == "completions" || operation == "responses")
	}
	return hasCallable && (!requiresStreaming || hasStreaming)
}

// PublicProjectionAt explicitly strips operator workspace, plan, supplier, and
// evidence identifiers. Expired rates and evidence fail closed.
func PublicProjectionAt(product Product, publication *OperatorPublication, at time.Time) (PublicProjection, error) {
	if err := product.Validate(); err != nil {
		return PublicProjection{}, err
	}
	projection := PublicProjection{
		SchemaVersion: PublicProjectionSchemaVersion,
		ID:            product.ID, DisplayName: product.DisplayName, Publisher: product.Publisher,
		Description: product.Description, Protocol: product.Protocol,
		Tasks: append([]string(nil), product.Tasks...), Capabilities: publicCapabilitiesAt(product.Capabilities, at),
		InputModalities: append([]string(nil), product.InputModalities...), OutputModalities: append([]string(nil), product.OutputModalities...),
		ContextWindowTokens: copyInt64(product.ContextWindowTokens), Availability: product.Availability,
		SelfHostEligibility: product.SelfHostEligibility, Qualification: QualificationPending,
	}
	if publication == nil {
		return projection, nil
	}
	if err := publication.validateStructure(product); err != nil {
		return PublicProjection{}, err
	}
	projection.Qualification = publication.Qualification.State
	if publication.Qualification.CurrentAt(at) {
		value := publication.Qualification.EvidenceUntil.UTC()
		projection.EvidenceValidUntil = &value
	} else if publication.Qualification.State == QualificationQualified {
		projection.Qualification = QualificationStale
	}
	if publication.RetailRate != nil && publication.RetailRate.ProductID == product.ID && publication.RetailRate.CurrentAt(at) {
		copy := *publication.RetailRate
		copy.CachedInputMicrousdPerMillion = copyInt64(publication.RetailRate.CachedInputMicrousdPerMillion)
		projection.RetailRate = &copy
	}
	callableState := product.Availability == AvailabilityAvailable || product.Availability == AvailabilityDegraded
	projection.Callable = callableState && publication.OperatorWorkspaceID != "" && publication.ServingPlanID != "" && publication.SupplyPlanID != "" && projection.RetailRate != nil && publication.Qualification.CurrentAt(at) && projection.HasCurrentCallableCapabilityAt(at)
	if callableState && !projection.Callable {
		// Never leave an externally visible product labelled available when a
		// stale price or qualification means admission must fail closed.
		projection.Availability = AvailabilityUnavailable
	}
	return projection, nil
}

func publicCapabilitiesAt(claims []CapabilityClaim, at time.Time) []PublicCapabilityClaim {
	public := make([]PublicCapabilityClaim, 0, len(claims))
	for _, claim := range claims {
		item := PublicCapabilityClaim{Name: claim.Name, State: claim.State}
		if claim.CurrentAt(at) {
			value := claim.EvidenceUntil.UTC()
			item.EvidenceValidUntil = &value
		} else if claim.State == ClaimQualified {
			item.State = ClaimCataloged
		}
		public = append(public, item)
	}
	return public
}
