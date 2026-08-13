package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/infercrane/infercrane/internal/domain"
)

var endpointNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

func validEndpointName(value string) bool { return endpointNamePattern.MatchString(value) }

func (s *Store) CreateEnvironment(ctx context.Context, tenant string, environment domain.Environment) (domain.Environment, error) {
	if tenant == "" || !validEndpointName(environment.Name) || !validBoundedJSON(environment.PolicyJSON, 64<<10) {
		return domain.Environment{}, errors.New("tenant, valid environment name, and bounded JSON policy are required")
	}
	if environment.PolicyJSON == "" {
		environment.PolicyJSON = "{}"
	}
	if environment.ID == "" {
		var err error
		environment.ID, err = newID()
		if err != nil {
			return domain.Environment{}, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO environments(id,tenant_id,name,policy_json,created_at,updated_at) VALUES(?,?,?,?::jsonb,?,?)`, environment.ID, tenant, environment.Name, environment.PolicyJSON, stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Environment{}, fmt.Errorf("%w: environment already exists", ErrConflict)
		}
		return domain.Environment{}, err
	}
	environment.TenantID = tenant
	environment.CreatedAt, environment.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return environment, nil
}

func (s *Store) EnvironmentsForTenant(ctx context.Context, tenant string) ([]domain.Environment, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,name,policy_json::text,created_at,updated_at FROM environments WHERE tenant_id=? ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Environment
	for rows.Next() {
		var item domain.Environment
		var created, updated string
		if err = rows.Scan(&item.ID, &item.TenantID, &item.Name, &item.PolicyJSON, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) EnvironmentForTenant(ctx context.Context, tenant, name string) (domain.Environment, error) {
	var item domain.Environment
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,name,policy_json::text,created_at,updated_at FROM environments WHERE tenant_id=? AND name=?`, tenant, name).Scan(&item.ID, &item.TenantID, &item.Name, &item.PolicyJSON, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return item, nil
}

func (s *Store) CreateLogicalModel(ctx context.Context, tenant string, model domain.LogicalModel) (domain.LogicalModel, error) {
	if tenant == "" || !validEndpointName(model.Name) || len(model.Description) > 4096 {
		return domain.LogicalModel{}, errors.New("tenant and valid logical model name are required")
	}
	if model.ID == "" {
		var err error
		model.ID, err = newID()
		if err != nil {
			return domain.LogicalModel{}, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO logical_models(id,tenant_id,name,description,created_at,updated_at) VALUES(?,?,?,?,?,?)`, model.ID, tenant, model.Name, model.Description, stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.LogicalModel{}, fmt.Errorf("%w: logical model already exists", ErrConflict)
		}
		return domain.LogicalModel{}, err
	}
	model.TenantID = tenant
	model.CreatedAt, model.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return model, nil
}

func (s *Store) LogicalModelsForTenant(ctx context.Context, tenant string) ([]domain.LogicalModel, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,name,description,created_at,updated_at FROM logical_models WHERE tenant_id=? ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LogicalModel
	for rows.Next() {
		var item domain.LogicalModel
		var created, updated string
		if err = rows.Scan(&item.ID, &item.TenantID, &item.Name, &item.Description, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) LogicalModelForTenant(ctx context.Context, tenant, name string) (domain.LogicalModel, error) {
	var item domain.LogicalModel
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,name,description,created_at,updated_at FROM logical_models WHERE tenant_id=? AND name=?`, tenant, name).Scan(&item.ID, &item.TenantID, &item.Name, &item.Description, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return item, nil
}

func (s *Store) CreateEndpoint(ctx context.Context, tenant string, endpoint domain.Endpoint) (domain.Endpoint, error) {
	if tenant == "" || !validEndpointName(endpoint.Name) || endpoint.LogicalModelID == "" || endpoint.EnvironmentID == "" {
		return domain.Endpoint{}, errors.New("tenant, valid endpoint name, logical model, and environment are required")
	}
	if endpoint.ID == "" {
		var err error
		endpoint.ID, err = newID()
		if err != nil {
			return domain.Endpoint{}, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO endpoints(id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,created_at,updated_at) SELECT ?,?,?,?,?, 'serving','pending',?,? WHERE EXISTS(SELECT 1 FROM logical_models WHERE id=? AND tenant_id=?) AND EXISTS(SELECT 1 FROM environments WHERE id=? AND tenant_id=?)`, endpoint.ID, tenant, endpoint.LogicalModelID, endpoint.EnvironmentID, endpoint.Name, stamp, stamp, endpoint.LogicalModelID, tenant, endpoint.EnvironmentID, tenant)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Endpoint{}, fmt.Errorf("%w: endpoint already exists", ErrConflict)
		}
		return domain.Endpoint{}, err
	}
	var count int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM endpoints WHERE id=? AND tenant_id=?`, endpoint.ID, tenant).Scan(&count); err != nil {
		return domain.Endpoint{}, err
	}
	if count == 0 {
		return domain.Endpoint{}, fmt.Errorf("%w: logical model or environment", ErrNotFound)
	}
	endpoint.TenantID = tenant
	endpoint.DesiredState = "serving"
	endpoint.ObservedState = "pending"
	endpoint.CreatedAt, endpoint.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return endpoint, nil
}

func (s *Store) CreateBackendBinding(ctx context.Context, tenant string, binding domain.BackendBinding) (domain.BackendBinding, error) {
	if tenant == "" || binding.EndpointID == "" || !validEndpointName(binding.Name) {
		return domain.BackendBinding{}, errors.New("tenant, endpoint, and valid binding name are required")
	}
	if binding.Kind != "deployment" && binding.Kind != "external" {
		return domain.BackendBinding{}, errors.New("binding kind must be deployment or external")
	}
	if binding.OwnershipMode != "observe-only" && binding.OwnershipMode != "traffic-managed" && binding.OwnershipMode != "lifecycle-managed" {
		return domain.BackendBinding{}, errors.New("invalid ownership mode")
	}
	if !validBoundedJSON(binding.ConfigJSON, 64<<10) {
		return domain.BackendBinding{}, errors.New("binding config must be bounded JSON")
	}
	if binding.ConfigJSON == "" {
		binding.ConfigJSON = "{}"
	}
	if binding.ID == "" {
		var err error
		binding.ID, err = newID()
		if err != nil {
			return domain.BackendBinding{}, err
		}
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,deployment_id,target_id,config_json,created_at,updated_at) SELECT ?,?,?,?,?,?,?,?,?::jsonb,?,? WHERE EXISTS(SELECT 1 FROM endpoints WHERE id=? AND tenant_id=?) AND ((?='deployment' AND EXISTS(SELECT 1 FROM deployments WHERE id=? AND tenant_id=?)) OR (?='external' AND EXISTS(SELECT 1 FROM targets WHERE id=? AND tenant_id=?)))`, binding.ID, tenant, binding.EndpointID, binding.Name, binding.Kind, binding.OwnershipMode, null(binding.DeploymentID), null(binding.TargetID), binding.ConfigJSON, stamp, stamp, binding.EndpointID, tenant, binding.Kind, binding.DeploymentID, tenant, binding.Kind, binding.TargetID, tenant)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.BackendBinding{}, fmt.Errorf("%w: binding already exists", ErrConflict)
		}
		return domain.BackendBinding{}, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return domain.BackendBinding{}, fmt.Errorf("%w: endpoint or backing resource", ErrNotFound)
	}
	binding.TenantID = tenant
	binding.CreatedAt, binding.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return binding, nil
}

type canonicalPlan struct {
	RoutingPolicy string                      `json:"routing_policy"`
	Bindings      []domain.ServingPlanBinding `json:"bindings"`
}

func normalizePlan(policy string, bindings []domain.ServingPlanBinding) (canonicalPlan, []byte, error) {
	if policy != "manual" && policy != "primary-fallback" && policy != "weighted" {
		return canonicalPlan{}, nil, errors.New("unsupported routing policy")
	}
	if len(bindings) == 0 || len(bindings) > 32 {
		return canonicalPlan{}, nil, errors.New("serving plan requires 1..32 bindings")
	}
	copyBindings := append([]domain.ServingPlanBinding(nil), bindings...)
	sort.Slice(copyBindings, func(i, j int) bool { return copyBindings[i].Priority < copyBindings[j].Priority })
	seenID := map[string]bool{}
	seenPriority := map[int]bool{}
	for _, binding := range copyBindings {
		if binding.BindingID == "" || binding.Priority < 0 || binding.Weight < 1 || binding.Weight > 10000 || seenID[binding.BindingID] || seenPriority[binding.Priority] {
			return canonicalPlan{}, nil, errors.New("serving plan bindings must have unique IDs and priorities with bounded weights")
		}
		seenID[binding.BindingID] = true
		seenPriority[binding.Priority] = true
	}
	if policy == "manual" && len(copyBindings) != 1 {
		return canonicalPlan{}, nil, errors.New("manual routing requires exactly one binding")
	}
	plan := canonicalPlan{RoutingPolicy: policy, Bindings: copyBindings}
	body, err := json.Marshal(plan)
	return plan, body, err
}

func (s *Store) CreateServingPlan(ctx context.Context, tenant string, plan domain.ServingPlan) (domain.ServingPlan, error) {
	_, body, err := normalizePlan(plan.RoutingPolicy, plan.Bindings)
	if err != nil {
		return domain.ServingPlan{}, err
	}
	digest := sha256.Sum256(body)
	plan.SpecJSON = string(body)
	plan.SpecDigest = "sha256:" + hex.EncodeToString(digest[:])
	if plan.ID == "" {
		plan.ID, err = newID()
		if err != nil {
			return domain.ServingPlan{}, err
		}
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ServingPlan{}, err
	}
	defer tx.Rollback()
	var endpointExists bool
	if err = tx.QueryRowContext(ctx, `SELECT true FROM endpoints WHERE id=? AND tenant_id=? FOR UPDATE`, plan.EndpointID, tenant).Scan(&endpointExists); errors.Is(err, sql.ErrNoRows) {
		return domain.ServingPlan{}, fmt.Errorf("%w: endpoint", ErrNotFound)
	} else if err != nil {
		return domain.ServingPlan{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM serving_plans WHERE endpoint_id=?`, plan.EndpointID).Scan(&plan.Version); err != nil {
		return domain.ServingPlan{}, err
	}
	stamp := now()
	result, err := tx.ExecContext(ctx, `INSERT INTO serving_plans(id,tenant_id,endpoint_id,version,routing_policy,spec_json,spec_digest,created_at) SELECT ?,?,?,?,?,?::jsonb,?,? WHERE EXISTS(SELECT 1 FROM endpoints WHERE id=? AND tenant_id=?)`, plan.ID, tenant, plan.EndpointID, plan.Version, plan.RoutingPolicy, plan.SpecJSON, plan.SpecDigest, stamp, plan.EndpointID, tenant)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ServingPlan{}, fmt.Errorf("%w: serving plan already exists", ErrConflict)
		}
		return domain.ServingPlan{}, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return domain.ServingPlan{}, fmt.Errorf("%w: endpoint", ErrNotFound)
	}
	for _, binding := range plan.Bindings {
		result, err = tx.ExecContext(ctx, `INSERT INTO serving_plan_bindings(serving_plan_id,binding_id,priority,weight) SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM backend_bindings WHERE id=? AND endpoint_id=? AND tenant_id=?)`, plan.ID, binding.BindingID, binding.Priority, binding.Weight, binding.BindingID, plan.EndpointID, tenant)
		if err != nil {
			return domain.ServingPlan{}, err
		}
		n, _ = result.RowsAffected()
		if n == 0 {
			return domain.ServingPlan{}, fmt.Errorf("%w: binding %s", ErrNotFound, binding.BindingID)
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.ServingPlan{}, err
	}
	plan.TenantID = tenant
	plan.CreatedAt = parseTime(stamp)
	return plan, nil
}

func (s *Store) SetEndpointPlan(ctx context.Context, tenant, name, planID, slot string) error {
	if slot != "active" && slot != "candidate" {
		return errors.New("slot must be active or candidate")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var endpointID, activePlanID, candidatePlanID string
	if err = tx.QueryRowContext(ctx, `SELECT id,COALESCE(active_serving_plan_id,''),COALESCE(candidate_serving_plan_id,'') FROM endpoints WHERE tenant_id=? AND name=? FOR UPDATE`, tenant, name).Scan(&endpointID, &activePlanID, &candidatePlanID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var planEndpoint string
	if err = tx.QueryRowContext(ctx, `SELECT endpoint_id FROM serving_plans WHERE id=? AND tenant_id=?`, planID, tenant).Scan(&planEndpoint); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if planEndpoint != endpointID {
		return fmt.Errorf("%w: serving plan belongs to another endpoint", ErrConflict)
	}
	if slot == "active" && activePlanID != "" {
		if candidatePlanID != planID {
			return fmt.Errorf("%w: serving plan is not the current candidate", ErrConflict)
		}
		var decision, evaluatedActive string
		err = tx.QueryRowContext(ctx, `SELECT decision,active_serving_plan_id FROM endpoint_release_guard_evaluations WHERE endpoint_id=? AND candidate_serving_plan_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, endpointID, planID).Scan(&decision, &evaluatedActive)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: current endpoint candidate lacks a PASS decision against the current active plan", ErrConflict)
		}
		if err != nil {
			return err
		}
		if decision != "PASS" || evaluatedActive != activePlanID {
			return fmt.Errorf("%w: current endpoint candidate lacks a PASS decision against the current active plan", ErrConflict)
		}
	}
	column := "candidate_serving_plan_id"
	if slot == "active" {
		column = "active_serving_plan_id"
	}
	query := `UPDATE endpoints SET ` + column + `=?,updated_at=? WHERE id=?`
	if slot == "active" {
		query = `UPDATE endpoints SET active_serving_plan_id=?,candidate_serving_plan_id=CASE WHEN candidate_serving_plan_id=? THEN NULL ELSE candidate_serving_plan_id END,updated_at=? WHERE id=?`
		if _, err = tx.ExecContext(ctx, query, planID, planID, now(), endpointID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, query, planID, now(), endpointID); err != nil {
		return err
	}
	return tx.Commit()
}

// StageEnvironmentPromotion atomically copies the source endpoint's active
// serving plan into the destination candidate slot. It never activates the
// candidate; Endpoint Release Guard remains the promotion authority.
func (s *Store) StageEnvironmentPromotion(ctx context.Context, tenant, sourceName, destinationName, idempotencyKey string) (domain.EnvironmentPromotion, bool, error) {
	if tenant == "" || sourceName == "" || destinationName == "" || sourceName == destinationName || idempotencyKey == "" || len(idempotencyKey) > 255 {
		return domain.EnvironmentPromotion{}, false, errors.New("tenant, distinct source/destination endpoints, and a bounded idempotency key are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	defer tx.Rollback()
	var existing domain.EnvironmentPromotion
	var existingCreated string
	err = tx.QueryRowContext(ctx, `SELECT id,tenant_id,source_endpoint_id,source_plan_id,destination_endpoint_id,destination_plan_id,idempotency_key,created_at FROM environment_promotions WHERE tenant_id=? AND idempotency_key=?`, tenant, idempotencyKey).Scan(&existing.ID, &existing.TenantID, &existing.SourceEndpointID, &existing.SourcePlanID, &existing.DestinationEndpointID, &existing.DestinationPlanID, &existing.IdempotencyKey, &existingCreated)
	if err == nil {
		existing.CreatedAt = parseTime(existingCreated)
		var sourceID, destinationID string
		if lookupErr := tx.QueryRowContext(ctx, `SELECT (SELECT id FROM endpoints WHERE tenant_id=? AND name=?),(SELECT id FROM endpoints WHERE tenant_id=? AND name=?)`, tenant, sourceName, tenant, destinationName).Scan(&sourceID, &destinationID); lookupErr != nil {
			return domain.EnvironmentPromotion{}, false, lookupErr
		}
		if existing.SourceEndpointID != sourceID || existing.DestinationEndpointID != destinationID {
			return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: promotion idempotency key identifies another endpoint pair", ErrConflict)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.EnvironmentPromotion{}, false, err
	}
	var sourceID, sourceModelID, sourcePlanID, sourcePolicy string
	if err = tx.QueryRowContext(ctx, `SELECT e.id,e.logical_model_id,COALESCE(e.active_serving_plan_id,''),COALESCE(p.routing_policy,'') FROM endpoints e LEFT JOIN serving_plans p ON p.id=e.active_serving_plan_id WHERE e.tenant_id=? AND e.name=? AND e.desired_state='serving' FOR UPDATE OF e`, tenant, sourceName).Scan(&sourceID, &sourceModelID, &sourcePlanID, &sourcePolicy); errors.Is(err, sql.ErrNoRows) {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: source endpoint", ErrNotFound)
	} else if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	if sourcePlanID == "" {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: source endpoint has no active serving plan", ErrConflict)
	}
	var destinationID, destinationModelID, destinationActive, destinationCandidate string
	if err = tx.QueryRowContext(ctx, `SELECT id,logical_model_id,COALESCE(active_serving_plan_id,''),COALESCE(candidate_serving_plan_id,'') FROM endpoints WHERE tenant_id=? AND name=? AND desired_state='serving' FOR UPDATE`, tenant, destinationName).Scan(&destinationID, &destinationModelID, &destinationActive, &destinationCandidate); errors.Is(err, sql.ErrNoRows) {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: destination endpoint", ErrNotFound)
	} else if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	if sourceModelID != destinationModelID {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: endpoints belong to different logical models", ErrConflict)
	}
	if destinationActive == "" {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: destination has no active plan; initialize it explicitly before environment promotion", ErrConflict)
	}
	if destinationCandidate != "" {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: destination already has a candidate plan", ErrConflict)
	}
	type sourceBinding struct {
		Name, Kind, Ownership, DeploymentID, TargetID, Config string
		Priority, Weight                                      int
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.name,b.kind,b.ownership_mode,COALESCE(b.deployment_id,''),COALESCE(b.target_id,''),b.config_json::text,pb.priority,pb.weight FROM serving_plan_bindings pb JOIN backend_bindings b ON b.id=pb.binding_id WHERE pb.serving_plan_id=? ORDER BY pb.priority`, sourcePlanID)
	if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	bindings := make([]sourceBinding, 0)
	for rows.Next() {
		var binding sourceBinding
		if err = rows.Scan(&binding.Name, &binding.Kind, &binding.Ownership, &binding.DeploymentID, &binding.TargetID, &binding.Config, &binding.Priority, &binding.Weight); err != nil {
			_ = rows.Close()
			return domain.EnvironmentPromotion{}, false, err
		}
		bindings = append(bindings, binding)
	}
	if err = rows.Close(); err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	if len(bindings) == 0 {
		return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: source serving plan has no bindings", ErrConflict)
	}
	destinationBindings := make([]domain.ServingPlanBinding, 0, len(bindings))
	for _, source := range bindings {
		// Bind the destination snapshot to the immutable source plan. Reusing a
		// destination binding across source plans would let a later mutable
		// binding field (for example adoption ownership) alter historical plans.
		bindingDigest := sha256.Sum256([]byte(sourcePlanID + "\x00" + source.Name))
		bindingName := "promoted-" + source.Name
		if len(bindingName) > 53 {
			bindingName = bindingName[:53]
		}
		bindingName += "-" + hex.EncodeToString(bindingDigest[:4])
		var bindingID, kind, ownership, deploymentID, targetID, configJSON string
		lookupErr := tx.QueryRowContext(ctx, `SELECT id,kind,ownership_mode,COALESCE(deployment_id,''),COALESCE(target_id,''),config_json::text FROM backend_bindings WHERE endpoint_id=? AND name=?`, destinationID, bindingName).Scan(&bindingID, &kind, &ownership, &deploymentID, &targetID, &configJSON)
		if lookupErr == nil {
			if kind != source.Kind || ownership != source.Ownership || deploymentID != source.DeploymentID || targetID != source.TargetID || configJSON != source.Config {
				return domain.EnvironmentPromotion{}, false, fmt.Errorf("%w: destination promoted binding %q differs from source", ErrConflict, bindingName)
			}
		} else if errors.Is(lookupErr, sql.ErrNoRows) {
			bindingID, err = newID()
			if err != nil {
				return domain.EnvironmentPromotion{}, false, err
			}
			stamp := now()
			if _, err = tx.ExecContext(ctx, `INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,deployment_id,target_id,config_json,created_at,updated_at) VALUES(?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),?::jsonb,?,?)`, bindingID, tenant, destinationID, bindingName, source.Kind, source.Ownership, source.DeploymentID, source.TargetID, source.Config, stamp, stamp); err != nil {
				return domain.EnvironmentPromotion{}, false, err
			}
		} else {
			return domain.EnvironmentPromotion{}, false, lookupErr
		}
		destinationBindings = append(destinationBindings, domain.ServingPlanBinding{BindingID: bindingID, Priority: source.Priority, Weight: source.Weight})
	}
	_, specBody, err := normalizePlan(sourcePolicy, destinationBindings)
	if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	digest := sha256.Sum256(specBody)
	planDigest := "sha256:" + hex.EncodeToString(digest[:])
	var destinationPlanID string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM serving_plans WHERE endpoint_id=? AND spec_digest=?`, destinationID, planDigest).Scan(&destinationPlanID); errors.Is(err, sql.ErrNoRows) {
		destinationPlanID, err = newID()
		if err != nil {
			return domain.EnvironmentPromotion{}, false, err
		}
		var version int
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM serving_plans WHERE endpoint_id=?`, destinationID).Scan(&version); err != nil {
			return domain.EnvironmentPromotion{}, false, err
		}
		stamp := now()
		if _, err = tx.ExecContext(ctx, `INSERT INTO serving_plans(id,tenant_id,endpoint_id,version,routing_policy,spec_json,spec_digest,created_at) VALUES(?,?,?,?,?,?::jsonb,?,?)`, destinationPlanID, tenant, destinationID, version, sourcePolicy, string(specBody), planDigest, stamp); err != nil {
			return domain.EnvironmentPromotion{}, false, err
		}
		for _, binding := range destinationBindings {
			if _, err = tx.ExecContext(ctx, `INSERT INTO serving_plan_bindings(serving_plan_id,binding_id,priority,weight) VALUES(?,?,?,?)`, destinationPlanID, binding.BindingID, binding.Priority, binding.Weight); err != nil {
				return domain.EnvironmentPromotion{}, false, err
			}
		}
	} else if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	promotionID, err := newID()
	if err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO environment_promotions(id,tenant_id,source_endpoint_id,source_plan_id,destination_endpoint_id,destination_plan_id,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?)`, promotionID, tenant, sourceID, sourcePlanID, destinationID, destinationPlanID, idempotencyKey, stamp); err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE endpoints SET candidate_serving_plan_id=?,updated_at=? WHERE id=? AND candidate_serving_plan_id IS NULL`, destinationPlanID, stamp, destinationID); err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.EnvironmentPromotion{}, false, err
	}
	return domain.EnvironmentPromotion{ID: promotionID, TenantID: tenant, SourceEndpointID: sourceID, SourcePlanID: sourcePlanID, DestinationEndpointID: destinationID, DestinationPlanID: destinationPlanID, IdempotencyKey: idempotencyKey, CreatedAt: parseTime(stamp)}, true, nil
}

func (s *Store) SetEndpointState(ctx context.Context, tenant, id, state string) error {
	if state != "pending" && state != "serving" && state != "degraded" && state != "suspended" && state != "deleted" {
		return errors.New("invalid endpoint observed state")
	}
	result, err := s.ExecContext(ctx, `UPDATE endpoints SET observed_state=?,updated_at=? WHERE id=? AND tenant_id=?`, state, now(), id, tenant)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteEndpointForTenant(ctx context.Context, tenant, name string) error {
	result, err := s.ExecContext(ctx, `UPDATE endpoints SET desired_state='deleted',observed_state='deleted',updated_at=? WHERE tenant_id=? AND name=? AND desired_state<>'deleted'`, now(), tenant, name)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) EndpointsForTenant(ctx context.Context, tenant string) ([]domain.Endpoint, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,COALESCE(active_serving_plan_id,''),COALESCE(candidate_serving_plan_id,''),created_at,updated_at FROM endpoints WHERE tenant_id=? AND desired_state<>'deleted' ORDER BY name`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Endpoint
	for rows.Next() {
		item, scanErr := scanEndpoint(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) TenantsWithEndpoints(ctx context.Context) ([]string, error) {
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT tenant_id FROM endpoints WHERE desired_state<>'deleted' ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tenant string
		if err = rows.Scan(&tenant); err != nil {
			return nil, err
		}
		out = append(out, tenant)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanEndpoint(row rowScanner) (domain.Endpoint, error) {
	var item domain.Endpoint
	var created, updated string
	err := row.Scan(&item.ID, &item.TenantID, &item.LogicalModelID, &item.EnvironmentID, &item.Name, &item.DesiredState, &item.ObservedState, &item.ActiveServingPlanID, &item.CandidateServingPlanID, &created, &updated)
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return item, err
}

func (s *Store) ResolveEndpointForTenant(ctx context.Context, tenant, name string) (domain.ResolvedEndpoint, error) {
	var out domain.ResolvedEndpoint
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT e.id,e.tenant_id,e.logical_model_id,e.environment_id,e.name,e.desired_state,e.observed_state,COALESCE(e.active_serving_plan_id,''),COALESCE(e.candidate_serving_plan_id,''),e.created_at,e.updated_at,m.id,m.tenant_id,m.name,m.description,m.created_at,m.updated_at,v.id,v.tenant_id,v.name,v.policy_json::text,v.created_at,v.updated_at FROM endpoints e JOIN logical_models m ON m.id=e.logical_model_id JOIN environments v ON v.id=e.environment_id WHERE e.tenant_id=? AND e.name=? AND e.desired_state<>'deleted'`, tenant, name).Scan(&out.Endpoint.ID, &out.Endpoint.TenantID, &out.Endpoint.LogicalModelID, &out.Endpoint.EnvironmentID, &out.Endpoint.Name, &out.Endpoint.DesiredState, &out.Endpoint.ObservedState, &out.Endpoint.ActiveServingPlanID, &out.Endpoint.CandidateServingPlanID, &created, &updated, &out.LogicalModel.ID, &out.LogicalModel.TenantID, &out.LogicalModel.Name, &out.LogicalModel.Description, &out.LogicalModel.CreatedAt, &out.LogicalModel.UpdatedAt, &out.Environment.ID, &out.Environment.TenantID, &out.Environment.Name, &out.Environment.PolicyJSON, &out.Environment.CreatedAt, &out.Environment.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.Endpoint.CreatedAt, out.Endpoint.UpdatedAt = parseTime(created), parseTime(updated)
	plans, err := s.plansForEndpoint(ctx, tenant, out.Endpoint.ID)
	if err != nil {
		return out, err
	}
	for i := range plans {
		if plans[i].ID == out.Endpoint.ActiveServingPlanID {
			out.ActivePlan = plans[i]
		}
		if plans[i].ID == out.Endpoint.CandidateServingPlanID {
			candidate := plans[i]
			out.CandidatePlan = &candidate
		}
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,endpoint_id,name,kind,ownership_mode,COALESCE(deployment_id,''),COALESCE(target_id,''),config_json::text,created_at,updated_at FROM backend_bindings WHERE tenant_id=? AND endpoint_id=? ORDER BY name`, tenant, out.Endpoint.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item domain.BackendBinding
		var c, u string
		if err = rows.Scan(&item.ID, &item.TenantID, &item.EndpointID, &item.Name, &item.Kind, &item.OwnershipMode, &item.DeploymentID, &item.TargetID, &item.ConfigJSON, &c, &u); err != nil {
			return out, err
		}
		item.CreatedAt, item.UpdatedAt = parseTime(c), parseTime(u)
		out.Bindings = append(out.Bindings, item)
	}
	return out, rows.Err()
}

func (s *Store) plansForEndpoint(ctx context.Context, tenant, endpointID string) ([]domain.ServingPlan, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,endpoint_id,version,routing_policy,spec_json::text,spec_digest,created_at FROM serving_plans WHERE tenant_id=? AND endpoint_id=? ORDER BY version`, tenant, endpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ServingPlan
	for rows.Next() {
		var p domain.ServingPlan
		var c string
		if err = rows.Scan(&p.ID, &p.TenantID, &p.EndpointID, &p.Version, &p.RoutingPolicy, &p.SpecJSON, &p.SpecDigest, &c); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTime(c)
		bindingRows, e := s.QueryContext(ctx, `SELECT binding_id,priority,weight FROM serving_plan_bindings WHERE serving_plan_id=? ORDER BY priority`, p.ID)
		if e != nil {
			return nil, e
		}
		for bindingRows.Next() {
			var b domain.ServingPlanBinding
			if e = bindingRows.Scan(&b.BindingID, &b.Priority, &b.Weight); e != nil {
				bindingRows.Close()
				return nil, e
			}
			p.Bindings = append(p.Bindings, b)
		}
		if e = bindingRows.Close(); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
