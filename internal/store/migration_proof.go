package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/apibaseline"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapicatalog"
)

// externalBinding resolves one external binding of an endpoint by name or ID.
func (s *Store) externalBinding(ctx context.Context, tenant, endpointName, binding string) (domain.ResolvedEndpoint, domain.BackendBinding, error) {
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return resolved, domain.BackendBinding{}, err
	}
	found, ok := endpointBinding(resolved, binding)
	if !ok || found.Kind != "external" {
		return resolved, domain.BackendBinding{}, domain.ErrNotFound
	}
	return resolved, found, nil
}

// ExternalBindingTarget returns an endpoint's external binding and the target
// it routes to.
func (s *Store) ExternalBindingTarget(ctx context.Context, tenant, endpointName, binding string) (domain.ResolvedEndpoint, domain.BackendBinding, domain.Target, error) {
	resolved, found, err := s.externalBinding(ctx, tenant, endpointName, binding)
	if err != nil {
		return resolved, found, domain.Target{}, err
	}
	target, err := s.TargetForTenantByID(ctx, tenant, found.TargetID)
	return resolved, found, target, err
}

// RecordBindingRateCard records what a closed API behind an external binding
// charges per token. Rows are immutable; the newest is the binding's rate
// card.
func (s *Store) RecordBindingRateCard(ctx context.Context, tenant, endpointName, binding string, card apibaseline.RateCard) (apibaseline.RateCard, error) {
	_, found, err := s.externalBinding(ctx, tenant, endpointName, binding)
	if err != nil {
		return card, err
	}
	stamp := time.Now().UTC()
	if err = card.Validate(stamp); err != nil {
		return card, err
	}
	if card.ID, err = newID(); err != nil {
		return card, err
	}
	card.BindingID, card.CreatedAt = found.ID, stamp.Truncate(time.Microsecond)
	_, err = s.ExecContext(ctx, `INSERT INTO binding_rate_cards(id,tenant_id,binding_id,authority,currency,input_microusd_per_million,cached_input_microusd_per_million,output_microusd_per_million,source,valid_until,recorded_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, card.ID, tenant, card.BindingID, card.Authority, card.Pricing.Currency, card.Pricing.InputMicrousdPerMillion, card.Pricing.CachedInputMicrousdPerMillion, card.Pricing.OutputMicrousdPerMillion, card.Source, card.ValidUntil.UTC(), card.RecordedBy, card.CreatedAt)
	return card, err
}

// BindingRateCard returns a binding's newest rate card, or nil when it has
// none. An expired card is returned too: pricing decides it prices nothing.
func (s *Store) BindingRateCard(ctx context.Context, tenant, bindingID string) (*apibaseline.RateCard, error) {
	var card apibaseline.RateCard
	var input, output int64
	var cached sql.NullInt64
	var validUntil, created time.Time
	err := s.QueryRowContext(ctx, `SELECT id,binding_id,authority,currency,input_microusd_per_million,cached_input_microusd_per_million,output_microusd_per_million,source,valid_until,recorded_by,created_at FROM binding_rate_cards WHERE tenant_id=? AND binding_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, tenant, bindingID).Scan(&card.ID, &card.BindingID, &card.Authority, &card.Pricing.Currency, &input, &cached, &output, &card.Source, &validUntil, &card.RecordedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	card.Pricing = modelapicatalog.Pricing{Currency: card.Pricing.Currency, InputMicrousdPerMillion: &input, OutputMicrousdPerMillion: &output, Provenance: card.Authority}
	if cached.Valid {
		card.Pricing.CachedInputMicrousdPerMillion = &cached.Int64
	}
	card.ValidUntil, card.CreatedAt = validUntil.UTC(), created.UTC()
	card.Pricing.ValidUntil = card.ValidUntil.Format(time.RFC3339)
	return &card, nil
}

// BindingMetrics reads one binding's served requests in a window, the way
// RevisionMetrics reads a revision's. ReadyReplicas is the binding's healthy
// routable targets: one for a healthy external target, the healthy targets
// of a deployment binding's deployment.
func (s *Store) BindingMetrics(ctx context.Context, tenant string, binding domain.BackendBinding, window time.Duration) (domain.RevisionMetrics, error) {
	if window <= 0 {
		return domain.RevisionMetrics{}, errors.New("metrics window must be positive")
	}
	var metrics domain.RevisionMetrics
	var ttft, latency, output sql.NullFloat64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(AVG(CASE WHEN error_type IS NOT NULL OR status_code IS NULL OR status_code>=400 THEN 1.0 ELSE 0.0 END),0),percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL),percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE latency_ms IS NOT NULL),`+requestOutputSpeedSQL+` FROM request_records WHERE tenant_id=? AND binding_id=? AND started_at>=NOW()-(?*INTERVAL '1 second')`, tenant, binding.ID, window.Seconds()).Scan(&metrics.Requests, &metrics.ErrorRate, &ttft, &latency, &output)
	if err != nil {
		return metrics, err
	}
	if ttft.Valid {
		metrics.P95TTFTMS = &ttft.Float64
	}
	if latency.Valid {
		metrics.P95LatencyMS = &latency.Float64
	}
	if output.Valid {
		metrics.OutputTokensPerSecond = &output.Float64
	}
	switch binding.Kind {
	case "external":
		err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets WHERE id=? AND health='healthy'`, binding.TargetID).Scan(&metrics.ReadyReplicas)
	default:
		err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_targets dt JOIN targets t ON t.id=dt.target_id WHERE dt.deployment_id=? AND t.health='healthy'`, binding.DeploymentID).Scan(&metrics.ReadyReplicas)
	}
	if err != nil {
		return domain.RevisionMetrics{}, err
	}
	metrics.EvidenceSource = "binding_request_telemetry"
	return metrics, nil
}

const bindingQualityColumns = `id,tenant_id,endpoint_id,binding_id,source,suite,suite_version,evaluator,evaluator_version,score,sample_count,pairing_digest,payload_json,payload_digest,signature,public_key,algorithm,key_id,evaluated_at,created_at`

func scanBindingQuality(row interface{ Scan(...any) error }) (domain.BindingQualityEvidence, error) {
	var item domain.BindingQualityEvidence
	var evaluated, created time.Time
	err := row.Scan(&item.ID, &item.TenantID, &item.EndpointID, &item.BindingID, &item.Source, &item.Suite, &item.SuiteVersion, &item.Evaluator, &item.EvaluatorVersion, &item.Score, &item.SampleCount, &item.PairingDigest, &item.PayloadJSON, &item.PayloadDigest, &item.Signature, &item.PublicKey, &item.Algorithm, &item.KeyID, &evaluated, &created)
	item.EvaluatedAt, item.CreatedAt = evaluated.UTC(), created.UTC()
	return item, err
}

// RecordBindingQualityEvidence stores verified evidence for a closed API's
// binding. Recording the same signed payload twice returns the first row.
func (s *Store) RecordBindingQualityEvidence(ctx context.Context, tenant, endpointName, binding string, evidence domain.BindingQualityEvidence) (domain.BindingQualityEvidence, bool, error) {
	resolved, found, err := s.externalBinding(ctx, tenant, endpointName, binding)
	if err != nil {
		return evidence, false, err
	}
	if evidence.Source != domain.QualityEvidenceSourceCustomer && evidence.Source != domain.QualityEvidenceSourceInferCrane {
		return evidence, false, errors.New("binding quality evidence needs a source")
	}
	if evidence.ID, err = newID(); err != nil {
		return evidence, false, err
	}
	evidence.TenantID, evidence.EndpointID, evidence.BindingID = tenant, resolved.Endpoint.ID, found.ID
	evidence.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	result, err := s.ExecContext(ctx, `INSERT INTO binding_quality_evidence(id,tenant_id,endpoint_id,binding_id,source,suite,suite_version,evaluator,evaluator_version,score,sample_count,pairing_digest,payload_json,payload_digest,signature,public_key,algorithm,key_id,evaluated_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,payload_digest) DO NOTHING`, evidence.ID, tenant, evidence.EndpointID, evidence.BindingID, evidence.Source, evidence.Suite, evidence.SuiteVersion, evidence.Evaluator, evidence.EvaluatorVersion, evidence.Score, evidence.SampleCount, evidence.PairingDigest, evidence.PayloadJSON, evidence.PayloadDigest, evidence.Signature, evidence.PublicKey, evidence.Algorithm, evidence.KeyID, evidence.EvaluatedAt.UTC(), evidence.CreatedAt)
	if err != nil {
		return evidence, false, err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return evidence, true, nil
	}
	existing, err := scanBindingQuality(s.QueryRowContext(ctx, `SELECT `+bindingQualityColumns+` FROM binding_quality_evidence WHERE tenant_id=? AND payload_digest=?`, tenant, evidence.PayloadDigest))
	if err != nil {
		return evidence, false, err
	}
	if existing.BindingID != evidence.BindingID || existing.Source != evidence.Source {
		return existing, false, domain.ErrConflict
	}
	return existing, false, nil
}

// BindingQualityEvidence lists a binding's evidence, newest first.
func (s *Store) BindingQualityEvidence(ctx context.Context, tenant, bindingID string, limit int) ([]domain.BindingQualityEvidence, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT `+bindingQualityColumns+` FROM binding_quality_evidence WHERE tenant_id=? AND binding_id=? ORDER BY evaluated_at DESC,id DESC LIMIT ?`, tenant, bindingID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.BindingQualityEvidence, 0)
	for rows.Next() {
		item, scanErr := scanBindingQuality(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

const migrationVerdictColumns = `id,tenant_id,campaign_id,candidate_run_id,endpoint_id,binding_id,deployment_id,revision_id,verdict,code,margin_percent,payload_json,payload_digest,signature,public_key,algorithm,key_id,evaluated_at,created_at`

func scanMigrationVerdict(row interface{ Scan(...any) error }) (domain.MigrationQualityVerdict, error) {
	var item domain.MigrationQualityVerdict
	var evaluated, created time.Time
	err := row.Scan(&item.ID, &item.TenantID, &item.CampaignID, &item.CandidateRunID, &item.EndpointID, &item.BindingID, &item.DeploymentID, &item.RevisionID, &item.Verdict, &item.Code, &item.MarginPercent, &item.PayloadJSON, &item.PayloadDigest, &item.Signature, &item.PublicKey, &item.Algorithm, &item.KeyID, &evaluated, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return item, domain.ErrNotFound
	}
	item.EvaluatedAt, item.CreatedAt = evaluated.UTC(), created.UTC()
	return item, err
}

// RecordMigrationVerdict stores a signed migration verdict. The candidate
// deployment is resolved by name within the tenant; the same signed payload
// recorded twice returns the first row.
func (s *Store) RecordMigrationVerdict(ctx context.Context, tenant, deploymentName string, verdict domain.MigrationQualityVerdict) (domain.MigrationQualityVerdict, error) {
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return verdict, err
	}
	if verdict.ID, err = newID(); err != nil {
		return verdict, err
	}
	verdict.TenantID, verdict.DeploymentID = tenant, resolved.Deployment.ID
	verdict.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	result, err := s.ExecContext(ctx, `INSERT INTO migration_quality_verdicts(id,tenant_id,campaign_id,candidate_run_id,endpoint_id,binding_id,deployment_id,revision_id,verdict,code,margin_percent,payload_json,payload_digest,signature,public_key,algorithm,key_id,evaluated_at,created_at) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM backend_bindings WHERE id=? AND tenant_id=? AND endpoint_id=? AND kind='external') AND EXISTS(SELECT 1 FROM optimization_campaigns WHERE id=? AND tenant_id=?) ON CONFLICT(tenant_id,payload_digest) DO NOTHING`, verdict.ID, tenant, verdict.CampaignID, verdict.CandidateRunID, verdict.EndpointID, verdict.BindingID, verdict.DeploymentID, verdict.RevisionID, verdict.Verdict, verdict.Code, verdict.MarginPercent, verdict.PayloadJSON, verdict.PayloadDigest, verdict.Signature, verdict.PublicKey, verdict.Algorithm, verdict.KeyID, verdict.EvaluatedAt.UTC(), verdict.CreatedAt, verdict.BindingID, tenant, verdict.EndpointID, verdict.CampaignID, tenant)
	if err != nil {
		return verdict, err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return verdict, nil
	}
	existing, err := scanMigrationVerdict(s.QueryRowContext(ctx, `SELECT `+migrationVerdictColumns+` FROM migration_quality_verdicts WHERE tenant_id=? AND payload_digest=?`, tenant, verdict.PayloadDigest))
	if errors.Is(err, domain.ErrNotFound) {
		// Nothing was inserted and no row has this digest: the binding or the
		// campaign is not this tenant's.
		return verdict, domain.ErrNotFound
	}
	return existing, err
}

// MigrationVerdict reads one verdict by ID.
func (s *Store) MigrationVerdict(ctx context.Context, tenant, id string) (domain.MigrationQualityVerdict, error) {
	return scanMigrationVerdict(s.QueryRowContext(ctx, `SELECT `+migrationVerdictColumns+` FROM migration_quality_verdicts WHERE tenant_id=? AND id=?`, tenant, id))
}

// MigrationVerdictsForCampaign lists a campaign's verdicts, newest first.
func (s *Store) MigrationVerdictsForCampaign(ctx context.Context, tenant, campaignID string) ([]domain.MigrationQualityVerdict, error) {
	rows, err := s.QueryContext(ctx, `SELECT `+migrationVerdictColumns+` FROM migration_quality_verdicts WHERE tenant_id=? AND campaign_id=? ORDER BY created_at DESC,id DESC LIMIT 100`, tenant, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.MigrationQualityVerdict, 0)
	for rows.Next() {
		item, scanErr := scanMigrationVerdict(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
