package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// BenchmarkOperationalMeasurement returns one content-free aggregate only
// when qualified samples for the exact immutable revision overlap the complete
// benchmark window. The weighted mean preserves collector sample counts and
// avoids turning one replica's peak into an apparent fleet average.
func (s *Store) BenchmarkOperationalMeasurement(ctx context.Context, tenant, deploymentID, revisionID, name string, startedAt, endedAt time.Time) (domain.MeasurementEvidence, error) {
	if tenant == "" {
		tenant = "global"
	}
	if !measurementIdentifier.MatchString(name) || deploymentID == "" || revisionID == "" || startedAt.IsZero() || !endedAt.After(startedAt) {
		return domain.MeasurementEvidence{}, errors.New("benchmark measurement lookup requires bounded identity and time window")
	}
	var evidence domain.MeasurementEvidence
	var value float64
	var observedAt, freshUntil time.Time
	var sources string
	err := s.QueryRowContext(ctx, `SELECT SUM(value*sample_count)/NULLIF(SUM(sample_count),0),MIN(unit),MIN(evidence_class),STRING_AGG(DISTINCT source,',' ORDER BY source),SUM(sample_count),MAX(observed_at),MIN(valid_until) FROM operational_measurements WHERE tenant_id=? AND deployment_id=? AND revision_id=? AND name=? AND evidence_class='measured' AND observed_at<=? AND valid_until>=? HAVING COUNT(*)>0`, tenant, deploymentID, revisionID, name, endedAt.UTC(), startedAt.UTC()).Scan(&value, &evidence.Unit, &evidence.EvidenceClass, &sources, &evidence.SampleCount, &observedAt, &freshUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MeasurementEvidence{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.MeasurementEvidence{}, err
	}
	// Rows with different units cannot be meaningfully aggregated. Canonical
	// measurement validation normally prevents this; retain a defensive check.
	var unitCount int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(DISTINCT unit) FROM operational_measurements WHERE tenant_id=? AND deployment_id=? AND revision_id=? AND name=? AND evidence_class='measured' AND observed_at<=? AND valid_until>=?`, tenant, deploymentID, revisionID, name, endedAt.UTC(), startedAt.UTC()).Scan(&unitCount); err != nil {
		return domain.MeasurementEvidence{}, err
	}
	if unitCount != 1 {
		return domain.MeasurementEvidence{}, errors.New("benchmark measurement evidence has inconsistent units")
	}
	evidence.Name, evidence.Value, evidence.Availability, evidence.Source = name, &value, "available", strings.TrimSpace(sources)
	observedAt, freshUntil = observedAt.UTC(), freshUntil.UTC()
	evidence.ObservedAt, evidence.FreshUntil = &observedAt, &freshUntil
	return evidence, nil
}
