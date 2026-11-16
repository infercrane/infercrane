package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// MaxNotificationSubscriptions bounds the destinations one workspace may hold.
const MaxNotificationSubscriptions = 10

const notificationSubscriptionColumns = `s.id,s.tenant_id,s.name,s.destination,s.url_hint,s.url_ciphertext,s.url_nonce,s.secret_ciphertext,s.secret_nonce,s.key_reference,s.events_json::text,s.enabled,s.created_by,s.created_at,s.updated_at`

const notificationDeliveryColumns = `id,tenant_id,subscription_id,event_id,event_type,COALESCE(campaign_id,''),body,body_digest,status,attempts,max_attempts,COALESCE(response_status,0),COALESCE(error_code,''),occurred_at,next_attempt_at,created_at,updated_at,delivered_at`

// lastDeliveryJoin attaches the newest delivery of each subscription.
const lastDeliveryJoin = ` LEFT JOIN LATERAL (SELECT d.event_type,d.status,d.attempts,COALESCE(d.response_status,0) AS response_status,COALESCE(d.error_code,'') AS error_code,d.occurred_at,d.updated_at,d.delivered_at FROM notification_deliveries d WHERE d.tenant_id=s.tenant_id AND d.subscription_id=s.id ORDER BY d.created_at DESC LIMIT 1) last ON TRUE`

const lastDeliveryColumns = `,last.event_type,last.status,last.attempts,last.response_status,last.error_code,last.occurred_at,last.updated_at,last.delivered_at`

func (s *Store) CreateNotificationSubscription(ctx context.Context, tenant string, item domain.NotificationSubscription) (domain.NotificationSubscription, error) {
	if tenant == "" || item.Name == "" || item.Destination == "" || item.URLHint == "" || len(item.URLCiphertext) == 0 || len(item.URLNonce) == 0 || len(item.SecretCiphertext) == 0 || len(item.SecretNonce) == 0 || item.KeyReference == "" || len(item.Events) == 0 || item.CreatedBy == "" {
		return domain.NotificationSubscription{}, errors.New("a named destination, sealed URL and signing secret, events, and creator are required")
	}
	events, err := json.Marshal(item.Events)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	if item.ID == "" {
		if item.ID, err = newID(); err != nil {
			return domain.NotificationSubscription{}, err
		}
	}
	transaction, err := s.beginTx(ctx)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	defer transaction.Rollback()
	// The tenant row lock serializes concurrent creates so the bound holds.
	if _, err = transaction.ExecContext(ctx, `SELECT id FROM tenants WHERE id=? FOR UPDATE`, tenant); err != nil {
		return domain.NotificationSubscription{}, err
	}
	var count int
	if err = transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_subscriptions WHERE tenant_id=?`, tenant).Scan(&count); err != nil {
		return domain.NotificationSubscription{}, err
	}
	if count >= MaxNotificationSubscriptions {
		return domain.NotificationSubscription{}, fmt.Errorf("%w: a workspace can have at most %d notification destinations", ErrConflict, MaxNotificationSubscriptions)
	}
	stamp := now()
	if _, err = transaction.ExecContext(ctx, `INSERT INTO notification_subscriptions(id,tenant_id,name,destination,url_hint,url_ciphertext,url_nonce,secret_ciphertext,secret_nonce,key_reference,events_json,enabled,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?::jsonb,?,?,?,?)`, item.ID, tenant, item.Name, item.Destination, item.URLHint, item.URLCiphertext, item.URLNonce, item.SecretCiphertext, item.SecretNonce, item.KeyReference, string(events), item.Enabled, item.CreatedBy, stamp, stamp); err != nil {
		return domain.NotificationSubscription{}, err
	}
	if err = transaction.Commit(); err != nil {
		return domain.NotificationSubscription{}, err
	}
	return s.NotificationSubscriptionForTenant(ctx, tenant, item.ID)
}

// UpdateNotificationSubscription replaces the mutable fields. The caller seals
// a changed URL; the signing secret never changes after creation.
func (s *Store) UpdateNotificationSubscription(ctx context.Context, tenant string, item domain.NotificationSubscription) (domain.NotificationSubscription, error) {
	if tenant == "" || item.ID == "" || item.Name == "" || item.URLHint == "" || len(item.URLCiphertext) == 0 || len(item.URLNonce) == 0 || len(item.Events) == 0 {
		return domain.NotificationSubscription{}, errors.New("subscription identity, name, sealed URL, and events are required")
	}
	events, err := json.Marshal(item.Events)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	result, err := s.ExecContext(ctx, `UPDATE notification_subscriptions SET name=?,url_hint=?,url_ciphertext=?,url_nonce=?,events_json=?::jsonb,enabled=?,updated_at=? WHERE tenant_id=? AND id=?`, item.Name, item.URLHint, item.URLCiphertext, item.URLNonce, string(events), item.Enabled, now(), tenant, item.ID)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return domain.NotificationSubscription{}, ErrNotFound
	}
	return s.NotificationSubscriptionForTenant(ctx, tenant, item.ID)
}

func (s *Store) DeleteNotificationSubscription(ctx context.Context, tenant, id string) error {
	result, err := s.ExecContext(ctx, `DELETE FROM notification_subscriptions WHERE tenant_id=? AND id=?`, tenant, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) NotificationSubscriptionForTenant(ctx context.Context, tenant, id string) (domain.NotificationSubscription, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+notificationSubscriptionColumns+lastDeliveryColumns+` FROM notification_subscriptions s`+lastDeliveryJoin+` WHERE s.tenant_id=? AND s.id=?`, tenant, id)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	items, err := scanNotificationSubscriptions(rows)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	if len(items) == 0 {
		return domain.NotificationSubscription{}, ErrNotFound
	}
	return items[0], nil
}

func (s *Store) NotificationSubscriptionsForTenant(ctx context.Context, tenant string) ([]domain.NotificationSubscription, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+notificationSubscriptionColumns+lastDeliveryColumns+` FROM notification_subscriptions s`+lastDeliveryJoin+` WHERE s.tenant_id=? ORDER BY s.created_at,s.id`, tenant)
	if err != nil {
		return nil, err
	}
	return scanNotificationSubscriptions(rows)
}

// NotificationTenants lists the workspaces with at least one enabled
// destination; the notifier visits only those.
func (s *Store) NotificationTenants(ctx context.Context) ([]string, error) {
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT tenant_id FROM notification_subscriptions WHERE enabled ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tenants := make([]string, 0)
	for rows.Next() {
		var tenant string
		if err = rows.Scan(&tenant); err != nil {
			return nil, err
		}
		tenants = append(tenants, tenant)
	}
	return tenants, rows.Err()
}

func scanNotificationSubscriptions(rows *sql.Rows) ([]domain.NotificationSubscription, error) {
	defer rows.Close()
	items := make([]domain.NotificationSubscription, 0)
	for rows.Next() {
		var item domain.NotificationSubscription
		var events string
		var lastType, lastStatus, lastCode sql.NullString
		var lastAttempts, lastResponse sql.NullInt64
		var lastOccurred, lastUpdated, lastDelivered sql.NullTime
		if err := rows.Scan(&item.ID, &item.TenantID, &item.Name, &item.Destination, &item.URLHint, &item.URLCiphertext, &item.URLNonce, &item.SecretCiphertext, &item.SecretNonce, &item.KeyReference, &events, &item.Enabled, &item.CreatedBy, &item.CreatedAt, &item.UpdatedAt, &lastType, &lastStatus, &lastAttempts, &lastResponse, &lastCode, &lastOccurred, &lastUpdated, &lastDelivered); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(events), &item.Events); err != nil {
			return nil, fmt.Errorf("decode notification events: %w", err)
		}
		if lastType.Valid {
			last := &domain.NotificationDelivery{EventType: lastType.String, Status: lastStatus.String, Attempts: int(lastAttempts.Int64), ResponseStatus: int(lastResponse.Int64), ErrorCode: lastCode.String, OccurredAt: lastOccurred.Time, UpdatedAt: lastUpdated.Time}
			if lastDelivered.Valid {
				delivered := lastDelivered.Time
				last.DeliveredAt = &delivered
			}
			item.LastDelivery = last
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// OptimizationCampaignsUpdatedSince returns the newest campaigns that changed
// at or after since, with their candidates.
func (s *Store) OptimizationCampaignsUpdatedSince(ctx context.Context, tenant string, since time.Time, limit int) ([]domain.OptimizationCampaign, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT id FROM optimization_campaigns WHERE tenant_id=? AND updated_at>=? ORDER BY updated_at DESC LIMIT ?`, tenant, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.OptimizationCampaign, 0, len(ids))
	for _, id := range ids {
		row, lookupErr := s.OptimizationCampaign(ctx, tenant, id)
		if errors.Is(lookupErr, ErrNotFound) {
			continue
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) OptimizationCampaignTransitionsSince(ctx context.Context, tenant string, since time.Time) ([]domain.OptimizationCampaignTransition, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,campaign_id,from_state,to_state,created_at FROM optimization_campaign_transitions WHERE tenant_id=? AND created_at>=? ORDER BY created_at,id LIMIT 1000`, tenant, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.OptimizationCampaignTransition, 0)
	for rows.Next() {
		var item domain.OptimizationCampaignTransition
		if err = rows.Scan(&item.ID, &item.TenantID, &item.CampaignID, &item.From, &item.To, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// NotificationEventKeysSince returns subscription_id+"/"+event_id for every
// delivery of an event that occurred at or after since, so the notifier can
// skip building bodies it already queued.
func (s *Store) NotificationEventKeysSince(ctx context.Context, tenant string, since time.Time) (map[string]bool, error) {
	rows, err := s.QueryContext(ctx, `SELECT subscription_id,event_id FROM notification_deliveries WHERE tenant_id=? AND occurred_at>=?`, tenant, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var subscription, event string
		if err = rows.Scan(&subscription, &event); err != nil {
			return nil, err
		}
		keys[subscription+"/"+event] = true
	}
	return keys, rows.Err()
}

// InsertNotificationDelivery queues one event for one subscription. A repeat
// of the same event is a no-op and reports false. A positive claim holds the
// new row for the caller, which sends it at once instead of the background
// sender.
func (s *Store) InsertNotificationDelivery(ctx context.Context, item domain.NotificationDelivery, claim time.Duration) (domain.NotificationDelivery, bool, error) {
	if item.TenantID == "" || item.SubscriptionID == "" || item.EventID == "" || item.EventType == "" || item.Body == "" || item.BodyDigest == "" || item.MaxAttempts < 1 || item.OccurredAt.IsZero() {
		return domain.NotificationDelivery{}, false, errors.New("notification delivery requires tenant, subscription, event, body, attempts, and occurrence time")
	}
	id, err := newID()
	if err != nil {
		return domain.NotificationDelivery{}, false, err
	}
	stamp := time.Now().UTC()
	var claimedUntil any
	if claim > 0 {
		claimedUntil = stamp.Add(claim)
	}
	result, err := s.ExecContext(ctx, `INSERT INTO notification_deliveries(id,tenant_id,subscription_id,event_id,event_type,campaign_id,body,body_digest,status,attempts,max_attempts,next_attempt_at,claimed_until,occurred_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'pending',0,?,?,?,?,?,?) ON CONFLICT(subscription_id,event_id) DO NOTHING`, id, item.TenantID, item.SubscriptionID, item.EventID, item.EventType, null(item.CampaignID), item.Body, item.BodyDigest, item.MaxAttempts, stamp, claimedUntil, item.OccurredAt.UTC(), stamp, stamp)
	if isForeignKeyViolation(err) {
		return domain.NotificationDelivery{}, false, ErrNotFound
	}
	if err != nil {
		return domain.NotificationDelivery{}, false, err
	}
	created, _ := result.RowsAffected()
	row, err := scanNotificationDelivery(s.QueryRowContext(ctx, `SELECT `+notificationDeliveryColumns+` FROM notification_deliveries WHERE subscription_id=? AND event_id=?`, item.SubscriptionID, item.EventID))
	return row, created == 1, err
}

// ClaimNotificationDeliveries leases due pending deliveries. SKIP LOCKED and
// the lease keep two control-plane replicas from sending the same attempt.
func (s *Store) ClaimNotificationDeliveries(ctx context.Context, lease time.Duration, limit int) ([]domain.NotificationDelivery, error) {
	if lease <= 0 || limit < 1 || limit > 100 {
		return nil, errors.New("claim requires a positive lease and a limit of 1..100")
	}
	stamp := time.Now().UTC()
	rows, err := s.QueryContext(ctx, `UPDATE notification_deliveries SET claimed_until=?,updated_at=? WHERE id IN (SELECT id FROM notification_deliveries WHERE status='pending' AND next_attempt_at<=? AND (claimed_until IS NULL OR claimed_until<?) ORDER BY next_attempt_at LIMIT ? FOR UPDATE SKIP LOCKED) RETURNING `+notificationDeliveryColumns, stamp.Add(lease), stamp, stamp, stamp, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.NotificationDelivery, 0)
	for rows.Next() {
		item, scanErr := scanNotificationDelivery(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// RecordNotificationDeliveryAttempt records one attempt. A nil retryAt on a
// failed attempt makes the delivery terminally failed.
func (s *Store) RecordNotificationDeliveryAttempt(ctx context.Context, id string, delivered bool, responseStatus int, errorCode string, retryAt *time.Time) (domain.NotificationDelivery, error) {
	stamp := time.Now().UTC()
	status, next := "failed", stamp
	var deliveredAt any
	switch {
	case delivered:
		status, deliveredAt = "delivered", stamp
	case retryAt != nil:
		status, next = "pending", retryAt.UTC()
	}
	var response any
	if responseStatus > 0 {
		response = responseStatus
	}
	row, err := scanNotificationDelivery(s.QueryRowContext(ctx, `UPDATE notification_deliveries SET status=CASE WHEN ?='pending' AND attempts+1>=max_attempts THEN 'failed' ELSE ? END,attempts=attempts+1,response_status=?,error_code=?,next_attempt_at=?,claimed_until=NULL,updated_at=?,delivered_at=? WHERE id=? AND status='pending' RETURNING `+notificationDeliveryColumns, status, status, response, null(errorCode), next, stamp, deliveredAt, id))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return domain.NotificationDelivery{}, ErrConflict
	}
	return row, err
}

func scanNotificationDelivery(row rowScanner) (domain.NotificationDelivery, error) {
	var item domain.NotificationDelivery
	var delivered sql.NullTime
	err := row.Scan(&item.ID, &item.TenantID, &item.SubscriptionID, &item.EventID, &item.EventType, &item.CampaignID, &item.Body, &item.BodyDigest, &item.Status, &item.Attempts, &item.MaxAttempts, &item.ResponseStatus, &item.ErrorCode, &item.OccurredAt, &item.NextAttemptAt, &item.CreatedAt, &item.UpdatedAt, &delivered)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotificationDelivery{}, ErrNotFound
	}
	if delivered.Valid {
		at := delivered.Time
		item.DeliveredAt = &at
	}
	return item, err
}
