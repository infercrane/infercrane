package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

var costCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
var costToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@+ -]{0,254}$`)

// RecordCostEvidence binds an imported snapshot to the active immutable
// revision in the same transaction. Replaying the same source window is safe;
// changing its value is an explicit conflict rather than a silent rewrite.
func (s *Store) RecordCostEvidence(ctx context.Context, tenant, deploymentName string, rows []domain.CostEvidence) ([]domain.CostEvidence, error) {
	if tenant == "" {
		tenant = "global"
	}
	if len(rows) == 0 || len(rows) > 128 {
		return nil, errors.New("cost evidence batch must contain 1..128 rows")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var deploymentID, resolvedName, revisionID string
	if err = tx.QueryRowContext(ctx, `SELECT id,name,active_revision_id FROM deployments WHERE tenant_id=? AND (id=? OR name=?) AND desired_state<>'deleted' FOR SHARE`, tenant, deploymentName, deploymentName).Scan(&deploymentID, &resolvedName, &revisionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	result := make([]domain.CostEvidence, 0, len(rows))
	for _, row := range rows {
		row.WindowStart = row.WindowStart.UTC().Truncate(time.Microsecond)
		row.WindowEnd = row.WindowEnd.UTC().Truncate(time.Microsecond)
		row.ObservedAt = row.ObservedAt.UTC().Truncate(time.Microsecond)
		row.ValidUntil = row.ValidUntil.UTC().Truncate(time.Microsecond)
		row.TenantID, row.DeploymentID, row.Deployment, row.RevisionID = tenant, deploymentID, resolvedName, revisionID
		if err = validateCostEvidence(row, now); err != nil {
			return nil, err
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", tenant, deploymentID, revisionID, row.Source, row.Scope, row.Resource, row.WindowStart.Format(time.RFC3339Nano), row.WindowEnd.Format(time.RFC3339Nano))))
		row.ID = hex.EncodeToString(digest[:16])
		row.CreatedAt = now
		inserted, insertErr := tx.ExecContext(ctx, `INSERT INTO cost_evidence(id,tenant_id,deployment_id,revision_id,source,scope,resource,currency,billing_unit,evidence_class,amount,window_start,window_end,observed_at,valid_until,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,deployment_id,revision_id,source,scope,resource,window_start,window_end) DO NOTHING`, row.ID, tenant, deploymentID, revisionID, row.Source, row.Scope, row.Resource, row.Currency, row.BillingUnit, row.EvidenceClass, row.Amount, row.WindowStart, row.WindowEnd, row.ObservedAt, row.ValidUntil, now)
		if insertErr != nil {
			return nil, insertErr
		}
		count, countErr := inserted.RowsAffected()
		if countErr != nil {
			return nil, countErr
		}
		if count == 0 {
			var existing domain.CostEvidence
			if err = tx.QueryRowContext(ctx, `SELECT id,currency,billing_unit,evidence_class,amount,observed_at,valid_until,created_at FROM cost_evidence WHERE tenant_id=? AND deployment_id=? AND revision_id=? AND source=? AND scope=? AND resource=? AND window_start=? AND window_end=?`, tenant, deploymentID, revisionID, row.Source, row.Scope, row.Resource, row.WindowStart, row.WindowEnd).Scan(&existing.ID, &existing.Currency, &existing.BillingUnit, &existing.EvidenceClass, &existing.Amount, &existing.ObservedAt, &existing.ValidUntil, &existing.CreatedAt); err != nil {
				return nil, err
			}
			if existing.ID != row.ID || existing.Currency != row.Currency || existing.BillingUnit != row.BillingUnit || existing.EvidenceClass != row.EvidenceClass || existing.Amount != row.Amount || !existing.ObservedAt.Equal(row.ObservedAt) || !existing.ValidUntil.Equal(row.ValidUntil) {
				return nil, fmt.Errorf("%w: cost evidence retry changed immutable evidence", domain.ErrConflict)
			}
			row.CreatedAt = existing.CreatedAt.UTC()
		}
		result = append(result, row)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func validateCostEvidence(row domain.CostEvidence, now time.Time) error {
	if !costToken.MatchString(row.Source) || !costToken.MatchString(row.Scope) || !costToken.MatchString(row.Resource) || !costToken.MatchString(row.BillingUnit) {
		return errors.New("cost source, scope, resource, and billing_unit must be bounded identifiers")
	}
	if !costCurrency.MatchString(row.Currency) {
		return errors.New("cost currency must be an explicit ISO-style three-letter uppercase code")
	}
	if row.BillingUnit != "hour" {
		return errors.New("v1 cost evidence requires the canonical billing_unit hour")
	}
	if row.EvidenceClass != "measured" && row.EvidenceClass != "provider_reported" {
		return errors.New("cost evidence_class must be measured or provider_reported")
	}
	if math.IsNaN(row.Amount) || math.IsInf(row.Amount, 0) || row.Amount < 0 {
		return errors.New("cost amount must be finite and non-negative")
	}
	if row.WindowStart.IsZero() || !row.WindowEnd.After(row.WindowStart) || row.WindowEnd.After(now.Add(time.Minute)) || row.WindowEnd.Sub(row.WindowStart) > 366*24*time.Hour {
		return errors.New("cost evidence requires a bounded completed window")
	}
	if row.ObservedAt.IsZero() || row.ObservedAt.After(now.Add(time.Minute)) || !row.ValidUntil.After(row.ObservedAt) || row.ValidUntil.Sub(row.ObservedAt) > 24*time.Hour {
		return errors.New("cost evidence requires a current observation and a validity window no longer than 24 hours")
	}
	return nil
}

func (s *Store) CostEvidenceForDeployment(ctx context.Context, tenant, deploymentName string, start, end time.Time, limit int) ([]domain.CostEvidence, error) {
	if limit < 1 || limit > 1000 {
		limit = 256
	}
	rows, err := s.QueryContext(ctx, `SELECT c.id,c.tenant_id,c.deployment_id,d.name,c.revision_id,c.source,c.scope,c.resource,c.currency,c.billing_unit,c.evidence_class,c.amount,c.window_start,c.window_end,c.observed_at,c.valid_until,c.created_at FROM cost_evidence c JOIN deployments d ON d.id=c.deployment_id AND d.tenant_id=c.tenant_id WHERE c.tenant_id=? AND (d.id=? OR d.name=?) AND c.window_end>=? AND c.window_start<=? ORDER BY c.observed_at DESC,c.id LIMIT ?`, tenant, deploymentName, deploymentName, start.UTC(), end.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.CostEvidence{}
	for rows.Next() {
		var row domain.CostEvidence
		if err = rows.Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.Deployment, &row.RevisionID, &row.Source, &row.Scope, &row.Resource, &row.Currency, &row.BillingUnit, &row.EvidenceClass, &row.Amount, &row.WindowStart, &row.WindowEnd, &row.ObservedAt, &row.ValidUntil, &row.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
