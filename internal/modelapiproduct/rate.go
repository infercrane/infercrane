package modelapiproduct

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CustomerRetailRateProvenance is the only pricing provenance exposed at the
// customer API boundary. Supplier cost evidence and route identity remain in
// the operator-private supply contract.
const CustomerRetailRateProvenance = "InferCrane retail rate card"

// RetailRate is an append-only, versioned public price contract. Supplier cost
// and supplier identity intentionally do not belong in this record.
type RetailRate struct {
	ID                            string    `json:"id"`
	ProductID                     string    `json:"product_id"`
	Version                       int       `json:"version"`
	Currency                      string    `json:"currency"`
	InputMicrousdPerMillion       int64     `json:"input_microusd_per_million"`
	CachedInputMicrousdPerMillion *int64    `json:"cached_input_microusd_per_million,omitempty"`
	OutputMicrousdPerMillion      int64     `json:"output_microusd_per_million"`
	ValidFrom                     time.Time `json:"valid_from"`
	ValidUntil                    time.Time `json:"valid_until"`
	PublishedAt                   time.Time `json:"published_at"`
	PublicProvenance              string    `json:"public_provenance"`
	ContractDigest                string    `json:"contract_digest"`
}

type RetailRateDraft struct {
	ID                            string
	ProductID                     string
	Version                       int
	InputMicrousdPerMillion       int64
	CachedInputMicrousdPerMillion *int64
	OutputMicrousdPerMillion      int64
	ValidFrom                     time.Time
	ValidUntil                    time.Time
	PublishedAt                   time.Time
	PublicProvenance              string
}

func NewRetailRate(draft RetailRateDraft) (RetailRate, error) {
	rate := RetailRate{
		ID:                            draft.ID,
		ProductID:                     draft.ProductID,
		Version:                       draft.Version,
		Currency:                      "USD",
		InputMicrousdPerMillion:       draft.InputMicrousdPerMillion,
		CachedInputMicrousdPerMillion: copyInt64(draft.CachedInputMicrousdPerMillion),
		OutputMicrousdPerMillion:      draft.OutputMicrousdPerMillion,
		ValidFrom:                     draft.ValidFrom.UTC(),
		ValidUntil:                    draft.ValidUntil.UTC(),
		PublishedAt:                   draft.PublishedAt.UTC(),
		PublicProvenance:              draft.PublicProvenance,
	}
	if err := rate.validateFields(); err != nil {
		return RetailRate{}, err
	}
	digest, err := rate.computeDigest()
	if err != nil {
		return RetailRate{}, err
	}
	rate.ContractDigest = digest
	return rate, nil
}

func (r RetailRate) Validate() error {
	if err := r.validateFields(); err != nil {
		return err
	}
	expected, err := r.computeDigest()
	if err != nil {
		return err
	}
	if r.ContractDigest != expected {
		return errors.New("retail rate contract digest does not match its immutable fields")
	}
	return nil
}

func (r RetailRate) CurrentAt(at time.Time) bool {
	return r.Validate() == nil && !at.UTC().Before(r.ValidFrom) && at.UTC().Before(r.ValidUntil)
}

func (r RetailRate) validateFields() error {
	if !validID(r.ID) || !validID(r.ProductID) || r.Version <= 0 || r.Currency != "USD" || r.PublicProvenance == "" {
		return errors.New("retail rate identity, positive version, USD currency, and public provenance are required")
	}
	if r.InputMicrousdPerMillion <= 0 || r.OutputMicrousdPerMillion <= 0 {
		return errors.New("published input and output rates must be positive")
	}
	if r.CachedInputMicrousdPerMillion != nil {
		return errors.New("cached-input pricing cannot be published until cached-token settlement is supported")
	}
	if r.ValidFrom.IsZero() || r.ValidUntil.IsZero() || r.PublishedAt.IsZero() || !r.ValidUntil.After(r.ValidFrom) || r.PublishedAt.After(r.ValidFrom) {
		return errors.New("retail rate needs ordered published_at, valid_from, and valid_until timestamps")
	}
	return nil
}

func (r RetailRate) computeDigest() (string, error) {
	contract := struct {
		ID                            string    `json:"id"`
		ProductID                     string    `json:"product_id"`
		Version                       int       `json:"version"`
		Currency                      string    `json:"currency"`
		InputMicrousdPerMillion       int64     `json:"input_microusd_per_million"`
		CachedInputMicrousdPerMillion *int64    `json:"cached_input_microusd_per_million,omitempty"`
		OutputMicrousdPerMillion      int64     `json:"output_microusd_per_million"`
		ValidFrom                     time.Time `json:"valid_from"`
		ValidUntil                    time.Time `json:"valid_until"`
		PublishedAt                   time.Time `json:"published_at"`
		PublicProvenance              string    `json:"public_provenance"`
	}{
		ID: r.ID, ProductID: r.ProductID, Version: r.Version, Currency: r.Currency,
		InputMicrousdPerMillion: r.InputMicrousdPerMillion, CachedInputMicrousdPerMillion: r.CachedInputMicrousdPerMillion,
		OutputMicrousdPerMillion: r.OutputMicrousdPerMillion, ValidFrom: r.ValidFrom.UTC(), ValidUntil: r.ValidUntil.UTC(),
		PublishedAt: r.PublishedAt.UTC(), PublicProvenance: r.PublicProvenance,
	}
	body, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("encode retail rate contract: %w", err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func copyInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
