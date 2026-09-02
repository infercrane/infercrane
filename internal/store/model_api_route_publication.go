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

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapirouting"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/supplieradapter"
)

// PublishedModelAPIRoutes compiles the complete secret-free hosted routing
// generation. It intentionally fails the whole generation when any active
// entitlement points at stale or mismatched control-plane evidence.
func (s *Store) PublishedModelAPIRoutes(ctx context.Context, at time.Time) ([]modelapirouting.RouteSource, error) {
	if at.IsZero() {
		return nil, errors.New("route publication time is required")
	}
	at = at.UTC()
	inner, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	tx := &tx{inner}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT
		e.id,e.customer_tenant_id,e.product_id,e.operator_tenant_id,e.serving_plan_id,e.retail_rate_card_id,e.retail_rate_version,e.state,e.requests_per_minute,e.tokens_per_minute,e.monthly_spend_microusd,e.max_request_microusd,e.valid_from,e.valid_until,
		p.supply_plan_id,p.qualification_state,p.qualification_evidence_id,p.qualification_valid_until,p.active_retail_rate_card_id,
		m.protocol,m.capability_contract_json::text,m.availability,
		r.contract_digest,r.input_microusd_per_million,r.cached_input_microusd_per_million,r.output_microusd_per_million,r.valid_from,r.valid_until,
		sp.protocol,sp.schema_version,sp.digest,sp.status,sp.plan_json::text,sp.generated_at,sp.valid_until
	FROM model_api_product_entitlements e
	JOIN model_api_operator_publications p ON p.product_id=e.product_id AND p.operator_tenant_id=e.operator_tenant_id AND p.serving_plan_id=e.serving_plan_id
	JOIN model_api_products m ON m.id=e.product_id
	JOIN model_api_retail_rate_cards r ON r.product_id=e.product_id AND r.id=e.retail_rate_card_id AND r.version=e.retail_rate_version
	JOIN model_api_supply_plans sp ON sp.id=p.supply_plan_id AND sp.managed_product_id=e.product_id AND sp.operator_tenant_id=e.operator_tenant_id
	WHERE e.state='active' ORDER BY e.customer_tenant_id,e.product_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type draft struct {
		source     modelapirouting.RouteSource
		operations []string
		plan       modelapisupply.Plan
	}
	drafts := make([]draft, 0)
	for rows.Next() {
		var source modelapirouting.RouteSource
		var entitlementUntil, publicationUntil, planUntil sql.NullTime
		var cachedInput sql.NullInt64
		var requestsPerMinute, tokensPerMinute, monthlySpend, maxRequestValue sql.NullInt64
		var publicationState, activeRateID, productProtocol, capabilitiesJSON, availability string
		var planProtocol, planSchema, planDigest, planStatus, planJSON string
		var generatedAt time.Time
		if err = rows.Scan(
			&source.Entitlement.ID, &source.Entitlement.CustomerTenantID, &source.Entitlement.ProductID, &source.Entitlement.OperatorTenantID,
			&source.Entitlement.ServingPlanID, &source.Entitlement.RetailRateID, &source.Entitlement.RetailRateVersion, &source.Entitlement.State,
			&requestsPerMinute, &tokensPerMinute, &monthlySpend, &maxRequestValue, &source.Entitlement.ValidFrom, &entitlementUntil,
			&source.Publication.SupplyPlanID, &publicationState, &source.Publication.EvidenceID, &publicationUntil, &activeRateID,
			&productProtocol, &capabilitiesJSON, &availability,
			&source.Rate.ContractDigest, &source.Rate.InputMicrousdPerMillion, &cachedInput, &source.Rate.OutputMicrousdPerMillion, &source.Rate.ValidFrom, &source.Rate.ValidUntil,
			&planProtocol, &planSchema, &planDigest, &planStatus, &planJSON, &generatedAt, &planUntil,
		); err != nil {
			return nil, err
		}
		source.Publication.ProductID = source.Entitlement.ProductID
		source.Publication.OperatorTenantID = source.Entitlement.OperatorTenantID
		source.Publication.ServingPlanID = source.Entitlement.ServingPlanID
		source.Rate.ID, source.Rate.ProductID, source.Rate.Version = source.Entitlement.RetailRateID, source.Entitlement.ProductID, source.Entitlement.RetailRateVersion
		if maxRequestValue.Valid {
			source.Entitlement.MaxRequestMicrousd = maxRequestValue.Int64
		}
		if entitlementUntil.Valid {
			value := entitlementUntil.Time.UTC()
			source.Entitlement.ValidUntil = &value
		}
		if cachedInput.Valid {
			value := cachedInput.Int64
			source.Rate.CachedInputMicrousdPerMillion = &value
		}
		var claims []modelapiproduct.CapabilityClaim
		if json.Unmarshal([]byte(capabilitiesJSON), &claims) != nil {
			return nil, fmt.Errorf("hosted product %q has invalid capability contract", source.Entitlement.ProductID)
		}
		operations, capabilityUntil, operationErr := qualifiedOperations(claims, at)
		if operationErr != nil {
			return nil, fmt.Errorf("hosted product %q: %w", source.Entitlement.ProductID, operationErr)
		}
		if requestsPerMinute.Valid || tokensPerMinute.Valid || monthlySpend.Valid || source.Entitlement.MaxRequestMicrousd <= 0 || source.Rate.CachedInputMicrousdPerMillion != nil ||
			publicationState != "qualified" || !publicationUntil.Valid || !at.Before(publicationUntil.Time.UTC()) || activeRateID != source.Rate.ID ||
			productProtocol != "openai" || (availability != "available" && availability != "degraded") || planProtocol != "openai" || planSchema != modelapisupply.SchemaVersion ||
			planStatus != modelapisupply.StatusReady || !planUntil.Valid || !at.Before(planUntil.Time.UTC()) || at.Before(source.Entitlement.ValidFrom.UTC()) ||
			(source.Entitlement.ValidUntil != nil && !at.Before(source.Entitlement.ValidUntil.UTC())) || at.Before(source.Rate.ValidFrom.UTC()) || !at.Before(source.Rate.ValidUntil.UTC()) {
			return nil, fmt.Errorf("hosted product %q has stale or incompatible publication, entitlement, plan, or rate", source.Entitlement.ProductID)
		}
		var plan modelapisupply.Plan
		if json.Unmarshal([]byte(planJSON), &plan) != nil || plan.Digest != planDigest || !plan.HasCanonicalDigest() || plan.SchemaVersion != planSchema || plan.ModelID != source.Entitlement.ProductID ||
			plan.Protocol != planProtocol || plan.Status != modelapisupply.StatusReady || plan.Primary == nil || !plan.ValidUntil.Equal(planUntil.Time.UTC()) || !generatedAt.UTC().Equal(plan.GeneratedAt.UTC()) {
			return nil, fmt.Errorf("hosted product %q supply plan materialization does not match its persisted contract", source.Entitlement.ProductID)
		}
		source.Publication.ValidUntil = earliestTime(publicationUntil.Time, source.Rate.ValidUntil, planUntil.Time, capabilityUntil)
		source.Publication.EvidenceValidUntil = publicationUntil.Time.UTC()
		drafts = append(drafts, draft{source: source, operations: operations, plan: plan})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	sources := make([]modelapirouting.RouteSource, 0, len(drafts))
	for _, item := range drafts {
		source := item.source
		source.Candidates, err = publishedModelAPICandidates(ctx, tx, at, source, item.operations, item.plan)
		if err != nil {
			return nil, err
		}
		if len(source.Candidates) == 0 {
			return nil, fmt.Errorf("hosted product %q has no qualified supply candidate", source.Entitlement.ProductID)
		}
		source.Publication.CompatibilityKey = source.Candidates[0].Candidate.CompatibilityKey
		sources = append(sources, source)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return sources, nil
}

func publishedModelAPICandidates(ctx context.Context, tx *tx, at time.Time, source modelapirouting.RouteSource, operations []string, plan modelapisupply.Plan) ([]modelapirouting.CandidateSource, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.candidate_id,c.offer_id,c.offer_version,c.qualification_id,c.retail_rate_card_id,c.retail_rate_version,c.disposition,c.position,c.traffic_weight_bps,
		o.supplier,o.adapter,o.supplier_model_id,o.protocol,o.tuple_key,o.region,o.credential_reference,o.state,o.capabilities_json::text,o.access_state,o.availability_state,o.health_state,o.observed_at,o.cost_valid_until,o.commercial_state,o.commercial_valid_until,
		q.id,q.state,q.tuple_key,q.protocol,q.region,q.capabilities_json::text,q.evidence_ref,q.evidence_digest,q.observed_at,q.valid_until,
		b.binding_count,b.id,b.endpoint_reference,b.endpoint_config_digest,b.contract_digest,b.valid_until
	FROM model_api_supply_plan_candidates c
	JOIN model_api_supplier_offers o ON o.id=c.offer_id AND o.version=c.offer_version AND o.managed_product_id=c.managed_product_id
	JOIN model_api_supply_qualifications q ON q.id=c.qualification_id AND q.offer_id=c.offer_id AND q.offer_version=c.offer_version
	LEFT JOIN LATERAL (
		SELECT count(*) AS binding_count,min(id) AS id,min(endpoint_reference) AS endpoint_reference,min(endpoint_config_digest) AS endpoint_config_digest,min(contract_digest) AS contract_digest,min(valid_until) AS valid_until
		FROM model_api_target_bindings
		WHERE operator_tenant_id=o.operator_tenant_id AND managed_product_id=o.managed_product_id AND offer_id=o.id AND offer_version=o.version
			AND adapter=o.adapter AND supplier_model_id=o.supplier_model_id AND region=o.region AND valid_from<=? AND valid_until>?
	) b ON true
	WHERE c.plan_id=? AND c.managed_product_id=? AND c.disposition IN ('primary','fallback')
	ORDER BY c.position`, at, at, source.Publication.SupplyPlanID, source.Entitlement.ProductID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]modelapirouting.CandidateSource, 0)
	for rows.Next() {
		var item modelapirouting.CandidateSource
		var candidateRateVersion int
		var qualificationID, rateID, disposition, offerState, access, availability, health, qualificationState string
		var region, offerCapabilitiesJSON, qualificationTuple, qualificationProtocol, qualificationRegion, qualificationCapabilitiesJSON string
		var evidenceRef, evidenceDigest sql.NullString
		var bindingID, endpointReference, endpointConfigDigest, bindingDigest sql.NullString
		var position int
		var bindingCount int
		var observed, costUntil, commercialUntil, qualificationObserved, qualificationUntil, bindingUntil sql.NullTime
		var commercialState string
		candidate := &item.Candidate
		if err = rows.Scan(&candidate.ID, &candidate.OfferID, &candidate.OfferVersion, &qualificationID, &rateID, &candidateRateVersion, &disposition, &position, &candidate.TrafficWeightBPS,
			&candidate.Supplier, &item.Adapter, &candidate.SupplierModelID, &candidate.Protocol, &candidate.CompatibilityKey, &region, &item.CredentialReference, &offerState, &offerCapabilitiesJSON, &access, &availability, &health, &observed, &costUntil, &commercialState, &commercialUntil,
			&candidate.QualificationEvidenceID, &qualificationState, &qualificationTuple, &qualificationProtocol, &qualificationRegion, &qualificationCapabilitiesJSON, &evidenceRef, &evidenceDigest, &qualificationObserved, &qualificationUntil,
			&bindingCount, &bindingID, &endpointReference, &endpointConfigDigest, &bindingDigest, &bindingUntil); err != nil {
			return nil, err
		}
		candidate.ProductID, candidate.OperatorTenantID = source.Entitlement.ProductID, source.Entitlement.OperatorTenantID
		candidate.ServingPlanID, candidate.SupplyPlanID = source.Entitlement.ServingPlanID, source.Publication.SupplyPlanID
		var offerCapabilities, qualificationCapabilities []string
		if json.Unmarshal([]byte(offerCapabilitiesJSON), &offerCapabilities) != nil || json.Unmarshal([]byte(qualificationCapabilitiesJSON), &qualificationCapabilities) != nil {
			return nil, fmt.Errorf("hosted candidate %q has invalid capabilities", candidate.ID)
		}
		sort.Strings(offerCapabilities)
		sort.Strings(qualificationCapabilities)
		selection, expectedPosition, selectionOK := selectionAt(plan, disposition, position)
		if !selectionOK || selection.CandidateID != candidate.ID || selection.OfferID != candidate.OfferID || selection.OfferVersion != candidate.OfferVersion ||
			selection.RetailRateID != rateID || selection.RetailRateVersion != candidateRateVersion || selection.QualificationEvidenceID != qualificationID || selection.Supplier != candidate.Supplier ||
			selection.SupplierModelID != candidate.SupplierModelID || selection.TupleKey != candidate.CompatibilityKey || expectedPosition != position || rateID != source.Rate.ID || candidateRateVersion != source.Rate.Version ||
			offerState != "active" || access != "ready" || availability != "available" || health != "healthy" || !observed.Valid || observed.Time.After(at) || !costUntil.Valid || !at.Before(costUntil.Time) ||
			commercialState != "ready" || !commercialUntil.Valid || !at.Before(commercialUntil.Time) || qualificationState != "qualified" || qualificationTuple != candidate.CompatibilityKey ||
			qualificationProtocol != candidate.Protocol || qualificationRegion != region || !reflect.DeepEqual(offerCapabilities, qualificationCapabilities) || !evidenceRef.Valid || evidenceRef.String == "" ||
			!evidenceDigest.Valid || evidenceDigest.String == "" || !qualificationObserved.Valid || qualificationObserved.Time.After(at) || !qualificationUntil.Valid || !at.Before(qualificationUntil.Time) ||
			selection.ValidUntil.IsZero() || !at.Before(selection.ValidUntil.UTC()) || !supportsPublishedOperations(offerCapabilities, operations) {
			return nil, fmt.Errorf("hosted candidate %q does not exactly match current plan, rate, qualification, and evidence", candidate.ID)
		}
		if bindingCount > 1 {
			return nil, fmt.Errorf("hosted candidate %q has ambiguous current target bindings", candidate.ID)
		}
		if bindingCount == 1 {
			if !bindingID.Valid || !endpointReference.Valid || !endpointConfigDigest.Valid || !bindingDigest.Valid || !bindingUntil.Valid {
				return nil, fmt.Errorf("hosted candidate %q has an incomplete target binding", candidate.ID)
			}
			candidate.TargetBindingID, candidate.TargetBindingDigest = bindingID.String, bindingDigest.String
			item.EndpointReference, item.EndpointConfigDigest = endpointReference.String, endpointConfigDigest.String
		} else if item.Adapter == supplieradapter.RunPodVLLMAdapterName {
			return nil, fmt.Errorf("hosted RunPod candidate %q has no current immutable target binding", candidate.ID)
		}
		candidate.Operations = append([]string(nil), operations...)
		candidate.Qualified, candidate.Available = true, true
		candidate.ValidUntil = earliestTime(source.Publication.ValidUntil, selection.ValidUntil, costUntil.Time, commercialUntil.Time, qualificationUntil.Time, bindingUntil.Time)
		candidates = append(candidates, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) != 1+len(plan.Fallbacks) {
		return nil, errors.New("persisted hosted candidates do not exactly match the supply plan")
	}
	compatibility := candidates[0].Candidate.CompatibilityKey
	for _, candidate := range candidates[1:] {
		if candidate.Candidate.CompatibilityKey != compatibility {
			return nil, errors.New("hosted fallback is not exactly compatible with the primary")
		}
	}
	return candidates, nil
}

func supportsPublishedOperations(capabilities, operations []string) bool {
	available := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		available[capability] = true
	}
	for _, operation := range operations {
		claim := map[string]string{"chat": "chat-completions", "completions": "completions", "responses": "responses", "embeddings": "embeddings", "streaming": "streaming"}[operation]
		if claim == "" || !available[claim] {
			return false
		}
	}
	return true
}

func selectionAt(plan modelapisupply.Plan, disposition string, position int) (modelapisupply.Selection, int, bool) {
	if disposition == "primary" && position == 0 && plan.Primary != nil {
		return *plan.Primary, 0, true
	}
	if disposition == "fallback" && position > 0 && position <= len(plan.Fallbacks) {
		return plan.Fallbacks[position-1], position, true
	}
	return modelapisupply.Selection{}, 0, false
}

func qualifiedOperations(claims []modelapiproduct.CapabilityClaim, at time.Time) ([]string, time.Time, error) {
	operations := make([]string, 0)
	var validUntil time.Time
	var streamingUntil time.Time
	requiresStreaming := false
	for _, claim := range claims {
		if !claim.CurrentAt(at) {
			continue
		}
		if claim.Name == "streaming" {
			streamingUntil = claim.EvidenceUntil.UTC()
			continue
		}
		operation, callable := claim.CallableOperation()
		if !callable {
			continue
		}
		operations = append(operations, operation)
		requiresStreaming = requiresStreaming || operation == "chat" || operation == "completions" || operation == "responses"
		if validUntil.IsZero() || claim.EvidenceUntil.Before(validUntil) {
			validUntil = claim.EvidenceUntil.UTC()
		}
	}
	if len(operations) == 0 || validUntil.IsZero() {
		return nil, time.Time{}, errors.New("no current qualified callable capability evidence")
	}
	if requiresStreaming {
		if streamingUntil.IsZero() {
			return nil, time.Time{}, errors.New("stream-capable operations require current qualified streaming evidence")
		}
		operations = append(operations, "streaming")
		if streamingUntil.Before(validUntil) {
			validUntil = streamingUntil
		}
	}
	sort.Strings(operations)
	return operations, validUntil, nil
}

func earliestTime(values ...time.Time) time.Time {
	var earliest time.Time
	for _, value := range values {
		value = value.UTC()
		if value.IsZero() {
			continue
		}
		if earliest.IsZero() || value.Before(earliest) {
			earliest = value
		}
	}
	return earliest
}
