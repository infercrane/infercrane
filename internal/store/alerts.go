package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) CreateAlertPolicy(ctx context.Context, tenant, endpointName string, policy domain.AlertPolicy) (domain.AlertPolicy, error) {
	parsed, err := url.Parse(policy.WebhookURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return domain.AlertPolicy{}, errors.New("webhook URL must be absolute HTTPS without credentials or fragments")
	}
	if !validEndpointName(policy.Name) || policy.SecretReferenceID == "" {
		return domain.AlertPolicy{}, errors.New("valid alert policy name and secret reference are required")
	}
	if policy.MinimumSeverity != "info" && policy.MinimumSeverity != "warning" && policy.MinimumSeverity != "critical" {
		return domain.AlertPolicy{}, errors.New("minimum severity must be info, warning, or critical")
	}
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 5 {
		return domain.AlertPolicy{}, errors.New("max attempts must be between 1 and 5")
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return domain.AlertPolicy{}, err
	}
	var secretExists bool
	if err = s.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secret_references WHERE tenant_id=? AND id=?)`, tenant, policy.SecretReferenceID).Scan(&secretExists); err != nil || !secretExists {
		if err == nil {
			err = ErrNotFound
		}
		return domain.AlertPolicy{}, err
	}
	if policy.ID == "" {
		policy.ID, err = newID()
		if err != nil {
			return domain.AlertPolicy{}, err
		}
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO alert_policies(id,tenant_id,endpoint_id,name,webhook_url,secret_reference_id,minimum_severity,enabled,max_attempts,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, policy.ID, tenant, resolved.Endpoint.ID, policy.Name, policy.WebhookURL, policy.SecretReferenceID, policy.MinimumSeverity, policy.Enabled, policy.MaxAttempts, stamp, stamp)
	if isUniqueViolation(err) {
		return domain.AlertPolicy{}, ErrConflict
	}
	policy.TenantID, policy.EndpointID = tenant, resolved.Endpoint.ID
	policy.CreatedAt, policy.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return policy, err
}

func (s *Store) AlertPoliciesForEndpoint(ctx context.Context, tenant, endpointName string) ([]domain.AlertPolicy, error) {
	rows, err := s.QueryContext(ctx, `SELECT p.id,p.tenant_id,p.endpoint_id,p.name,p.webhook_url,p.secret_reference_id,p.minimum_severity,p.enabled,p.max_attempts,p.created_at,p.updated_at FROM alert_policies p JOIN endpoints e ON e.id=p.endpoint_id WHERE p.tenant_id=? AND e.name=? AND e.desired_state<>'deleted' ORDER BY p.name`, tenant, endpointName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AlertPolicy
	for rows.Next() {
		var item domain.AlertPolicy
		if err = rows.Scan(&item.ID, &item.TenantID, &item.EndpointID, &item.Name, &item.WebhookURL, &item.SecretReferenceID, &item.MinimumSeverity, &item.Enabled, &item.MaxAttempts, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) BeginAlertDelivery(ctx context.Context, policy domain.AlertPolicy, finding domain.DiagnosticFinding, bodyDigest string) (domain.AlertDelivery, bool, error) {
	id, err := newID()
	if err != nil {
		return domain.AlertDelivery{}, false, err
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO alert_deliveries(id,tenant_id,policy_id,finding_id,status,attempts,body_digest,created_at,updated_at) VALUES(?,?,?,?,'pending',0,?,?,?) ON CONFLICT(policy_id,finding_id) DO NOTHING`, id, policy.TenantID, policy.ID, finding.ID, bodyDigest, stamp, stamp)
	if err != nil {
		return domain.AlertDelivery{}, false, err
	}
	count, _ := result.RowsAffected()
	var item domain.AlertDelivery
	var delivered sql.NullTime
	err = s.QueryRowContext(ctx, `SELECT id,tenant_id,policy_id,finding_id,status,attempts,COALESCE(response_status,0),COALESCE(error_code,''),body_digest,created_at,updated_at,delivered_at FROM alert_deliveries WHERE policy_id=? AND finding_id=?`, policy.ID, finding.ID).Scan(&item.ID, &item.TenantID, &item.PolicyID, &item.FindingID, &item.Status, &item.Attempts, &item.ResponseStatus, &item.ErrorCode, &item.BodyDigest, &item.CreatedAt, &item.UpdatedAt, &delivered)
	if delivered.Valid {
		item.DeliveredAt = delivered.Time
	}
	return item, count == 1, err
}

func (s *Store) RecordAlertDeliveryAttempt(ctx context.Context, id string, delivered bool, responseStatus int, errorCode string, maxAttempts int) error {
	if maxAttempts < 1 || maxAttempts > 5 {
		return errors.New("invalid maximum delivery attempts")
	}
	deliveredAt := any(nil)
	if delivered {
		deliveredAt = time.Now().UTC()
	}
	_, err := s.ExecContext(ctx, `UPDATE alert_deliveries SET status=CASE WHEN ? THEN 'delivered' WHEN attempts+1>=? THEN 'failed' ELSE 'pending' END,attempts=attempts+1,response_status=?,error_code=?,updated_at=?,delivered_at=? WHERE id=? AND status='pending'`, delivered, maxAttempts, responseStatus, null(strings.TrimSpace(errorCode)), now(), deliveredAt, id)
	return err
}
