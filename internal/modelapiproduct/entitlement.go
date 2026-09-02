package modelapiproduct

import (
	"errors"
	"fmt"
	"time"
)

type EntitlementState string

const (
	EntitlementPending   EntitlementState = "pending"
	EntitlementActive    EntitlementState = "active"
	EntitlementSuspended EntitlementState = "suspended"
	EntitlementRevoked   EntitlementState = "revoked"
)

func (s EntitlementState) Valid() bool {
	switch s {
	case EntitlementPending, EntitlementActive, EntitlementSuspended, EntitlementRevoked:
		return true
	default:
		return false
	}
}

type CustomerLimits struct {
	RequestsPerMinute    *int64 `json:"requests_per_minute,omitempty"`
	TokensPerMinute      *int64 `json:"tokens_per_minute,omitempty"`
	MonthlySpendMicrousd *int64 `json:"monthly_spend_microusd,omitempty"`
	MaxRequestMicrousd   *int64 `json:"max_request_microusd,omitempty"`
}

func (l CustomerLimits) Validate() error {
	for name, value := range map[string]*int64{
		"requests_per_minute":    l.RequestsPerMinute,
		"tokens_per_minute":      l.TokensPerMinute,
		"monthly_spend_microusd": l.MonthlySpendMicrousd,
		"max_request_microusd":   l.MaxRequestMicrousd,
	} {
		if value != nil && *value <= 0 {
			return fmt.Errorf("customer limit %s must be positive when set", name)
		}
	}
	if l.MaxRequestMicrousd != nil && l.MonthlySpendMicrousd != nil && *l.MaxRequestMicrousd > *l.MonthlySpendMicrousd {
		return errors.New("maximum request cost cannot exceed the monthly spend limit")
	}
	return nil
}

// SupportedForAdmission is deliberately narrower than Validate. These limit
// shapes remain representable for future control-plane work, but an active
// hosted entitlement cannot promise enforcement the request path does not yet
// implement.
func (l CustomerLimits) SupportedForAdmission() bool {
	return l.RequestsPerMinute == nil && l.TokensPerMinute == nil && l.MonthlySpendMicrousd == nil && l.MaxRequestMicrousd != nil
}

// ProductEntitlement is the trusted mapping from a customer workspace to
// shared operator supply. It grants routing authority, not read access to the
// operator's targets, supplier offers, or credentials.
type ProductEntitlement struct {
	SchemaVersion       string           `json:"schema_version"`
	ID                  string           `json:"id"`
	CustomerWorkspaceID string           `json:"customer_workspace_id"`
	ProductID           string           `json:"product_id"`
	OperatorWorkspaceID string           `json:"operator_workspace_id"`
	ServingPlanID       string           `json:"serving_plan_id"`
	RetailRateID        string           `json:"retail_rate_id"`
	RetailRateVersion   int              `json:"retail_rate_version"`
	State               EntitlementState `json:"state"`
	Limits              CustomerLimits   `json:"limits"`
	ValidFrom           time.Time        `json:"valid_from"`
	ValidUntil          *time.Time       `json:"valid_until,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
}

func (e ProductEntitlement) Validate() error {
	if e.SchemaVersion != EntitlementSchemaVersion {
		return fmt.Errorf("product entitlement schema_version must be %q", EntitlementSchemaVersion)
	}
	if !validID(e.ID) || !validID(e.ProductID) || e.CustomerWorkspaceID == "" || e.OperatorWorkspaceID == "" || e.ServingPlanID == "" || !validID(e.RetailRateID) || e.RetailRateVersion <= 0 {
		return errors.New("entitlement identity, workspaces, serving plan, and retail rate are required")
	}
	if e.CustomerWorkspaceID == e.OperatorWorkspaceID {
		return errors.New("shared hosted entitlement must separate customer and operator workspaces")
	}
	if !e.State.Valid() {
		return errors.New("entitlement state is invalid")
	}
	if err := e.Limits.Validate(); err != nil {
		return err
	}
	if e.ValidFrom.IsZero() || e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) {
		return errors.New("entitlement needs ordered lifecycle timestamps")
	}
	if e.ValidUntil != nil && !e.ValidUntil.After(e.ValidFrom) {
		return errors.New("entitlement valid_until must follow valid_from")
	}
	return nil
}

func (e ProductEntitlement) ActiveAt(at time.Time) bool {
	if e.Validate() != nil || e.State != EntitlementActive || at.UTC().Before(e.ValidFrom.UTC()) {
		return false
	}
	return e.ValidUntil == nil || at.UTC().Before(e.ValidUntil.UTC())
}

type CustomerEntitlementProjection struct {
	SchemaVersion     string           `json:"schema_version"`
	ID                string           `json:"id"`
	ProductID         string           `json:"product_id"`
	RetailRateVersion int              `json:"retail_rate_version"`
	State             EntitlementState `json:"state"`
	Limits            CustomerLimits   `json:"limits"`
	ValidFrom         time.Time        `json:"valid_from"`
	ValidUntil        *time.Time       `json:"valid_until,omitempty"`
}

func (e ProductEntitlement) CustomerProjection() (CustomerEntitlementProjection, error) {
	if err := e.Validate(); err != nil {
		return CustomerEntitlementProjection{}, err
	}
	projection := CustomerEntitlementProjection{
		SchemaVersion: CustomerEntitlementSchemaVersion,
		ID:            e.ID, ProductID: e.ProductID, RetailRateVersion: e.RetailRateVersion,
		State: e.State, Limits: copyLimits(e.Limits), ValidFrom: e.ValidFrom.UTC(),
	}
	if e.ValidUntil != nil {
		value := e.ValidUntil.UTC()
		projection.ValidUntil = &value
	}
	return projection, nil
}

func copyLimits(limits CustomerLimits) CustomerLimits {
	return CustomerLimits{
		RequestsPerMinute:    copyInt64(limits.RequestsPerMinute),
		TokensPerMinute:      copyInt64(limits.TokensPerMinute),
		MonthlySpendMicrousd: copyInt64(limits.MonthlySpendMicrousd),
		MaxRequestMicrousd:   copyInt64(limits.MaxRequestMicrousd),
	}
}
