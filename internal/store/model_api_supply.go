package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiqualification"
	"github.com/infercrane/infercrane/internal/modelapisupply"
)

const modelAPISupplierOfferSelect = `SELECT id,version,operator_tenant_id,managed_product_id,supplier,adapter,supplier_model_id,protocol,tuple_key,region,credential_reference,state,capabilities_json::text,access_state,availability_state,health_state,observed_at,cost_currency,cost_input_microusd_per_mtoken,cost_output_microusd_per_mtoken,cost_cached_input_microusd_per_mtoken,cost_basis_provenance,cost_valid_from,cost_valid_until,commercial_state,commercial_terms_ref,commercial_valid_until FROM model_api_supplier_offers`
const modelAPISupplierOfferInsert = `INSERT INTO model_api_supplier_offers(id,version,operator_tenant_id,managed_product_id,supplier,adapter,supplier_model_id,protocol,tuple_key,region,credential_reference,state,capabilities_json,access_state,availability_state,health_state,observed_at,cost_currency,cost_input_microusd_per_mtoken,cost_output_microusd_per_mtoken,cost_cached_input_microusd_per_mtoken,cost_basis_provenance,cost_valid_from,cost_valid_until,commercial_state,commercial_terms_ref,commercial_valid_until,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id,version) DO NOTHING`
const modelAPISupplyQualificationSelect = `SELECT id,state,tuple_key,protocol,region,capabilities_json::text,scope,evidence_ref,evidence_digest,reason,observed_at,valid_until,sample_count,ttft_p95_ms,output_tokens_p5 FROM model_api_supply_qualifications`

// SupplyCandidateReference binds a plan candidate to exact immutable offer,
// qualification, and customer-rate revisions. No supplier credential value is
// accepted by this contract.
type SupplyCandidateReference struct {
	CandidateID       string `json:"candidate_id"`
	OfferID           string `json:"offer_id"`
	OfferVersion      int64  `json:"offer_version"`
	QualificationID   string `json:"qualification_id"`
	RetailRateVersion int    `json:"retail_rate_version"`
	TrafficWeightBPS  int    `json:"traffic_weight_bps,omitempty"`
}

// SupplyPlanDraft is the operator input to deterministic plan compilation.
// The store reloads every referenced immutable record instead of trusting a
// caller-supplied materialization.
type SupplyPlanDraft struct {
	ID               string                     `json:"id"`
	OperatorTenantID string                     `json:"operator_tenant_id"`
	ProductID        string                     `json:"product_id"`
	Request          modelapisupply.Request     `json:"request"`
	Candidates       []SupplyCandidateReference `json:"candidates"`
}

// PublishModelAPISupplierOffer inserts one immutable supplier offer revision.
// Exact replays are idempotent; a conflicting identity fails closed.
func (s *Store) PublishModelAPISupplierOffer(ctx context.Context, operatorTenant string, offer modelapisupply.Offer) (modelapisupply.Offer, error) {
	if operatorTenant == "" || offer.OperatorTenantID != operatorTenant {
		return modelapisupply.Offer{}, errors.New("operator tenant must own the supplier offer")
	}
	if offer.Qualification != nil {
		return modelapisupply.Offer{}, errors.New("publish qualification evidence separately from the immutable supplier offer")
	}
	offer.Capabilities = normalizedModelAPIStrings(offer.Capabilities)
	if err := offer.Validate(); err != nil {
		return modelapisupply.Offer{}, err
	}
	if err := requirePostgresSafeOfferTimes(offer); err != nil {
		return modelapisupply.Offer{}, err
	}
	product, err := s.ManagedModelAPIProduct(ctx, offer.ProductID)
	if err != nil {
		return modelapisupply.Offer{}, err
	}
	if product.Protocol != offer.Protocol {
		return modelapisupply.Offer{}, errors.New("supplier offer protocol does not match the managed product")
	}
	if _, err = s.SecretReferenceForTenant(ctx, operatorTenant, offer.CredentialReference); err != nil {
		return modelapisupply.Offer{}, fmt.Errorf("supplier credential reference: %w", err)
	}
	capabilities, err := json.Marshal(offer.Capabilities)
	if err != nil {
		return modelapisupply.Offer{}, err
	}
	result, err := s.ExecContext(ctx, modelAPISupplierOfferInsert,
		offer.ID, offer.Version, operatorTenant, offer.ProductID, offer.Supplier, offer.Adapter, offer.SupplierModelID, offer.Protocol,
		offer.TupleKey, offer.Region, offer.CredentialReference, offer.State, capabilities, offer.Access, offer.Availability, offer.Health,
		nullableModelAPITime(offer.ObservedAt), offer.CostRate.Currency, nullableModelAPIInt64(offer.CostRate.InputMicrousdPerMTok), nullableModelAPIInt64(offer.CostRate.OutputMicrousdPerMTok), nullableModelAPIInt64(offer.CostRate.CachedInputMicrousdPerMTok),
		nullableModelAPIString(offer.CostRate.Provenance), nullableModelAPITime(offer.CostRate.ValidFrom), nullableModelAPITime(offer.CostRate.ValidUntil),
		offer.Commercial.State, nullableModelAPIString(offer.Commercial.TermsRef), nullableModelAPITime(offer.Commercial.ValidUntil), time.Now().UTC())
	if err != nil {
		return modelapisupply.Offer{}, err
	}
	stored, err := s.ModelAPISupplierOffer(ctx, operatorTenant, offer.ID, offer.Version)
	if err != nil {
		return modelapisupply.Offer{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 && !reflect.DeepEqual(stored, offer) {
		return modelapisupply.Offer{}, fmt.Errorf("%w: supplier offer identity already has a different immutable contract", ErrConflict)
	}
	return stored, nil
}

func (s *Store) ModelAPISupplierOffer(ctx context.Context, operatorTenant, offerID string, version int64) (modelapisupply.Offer, error) {
	if operatorTenant == "" || offerID == "" || version <= 0 {
		return modelapisupply.Offer{}, errors.New("operator tenant, offer id, and positive version are required")
	}
	return scanModelAPISupplierOffer(s.QueryRowContext(ctx, modelAPISupplierOfferSelect+` WHERE operator_tenant_id=? AND id=? AND version=?`, operatorTenant, offerID, version))
}

// PublishModelAPISupplyQualification inserts evidence for one exact immutable
// offer revision. Evidence is append-only and cannot silently re-qualify a
// different tuple.
func (s *Store) PublishModelAPISupplyQualification(ctx context.Context, operatorTenant, offerID string, offerVersion int64, evidence modelapisupply.QualificationEvidence) (modelapisupply.QualificationEvidence, error) {
	if operatorTenant == "" || offerID == "" || offerVersion <= 0 {
		return modelapisupply.QualificationEvidence{}, errors.New("operator tenant and offer revision are required")
	}
	evidence.Capabilities = normalizedModelAPIStrings(evidence.Capabilities)
	if err := evidence.Validate(); err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	if err := requirePostgresSafeQualificationTimes(evidence); err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	offer, err := s.ModelAPISupplierOffer(ctx, operatorTenant, offerID, offerVersion)
	if err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	if evidence.TupleKey != offer.TupleKey || evidence.Protocol != offer.Protocol || evidence.Region != offer.Region {
		return modelapisupply.QualificationEvidence{}, errors.New("qualification tuple, protocol, and region must exactly match the supplier offer")
	}
	capabilities, err := json.Marshal(evidence.Capabilities)
	if err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	result, err := s.ExecContext(ctx, `INSERT INTO model_api_supply_qualifications(id,offer_id,offer_version,state,tuple_key,protocol,region,capabilities_json,scope,evidence_ref,evidence_digest,reason,observed_at,valid_until,sample_count,ttft_p95_ms,output_tokens_p5,created_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		evidence.ID, offerID, offerVersion, evidence.State, evidence.TupleKey, evidence.Protocol, evidence.Region, capabilities,
		evidence.Scope, nullableModelAPIString(evidence.EvidenceRef), nullableModelAPIString(evidence.EvidenceDigest), nullableModelAPIString(evidence.Reason),
		nullableModelAPITime(evidence.ObservedAt), nullableModelAPITime(evidence.ValidUntil), evidence.SampleCount, evidence.TTFTP95MS, evidence.OutputTokensP5, time.Now().UTC())
	if err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	stored, err := s.ModelAPISupplyQualification(ctx, operatorTenant, offerID, offerVersion, evidence.ID)
	if err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 && !reflect.DeepEqual(stored, evidence) {
		return modelapisupply.QualificationEvidence{}, fmt.Errorf("%w: qualification identity already has different evidence", ErrConflict)
	}
	return stored, nil
}

// PublishMeasuredModelAPISupplyQualification promotes only a deterministic
// measurement of the exact immutable offer tuple into callable supply
// evidence. The raw artifact stays outside PostgreSQL and is referenced by
// evidenceRef; its digest is committed by the measured evidence itself.
func (s *Store) PublishMeasuredModelAPISupplyQualification(ctx context.Context, operatorTenant, offerID string, offerVersion int64, qualificationID, scope, evidenceRef string, measured modelapiqualification.Evidence) (modelapisupply.QualificationEvidence, error) {
	if qualificationID == "" || scope == "" || evidenceRef == "" || measured.Digest == "" {
		return modelapisupply.QualificationEvidence{}, errors.New("qualification identity, scope, evidence reference, and measured digest are required")
	}
	offer, err := s.ModelAPISupplierOffer(ctx, operatorTenant, offerID, offerVersion)
	if err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	capabilities := normalizedModelAPIStrings(measured.Target.Capabilities)
	if measured.Target.TupleKey != offer.TupleKey || measured.Target.Supplier != offer.Supplier || measured.Target.Adapter != offer.Adapter ||
		measured.Target.SupplierModelID != offer.SupplierModelID || measured.Target.Protocol != offer.Protocol || measured.Target.Region != offer.Region ||
		!reflect.DeepEqual(capabilities, normalizedModelAPIStrings(offer.Capabilities)) {
		return modelapisupply.QualificationEvidence{}, errors.New("measured qualification does not exactly match the immutable supplier offer")
	}
	ttft, output := measured.TTFTP95MS, measured.OutputTokensPerSecondP5
	return s.PublishModelAPISupplyQualification(ctx, operatorTenant, offerID, offerVersion, modelapisupply.QualificationEvidence{
		ID: qualificationID, State: modelapisupply.QualificationQualified,
		TupleKey: measured.Target.TupleKey, Protocol: measured.Target.Protocol, Region: measured.Target.Region,
		Capabilities: capabilities, Scope: scope, EvidenceRef: evidenceRef, EvidenceDigest: measured.Digest,
		ObservedAt: measured.ObservedAt, ValidUntil: measured.ValidUntil, SampleCount: measured.SampleCount,
		TTFTP95MS: &ttft, OutputTokensP5: &output,
	})
}

func (s *Store) ModelAPISupplyQualification(ctx context.Context, operatorTenant, offerID string, offerVersion int64, qualificationID string) (modelapisupply.QualificationEvidence, error) {
	if operatorTenant == "" || offerID == "" || offerVersion <= 0 || qualificationID == "" {
		return modelapisupply.QualificationEvidence{}, errors.New("operator tenant, offer revision, and qualification id are required")
	}
	row := s.QueryRowContext(ctx, modelAPISupplyQualificationSelect+` WHERE id=? AND offer_id=? AND offer_version=? AND EXISTS(SELECT 1 FROM model_api_supplier_offers o WHERE o.id=? AND o.version=? AND o.operator_tenant_id=?)`, qualificationID, offerID, offerVersion, offerID, offerVersion, operatorTenant)
	return scanModelAPISupplyQualification(row)
}

// CompileAndPublishModelAPISupplyPlan reloads exact immutable inputs, compiles
// the deterministic plan, then stores the plan and every accepted/rejected
// candidate atomically. Only a ready plan is publishable to customer traffic.
func (s *Store) CompileAndPublishModelAPISupplyPlan(ctx context.Context, draft SupplyPlanDraft) (modelapisupply.Plan, error) {
	if draft.OperatorTenantID == "" || draft.ProductID == "" || len(draft.Candidates) == 0 {
		return modelapisupply.Plan{}, errors.New("operator tenant, product, and supply candidates are required")
	}
	if draft.Request.ModelID != draft.ProductID {
		return modelapisupply.Plan{}, errors.New("supply request model must match the managed product")
	}
	if draft.ID == "" {
		id, err := newID()
		if err != nil {
			return modelapisupply.Plan{}, err
		}
		draft.ID = id
	}
	if !draft.Request.At.Equal(draft.Request.At.Truncate(time.Microsecond)) {
		return modelapisupply.Plan{}, errors.New("supply plan timestamp must use PostgreSQL-safe microsecond precision")
	}
	materialized := make([]modelapisupply.Candidate, 0, len(draft.Candidates))
	for _, reference := range draft.Candidates {
		if reference.CandidateID == "" || reference.OfferID == "" || reference.OfferVersion <= 0 || reference.QualificationID == "" || reference.RetailRateVersion <= 0 {
			return modelapisupply.Plan{}, errors.New("each supply candidate requires exact candidate, offer, qualification, and rate identities")
		}
		if reference.TrafficWeightBPS < 0 || reference.TrafficWeightBPS > 10_000 {
			return modelapisupply.Plan{}, errors.New("candidate traffic weight must be between 0 and 10000 basis points")
		}
		offer, err := s.ModelAPISupplierOffer(ctx, draft.OperatorTenantID, reference.OfferID, reference.OfferVersion)
		if err != nil {
			return modelapisupply.Plan{}, err
		}
		if offer.ProductID != draft.ProductID {
			return modelapisupply.Plan{}, errors.New("supplier offer product does not match the supply plan")
		}
		qualification, err := s.ModelAPISupplyQualification(ctx, draft.OperatorTenantID, reference.OfferID, reference.OfferVersion, reference.QualificationID)
		if err != nil {
			return modelapisupply.Plan{}, err
		}
		offer.Qualification = &qualification
		rate, err := s.ModelAPIRetailRate(ctx, draft.ProductID, reference.RetailRateVersion)
		if err != nil {
			return modelapisupply.Plan{}, err
		}
		candidate, err := modelapisupply.MaterializeCandidate(modelapisupply.CandidateMaterialization{CandidateID: reference.CandidateID, Offer: offer, RetailRate: rate})
		if err != nil {
			return modelapisupply.Plan{}, err
		}
		materialized = append(materialized, candidate)
	}
	plan, err := modelapisupply.Compile(draft.Request, materialized)
	if err != nil {
		return modelapisupply.Plan{}, err
	}
	if err = validateModelAPIRolloutWeights(plan, draft.Candidates); err != nil {
		return modelapisupply.Plan{}, err
	}
	requestJSON, err := json.Marshal(draft.Request)
	if err != nil {
		return modelapisupply.Plan{}, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return modelapisupply.Plan{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return modelapisupply.Plan{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO model_api_supply_plans(id,operator_tenant_id,managed_product_id,protocol,schema_version,digest,status,ranking_basis,request_json,plan_json,generated_at,valid_until,created_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?) ON CONFLICT(id) DO NOTHING`,
		draft.ID, draft.OperatorTenantID, draft.ProductID, plan.Protocol, plan.SchemaVersion, plan.Digest, plan.Status, plan.RankingBasis,
		requestJSON, planJSON, plan.GeneratedAt.UTC(), nullableModelAPITime(plan.ValidUntil), time.Now().UTC())
	if err != nil {
		return modelapisupply.Plan{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var existingJSON string
		if err = tx.QueryRowContext(ctx, `SELECT plan_json::text FROM model_api_supply_plans WHERE id=? AND operator_tenant_id=? AND managed_product_id=?`, draft.ID, draft.OperatorTenantID, draft.ProductID).Scan(&existingJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return modelapisupply.Plan{}, ErrConflict
			}
			return modelapisupply.Plan{}, err
		}
		var existing modelapisupply.Plan
		if json.Unmarshal([]byte(existingJSON), &existing) != nil || !reflect.DeepEqual(existing, plan) {
			return modelapisupply.Plan{}, fmt.Errorf("%w: supply plan identity already has a different immutable contract", ErrConflict)
		}
		if err = validateExistingModelAPISupplyPlanCandidates(ctx, tx, draft); err != nil {
			return modelapisupply.Plan{}, err
		}
		return existing, tx.Commit()
	}
	for index, candidate := range materialized {
		reference := draft.Candidates[index]
		disposition, position, reasons, ok := modelAPICandidateDisposition(plan, candidate.ID)
		if !ok {
			return modelapisupply.Plan{}, errors.New("compiled supply plan omitted a materialized candidate")
		}
		reasonsJSON, _ := json.Marshal(reasons)
		materializationJSON, _ := json.Marshal(candidate)
		if _, err = tx.ExecContext(ctx, `INSERT INTO model_api_supply_plan_candidates(plan_id,managed_product_id,candidate_id,offer_id,offer_version,qualification_id,retail_rate_card_id,retail_rate_version,disposition,position,traffic_weight_bps,rejection_reasons_json,materialization_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb)`,
			draft.ID, draft.ProductID, candidate.ID, candidate.OfferID, candidate.OfferVersion, reference.QualificationID, candidate.RetailRateID, candidate.RetailRateVersion,
			disposition, position, reference.TrafficWeightBPS, reasonsJSON, materializationJSON); err != nil {
			return modelapisupply.Plan{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return modelapisupply.Plan{}, err
	}
	return plan, nil
}

func validateExistingModelAPISupplyPlanCandidates(ctx context.Context, tx *tx, draft SupplyPlanDraft) error {
	rows, err := tx.QueryContext(ctx, `SELECT candidate_id,offer_id,offer_version,qualification_id,retail_rate_version,traffic_weight_bps FROM model_api_supply_plan_candidates WHERE plan_id=? AND managed_product_id=?`, draft.ID, draft.ProductID)
	if err != nil {
		return err
	}
	defer rows.Close()
	existing := make(map[string]SupplyCandidateReference, len(draft.Candidates))
	for rows.Next() {
		var reference SupplyCandidateReference
		if err = rows.Scan(&reference.CandidateID, &reference.OfferID, &reference.OfferVersion, &reference.QualificationID, &reference.RetailRateVersion, &reference.TrafficWeightBPS); err != nil {
			return err
		}
		existing[reference.CandidateID] = reference
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(existing) != len(draft.Candidates) {
		return fmt.Errorf("%w: supply plan identity already has different immutable candidates", ErrConflict)
	}
	for _, reference := range draft.Candidates {
		stored, ok := existing[reference.CandidateID]
		if !ok || !reflect.DeepEqual(stored, reference) {
			return fmt.Errorf("%w: supply plan identity already has different immutable candidates or rollout weights", ErrConflict)
		}
	}
	return nil
}

func validateModelAPIRolloutWeights(plan modelapisupply.Plan, references []SupplyCandidateReference) error {
	accepted := make(map[string]bool, 1+len(plan.Fallbacks))
	if plan.Primary != nil {
		accepted[plan.Primary.CandidateID] = true
	}
	for _, candidate := range plan.Fallbacks {
		accepted[candidate.CandidateID] = true
	}
	total, weighted := 0, false
	for _, reference := range references {
		if reference.TrafficWeightBPS > 0 {
			weighted = true
			if !accepted[reference.CandidateID] {
				return errors.New("rejected supply candidates cannot receive rollout traffic")
			}
			total += reference.TrafficWeightBPS
		}
	}
	if weighted && total != 10_000 {
		return errors.New("accepted rollout traffic weights must total 10000 basis points")
	}
	return nil
}

func modelAPICandidateDisposition(plan modelapisupply.Plan, candidateID string) (string, any, []string, bool) {
	if plan.Primary != nil && plan.Primary.CandidateID == candidateID {
		return "primary", 0, []string{}, true
	}
	for index, candidate := range plan.Fallbacks {
		if candidate.CandidateID == candidateID {
			return "fallback", index + 1, []string{}, true
		}
	}
	for _, candidate := range plan.Rejections {
		if candidate.CandidateID == candidateID {
			return "rejected", nil, candidate.Reasons, true
		}
	}
	return "", nil, nil, false
}

func scanModelAPISupplierOffer(row interface{ Scan(...any) error }) (modelapisupply.Offer, error) {
	var out modelapisupply.Offer
	var capabilitiesJSON string
	var observed, costFrom, costUntil, commercialUntil sql.NullTime
	var costInput, costOutput, costCached sql.NullInt64
	var costProvenance, terms sql.NullString
	err := row.Scan(&out.ID, &out.Version, &out.OperatorTenantID, &out.ProductID, &out.Supplier, &out.Adapter, &out.SupplierModelID, &out.Protocol, &out.TupleKey, &out.Region, &out.CredentialReference, &out.State, &capabilitiesJSON, &out.Access, &out.Availability, &out.Health, &observed,
		&out.CostRate.Currency, &costInput, &costOutput, &costCached, &costProvenance, &costFrom, &costUntil, &out.Commercial.State, &terms, &commercialUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapisupply.Offer{}, ErrNotFound
	}
	if err != nil {
		return modelapisupply.Offer{}, err
	}
	if json.Unmarshal([]byte(capabilitiesJSON), &out.Capabilities) != nil {
		return modelapisupply.Offer{}, errors.New("stored supplier offer capabilities are invalid")
	}
	out.Capabilities = normalizedModelAPIStrings(out.Capabilities)
	if observed.Valid {
		out.ObservedAt = observed.Time.UTC()
	}
	if costInput.Valid {
		value := costInput.Int64
		out.CostRate.InputMicrousdPerMTok = &value
	}
	if costOutput.Valid {
		value := costOutput.Int64
		out.CostRate.OutputMicrousdPerMTok = &value
	}
	if costCached.Valid {
		value := costCached.Int64
		out.CostRate.CachedInputMicrousdPerMTok = &value
	}
	if costProvenance.Valid {
		out.CostRate.Provenance = costProvenance.String
	}
	if costFrom.Valid {
		out.CostRate.ValidFrom = costFrom.Time.UTC()
	}
	if costUntil.Valid {
		out.CostRate.ValidUntil = costUntil.Time.UTC()
	}
	if terms.Valid {
		out.Commercial.TermsRef = terms.String
	}
	if commercialUntil.Valid {
		out.Commercial.ValidUntil = commercialUntil.Time.UTC()
	}
	return out, out.Validate()
}

func scanModelAPISupplyQualification(row interface{ Scan(...any) error }) (modelapisupply.QualificationEvidence, error) {
	var out modelapisupply.QualificationEvidence
	var capabilitiesJSON string
	var evidenceRef, evidenceDigest, reason sql.NullString
	var observed, until sql.NullTime
	var ttft, output sql.NullFloat64
	err := row.Scan(&out.ID, &out.State, &out.TupleKey, &out.Protocol, &out.Region, &capabilitiesJSON, &out.Scope, &evidenceRef, &evidenceDigest, &reason, &observed, &until, &out.SampleCount, &ttft, &output)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapisupply.QualificationEvidence{}, ErrNotFound
	}
	if err != nil {
		return modelapisupply.QualificationEvidence{}, err
	}
	if json.Unmarshal([]byte(capabilitiesJSON), &out.Capabilities) != nil {
		return modelapisupply.QualificationEvidence{}, errors.New("stored qualification capabilities are invalid")
	}
	out.Capabilities = normalizedModelAPIStrings(out.Capabilities)
	if evidenceRef.Valid {
		out.EvidenceRef = evidenceRef.String
	}
	if evidenceDigest.Valid {
		out.EvidenceDigest = evidenceDigest.String
	}
	if reason.Valid {
		out.Reason = reason.String
	}
	if observed.Valid {
		out.ObservedAt = observed.Time.UTC()
	}
	if until.Valid {
		out.ValidUntil = until.Time.UTC()
	}
	if ttft.Valid {
		value := ttft.Float64
		out.TTFTP95MS = &value
	}
	if output.Valid {
		value := output.Float64
		out.OutputTokensP5 = &value
	}
	return out, out.Validate()
}

func normalizedModelAPIStrings(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func requirePostgresSafeOfferTimes(offer modelapisupply.Offer) error {
	values := []time.Time{offer.ObservedAt, offer.CostRate.ValidFrom, offer.CostRate.ValidUntil, offer.Commercial.ValidUntil}
	for _, value := range values {
		if !value.IsZero() && !value.Equal(value.Truncate(time.Microsecond)) {
			return errors.New("supplier offer timestamps must use PostgreSQL-safe microsecond precision")
		}
	}
	return nil
}

func requirePostgresSafeQualificationTimes(evidence modelapisupply.QualificationEvidence) error {
	for _, value := range []time.Time{evidence.ObservedAt, evidence.ValidUntil} {
		if !value.IsZero() && !value.Equal(value.Truncate(time.Microsecond)) {
			return errors.New("qualification timestamps must use PostgreSQL-safe microsecond precision")
		}
	}
	return nil
}

func nullableModelAPIString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableModelAPIInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
func nullableModelAPITime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}
