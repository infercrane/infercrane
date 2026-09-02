package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

const maxFundingIntentLease = 5 * time.Minute

// PrepareManagedFundingIntent persists the caller's immutable funding intent
// before any payment-provider call and grants one bounded creation lease. A
// retry with the same key adopts the completed intent, waits behind a live
// lease, or reclaims an expired lease. Stripe receives the intent ID as its
// own idempotency key, closing the remote-create/local-persist crash window.
func (s *Store) PrepareManagedFundingIntent(ctx context.Context, tenant string, requested domain.ManagedFundingIntent, leaseDuration time.Duration) (domain.ManagedFundingIntent, string, error) {
	if err := validateManagedFundingIntent(tenant, requested, leaseDuration); err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, managedPaymentLockID("funding:"+requested.Provider, tenant+"\x00"+requested.IdempotencyKey)); err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	stampTime := time.Now().UTC()
	stamp := stampTime.Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_funding_intents(id,tenant_id,provider,idempotency_key,amount_microusd,currency,status,created_at,updated_at) VALUES(?,?,?,?,?,?,'pending',?,?) ON CONFLICT(provider,tenant_id,idempotency_key) DO NOTHING`, requested.ID, tenant, requested.Provider, requested.IdempotencyKey, requested.AmountMicrousd, requested.Currency, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.ManagedFundingIntent{}, "", fmt.Errorf("%w: funding intent identity is already in use", ErrConflict)
		}
		return domain.ManagedFundingIntent{}, "", err
	}
	current, currentLease, currentLeaseExpires, err := managedFundingIntentTx(ctx, tx, tenant, requested.Provider, requested.IdempotencyKey)
	if err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	if current.ID != requested.ID || current.AmountMicrousd != requested.AmountMicrousd || current.Currency != requested.Currency {
		return domain.ManagedFundingIntent{}, "", fmt.Errorf("%w: idempotency key was already used for a different funding intent", ErrConflict)
	}
	if current.Status == "completed" {
		return current, "", tx.Commit()
	}
	if currentLease != "" && currentLeaseExpires.Valid && currentLeaseExpires.Time.After(stampTime) {
		return current, "", tx.Commit()
	}
	leaseToken, err := newID()
	if err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	leaseExpires := stampTime.Add(leaseDuration)
	if _, err = tx.ExecContext(ctx, `UPDATE managed_funding_intents SET lease_token=?,lease_expires_at=?,attempt=attempt+1,updated_at=? WHERE id=? AND tenant_id=? AND status='pending'`, leaseToken, leaseExpires, stamp, current.ID, tenant); err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	current.UpdatedAt = stampTime
	if err = tx.Commit(); err != nil {
		return domain.ManagedFundingIntent{}, "", err
	}
	return current, leaseToken, nil
}

// CompleteManagedFundingIntent records the provider session before returning
// its redirect URL to the caller. Repeating the same completion is safe;
// changing any session identity or immutable amount is a conflict.
func (s *Store) CompleteManagedFundingIntent(ctx context.Context, tenant, id, leaseToken string, session domain.ManagedCheckoutSession) (domain.ManagedFundingIntent, error) {
	if err := validateManagedCheckoutCompletion(id, leaseToken, session); err != nil {
		return domain.ManagedFundingIntent{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedFundingIntent{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, managedPaymentLockID("funding-complete", id)); err != nil {
		return domain.ManagedFundingIntent{}, err
	}
	current, storedLease, _, err := managedFundingIntentByIDTx(ctx, tx, tenant, id)
	if err != nil {
		return domain.ManagedFundingIntent{}, err
	}
	if current.Status == "completed" {
		if !fundingIntentMatchesSession(current, session) {
			return domain.ManagedFundingIntent{}, fmt.Errorf("%w: funding intent has a different Checkout session", ErrConflict)
		}
		return current, tx.Commit()
	}
	if storedLease == "" || storedLease != leaseToken {
		return domain.ManagedFundingIntent{}, fmt.Errorf("%w: funding intent creation lease is not held", ErrConflict)
	}
	if current.Provider != session.Provider || current.AmountMicrousd != session.AmountMicrousd || current.Currency != session.Currency {
		return domain.ManagedFundingIntent{}, fmt.Errorf("%w: Checkout session does not match the funding intent", ErrConflict)
	}
	stampTime := time.Now().UTC()
	stamp := stampTime.Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE managed_funding_intents SET status='completed',checkout_session_id=?,checkout_url=?,checkout_expires_at=?,lease_token=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND tenant_id=? AND status='pending' AND lease_token=?`, session.ProviderID, session.URL, session.ExpiresAt.UTC(), stamp, id, tenant, leaseToken)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ManagedFundingIntent{}, fmt.Errorf("%w: Checkout session is already assigned to another funding intent", ErrConflict)
		}
		return domain.ManagedFundingIntent{}, err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
		return domain.ManagedFundingIntent{}, rowsErr
	} else if rows != 1 {
		return domain.ManagedFundingIntent{}, fmt.Errorf("%w: funding intent creation lease changed", ErrConflict)
	}
	current.Status = "completed"
	current.CheckoutSessionID = session.ProviderID
	current.CheckoutURL = session.URL
	current.CheckoutExpiresAt = session.ExpiresAt.UTC()
	current.UpdatedAt = stampTime
	if err = tx.Commit(); err != nil {
		return domain.ManagedFundingIntent{}, err
	}
	return current, nil
}

// ReleaseManagedFundingIntentLease makes a failed provider attempt immediately
// retryable without disturbing a newer claimant.
func (s *Store) ReleaseManagedFundingIntentLease(ctx context.Context, tenant, id, leaseToken string) error {
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(leaseToken) == "" {
		return errors.New("tenant, funding intent ID, and lease token are required")
	}
	_, err := s.ExecContext(ctx, `UPDATE managed_funding_intents SET lease_token=NULL,lease_expires_at=NULL,updated_at=? WHERE id=? AND tenant_id=? AND status='pending' AND lease_token=?`, now(), id, tenant, leaseToken)
	return err
}

func managedFundingIntentTx(ctx context.Context, tx *tx, tenant, provider, idempotencyKey string) (domain.ManagedFundingIntent, string, sql.NullTime, error) {
	return scanManagedFundingIntent(tx.QueryRowContext(ctx, `SELECT id,tenant_id,provider,idempotency_key,amount_microusd,currency,status,COALESCE(checkout_session_id,''),COALESCE(checkout_url,''),checkout_expires_at,COALESCE(lease_token,''),lease_expires_at,created_at,updated_at FROM managed_funding_intents WHERE tenant_id=? AND provider=? AND idempotency_key=? FOR UPDATE`, tenant, provider, idempotencyKey))
}

func managedFundingIntentByIDTx(ctx context.Context, tx *tx, tenant, id string) (domain.ManagedFundingIntent, string, sql.NullTime, error) {
	return scanManagedFundingIntent(tx.QueryRowContext(ctx, `SELECT id,tenant_id,provider,idempotency_key,amount_microusd,currency,status,COALESCE(checkout_session_id,''),COALESCE(checkout_url,''),checkout_expires_at,COALESCE(lease_token,''),lease_expires_at,created_at,updated_at FROM managed_funding_intents WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, id))
}

func scanManagedFundingIntent(row rowScanner) (domain.ManagedFundingIntent, string, sql.NullTime, error) {
	var intent domain.ManagedFundingIntent
	var checkoutExpires, leaseExpires sql.NullTime
	var leaseToken, created, updated string
	err := row.Scan(&intent.ID, &intent.TenantID, &intent.Provider, &intent.IdempotencyKey, &intent.AmountMicrousd, &intent.Currency, &intent.Status, &intent.CheckoutSessionID, &intent.CheckoutURL, &checkoutExpires, &leaseToken, &leaseExpires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedFundingIntent{}, "", sql.NullTime{}, ErrNotFound
	}
	if err != nil {
		return domain.ManagedFundingIntent{}, "", sql.NullTime{}, err
	}
	if checkoutExpires.Valid {
		intent.CheckoutExpiresAt = checkoutExpires.Time.UTC()
	}
	intent.CreatedAt, intent.UpdatedAt = parseTime(created), parseTime(updated)
	return intent, leaseToken, leaseExpires, nil
}

func validateManagedFundingIntent(tenant string, intent domain.ManagedFundingIntent, leaseDuration time.Duration) error {
	if strings.TrimSpace(tenant) == "" || intent.TenantID != tenant || intent.Provider != "stripe" || strings.TrimSpace(intent.IdempotencyKey) == "" || len(intent.IdempotencyKey) > 128 || intent.Currency != "USD" || !managedbilling.ValidateCheckoutAmount(intent.AmountMicrousd) {
		return errors.New("valid tenant, Stripe provider, bounded idempotency key, fixed USD amount, and currency are required")
	}
	if intent.ID != managedbilling.FundingIntentID(tenant, intent.IdempotencyKey) {
		return errors.New("funding intent ID does not match its tenant and idempotency key")
	}
	if leaseDuration <= 0 || leaseDuration > maxFundingIntentLease {
		return errors.New("funding intent lease must be positive and at most five minutes")
	}
	return nil
}

func validateManagedCheckoutCompletion(id, leaseToken string, session domain.ManagedCheckoutSession) error {
	parsed, err := url.Parse(session.URL)
	if strings.TrimSpace(id) == "" || strings.TrimSpace(leaseToken) == "" || session.FundingIntentID != id || session.Provider != "stripe" || strings.TrimSpace(session.ProviderID) == "" || err != nil || parsed.Scheme != "https" || parsed.Host == "" || session.Currency != "USD" || !managedbilling.ValidateCheckoutAmount(session.AmountMicrousd) || !session.ExpiresAt.After(time.Now()) {
		return errors.New("complete Stripe Checkout session and matching funding intent lease are required")
	}
	return nil
}

func fundingIntentMatchesSession(intent domain.ManagedFundingIntent, session domain.ManagedCheckoutSession) bool {
	return intent.ID == session.FundingIntentID && intent.Provider == session.Provider && intent.CheckoutSessionID == session.ProviderID && intent.CheckoutURL == session.URL && intent.AmountMicrousd == session.AmountMicrousd && intent.Currency == session.Currency && intent.CheckoutExpiresAt.Equal(session.ExpiresAt.UTC())
}
