package modelapirouting

import (
	"context"
	"errors"
	"time"
)

var ErrInsufficientPrepaidBalance = errors.New("prepaid Model API balance is insufficient")

type ReservationRequest struct {
	ID, TenantID, ProductID, EntitlementID          string
	OperatorTenantID, ServingPlanID, SupplyPlanID   string
	RetailRate                                      RetailRate
	MaxRequestMicrousd                              int64
	CandidateID, OfferID, Supplier, SupplierModelID string
	OfferVersion                                    int64
	CreatedAt                                       time.Time
}

func (r ReservationRequest) Validate() error {
	if r.ID == "" || r.TenantID == "" || r.ProductID == "" || r.EntitlementID == "" || r.OperatorTenantID == "" ||
		r.ServingPlanID == "" || r.SupplyPlanID == "" || r.CandidateID == "" || r.OfferID == "" || r.Supplier == "" ||
		r.SupplierModelID == "" || r.OfferVersion <= 0 || r.MaxRequestMicrousd <= 0 || r.CreatedAt.IsZero() {
		return errors.New("hosted reservation identity, route, supplier, limit, and timestamp are required")
	}
	if r.RetailRate.ProductID != r.ProductID || !r.RetailRate.currentAt(r.CreatedAt.UTC()) {
		return errors.New("hosted reservation requires the exact current immutable retail rate")
	}
	return nil
}

type Reservation struct {
	ID, TenantID, ProductID, EntitlementID            string
	RetailRateID, RetailRateDigest                    string
	RetailRateVersion                                 int
	InputMicrousdPerMillion, OutputMicrousdPerMillion int64
	ReservedMicrousd, ActualMicrousd                  int64
	InputTokens, OutputTokens                         *int
	State, Resolution                                 string
	TransmittedAt, ResponseStartedAt                  *time.Time
	CreatedAt, UpdatedAt                              time.Time
}

type Usage struct {
	StatusCode                int
	InputTokens, OutputTokens *int
}

type Billing interface {
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	MarkTransmitted(context.Context, string, string, time.Time) error
	MarkResponseStarted(context.Context, string, string, time.Time) error
	Settle(context.Context, string, string, Usage) (Reservation, error)
	ReleaseUnsent(context.Context, string, string, string) error
}
