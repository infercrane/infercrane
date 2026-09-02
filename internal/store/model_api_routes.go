package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/managedbilling"
	"github.com/infercrane/infercrane/internal/modelapireconciliation"
	"github.com/infercrane/infercrane/internal/modelapirouting"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

// ReserveModelAPIUsage durably reserves the entitlement's per-request ceiling
// before any supplier transmission. Routing is already resolved from an
// in-memory snapshot; this transaction is the money fence only.
func (s *Store) ReserveModelAPIUsage(ctx context.Context, request modelapirouting.ReservationRequest) (modelapirouting.Reservation, error) {
	if err := request.Validate(); err != nil {
		return modelapirouting.Reservation{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	defer tx.Rollback()

	if existing, found, lookupErr := modelAPIReservationMaybe(ctx, tx, request.TenantID, request.ID); lookupErr != nil {
		return modelapirouting.Reservation{}, lookupErr
	} else if found {
		if existing.ProductID != request.ProductID || existing.EntitlementID != request.EntitlementID || existing.RetailRateDigest != request.RetailRate.ContractDigest ||
			existing.SupplyPlanID != request.SupplyPlanID || existing.CandidateID != request.CandidateID || existing.OfferID != request.OfferID ||
			existing.OfferVersion != request.OfferVersion || existing.Supplier != request.Supplier || existing.SupplierModelID != request.SupplierModelID ||
			existing.TargetBindingID != request.TargetBindingID || existing.TargetBindingDigest != request.TargetBindingDigest {
			return modelapirouting.Reservation{}, fmt.Errorf("%w: hosted reservation identity belongs to a different contract", ErrConflict)
		}
		return existing, tx.Commit()
	}

	var operatorTenant, servingPlan, supplyPlan, rateID, state string
	var rateVersion int
	var maxRequest int64
	var requestsPerMinute, tokensPerMinute, monthlySpend sql.NullInt64
	var validFrom time.Time
	var validUntil sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT e.operator_tenant_id,e.serving_plan_id,p.supply_plan_id,e.retail_rate_card_id,e.retail_rate_version,e.state,e.requests_per_minute,e.tokens_per_minute,e.monthly_spend_microusd,COALESCE(e.max_request_microusd,0),e.valid_from,e.valid_until FROM model_api_product_entitlements e JOIN model_api_operator_publications p ON p.product_id=e.product_id AND p.operator_tenant_id=e.operator_tenant_id AND p.serving_plan_id=e.serving_plan_id WHERE e.id=? AND e.customer_tenant_id=? AND e.product_id=? FOR SHARE OF e,p`, request.EntitlementID, request.TenantID, request.ProductID).Scan(&operatorTenant, &servingPlan, &supplyPlan, &rateID, &rateVersion, &state, &requestsPerMinute, &tokensPerMinute, &monthlySpend, &maxRequest, &validFrom, &validUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapirouting.Reservation{}, ErrNotFound
	}
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	at := request.CreatedAt.UTC()
	active := state == "active" && !at.Before(validFrom.UTC()) && (!validUntil.Valid || at.Before(validUntil.Time.UTC()))
	if !active || requestsPerMinute.Valid || tokensPerMinute.Valid || monthlySpend.Valid || request.RetailRate.CachedInputMicrousdPerMillion != nil || operatorTenant != request.OperatorTenantID || servingPlan != request.ServingPlanID || supplyPlan != request.SupplyPlanID || rateID != request.RetailRate.ID || rateVersion != request.RetailRate.Version || maxRequest != request.MaxRequestMicrousd {
		return modelapirouting.Reservation{}, fmt.Errorf("%w: hosted entitlement changed before billing authorization", ErrConflict)
	}

	var storedDigest string
	var storedInput, storedOutput int64
	var storedCachedInput sql.NullInt64
	var storedFrom, storedUntil time.Time
	err = tx.QueryRowContext(ctx, `SELECT contract_digest,input_microusd_per_million,cached_input_microusd_per_million,output_microusd_per_million,valid_from,valid_until FROM model_api_retail_rate_cards WHERE product_id=? AND id=? AND version=?`, request.ProductID, rateID, rateVersion).Scan(&storedDigest, &storedInput, &storedCachedInput, &storedOutput, &storedFrom, &storedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapirouting.Reservation{}, ErrNotFound
	}
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	if storedCachedInput.Valid || storedDigest != request.RetailRate.ContractDigest || storedInput != request.RetailRate.InputMicrousdPerMillion || storedOutput != request.RetailRate.OutputMicrousdPerMillion || at.Before(storedFrom.UTC()) || !at.Before(storedUntil.UTC()) {
		return modelapirouting.Reservation{}, fmt.Errorf("%w: immutable retail rate changed or expired before billing authorization", ErrConflict)
	}

	var supplier, supplierModel string
	var offerVersion int64
	err = tx.QueryRowContext(ctx, `SELECT o.supplier,o.supplier_model_id,o.version FROM model_api_supply_plan_candidates c JOIN model_api_supplier_offers o ON o.id=c.offer_id AND o.version=c.offer_version AND o.managed_product_id=c.managed_product_id WHERE c.plan_id=? AND c.managed_product_id=? AND c.candidate_id=? AND c.offer_id=? AND c.offer_version=? AND c.disposition IN ('primary','fallback')`, supplyPlan, request.ProductID, request.CandidateID, request.OfferID, request.OfferVersion).Scan(&supplier, &supplierModel, &offerVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapirouting.Reservation{}, ErrNotFound
	}
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	if supplier != request.Supplier || supplierModel != request.SupplierModelID || offerVersion != request.OfferVersion {
		return modelapirouting.Reservation{}, fmt.Errorf("%w: hosted supplier candidate changed before billing authorization", ErrConflict)
	}
	offer, err := scanModelAPISupplierOffer(tx.QueryRowContext(ctx, modelAPISupplierOfferSelect+` WHERE operator_tenant_id=? AND id=? AND version=?`, request.OperatorTenantID, request.OfferID, request.OfferVersion))
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	supplierRate, err := supplierRateForOffer(offer)
	if err != nil || at.Before(supplierRate.ValidFrom) || !at.Before(supplierRate.ValidUntil) {
		return modelapirouting.Reservation{}, fmt.Errorf("%w: supplier cost rate is incomplete or expired before billing authorization", ErrConflict)
	}
	if request.TargetBindingID != "" {
		var bindingCount int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM model_api_target_bindings WHERE id=? AND contract_digest=? AND operator_tenant_id=? AND managed_product_id=? AND offer_id=? AND offer_version=? AND adapter=? AND supplier_model_id=? AND region=? AND valid_from<=? AND valid_until>?`,
			request.TargetBindingID, request.TargetBindingDigest, request.OperatorTenantID, request.ProductID, request.OfferID, request.OfferVersion,
			offer.Adapter, request.SupplierModelID, offer.Region, at, at).Scan(&bindingCount)
		if err != nil || bindingCount != 1 {
			return modelapirouting.Reservation{}, fmt.Errorf("%w: immutable target binding changed before billing authorization", ErrConflict)
		}
	} else if offer.Adapter == supplieradapter.RunPodVLLMAdapterName {
		return modelapirouting.Reservation{}, fmt.Errorf("%w: RunPod usage requires an immutable target binding", ErrConflict)
	}

	var balance, reserved, debt int64
	err = tx.QueryRowContext(ctx, `SELECT balance_microusd,reserved_microusd,debt_microusd FROM managed_wallets WHERE tenant_id=? FOR UPDATE`, request.TenantID).Scan(&balance, &reserved, &debt)
	if errors.Is(err, sql.ErrNoRows) || balance-reserved-debt < request.MaxRequestMicrousd {
		return modelapirouting.Reservation{}, modelapirouting.ErrInsufficientPrepaidBalance
	}
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	stamp := request.CreatedAt.UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO model_api_usage_reservations(id,customer_tenant_id,product_id,entitlement_id,operator_tenant_id,serving_plan_id,supply_plan_id,candidate_id,offer_id,offer_version,supplier,supplier_model_id,target_binding_id,target_binding_digest,supplier_rate_id,supplier_rate_version,supplier_rate_digest,retail_rate_card_id,retail_rate_version,retail_rate_contract_digest,input_microusd_per_million,output_microusd_per_million,reserved_microusd,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'reserved',?,?)`,
		request.ID, request.TenantID, request.ProductID, request.EntitlementID, request.OperatorTenantID,
		request.ServingPlanID, request.SupplyPlanID, request.CandidateID, request.OfferID, request.OfferVersion,
		request.Supplier, request.SupplierModelID, nullableModelAPIString(request.TargetBindingID), nullableModelAPIString(request.TargetBindingDigest),
		supplierRate.ID, supplierRate.Version, supplierRate.Digest, request.RetailRate.ID, request.RetailRate.Version,
		request.RetailRate.ContractDigest, request.RetailRate.InputMicrousdPerMillion,
		request.RetailRate.OutputMicrousdPerMillion, request.MaxRequestMicrousd, stamp, stamp)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd+?,updated_at=? WHERE tenant_id=?`, request.MaxRequestMicrousd, stamp, request.TenantID); err != nil {
		return modelapirouting.Reservation{}, err
	}
	if err = tx.Commit(); err != nil {
		return modelapirouting.Reservation{}, err
	}
	return s.ModelAPIUsageReservation(ctx, request.TenantID, request.ID)
}

func (s *Store) MarkModelAPIUsageTransmitted(ctx context.Context, tenant, reservationID string, at time.Time) error {
	return s.advanceModelAPIUsage(ctx, tenant, reservationID, "transmitted", at)
}

func (s *Store) MarkModelAPIUsageResponseStarted(ctx context.Context, tenant, reservationID string, at time.Time) error {
	return s.advanceModelAPIUsage(ctx, tenant, reservationID, "response_started", at)
}

func (s *Store) advanceModelAPIUsage(ctx context.Context, tenant, reservationID, target string, at time.Time) error {
	if tenant == "" || reservationID == "" || at.IsZero() || (target != "transmitted" && target != "response_started") {
		return errors.New("tenant, reservation, valid state, and timestamp are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row, err := modelAPIReservationForUpdate(ctx, tx, tenant, reservationID)
	if err != nil {
		return err
	}
	if target == "transmitted" {
		if row.State == "transmitted" || row.State == "response_started" || row.State == "pending_reconciliation" || row.State == "settled" {
			return tx.Commit()
		}
		if row.State != "reserved" {
			return fmt.Errorf("%w: only an unsent reservation can be marked transmitted", ErrConflict)
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_api_usage_reservations SET state='transmitted',transmitted_at=?,updated_at=? WHERE customer_tenant_id=? AND id=?`, at.UTC(), at.UTC(), tenant, reservationID)
	} else {
		if row.State == "response_started" || row.State == "pending_reconciliation" || row.State == "settled" {
			return tx.Commit()
		}
		if row.State != "transmitted" {
			return fmt.Errorf("%w: response start requires a transmitted reservation", ErrConflict)
		}
		_, err = tx.ExecContext(ctx, `UPDATE model_api_usage_reservations SET state='response_started',response_started_at=?,updated_at=? WHERE customer_tenant_id=? AND id=?`, at.UTC(), at.UTC(), tenant, reservationID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SettleModelAPIUsage(ctx context.Context, tenant, reservationID string, usage modelapirouting.Usage) (modelapirouting.Reservation, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	defer tx.Rollback()
	row, err := modelAPIReservationForUpdate(ctx, tx, tenant, reservationID)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	if row.State == "settled" || row.State == "released" {
		return row, tx.Commit()
	}
	if row.State == "reserved" {
		return modelapirouting.Reservation{}, fmt.Errorf("%w: unsent hosted usage cannot be settled", ErrConflict)
	}
	stamp := time.Now().UTC()
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		_, err = tx.ExecContext(ctx, `UPDATE model_api_usage_reservations SET state='pending_reconciliation',resolution='supplier usage absent; reservation retained',updated_at=? WHERE customer_tenant_id=? AND id=?`, stamp, tenant, reservationID)
		if err != nil {
			return modelapirouting.Reservation{}, err
		}
		row.State, row.Resolution, row.UpdatedAt = "pending_reconciliation", "supplier usage absent; reservation retained", stamp
		return row, tx.Commit()
	}
	if usage.CachedInputTokens != nil && (*usage.CachedInputTokens < 0 || *usage.CachedInputTokens > *usage.InputTokens) {
		return modelapirouting.Reservation{}, errors.New("cached input usage must be non-negative and no greater than input usage")
	}
	actual, err := managedbilling.TokenCostMicrousd(*usage.InputTokens, *usage.OutputTokens, row.InputMicrousdPerMillion, row.OutputMicrousdPerMillion)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	resolution := "observed token usage at reserved retail rate"
	if actual > row.ReservedMicrousd {
		actual = row.ReservedMicrousd
		resolution = "observed cost exceeded request ceiling; charged reservation ceiling"
	}
	offer, err := scanModelAPISupplierOffer(tx.QueryRowContext(ctx, modelAPISupplierOfferSelect+` WHERE operator_tenant_id=? AND id=? AND version=?`, row.OperatorTenantID, row.OfferID, row.OfferVersion))
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	supplierRate, err := supplierRateForOffer(offer)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	pinnedRateID, pinnedRateVersion, pinnedRateDigest := row.SupplierRateID, row.SupplierRateVersion, row.SupplierRateDigest
	if pinnedRateID == "" && pinnedRateVersion == 0 && pinnedRateDigest == "" {
		// Reservations created before the supplier-rate pin migration remain
		// reconcilable because their referenced offer revision is immutable.
		pinnedRateID, pinnedRateVersion, pinnedRateDigest = supplierRate.ID, supplierRate.Version, supplierRate.Digest
	}
	cachedInput := int64(0)
	if usage.CachedInputTokens != nil && supplierRate.HasCachedInputRate {
		cachedInput = int64(*usage.CachedInputTokens)
	}
	cogs, err := modelapireconciliation.Reconcile(modelapireconciliation.SettledUsage{
		ReservationID: row.ID, State: modelapireconciliation.SettlementSettled,
		OfferID: row.OfferID, OfferVersion: row.OfferVersion, TupleKey: offer.TupleKey,
		Supplier: row.Supplier, SupplierModelID: row.SupplierModelID,
		SupplierRateID: pinnedRateID, SupplierRateVersion: pinnedRateVersion, SupplierRateDigest: pinnedRateDigest,
		Currency: "USD", InputTokens: int64(*usage.InputTokens), CachedInputTokens: cachedInput,
		OutputTokens: int64(*usage.OutputTokens), RetailMicrousd: actual,
		ReservedAt: row.CreatedAt, SettledAt: stamp,
	}, supplierRate)
	if err != nil {
		return modelapirouting.Reservation{}, fmt.Errorf("reconcile supplier COGS: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=balance_microusd-?,reserved_microusd=reserved_microusd-?,updated_at=? WHERE tenant_id=?`, actual, row.ReservedMicrousd, stamp, tenant); err != nil {
		return modelapirouting.Reservation{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO model_api_usage_ledger(id,customer_tenant_id,reservation_id,kind,currency,amount_microusd,description,created_at) VALUES(?,?,?,'settlement','USD',?,?,?) ON CONFLICT(customer_tenant_id,reservation_id,kind) DO NOTHING`, "settlement_"+reservationID, tenant, reservationID, -actual, resolution, stamp); err != nil {
		return modelapirouting.Reservation{}, err
	}
	var observedCached any
	if usage.CachedInputTokens != nil {
		observedCached = *usage.CachedInputTokens
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_api_usage_reservations SET actual_microusd=?,input_tokens=?,cached_input_tokens=?,output_tokens=?,state='settled',resolution=?,updated_at=? WHERE customer_tenant_id=? AND id=?`, actual, *usage.InputTokens, observedCached, *usage.OutputTokens, resolution, stamp, tenant, reservationID); err != nil {
		return modelapirouting.Reservation{}, err
	}
	var grossMargin any
	if cogs.GrossMarginDefined {
		grossMargin = cogs.GrossMarginBPS
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO model_api_supplier_cogs(reservation_id,customer_tenant_id,operator_tenant_id,supplier_rate_id,supplier_rate_version,supplier_rate_digest,supplier,supplier_model_id,tuple_key,currency,input_tokens,cached_input_tokens,output_tokens,uncached_input_cogs_microusd,cached_input_cogs_microusd,output_cogs_microusd,supplier_cogs_microusd,retail_microusd,gross_profit_microusd,gross_margin_defined,gross_margin_bps,reconciliation_digest,reserved_at,settled_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,'USD',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(reservation_id) DO NOTHING`,
		row.ID, tenant, row.OperatorTenantID, cogs.SupplierRateID, cogs.SupplierRateVersion, cogs.SupplierRateDigest,
		cogs.Supplier, cogs.SupplierModelID, cogs.TupleKey, cogs.InputTokens, cogs.CachedInputTokens, cogs.OutputTokens,
		cogs.UncachedInputCOGSMicrousd, cogs.CachedInputCOGSMicrousd, cogs.OutputCOGSMicrousd, cogs.SupplierCOGSMicrousd,
		cogs.RetailMicrousd, cogs.GrossProfitMicrousd, cogs.GrossMarginDefined, grossMargin, cogs.Digest, cogs.ReservedAt, cogs.SettledAt, stamp); err != nil {
		return modelapirouting.Reservation{}, err
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, stamp.Format(time.RFC3339Nano)); err != nil {
		return modelapirouting.Reservation{}, err
	}
	if err = tx.Commit(); err != nil {
		return modelapirouting.Reservation{}, err
	}
	row.ActualMicrousd, row.InputTokens, row.CachedInputTokens, row.OutputTokens, row.State, row.Resolution, row.UpdatedAt = actual, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, "settled", resolution, stamp
	return row, nil
}

func supplierRateForOffer(offer modelapisupply.Offer) (modelapireconciliation.SupplierRate, error) {
	if offer.CostRate.InputMicrousdPerMTok == nil || offer.CostRate.OutputMicrousdPerMTok == nil {
		return modelapireconciliation.SupplierRate{}, errors.New("supplier offer has no complete cost rate")
	}
	draft := modelapireconciliation.SupplierRateDraft{
		ID: offer.ID + "/supplier-cost", Version: offer.Version,
		OfferID: offer.ID, OfferVersion: offer.Version, TupleKey: offer.TupleKey,
		Supplier: offer.Supplier, SupplierModelID: offer.SupplierModelID, Currency: offer.CostRate.Currency,
		InputMicrousdPerMillion:  *offer.CostRate.InputMicrousdPerMTok,
		OutputMicrousdPerMillion: *offer.CostRate.OutputMicrousdPerMTok,
		Provenance:               offer.CostRate.Provenance, ValidFrom: offer.CostRate.ValidFrom, ValidUntil: offer.CostRate.ValidUntil,
	}
	if offer.CostRate.CachedInputMicrousdPerMTok != nil {
		draft.HasCachedInputRate = true
		draft.CachedInputMicrousdPerMillion = *offer.CostRate.CachedInputMicrousdPerMTok
	}
	return modelapireconciliation.NewSupplierRate(draft)
}

func (s *Store) ReleaseUnsentModelAPIUsage(ctx context.Context, tenant, reservationID, reason string) error {
	if tenant == "" || reservationID == "" || reason == "" {
		return errors.New("tenant, reservation, and reason are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row, err := modelAPIReservationForUpdate(ctx, tx, tenant, reservationID)
	if err != nil {
		return err
	}
	if row.State == "released" {
		return tx.Commit()
	}
	if row.State != "reserved" || row.TransmittedAt != nil {
		return fmt.Errorf("%w: transmitted or response-started usage must be reconciled, not released", ErrConflict)
	}
	stamp := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd-?,updated_at=? WHERE tenant_id=?`, row.ReservedMicrousd, stamp, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_api_usage_reservations SET state='released',resolution=?,updated_at=? WHERE customer_tenant_id=? AND id=?`, reason, stamp, tenant, reservationID); err != nil {
		return err
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, stamp.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmNoChargeModelAPIUsage releases a transmitted reservation only after
// an operator reconciler has durable supplier evidence that no charge
// occurred. This is intentionally not part of Runtime's request-path Billing
// interface.
func (s *Store) ConfirmNoChargeModelAPIUsage(ctx context.Context, tenant, reservationID, evidence string) error {
	if tenant == "" || reservationID == "" || evidence == "" {
		return errors.New("tenant, reservation, and no-charge evidence are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row, err := modelAPIReservationForUpdate(ctx, tx, tenant, reservationID)
	if err != nil {
		return err
	}
	if row.State == "released" {
		return tx.Commit()
	}
	if row.State != "pending_reconciliation" && row.State != "transmitted" && row.State != "response_started" {
		return fmt.Errorf("%w: only ambiguous hosted usage can be confirmed uncharged", ErrConflict)
	}
	stamp := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd-?,updated_at=? WHERE tenant_id=?`, row.ReservedMicrousd, stamp, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE model_api_usage_reservations SET state='released',resolution=?,updated_at=? WHERE customer_tenant_id=? AND id=?`, "supplier confirmed no charge: "+evidence, stamp, tenant, reservationID); err != nil {
		return err
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, stamp.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PendingModelAPIUsageReservations(ctx context.Context, limit int) ([]modelapirouting.Reservation, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, modelAPIReservationSelect+` WHERE state='pending_reconciliation' ORDER BY updated_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]modelapirouting.Reservation, 0)
	for rows.Next() {
		item, scanErr := scanModelAPIUsageReservation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ModelAPIUsageReservation(ctx context.Context, tenant, reservationID string) (modelapirouting.Reservation, error) {
	row, found, err := modelAPIReservationMaybe(ctx, s, tenant, reservationID)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	if !found {
		return modelapirouting.Reservation{}, ErrNotFound
	}
	return row, nil
}

type modelAPIReservationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const modelAPIReservationSelect = `SELECT id,customer_tenant_id,product_id,entitlement_id,operator_tenant_id,supply_plan_id,candidate_id,offer_id,offer_version,supplier,supplier_model_id,target_binding_id,target_binding_digest,supplier_rate_id,supplier_rate_version,supplier_rate_digest,retail_rate_card_id,retail_rate_version,retail_rate_contract_digest,input_microusd_per_million,output_microusd_per_million,reserved_microusd,COALESCE(actual_microusd,0),input_tokens,cached_input_tokens,output_tokens,state,resolution,transmitted_at,response_started_at,created_at,updated_at FROM model_api_usage_reservations`

func modelAPIReservationMaybe(ctx context.Context, queryer modelAPIReservationQueryer, tenant, reservationID string) (modelapirouting.Reservation, bool, error) {
	row, err := scanModelAPIUsageReservation(queryer.QueryRowContext(ctx, modelAPIReservationSelect+` WHERE customer_tenant_id=? AND id=?`, tenant, reservationID))
	if errors.Is(err, sql.ErrNoRows) {
		return modelapirouting.Reservation{}, false, nil
	}
	return row, err == nil, err
}

func modelAPIReservationForUpdate(ctx context.Context, tx *tx, tenant, reservationID string) (modelapirouting.Reservation, error) {
	row, err := scanModelAPIUsageReservation(tx.QueryRowContext(ctx, modelAPIReservationSelect+` WHERE customer_tenant_id=? AND id=? FOR UPDATE`, tenant, reservationID))
	if errors.Is(err, sql.ErrNoRows) {
		return modelapirouting.Reservation{}, ErrNotFound
	}
	return row, err
}

func scanModelAPIUsageReservation(row interface{ Scan(...any) error }) (modelapirouting.Reservation, error) {
	var out modelapirouting.Reservation
	var transmitted, responseStarted sql.NullTime
	var targetBindingID, targetBindingDigest, supplierRateID, supplierRateDigest sql.NullString
	var supplierRateVersion sql.NullInt64
	err := row.Scan(&out.ID, &out.TenantID, &out.ProductID, &out.EntitlementID, &out.OperatorTenantID, &out.SupplyPlanID, &out.CandidateID,
		&out.OfferID, &out.OfferVersion, &out.Supplier, &out.SupplierModelID, &targetBindingID, &targetBindingDigest, &supplierRateID, &supplierRateVersion, &supplierRateDigest,
		&out.RetailRateID, &out.RetailRateVersion,
		&out.RetailRateDigest, &out.InputMicrousdPerMillion, &out.OutputMicrousdPerMillion, &out.ReservedMicrousd,
		&out.ActualMicrousd, &out.InputTokens, &out.CachedInputTokens, &out.OutputTokens, &out.State, &out.Resolution, &transmitted,
		&responseStarted, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return modelapirouting.Reservation{}, err
	}
	if transmitted.Valid {
		value := transmitted.Time.UTC()
		out.TransmittedAt = &value
	}
	if responseStarted.Valid {
		value := responseStarted.Time.UTC()
		out.ResponseStartedAt = &value
	}
	out.TargetBindingID, out.TargetBindingDigest = targetBindingID.String, targetBindingDigest.String
	out.SupplierRateID, out.SupplierRateVersion, out.SupplierRateDigest = supplierRateID.String, supplierRateVersion.Int64, supplierRateDigest.String
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	return out, nil
}

// ModelAPISupplierCOGS returns the immutable private economics record for one
// settled reservation. It is operator-scoped and never belongs in customer
// catalog projections.
func (s *Store) ModelAPISupplierCOGS(ctx context.Context, operatorTenant, reservationID string) (modelapireconciliation.Reconciliation, error) {
	if operatorTenant == "" || reservationID == "" {
		return modelapireconciliation.Reconciliation{}, errors.New("operator tenant and reservation id are required")
	}
	var out modelapireconciliation.Reconciliation
	var grossMargin sql.NullInt64
	err := s.QueryRowContext(ctx, `SELECT c.reservation_id,r.offer_id,r.offer_version,c.supplier_rate_id,c.supplier_rate_version,c.supplier_rate_digest,c.supplier,c.supplier_model_id,c.tuple_key,c.currency,c.input_tokens,c.cached_input_tokens,c.output_tokens,c.uncached_input_cogs_microusd,c.cached_input_cogs_microusd,c.output_cogs_microusd,c.supplier_cogs_microusd,c.retail_microusd,c.gross_profit_microusd,c.gross_margin_defined,c.gross_margin_bps,c.reconciliation_digest,c.reserved_at,c.settled_at FROM model_api_supplier_cogs c JOIN model_api_usage_reservations r ON r.id=c.reservation_id WHERE c.operator_tenant_id=? AND c.reservation_id=?`, operatorTenant, reservationID).Scan(
		&out.ReservationID, &out.OfferID, &out.OfferVersion, &out.SupplierRateID, &out.SupplierRateVersion, &out.SupplierRateDigest, &out.Supplier, &out.SupplierModelID,
		&out.TupleKey, &out.Currency, &out.InputTokens, &out.CachedInputTokens, &out.OutputTokens,
		&out.UncachedInputCOGSMicrousd, &out.CachedInputCOGSMicrousd, &out.OutputCOGSMicrousd, &out.SupplierCOGSMicrousd,
		&out.RetailMicrousd, &out.GrossProfitMicrousd, &out.GrossMarginDefined, &grossMargin, &out.Digest, &out.ReservedAt, &out.SettledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapireconciliation.Reconciliation{}, ErrNotFound
	}
	if err != nil {
		return modelapireconciliation.Reconciliation{}, err
	}
	out.SchemaVersion = modelapireconciliation.SchemaVersion
	if grossMargin.Valid {
		out.GrossMarginBPS = grossMargin.Int64
	}
	out.ReservedAt, out.SettledAt = out.ReservedAt.UTC(), out.SettledAt.UTC()
	return out, nil
}

// ModelAPIBillingAdapter gives Runtime the narrow money-fence interface while
// keeping Store method names explicit next to existing managed endpoint APIs.
type ModelAPIBillingAdapter struct{ Store *Store }

func (a ModelAPIBillingAdapter) Reserve(ctx context.Context, request modelapirouting.ReservationRequest) (modelapirouting.Reservation, error) {
	return a.Store.ReserveModelAPIUsage(ctx, request)
}
func (a ModelAPIBillingAdapter) MarkTransmitted(ctx context.Context, tenant, reservation string, at time.Time) error {
	return a.Store.MarkModelAPIUsageTransmitted(ctx, tenant, reservation, at)
}
func (a ModelAPIBillingAdapter) MarkResponseStarted(ctx context.Context, tenant, reservation string, at time.Time) error {
	return a.Store.MarkModelAPIUsageResponseStarted(ctx, tenant, reservation, at)
}
func (a ModelAPIBillingAdapter) Settle(ctx context.Context, tenant, reservation string, usage modelapirouting.Usage) (modelapirouting.Reservation, error) {
	return a.Store.SettleModelAPIUsage(ctx, tenant, reservation, usage)
}
func (a ModelAPIBillingAdapter) ReleaseUnsent(ctx context.Context, tenant, reservation, reason string) error {
	return a.Store.ReleaseUnsentModelAPIUsage(ctx, tenant, reservation, reason)
}
