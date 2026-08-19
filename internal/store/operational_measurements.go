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

var measurementIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var measurementSourceIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@+-]{0,127}$`)
var measurementReplicaIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@+-]{0,254}$`)

// RecordOperationalMeasurements records one bounded collector snapshot. The
// immutable active revision is resolved in the same transaction, preventing a
// measurement from being silently attached to a later rollout.
func (s *Store) RecordOperationalMeasurements(ctx context.Context, tenant, deploymentName string, rows []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error) {
	if tenant == "" {
		tenant = "global"
	}
	if len(rows) == 0 || len(rows) > 32 {
		return nil, errors.New("operational measurement batch must contain 1..32 rows")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var deploymentID, resolvedDeploymentName, revisionID string
	if err = tx.QueryRowContext(ctx, `SELECT id,name,active_revision_id FROM deployments WHERE tenant_id=? AND (id=? OR name=?) AND desired_state<>'deleted' FOR SHARE`, tenant, deploymentName, deploymentName).Scan(&deploymentID, &resolvedDeploymentName, &revisionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	result := make([]domain.OperationalMeasurement, 0, len(rows))
	for _, row := range rows {
		row.ObservedAt = row.ObservedAt.UTC().Truncate(time.Microsecond)
		row.ValidUntil = row.ValidUntil.UTC().Truncate(time.Microsecond)
		row.TenantID, row.DeploymentID, row.Deployment, row.RevisionID = tenant, deploymentID, resolvedDeploymentName, revisionID
		if err = validateOperationalMeasurement(row, now); err != nil {
			return nil, err
		}
		if row.ID == "" {
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%.17g", tenant, deploymentID, revisionID, row.ReplicaID, row.Name, row.Source, row.ObservedAt.UTC().Format(time.RFC3339Nano), row.Value)))
			row.ID = hex.EncodeToString(digest[:16])
		}
		row.CreatedAt = now
		inserted, insertErr := tx.ExecContext(ctx, `INSERT INTO operational_measurements(id,tenant_id,deployment_id,revision_id,replica_id,name,value,unit,evidence_class,source,sample_count,observed_at,valid_until,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,deployment_id,revision_id,replica_id,name,source,observed_at) DO NOTHING`, row.ID, tenant, deploymentID, revisionID, row.ReplicaID, row.Name, row.Value, row.Unit, row.EvidenceClass, row.Source, row.SampleCount, row.ObservedAt.UTC(), row.ValidUntil.UTC(), now)
		err = insertErr
		if err != nil {
			return nil, err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count == 0 {
			var existing domain.OperationalMeasurement
			err = tx.QueryRowContext(ctx, `SELECT id,value,unit,evidence_class,sample_count,valid_until,created_at FROM operational_measurements WHERE tenant_id=? AND deployment_id=? AND revision_id=? AND replica_id=? AND name=? AND source=? AND observed_at=?`, tenant, deploymentID, revisionID, row.ReplicaID, row.Name, row.Source, row.ObservedAt.UTC()).Scan(&existing.ID, &existing.Value, &existing.Unit, &existing.EvidenceClass, &existing.SampleCount, &existing.ValidUntil, &existing.CreatedAt)
			if err != nil {
				return nil, err
			}
			if existing.ID != row.ID || existing.Value != row.Value || existing.Unit != row.Unit || existing.EvidenceClass != row.EvidenceClass || existing.SampleCount != row.SampleCount || !existing.ValidUntil.Equal(row.ValidUntil.UTC()) {
				return nil, fmt.Errorf("%w: operational measurement retry changed immutable evidence", domain.ErrConflict)
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

func validateOperationalMeasurement(row domain.OperationalMeasurement, now time.Time) error {
	if !measurementIdentifier.MatchString(row.Name) || !measurementIdentifier.MatchString(row.Unit) {
		return errors.New("measurement name and unit must be bounded lowercase identifiers")
	}
	if !measurementSourceIdentifier.MatchString(row.Source) {
		return errors.New("measurement source must be a bounded identifier")
	}
	if row.ReplicaID != "" && !measurementReplicaIdentifier.MatchString(row.ReplicaID) {
		return errors.New("measurement replica_id must be a bounded identifier")
	}
	if row.EvidenceClass != "measured" && row.EvidenceClass != "provider_reported" {
		return errors.New("measurement evidence_class must be measured or provider_reported")
	}
	canonicalUnits := map[string]string{"gpu_utilization": "percent", "gpu_memory": "bytes", "gpu_temperature": "celsius", "gpu_power": "watts", "gpu_xid_errors": "count"}
	if unit, known := canonicalUnits[row.Name]; known && row.Unit != unit {
		return fmt.Errorf("measurement %s requires canonical unit %s", row.Name, unit)
	}
	if math.IsNaN(row.Value) || math.IsInf(row.Value, 0) || row.SampleCount < 1 {
		return errors.New("measurement value must be finite and sample_count must be positive")
	}
	if row.ObservedAt.IsZero() || row.ObservedAt.After(now.Add(time.Minute)) || !row.ValidUntil.After(row.ObservedAt) || row.ValidUntil.Sub(row.ObservedAt) > 24*time.Hour {
		return errors.New("measurement requires a current observation and a validity window no longer than 24 hours")
	}
	return nil
}

// PurgeOperationalMeasurements enforces the same configurable high-volume
// evidence retention boundary as request records.
func (s *Store) PurgeOperationalMeasurements(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 10000
	}
	result, err := s.ExecContext(ctx, `DELETE FROM operational_measurements WHERE ctid IN (SELECT ctid FROM operational_measurements WHERE observed_at<? ORDER BY observed_at LIMIT ?)`, before.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
