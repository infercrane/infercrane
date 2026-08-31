package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

func (s *Store) ManagedWallet(ctx context.Context, tenant string) (domain.ManagedWallet, error) {
	if tenant == "" {
		return domain.ManagedWallet{}, errors.New("tenant is required")
	}
	var out domain.ManagedWallet
	var updated string
	err := s.QueryRowContext(ctx, `SELECT tenant_id,currency,balance_microusd,reserved_microusd,debt_microusd,updated_at FROM managed_wallets WHERE tenant_id=?`, tenant).Scan(&out.TenantID, &out.Currency, &out.BalanceMicrousd, &out.ReservedMicrousd, &out.DebtMicrousd, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedWallet{TenantID: tenant, Currency: "USD"}, nil
	}
	if err != nil {
		return domain.ManagedWallet{}, err
	}
	out.AvailableMicrousd = out.BalanceMicrousd - out.ReservedMicrousd - out.DebtMicrousd
	if out.AvailableMicrousd < 0 {
		out.AvailableMicrousd = 0
	}
	out.UpdatedAt = parseTime(updated)
	return out, nil
}

// CreditManagedWallet is an idempotent funding primitive for an already
// collected prepaid payment or an operator-issued credit. Payment collection
// itself intentionally remains outside this store boundary.
func (s *Store) CreditManagedWallet(ctx context.Context, tenant, creditID, description string, amountMicrousd int64) (domain.ManagedWallet, error) {
	if tenant == "" || creditID == "" || description == "" || amountMicrousd < 1 {
		return domain.ManagedWallet{}, errors.New("tenant, credit id, description, and a positive amount are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedWallet{}, err
	}
	defer tx.Rollback()
	stamp := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_wallets(tenant_id,currency,balance_microusd,reserved_microusd,debt_microusd,updated_at) VALUES(?, 'USD', 0, 0, 0, ?) ON CONFLICT(tenant_id) DO NOTHING`, tenant, stamp); err != nil {
		return domain.ManagedWallet{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO managed_wallet_ledger(id,tenant_id,kind,currency,amount_microusd,description,created_at) VALUES(?,?,'credit','USD',?,?,?) ON CONFLICT(id) DO NOTHING`, creditID, tenant, amountMicrousd, description, stamp)
	if err != nil {
		return domain.ManagedWallet{}, err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 1 {
		if err = applyManagedCreditTx(ctx, tx, tenant, amountMicrousd, stamp); err != nil {
			return domain.ManagedWallet{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.ManagedWallet{}, err
	}
	return s.ManagedWallet(ctx, tenant)
}

// ProcessManagedPaymentEvent records a provider-signed webhook and its wallet
// mutation in one transaction. A browser redirect can never call this boundary.
// Provider/session is globally idempotent and transaction-locked so concurrent
// webhook deliveries cannot grant duplicate spending authority.
func (s *Store) ProcessManagedPaymentEvent(ctx context.Context, payment domain.ManagedPaymentEvent) (domain.ManagedPaymentResult, error) {
	result := domain.ManagedPaymentResult{Provider: payment.Provider, EventID: payment.EventID}
	if payment.Provider == "" || payment.EventID == "" || payment.EventType == "" || payment.PayloadDigest == "" {
		return result, errors.New("payment provider, event ID, event type, and payload digest are required")
	}
	if len(payment.PayloadDigest) != 64 {
		return result, errors.New("payment payload digest must be SHA-256 hex")
	}
	metadataJSON, err := validManagedPaymentMetadata(payment.MetadataJSON)
	if err != nil {
		return result, err
	}
	if payment.Operation == "" && payment.SessionID != "" {
		payment.Operation = "credit"
	}
	if payment.Apply && payment.Operation == "credit" && (payment.TenantID == "" || payment.SessionID == "" || payment.PaymentIntentID == "" || payment.AmountMicrousd <= 0 || payment.Currency != "USD") {
		return result, errors.New("applied credit requires tenant, session, payment intent, positive USD amount, and currency")
	}
	if payment.Apply && payment.Operation == "refund" && (payment.PaymentIntentID == "" || payment.RefundedMicrousd <= 0 || payment.Currency != "USD") {
		return result, errors.New("applied refund requires payment intent, positive cumulative refunded USD amount, and currency")
	}
	if payment.Apply && payment.Operation != "credit" && payment.Operation != "refund" {
		return result, errors.New("applied payment operation is invalid")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	stamp := now()
	inserted, err := tx.ExecContext(ctx, `INSERT INTO managed_payment_events(provider,event_id,tenant_id,session_id,event_type,payload_digest,status,metadata_json,created_at) VALUES(?,?,?,?,?,?,'received',?::jsonb,?) ON CONFLICT(provider,event_id) DO NOTHING`, payment.Provider, payment.EventID, null(payment.TenantID), null(payment.SessionID), payment.EventType, payment.PayloadDigest, metadataJSON, stamp)
	if err != nil {
		return result, err
	}
	if rows, rowsErr := inserted.RowsAffected(); rowsErr != nil {
		return result, rowsErr
	} else if rows == 0 {
		var digest, status string
		if err = tx.QueryRowContext(ctx, `SELECT payload_digest,status FROM managed_payment_events WHERE provider=? AND event_id=?`, payment.Provider, payment.EventID).Scan(&digest, &status); err != nil {
			return result, err
		}
		if digest != payment.PayloadDigest {
			return result, fmt.Errorf("%w: payment event ID has a different payload", ErrConflict)
		}
		result.Status = status
		return result, tx.Commit()
	}
	if !payment.Apply {
		if _, err = tx.ExecContext(ctx, `UPDATE managed_payment_events SET status='ignored',error_code=?,processed_at=? WHERE provider=? AND event_id=?`, null(payment.IgnoreReason), stamp, payment.Provider, payment.EventID); err != nil {
			return result, err
		}
		result.Status = "ignored"
		return result, tx.Commit()
	}
	if payment.Operation == "refund" {
		return processManagedRefundTx(ctx, tx, payment, result, stamp)
	}

	// Serialize different event IDs for the same Checkout session. This is a
	// transaction-scoped PostgreSQL lock, not a process-local mutex.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, managedPaymentLockID(payment.Provider, payment.SessionID)); err != nil {
		return result, err
	}
	var creditedTenant, creditedEvent, ledgerID, currency, paymentIntentID string
	var creditedAmount int64
	err = tx.QueryRowContext(ctx, `SELECT tenant_id,event_id,ledger_id,currency,amount_microusd,COALESCE(payment_intent_id,'') FROM managed_payment_credits WHERE provider=? AND session_id=?`, payment.Provider, payment.SessionID).Scan(&creditedTenant, &creditedEvent, &ledgerID, &currency, &creditedAmount, &paymentIntentID)
	if err == nil {
		if creditedTenant != payment.TenantID || currency != payment.Currency || creditedAmount != payment.AmountMicrousd || paymentIntentID != payment.PaymentIntentID {
			return result, fmt.Errorf("%w: checkout session has a different credited intent", ErrConflict)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE managed_payment_events SET tenant_id=?,session_id=?,status='applied',error_code=NULL,processed_at=? WHERE provider=? AND event_id=?`, payment.TenantID, payment.SessionID, stamp, payment.Provider, payment.EventID); err != nil {
			return result, err
		}
		wallet, walletErr := managedWalletTx(ctx, tx, payment.TenantID)
		if walletErr != nil {
			return result, walletErr
		}
		entry, entryErr := managedWalletLedgerEntryTx(ctx, tx, ledgerID)
		if entryErr != nil {
			return result, entryErr
		}
		result.Status, result.Wallet, result.LedgerEntry = "applied", &wallet, &entry
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_wallets(tenant_id,currency,balance_microusd,reserved_microusd,debt_microusd,updated_at) VALUES(?,'USD',0,0,0,?) ON CONFLICT(tenant_id) DO NOTHING`, payment.TenantID, stamp); err != nil {
		return result, err
	}
	ledgerDigest := sha256.Sum256([]byte(payment.Provider + "\x00" + payment.SessionID))
	ledgerID = "credit_" + hex.EncodeToString(ledgerDigest[:16])
	description := "Stripe prepaid balance"
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_wallet_ledger(id,tenant_id,kind,currency,amount_microusd,description,created_at) VALUES(?,?,'credit','USD',?,?,?)`, ledgerID, payment.TenantID, payment.AmountMicrousd, description, stamp); err != nil {
		return result, err
	}
	if err = applyManagedCreditTx(ctx, tx, payment.TenantID, payment.AmountMicrousd, stamp); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_payment_credits(provider,session_id,tenant_id,event_id,ledger_id,currency,amount_microusd,payment_intent_id,created_at) VALUES(?,?,?,?,?,'USD',?,?,?)`, payment.Provider, payment.SessionID, payment.TenantID, payment.EventID, ledgerID, payment.AmountMicrousd, payment.PaymentIntentID, stamp); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_payment_events SET tenant_id=?,session_id=?,status='applied',error_code=NULL,processed_at=? WHERE provider=? AND event_id=?`, payment.TenantID, payment.SessionID, stamp, payment.Provider, payment.EventID); err != nil {
		return result, err
	}
	wallet, err := managedWalletTx(ctx, tx, payment.TenantID)
	if err != nil {
		return result, err
	}
	entry, err := managedWalletLedgerEntryTx(ctx, tx, ledgerID)
	if err != nil {
		return result, err
	}
	result.Status, result.CreditApplied, result.Wallet, result.LedgerEntry = "applied", true, &wallet, &entry
	if err = tx.Commit(); err != nil {
		return domain.ManagedPaymentResult{Provider: payment.Provider, EventID: payment.EventID}, err
	}
	return result, nil
}

func processManagedRefundTx(ctx context.Context, tx *tx, payment domain.ManagedPaymentEvent, result domain.ManagedPaymentResult, stamp string) (domain.ManagedPaymentResult, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, managedPaymentLockID(payment.Provider, payment.PaymentIntentID)); err != nil {
		return result, err
	}
	var tenant, sessionID, currency string
	var credited, alreadyRefunded int64
	err := tx.QueryRowContext(ctx, `SELECT tenant_id,session_id,currency,amount_microusd,refunded_microusd FROM managed_payment_credits WHERE provider=? AND payment_intent_id=? FOR UPDATE`, payment.Provider, payment.PaymentIntentID).Scan(&tenant, &sessionID, &currency, &credited, &alreadyRefunded)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("%w: refunded payment intent has no credited InferCrane checkout", ErrNotFound)
	}
	if err != nil {
		return result, err
	}
	if currency != payment.Currency || payment.RefundedMicrousd > credited {
		return result, fmt.Errorf("%w: cumulative refund exceeds the credited payment intent", ErrConflict)
	}
	delta := payment.RefundedMicrousd - alreadyRefunded
	if delta < 0 {
		return result, fmt.Errorf("%w: cumulative refund moved backwards", ErrConflict)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_payment_events SET tenant_id=?,session_id=?,status='applied',error_code=NULL,processed_at=? WHERE provider=? AND event_id=?`, tenant, sessionID, stamp, payment.Provider, payment.EventID); err != nil {
		return result, err
	}
	if delta == 0 {
		wallet, walletErr := managedWalletTx(ctx, tx, tenant)
		if walletErr != nil {
			return result, walletErr
		}
		result.Status, result.Wallet = "applied", &wallet
		return result, tx.Commit()
	}
	var balance, reserved, debt int64
	if err = tx.QueryRowContext(ctx, `SELECT balance_microusd,reserved_microusd,debt_microusd FROM managed_wallets WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&balance, &reserved, &debt); err != nil {
		return result, err
	}
	available := balance - reserved
	if available < 0 {
		return result, fmt.Errorf("%w: managed wallet reservation invariant failed", ErrConflict)
	}
	appliedToBalance := delta
	if appliedToBalance > available {
		appliedToBalance = available
	}
	debtIncrease := delta - appliedToBalance
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=balance_microusd-?,debt_microusd=debt_microusd+?,updated_at=? WHERE tenant_id=?`, appliedToBalance, debtIncrease, stamp, tenant); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_payment_credits SET refunded_microusd=? WHERE provider=? AND payment_intent_id=?`, payment.RefundedMicrousd, payment.Provider, payment.PaymentIntentID); err != nil {
		return result, err
	}
	ledgerDigest := sha256.Sum256([]byte(payment.Provider + "\x00" + payment.EventID))
	ledgerID := "refund_" + hex.EncodeToString(ledgerDigest[:16])
	description := "Stripe prepaid balance refund"
	if debtIncrease > 0 {
		description = "Stripe prepaid balance refund; consumed or reserved funds suspended"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_wallet_ledger(id,tenant_id,kind,currency,amount_microusd,description,created_at) VALUES(?,?,'refund','USD',?,?,?)`, ledgerID, tenant, -delta, description, stamp); err != nil {
		return result, err
	}
	wallet, err := managedWalletTx(ctx, tx, tenant)
	if err != nil {
		return result, err
	}
	entry, err := managedWalletLedgerEntryTx(ctx, tx, ledgerID)
	if err != nil {
		return result, err
	}
	result.Status, result.RefundApplied, result.Wallet, result.LedgerEntry = "applied", true, &wallet, &entry
	if err = tx.Commit(); err != nil {
		return domain.ManagedPaymentResult{Provider: payment.Provider, EventID: payment.EventID}, err
	}
	return result, nil
}

func applyManagedCreditTx(ctx context.Context, tx *tx, tenant string, amountMicrousd int64, stamp string) error {
	_, err := tx.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=balance_microusd+GREATEST(?-debt_microusd,0),debt_microusd=GREATEST(debt_microusd-?,0),updated_at=? WHERE tenant_id=?`, amountMicrousd, amountMicrousd, stamp, tenant)
	return err
}

func reconcileManagedDebtTx(ctx context.Context, tx *tx, tenant string, stamp string) error {
	var balance, reserved, debt int64
	if err := tx.QueryRowContext(ctx, `SELECT balance_microusd,reserved_microusd,debt_microusd FROM managed_wallets WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&balance, &reserved, &debt); err != nil {
		return err
	}
	available := balance - reserved
	if available <= 0 || debt == 0 {
		return nil
	}
	reconciled := debt
	if reconciled > available {
		reconciled = available
	}
	_, err := tx.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=balance_microusd-?,debt_microusd=debt_microusd-?,updated_at=? WHERE tenant_id=?`, reconciled, reconciled, stamp, tenant)
	return err
}

// managedPaymentLockID maps a provider/session identity onto PostgreSQL's
// signed bigint advisory-lock namespace. Hashing locally avoids passing the
// NUL-delimited identity through PostgreSQL's UTF-8 text protocol.
func managedPaymentLockID(provider, sessionID string) int64 {
	digest := sha256.Sum256([]byte(provider + "\x00" + sessionID))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func validManagedPaymentMetadata(raw string) (string, error) {
	if raw == "" {
		raw = "{}"
	}
	var object map[string]any
	if len(raw) > 16<<10 || json.Unmarshal([]byte(raw), &object) != nil || object == nil {
		return "", errors.New("payment metadata must be a bounded JSON object")
	}
	return raw, nil
}

func managedWalletTx(ctx context.Context, tx *tx, tenant string) (domain.ManagedWallet, error) {
	var out domain.ManagedWallet
	var updated string
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id,currency,balance_microusd,reserved_microusd,debt_microusd,updated_at FROM managed_wallets WHERE tenant_id=?`, tenant).Scan(&out.TenantID, &out.Currency, &out.BalanceMicrousd, &out.ReservedMicrousd, &out.DebtMicrousd, &updated); err != nil {
		return domain.ManagedWallet{}, err
	}
	out.AvailableMicrousd = out.BalanceMicrousd - out.ReservedMicrousd - out.DebtMicrousd
	if out.AvailableMicrousd < 0 {
		out.AvailableMicrousd = 0
	}
	out.UpdatedAt = parseTime(updated)
	return out, nil
}

func managedWalletLedgerEntryTx(ctx context.Context, tx *tx, id string) (domain.ManagedWalletLedgerEntry, error) {
	var out domain.ManagedWalletLedgerEntry
	var created string
	if err := tx.QueryRowContext(ctx, `SELECT id,tenant_id,COALESCE(reservation_id,''),kind,currency,amount_microusd,description,created_at FROM managed_wallet_ledger WHERE id=?`, id).Scan(&out.ID, &out.TenantID, &out.ReservationID, &out.Kind, &out.Currency, &out.AmountMicrousd, &out.Description, &created); err != nil {
		return domain.ManagedWalletLedgerEntry{}, err
	}
	out.CreatedAt = parseTime(created)
	return out, nil
}

func (s *Store) AuthorizeManagedUsage(ctx context.Context, tenant, bindingID, requestID, supplier, model string) (domain.ManagedUsageAuthorization, error) {
	if tenant == "" || bindingID == "" || requestID == "" || supplier == "" || model == "" {
		return domain.ManagedUsageAuthorization{}, errors.New("tenant, binding, request, supplier, and model are required")
	}
	reservationID := managedUsageReservationID(tenant, requestID)
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedUsageAuthorization{}, err
	}
	defer tx.Rollback()
	var existingTenant, existingBinding, state string
	var reserved int64
	err = tx.QueryRowContext(ctx, `SELECT tenant_id,binding_id,reserved_microusd,state FROM managed_usage_reservations WHERE id=?`, reservationID).Scan(&existingTenant, &existingBinding, &reserved, &state)
	if err == nil {
		if existingTenant != tenant || existingBinding != bindingID {
			return domain.ManagedUsageAuthorization{}, fmt.Errorf("%w: request id belongs to another managed usage boundary", ErrConflict)
		}
		if state == "released" {
			return domain.ManagedUsageAuthorization{}, fmt.Errorf("%w: released request id cannot be reused", ErrConflict)
		}
		return domain.ManagedUsageAuthorization{ReservationID: reservationID, Required: true, ReservedMicrousd: reserved}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedUsageAuthorization{}, err
	}
	var configJSON string
	err = tx.QueryRowContext(ctx, `SELECT config_json::text FROM backend_bindings WHERE tenant_id=? AND id=?`, tenant, bindingID).Scan(&configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedUsageAuthorization{}, ErrNotFound
	}
	if err != nil {
		return domain.ManagedUsageAuthorization{}, err
	}
	config, managed, err := external.ParseManagedBindingConfig(configJSON)
	if err != nil || !managed {
		return domain.ManagedUsageAuthorization{}, fmt.Errorf("%w: managed billing policy is invalid", ErrConflict)
	}
	if config.BillingMode != "customer_wallet" {
		if err = tx.Commit(); err != nil {
			return domain.ManagedUsageAuthorization{}, err
		}
		return domain.ManagedUsageAuthorization{Required: false}, nil
	}
	if config.Adapter != supplier {
		return domain.ManagedUsageAuthorization{}, fmt.Errorf("%w: supplier does not match immutable binding policy", ErrConflict)
	}
	rateCardExpiry, err := time.Parse(time.RFC3339, config.RateCardValidUntil)
	if err != nil || !rateCardExpiry.After(time.Now().UTC()) {
		return domain.ManagedUsageAuthorization{}, fmt.Errorf("%w: managed rate card expired; supplier request was not sent", ErrConflict)
	}
	var balance, walletReserved, debt int64
	err = tx.QueryRowContext(ctx, `SELECT balance_microusd,reserved_microusd,debt_microusd FROM managed_wallets WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&balance, &walletReserved, &debt)
	if errors.Is(err, sql.ErrNoRows) || balance-walletReserved-debt < config.MaxRequestCostMicrousd {
		return domain.ManagedUsageAuthorization{}, fmt.Errorf("%w: prepaid balance is insufficient", ErrConflict)
	}
	if err != nil {
		return domain.ManagedUsageAuthorization{}, err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_usage_reservations(id,tenant_id,binding_id,supplier,model,reserved_microusd,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'reserved',?,?)`, reservationID, tenant, bindingID, supplier, model, config.MaxRequestCostMicrousd, stamp, stamp); err != nil {
		return domain.ManagedUsageAuthorization{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd+?,updated_at=? WHERE tenant_id=?`, config.MaxRequestCostMicrousd, stamp, tenant); err != nil {
		return domain.ManagedUsageAuthorization{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ManagedUsageAuthorization{}, err
	}
	return domain.ManagedUsageAuthorization{ReservationID: reservationID, Required: true, ReservedMicrousd: config.MaxRequestCostMicrousd}, nil
}

func managedUsageReservationID(tenant, requestID string) string {
	digest := sha256.Sum256([]byte(tenant + "\x00" + requestID))
	return "usage_" + hex.EncodeToString(digest[:16])
}

func (s *Store) SettleManagedUsage(ctx context.Context, tenant, reservationID string, settlement domain.ManagedUsageSettlement) (domain.ManagedUsageReservation, error) {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	defer tx.Rollback()
	row, config, err := managedReservationForUpdate(ctx, tx, tenant, reservationID)
	if err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	if row.State != "reserved" && row.State != "pending_reconciliation" {
		return row, tx.Commit()
	}
	stamp := now()
	if settlement.InputTokens == nil || settlement.OutputTokens == nil {
		if _, err = tx.ExecContext(ctx, `UPDATE managed_usage_reservations SET state='pending_reconciliation',resolution='supplier usage absent; reservation retained',updated_at=? WHERE tenant_id=? AND id=?`, stamp, tenant, reservationID); err != nil {
			return domain.ManagedUsageReservation{}, err
		}
		row.State, row.Resolution, row.UpdatedAt = "pending_reconciliation", "supplier usage absent; reservation retained", parseTime(stamp)
		return row, tx.Commit()
	}
	actual, err := managedbilling.TokenCostMicrousd(*settlement.InputTokens, *settlement.OutputTokens, config.InputMicrousdPerMTok, config.OutputMicrousdPerMTok)
	if err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	resolution := "observed token usage"
	if actual > row.ReservedMicrousd {
		actual = row.ReservedMicrousd
		resolution = "observed cost exceeded hard reservation; charged reservation ceiling"
	}
	ledgerID := "settlement_" + reservationID
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=balance_microusd-?,reserved_microusd=reserved_microusd-?,updated_at=? WHERE tenant_id=?`, actual, row.ReservedMicrousd, stamp, tenant); err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_wallet_ledger(id,tenant_id,reservation_id,kind,currency,amount_microusd,description,created_at) VALUES(?,?,?,'settlement','USD',?,?,?) ON CONFLICT(tenant_id,reservation_id,kind) DO NOTHING`, ledgerID, tenant, reservationID, -actual, resolution, stamp); err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_usage_reservations SET actual_microusd=?,input_tokens=?,output_tokens=?,state='settled',resolution=?,updated_at=? WHERE tenant_id=? AND id=?`, actual, *settlement.InputTokens, *settlement.OutputTokens, resolution, stamp, tenant, reservationID); err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, stamp); err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ManagedUsageReservation{}, err
	}
	row.ActualMicrousd, row.InputTokens, row.OutputTokens, row.State, row.Resolution = actual, settlement.InputTokens, settlement.OutputTokens, "settled", resolution
	row.UpdatedAt = parseTime(stamp)
	return row, nil
}

func (s *Store) ReleaseManagedUsage(ctx context.Context, tenant, reservationID, reason string) error {
	if tenant == "" || reservationID == "" || reason == "" {
		return errors.New("tenant, reservation, and reason are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row, _, err := managedReservationForUpdate(ctx, tx, tenant, reservationID)
	if err != nil {
		return err
	}
	if row.State != "reserved" && row.State != "pending_reconciliation" {
		return tx.Commit()
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd-?,updated_at=? WHERE tenant_id=?`, row.ReservedMicrousd, stamp, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_usage_reservations SET state='released',resolution=?,updated_at=? WHERE tenant_id=? AND id=?`, reason, stamp, tenant, reservationID); err != nil {
		return err
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ManagedUsageReservations(ctx context.Context, tenant, state string, limit int) ([]domain.ManagedUsageReservation, error) {
	if tenant == "" {
		return nil, errors.New("tenant is required")
	}
	if state != "" && state != "reserved" && state != "settled" && state != "released" && state != "pending_reconciliation" {
		return nil, errors.New("reservation state is invalid")
	}
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,binding_id,supplier,model,reserved_microusd,COALESCE(actual_microusd,0),input_tokens,output_tokens,state,resolution,created_at,updated_at FROM managed_usage_reservations WHERE tenant_id=? AND (?='' OR state=?) ORDER BY created_at DESC,id DESC LIMIT ?`, tenant, state, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ManagedUsageReservation, 0)
	for rows.Next() {
		var item domain.ManagedUsageReservation
		var created, updated time.Time
		if err = rows.Scan(&item.ID, &item.TenantID, &item.BindingID, &item.Supplier, &item.Model, &item.ReservedMicrousd, &item.ActualMicrousd, &item.InputTokens, &item.OutputTokens, &item.State, &item.Resolution, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, item.UpdatedAt = created.UTC(), updated.UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}

func managedReservationForUpdate(ctx context.Context, tx *tx, tenant, id string) (domain.ManagedUsageReservation, domain.ManagedExternalBindingConfig, error) {
	var out domain.ManagedUsageReservation
	var configJSON, created, updated string
	err := tx.QueryRowContext(ctx, `SELECT r.id,r.tenant_id,r.binding_id,r.supplier,r.model,r.reserved_microusd,COALESCE(r.actual_microusd,0),r.input_tokens,r.output_tokens,r.state,r.resolution,r.created_at,r.updated_at,b.config_json::text FROM managed_usage_reservations r JOIN backend_bindings b ON b.id=r.binding_id AND b.tenant_id=r.tenant_id WHERE r.tenant_id=? AND r.id=? FOR UPDATE OF r`, tenant, id).Scan(&out.ID, &out.TenantID, &out.BindingID, &out.Supplier, &out.Model, &out.ReservedMicrousd, &out.ActualMicrousd, &out.InputTokens, &out.OutputTokens, &out.State, &out.Resolution, &created, &updated, &configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedUsageReservation{}, domain.ManagedExternalBindingConfig{}, ErrNotFound
	}
	if err != nil {
		return domain.ManagedUsageReservation{}, domain.ManagedExternalBindingConfig{}, err
	}
	config, managed, err := external.ParseManagedBindingConfig(configJSON)
	if err != nil || !managed || config.BillingMode != "customer_wallet" {
		return domain.ManagedUsageReservation{}, domain.ManagedExternalBindingConfig{}, fmt.Errorf("%w: reservation billing policy is invalid", ErrConflict)
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, config, nil
}

func (s *Store) ManagedWalletLedger(ctx context.Context, tenant string, limit int) ([]domain.ManagedWalletLedgerEntry, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,COALESCE(reservation_id,''),kind,currency,amount_microusd,description,created_at FROM managed_wallet_ledger WHERE tenant_id=? ORDER BY created_at DESC,id DESC LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.ManagedWalletLedgerEntry, 0)
	for rows.Next() {
		var item domain.ManagedWalletLedgerEntry
		var created time.Time
		if err = rows.Scan(&item.ID, &item.TenantID, &item.ReservationID, &item.Kind, &item.Currency, &item.AmountMicrousd, &item.Description, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = created.UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}
