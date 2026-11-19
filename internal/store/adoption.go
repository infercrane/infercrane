package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

// AdoptEndpoint atomically registers an existing workload and its stable
// identity. It never creates a Deployment and therefore never claims provider
// lifecycle ownership.
func (s *Store) AdoptEndpoint(ctx context.Context, tenant, name, modelName, upstreamModel, rawURL, source, ownership, runtimeName string) (domain.ResolvedEndpoint, domain.AdoptedWorkload, error) {
	return s.AdoptEndpointWithCredential(ctx, tenant, name, modelName, upstreamModel, rawURL, source, ownership, runtimeName, nil)
}

// AdoptEndpointWithCredential adopts like AdoptEndpoint and stores an optional
// sealed bearer credential for the server. Only ciphertext reaches storage. A
// repeated adoption with the same identity replaces the stored credential.
func (s *Store) AdoptEndpointWithCredential(ctx context.Context, tenant, name, modelName, upstreamModel, rawURL, source, ownership, runtimeName string, credential *domain.SealedCredential) (domain.ResolvedEndpoint, domain.AdoptedWorkload, error) {
	if tenant == "" || !validEndpointName(name) || !validEndpointName(modelName) {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, errors.New("tenant, endpoint name, and logical model are required")
	}
	if ownership != "observe-only" && ownership != "traffic-managed" {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, errors.New("adoption ownership must be observe-only or traffic-managed")
	}
	if source != "openai-compatible" && source != "vllm" {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, errors.New("adoption source must be openai-compatible or vllm")
	}
	if runtimeName == "" {
		runtimeName = "openai-compatible"
	}
	if upstreamModel == "" {
		upstreamModel = modelName
	}
	normalizedURL, err := normalizeAdoptionURL(rawURL)
	if err != nil {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
	}
	digest := sha256.Sum256([]byte(source + "\x00" + normalizedURL + "\x00" + modelName + "\x00" + upstreamModel))
	identity := "sha256:" + hex.EncodeToString(digest[:])

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
	}
	defer tx.Rollback()

	var existing domain.AdoptedWorkload
	var existingURL, existingModel string
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.tenant_id,a.endpoint_id,a.binding_id,a.target_id,a.ownership_mode,a.source,a.immutable_identity,a.created_at,a.updated_at,t.url,t.upstream_model_name FROM adopted_workloads a JOIN endpoints e ON e.id=a.endpoint_id JOIN targets t ON t.id=a.target_id WHERE a.tenant_id=? AND e.name=? AND e.desired_state<>'deleted'`, tenant, name).Scan(&existing.ID, &existing.TenantID, &existing.EndpointID, &existing.BindingID, &existing.TargetID, &existing.OwnershipMode, &existing.Source, &existing.ImmutableIdentity, &existing.CreatedAt, &existing.UpdatedAt, &existingURL, &existingModel)
	if err == nil {
		if existing.OwnershipMode != ownership || existing.Source != source || existing.ImmutableIdentity != identity || existingURL != normalizedURL || existingModel != upstreamModel {
			return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, fmt.Errorf("%w: endpoint is already adopted with different immutable identity or ownership", ErrConflict)
		}
		if credential != nil {
			if _, err = tx.ExecContext(ctx, `UPDATE adopted_workloads SET upstream_credential_ciphertext=?,upstream_credential_nonce=?,updated_at=? WHERE id=? AND tenant_id=?`, credential.Ciphertext, credential.Nonce, now(), existing.ID, tenant); err != nil {
				return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
			}
		}
		if err = tx.Commit(); err != nil {
			return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
		}
		resolved, resolveErr := s.ResolveEndpointForTenant(ctx, tenant, name)
		return resolved, existing, resolveErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
	}

	targetID, err := newID()
	if err != nil || targetID == "" {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, errors.New("generate adoption identities")
	}
	stamp := now()
	targetName := name + "-adopted"
	var registeredID, registeredRuntime, registeredModel string
	targetErr := tx.QueryRowContext(ctx, `SELECT id,runtime,COALESCE(upstream_model_name,'') FROM targets WHERE tenant_id=? AND url=? AND agent_id IS NULL`, tenant, normalizedURL).Scan(&registeredID, &registeredRuntime, &registeredModel)
	if targetErr == nil {
		if registeredRuntime != runtimeName || (registeredModel != "" && registeredModel != upstreamModel) {
			return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, fmt.Errorf("%w: target URL is registered with incompatible runtime or model", ErrConflict)
		}
		targetID = registeredID
		if registeredModel == "" {
			if _, err = tx.ExecContext(ctx, `UPDATE targets SET upstream_model_name=?,updated_at=? WHERE id=? AND tenant_id=? AND upstream_model_name IS NULL`, upstreamModel, stamp, targetID, tenant); err != nil {
				return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
			}
		}
	} else if errors.Is(targetErr, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO targets(id,name,url,provider,runtime,upstream_model_name,health,provider_details_json,tenant_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'{}'::jsonb,?,?,?)`, targetID, targetName, normalizedURL, "external", runtimeName, upstreamModel, "starting", tenant, stamp, stamp); err != nil {
			if isUniqueViolation(err) {
				return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, fmt.Errorf("%w: target URL or generated name already registered", ErrConflict)
			}
			return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
		}
	} else {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, targetErr
	}
	adoption, err := insertAdoptedEndpointTx(ctx, tx, tenant, name, modelName, targetID, ownership, source, identity, stamp)
	if err != nil {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
	}
	if credential != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE adopted_workloads SET upstream_credential_ciphertext=?,upstream_credential_nonce=? WHERE id=? AND tenant_id=?`, credential.Ciphertext, credential.Nonce, adoption.ID, tenant); err != nil {
			return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.ResolvedEndpoint{}, domain.AdoptedWorkload{}, err
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, name)
	return resolved, adoption, err
}

// insertAdoptedEndpointTx creates the endpoint, its single binding to an
// existing target, a manual serving plan, and the adoption record. It never
// creates a Deployment, so no provider lifecycle is claimed.
func insertAdoptedEndpointTx(ctx context.Context, tx *tx, tenant, name, modelName, targetID, ownership, source, identity, stamp string) (domain.AdoptedWorkload, error) {
	var environmentID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM environments WHERE tenant_id=? AND name='production'`, tenant).Scan(&environmentID)
	if errors.Is(err, sql.ErrNoRows) {
		environmentID, err = newID()
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO environments(id,tenant_id,name,policy_json,created_at,updated_at) VALUES(?,?,'production','{}'::jsonb,?,?) ON CONFLICT(tenant_id,name) DO NOTHING`, environmentID, tenant, stamp, stamp)
		}
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT id FROM environments WHERE tenant_id=? AND name='production'`, tenant).Scan(&environmentID)
		}
	}
	if err != nil {
		return domain.AdoptedWorkload{}, fmt.Errorf("production environment: %w", err)
	}
	var modelID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM logical_models WHERE tenant_id=? AND name=?`, tenant, modelName).Scan(&modelID)
	if errors.Is(err, sql.ErrNoRows) {
		modelID, err = newID()
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO logical_models(id,tenant_id,name,description,created_at,updated_at) VALUES(?,?,?,?,?,?)`, modelID, tenant, modelName, "Adopted workload identity", stamp, stamp)
		}
	}
	if err != nil {
		return domain.AdoptedWorkload{}, err
	}
	endpointID, _ := newID()
	bindingID, _ := newID()
	planID, _ := newID()
	adoptionID, _ := newID()
	if endpointID == "" || bindingID == "" || planID == "" || adoptionID == "" {
		return domain.AdoptedWorkload{}, errors.New("generate adoption identities")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO endpoints(id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,created_at,updated_at) VALUES(?,?,?,?,?,'serving','pending',?,?)`, endpointID, tenant, modelID, environmentID, name, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.AdoptedWorkload{}, fmt.Errorf("%w: endpoint %s already exists", ErrConflict, name)
		}
		return domain.AdoptedWorkload{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,target_id,config_json,created_at,updated_at) VALUES(?,?,?,?,'external',?,?,'{}'::jsonb,?,?)`, bindingID, tenant, endpointID, "primary", ownership, targetID, stamp, stamp); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	planJSON := fmt.Sprintf(`{"routing_policy":"manual","bindings":[{"binding_id":"%s","priority":0,"weight":100}]}`, bindingID)
	planDigest := sha256.Sum256([]byte(planJSON))
	if _, err = tx.ExecContext(ctx, `INSERT INTO serving_plans(id,tenant_id,endpoint_id,version,routing_policy,spec_json,spec_digest,created_at) VALUES(?,?,?,1,'manual',?::jsonb,?,?)`, planID, tenant, endpointID, planJSON, "sha256:"+hex.EncodeToString(planDigest[:]), stamp); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO serving_plan_bindings(serving_plan_id,binding_id,priority,weight) VALUES(?,?,0,100)`, planID, bindingID); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE endpoints SET active_serving_plan_id=? WHERE id=?`, planID, endpointID); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO adopted_workloads(id,tenant_id,endpoint_id,binding_id,target_id,ownership_mode,source,immutable_identity,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, adoptionID, tenant, endpointID, bindingID, targetID, ownership, source, identity, stamp, stamp); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	return domain.AdoptedWorkload{ID: adoptionID, TenantID: tenant, EndpointID: endpointID, BindingID: bindingID, TargetID: targetID, OwnershipMode: ownership, Source: source, ImmutableIdentity: identity, CreatedAt: parseTime(stamp), UpdatedAt: parseTime(stamp)}, nil
}

func normalizeAdoptionURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("adoption URL must be an absolute http(s) URL without credentials or fragments")
	}
	// Users commonly copy the OpenAI SDK base ending in /v1. InferCrane stores
	// the service root because readiness lives at /health while request paths
	// are composed through openaicompat.Endpoint without duplicating /v1.
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		parsed.Path = strings.TrimSuffix(path, "/v1")
	}
	return NormalizeURL(parsed.String()), nil
}

// PromoteAdoptionOwnership changes who serves an adopted endpoint's traffic.
// A directly reached server may become traffic-managed (the control plane
// routes to it). A server only an InferCrane agent reaches may become
// agent-managed (the agent serves its traffic inside the customer's cloud)
// and return to observe-only; it can never become traffic-managed, because
// the control plane cannot reach it. Promotion to agent-managed also sets
// the endpoint's residency to customer-cloud in the same transaction.
func (s *Store) PromoteAdoptionOwnership(ctx context.Context, tenant, name, ownership string) (domain.AdoptedWorkload, error) {
	if ownership != "traffic-managed" && ownership != "agent-managed" && ownership != "observe-only" {
		return domain.AdoptedWorkload{}, errors.New("adoption ownership can be changed only to traffic-managed, agent-managed, or observe-only")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.AdoptedWorkload{}, err
	}
	defer tx.Rollback()
	var item domain.AdoptedWorkload
	err = tx.QueryRowContext(ctx, `SELECT a.id,a.tenant_id,a.endpoint_id,a.binding_id,a.target_id,a.ownership_mode,a.source,a.immutable_identity,a.created_at,a.updated_at FROM adopted_workloads a JOIN endpoints e ON e.id=a.endpoint_id WHERE a.tenant_id=? AND e.name=? AND e.desired_state<>'deleted' FOR UPDATE`, tenant, name).Scan(&item.ID, &item.TenantID, &item.EndpointID, &item.BindingID, &item.TargetID, &item.OwnershipMode, &item.Source, &item.ImmutableIdentity, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdoptedWorkload{}, ErrNotFound
	}
	if err != nil {
		return domain.AdoptedWorkload{}, err
	}
	if item.OwnershipMode == ownership {
		if err = tx.Commit(); err != nil {
			return domain.AdoptedWorkload{}, err
		}
		return item, nil
	}
	var throughAgent bool
	if err = tx.QueryRowContext(ctx, `SELECT agent_id IS NOT NULL FROM targets WHERE id=? AND tenant_id=?`, item.TargetID, tenant).Scan(&throughAgent); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	switch {
	case ownership == "traffic-managed" && throughAgent:
		// The control plane cannot reach a server that only an agent sees, so
		// it cannot route its traffic. The agent serves it instead.
		return domain.AdoptedWorkload{}, fmt.Errorf("%w: this server is reached only through an InferCrane agent; make it agent-managed so the agent serves its traffic inside your cloud", ErrConflict)
	case ownership == "traffic-managed" && item.OwnershipMode != "observe-only":
		return domain.AdoptedWorkload{}, fmt.Errorf("%w: unsupported adoption ownership transition", ErrConflict)
	case ownership == "agent-managed" && !throughAgent:
		return domain.AdoptedWorkload{}, fmt.Errorf("%w: agent-managed needs a server an InferCrane agent owns; run infercrane agent run beside it", ErrConflict)
	case ownership == "observe-only" && item.OwnershipMode != "agent-managed":
		return domain.AdoptedWorkload{}, fmt.Errorf("%w: unsupported adoption ownership transition", ErrConflict)
	}
	stamp := now()
	if ownership == "agent-managed" {
		var endpointID string
		if err = tx.QueryRowContext(ctx, `SELECT id FROM endpoints WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, item.EndpointID).Scan(&endpointID); err != nil {
			return domain.AdoptedWorkload{}, err
		}
		if err = setEndpointResidencyTx(ctx, tx, tenant, endpointID, ResidencyCustomerCloud, stamp); err != nil {
			return domain.AdoptedWorkload{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE adopted_workloads SET ownership_mode=?,updated_at=? WHERE id=? AND tenant_id=?`, ownership, stamp, item.ID, tenant); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE backend_bindings SET ownership_mode=?,updated_at=? WHERE id=? AND tenant_id=?`, ownership, stamp, item.BindingID, tenant); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.AdoptedWorkload{}, err
	}
	item.OwnershipMode, item.UpdatedAt = ownership, parseTime(stamp)
	return item, nil
}
