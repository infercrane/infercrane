// Package modelapireconciliation reconciles settled hosted Model API usage
// against the exact immutable supplier rate pinned by its reservation.
// Monetary values are integer micro-US dollars; floating point is never used.
package modelapireconciliation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	SchemaVersion     = "infercrane.model-api.supplier-reconciliation/v1"
	RateSchema        = "infercrane.model-api.supplier-rate/v1"
	SettlementSettled = "settled"
	tokensPerMillion  = int64(1_000_000)
	digestPrefix      = "sha256:"
)

// SupplierRateDraft is the source for a content-addressed private supplier
// rate. A new version is required for any price, provenance, identity, or
// validity change.
type SupplierRateDraft struct {
	ID                            string    `json:"id"`
	Version                       int64     `json:"version"`
	OfferID                       string    `json:"offer_id"`
	OfferVersion                  int64     `json:"offer_version"`
	TupleKey                      string    `json:"tuple_key"`
	Supplier                      string    `json:"supplier"`
	SupplierModelID               string    `json:"supplier_model_id"`
	Currency                      string    `json:"currency"`
	InputMicrousdPerMillion       int64     `json:"input_microusd_per_million"`
	OutputMicrousdPerMillion      int64     `json:"output_microusd_per_million"`
	HasCachedInputRate            bool      `json:"has_cached_input_rate"`
	CachedInputMicrousdPerMillion int64     `json:"cached_input_microusd_per_million"`
	Provenance                    string    `json:"provenance"`
	ValidFrom                     time.Time `json:"valid_from"`
	ValidUntil                    time.Time `json:"valid_until"`
}

// SupplierRate is an immutable-by-digest supplier price snapshot.
type SupplierRate struct {
	SupplierRateDraft
	Digest string `json:"digest"`
}

// SettledUsage is the final normalized token usage and customer charge for a
// reservation. SupplierRate* fields are the rate identity pinned before the
// request was transmitted.
type SettledUsage struct {
	ReservationID       string    `json:"reservation_id"`
	State               string    `json:"state"`
	OfferID             string    `json:"offer_id"`
	OfferVersion        int64     `json:"offer_version"`
	TupleKey            string    `json:"tuple_key"`
	Supplier            string    `json:"supplier"`
	SupplierModelID     string    `json:"supplier_model_id"`
	SupplierRateID      string    `json:"supplier_rate_id"`
	SupplierRateVersion int64     `json:"supplier_rate_version"`
	SupplierRateDigest  string    `json:"supplier_rate_digest"`
	Currency            string    `json:"currency"`
	InputTokens         int64     `json:"input_tokens"`
	CachedInputTokens   int64     `json:"cached_input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	RetailMicrousd      int64     `json:"retail_microusd"`
	ReservedAt          time.Time `json:"reserved_at"`
	SettledAt           time.Time `json:"settled_at"`
}

// Reconciliation is an auditable, content-addressed COGS result. Gross margin
// is deliberately marked undefined for zero retail revenue.
type Reconciliation struct {
	SchemaVersion             string    `json:"schema_version"`
	ReservationID             string    `json:"reservation_id"`
	OfferID                   string    `json:"offer_id"`
	OfferVersion              int64     `json:"offer_version"`
	TupleKey                  string    `json:"tuple_key"`
	Supplier                  string    `json:"supplier"`
	SupplierModelID           string    `json:"supplier_model_id"`
	SupplierRateID            string    `json:"supplier_rate_id"`
	SupplierRateVersion       int64     `json:"supplier_rate_version"`
	SupplierRateDigest        string    `json:"supplier_rate_digest"`
	Currency                  string    `json:"currency"`
	InputTokens               int64     `json:"input_tokens"`
	CachedInputTokens         int64     `json:"cached_input_tokens"`
	OutputTokens              int64     `json:"output_tokens"`
	UncachedInputCOGSMicrousd int64     `json:"uncached_input_cogs_microusd"`
	CachedInputCOGSMicrousd   int64     `json:"cached_input_cogs_microusd"`
	OutputCOGSMicrousd        int64     `json:"output_cogs_microusd"`
	SupplierCOGSMicrousd      int64     `json:"supplier_cogs_microusd"`
	RetailMicrousd            int64     `json:"retail_microusd"`
	GrossProfitMicrousd       int64     `json:"gross_profit_microusd"`
	GrossMarginDefined        bool      `json:"gross_margin_defined"`
	GrossMarginBPS            int64     `json:"gross_margin_bps,omitempty"`
	ReservedAt                time.Time `json:"reserved_at"`
	SettledAt                 time.Time `json:"settled_at"`
	Digest                    string    `json:"digest"`
}

// NewSupplierRate validates and content-addresses one supplier rate revision.
func NewSupplierRate(draft SupplierRateDraft) (SupplierRate, error) {
	draft.ValidFrom = canonicalTime(draft.ValidFrom)
	draft.ValidUntil = canonicalTime(draft.ValidUntil)
	if err := validateRateDraft(draft); err != nil {
		return SupplierRate{}, err
	}
	digest, err := digestJSON(struct {
		Schema string `json:"schema_version"`
		SupplierRateDraft
	}{Schema: RateSchema, SupplierRateDraft: draft})
	if err != nil {
		return SupplierRate{}, err
	}
	return SupplierRate{SupplierRateDraft: draft, Digest: digest}, nil
}

// Validate detects an incomplete or mutated supplier rate snapshot.
func (rate SupplierRate) Validate() error {
	rebuilt, err := NewSupplierRate(rate.SupplierRateDraft)
	if err != nil {
		return err
	}
	if rate.Digest == "" || rate.Digest != rebuilt.Digest {
		return errors.New("supplier rate digest does not match its immutable terms")
	}
	return nil
}

// Reconcile calculates supplier COGS and gross margin from settled usage. Each
// billable token dimension rounds up independently to one microdollar so COGS
// is never understated by favorable aggregation or truncation.
func Reconcile(usage SettledUsage, rate SupplierRate) (Reconciliation, error) {
	if err := rate.Validate(); err != nil {
		return Reconciliation{}, err
	}
	usage.ReservedAt = canonicalTime(usage.ReservedAt)
	usage.SettledAt = canonicalTime(usage.SettledAt)
	if err := validateUsage(usage, rate); err != nil {
		return Reconciliation{}, err
	}

	uncachedTokens := usage.InputTokens - usage.CachedInputTokens
	uncachedCost, err := roundedUpCost(uncachedTokens, rate.InputMicrousdPerMillion)
	if err != nil {
		return Reconciliation{}, err
	}
	cachedCost := int64(0)
	if usage.CachedInputTokens > 0 {
		cachedCost, err = roundedUpCost(usage.CachedInputTokens, rate.CachedInputMicrousdPerMillion)
		if err != nil {
			return Reconciliation{}, err
		}
	}
	outputCost, err := roundedUpCost(usage.OutputTokens, rate.OutputMicrousdPerMillion)
	if err != nil {
		return Reconciliation{}, err
	}
	totalCost, err := checkedSum(uncachedCost, cachedCost, outputCost)
	if err != nil {
		return Reconciliation{}, err
	}
	grossProfit, err := checkedDifference(usage.RetailMicrousd, totalCost)
	if err != nil {
		return Reconciliation{}, err
	}

	result := Reconciliation{
		SchemaVersion: SchemaVersion, ReservationID: usage.ReservationID,
		OfferID: usage.OfferID, OfferVersion: usage.OfferVersion, TupleKey: usage.TupleKey,
		Supplier: usage.Supplier, SupplierModelID: usage.SupplierModelID,
		SupplierRateID: rate.ID, SupplierRateVersion: rate.Version, SupplierRateDigest: rate.Digest,
		Currency: rate.Currency, InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
		OutputTokens: usage.OutputTokens, UncachedInputCOGSMicrousd: uncachedCost,
		CachedInputCOGSMicrousd: cachedCost, OutputCOGSMicrousd: outputCost,
		SupplierCOGSMicrousd: totalCost, RetailMicrousd: usage.RetailMicrousd,
		GrossProfitMicrousd: grossProfit, ReservedAt: usage.ReservedAt, SettledAt: usage.SettledAt,
	}
	if usage.RetailMicrousd > 0 {
		result.GrossMarginDefined = true
		result.GrossMarginBPS, err = floorRatioBPS(grossProfit, usage.RetailMicrousd)
		if err != nil {
			return Reconciliation{}, err
		}
	}
	result.Digest, err = reconciliationDigest(result)
	if err != nil {
		return Reconciliation{}, err
	}
	return result, nil
}

func validateRateDraft(rate SupplierRateDraft) error {
	required := map[string]string{
		"id": rate.ID, "offer id": rate.OfferID, "tuple key": rate.TupleKey,
		"supplier": rate.Supplier, "supplier model id": rate.SupplierModelID, "provenance": rate.Provenance,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("supplier rate %s must be non-empty and canonical", name)
		}
	}
	if rate.Version <= 0 || rate.OfferVersion <= 0 {
		return errors.New("supplier rate and offer versions must be positive")
	}
	if rate.Currency != "USD" {
		return fmt.Errorf("supplier rate currency %q is unsupported", rate.Currency)
	}
	if rate.InputMicrousdPerMillion < 0 || rate.OutputMicrousdPerMillion < 0 || rate.CachedInputMicrousdPerMillion < 0 {
		return errors.New("supplier token rates cannot be negative")
	}
	if !rate.HasCachedInputRate && rate.CachedInputMicrousdPerMillion != 0 {
		return errors.New("cached input price requires an explicit cached input rate")
	}
	if rate.ValidFrom.IsZero() || rate.ValidUntil.IsZero() || !rate.ValidUntil.After(rate.ValidFrom) {
		return errors.New("supplier rate requires an increasing validity window")
	}
	return nil
}

func validateUsage(usage SettledUsage, rate SupplierRate) error {
	required := map[string]string{
		"reservation id": usage.ReservationID, "offer id": usage.OfferID, "tuple key": usage.TupleKey,
		"supplier": usage.Supplier, "supplier model id": usage.SupplierModelID,
		"supplier rate id": usage.SupplierRateID, "supplier rate digest": usage.SupplierRateDigest,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("settled usage %s must be non-empty and canonical", name)
		}
	}
	if usage.State != SettlementSettled {
		return errors.New("supplier reconciliation requires settled usage")
	}
	if usage.OfferVersion <= 0 || usage.SupplierRateVersion <= 0 {
		return errors.New("settled usage offer and supplier rate versions must be positive")
	}
	if usage.Currency != "USD" {
		return fmt.Errorf("settled usage currency %q is unsupported", usage.Currency)
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens || usage.RetailMicrousd < 0 {
		return errors.New("settled token usage and retail charge must be non-negative, with cached input no greater than total input")
	}
	if usage.ReservedAt.IsZero() || usage.SettledAt.IsZero() || usage.SettledAt.Before(usage.ReservedAt) {
		return errors.New("settled usage requires ordered reservation and settlement timestamps")
	}
	if usage.ReservedAt.Before(rate.ValidFrom) || !usage.ReservedAt.Before(rate.ValidUntil) {
		return errors.New("supplier rate was not active when the reservation was created")
	}
	if usage.SupplierRateID != rate.ID || usage.SupplierRateVersion != rate.Version || usage.SupplierRateDigest != rate.Digest {
		return errors.New("settled usage does not reference the exact immutable supplier rate")
	}
	if usage.OfferID != rate.OfferID || usage.OfferVersion != rate.OfferVersion || usage.TupleKey != rate.TupleKey ||
		usage.Supplier != rate.Supplier || usage.SupplierModelID != rate.SupplierModelID || usage.Currency != rate.Currency {
		return errors.New("settled usage and supplier rate target identities do not match")
	}
	if usage.CachedInputTokens > 0 && !rate.HasCachedInputRate {
		return errors.New("cached input usage requires a pinned cached input supplier rate")
	}
	return nil
}

func roundedUpCost(tokens, price int64) (int64, error) {
	if tokens == 0 || price == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(price))
	product.Add(product, big.NewInt(tokensPerMillion-1))
	product.Quo(product, big.NewInt(tokensPerMillion))
	if !product.IsInt64() {
		return 0, errors.New("supplier COGS overflows int64")
	}
	return product.Int64(), nil
}

func checkedSum(values ...int64) (int64, error) {
	total := new(big.Int)
	for _, value := range values {
		total.Add(total, big.NewInt(value))
	}
	if !total.IsInt64() {
		return 0, errors.New("supplier COGS overflows int64")
	}
	return total.Int64(), nil
}

func checkedDifference(left, right int64) (int64, error) {
	value := new(big.Int).Sub(big.NewInt(left), big.NewInt(right))
	if !value.IsInt64() {
		return 0, errors.New("gross profit overflows int64")
	}
	return value.Int64(), nil
}

func floorRatioBPS(profit, revenue int64) (int64, error) {
	numerator := new(big.Int).Mul(big.NewInt(profit), big.NewInt(10_000))
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, big.NewInt(revenue), remainder)
	if numerator.Sign() < 0 && remainder.Sign() != 0 {
		quotient.Sub(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, errors.New("gross margin basis points overflow int64")
	}
	return quotient.Int64(), nil
}

func reconciliationDigest(result Reconciliation) (string, error) {
	result.Digest = ""
	return digestJSON(result)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("supplier accounting evidence could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return digestPrefix + hex.EncodeToString(digest[:]), nil
}

func canonicalTime(value time.Time) time.Time { return value.Round(0).UTC() }
