package external

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/openaicompat"
	"github.com/infercrane/infercrane/internal/overflow"
	"github.com/infercrane/infercrane/internal/routes"
	"github.com/infercrane/infercrane/internal/secrets"
)

type Store interface {
	ExternalTargetPolicyForDeployment(context.Context, string, string) (domain.ExternalTargetPolicy, error)
	SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error)
	LeaseExternalBudget(context.Context, string, string, int64) (domain.ExternalBudgetLease, error)
}

type OverflowStore interface {
	EvaluateOverflow(context.Context, string, string, overflow.Signal, bool, time.Time) (overflow.Decision, error)
}

type BindingStore interface {
	ManagedExternalBindingPolicy(context.Context, string, string) (domain.ManagedExternalBindingPolicy, error)
	LeaseManagedExternalBindingBudget(context.Context, string, string, int64) (domain.ExternalBudgetLease, error)
}

type Coordinator struct {
	Store      Store
	Secrets    secrets.Resolver
	Budgets    *BudgetPool
	Client     *http.Client
	Adapters   map[string]struct{}
	LeaseBatch int64

	HealthCacheTTL time.Duration
	healthMu       sync.Mutex
	healthUntil    map[[32]byte]time.Time
}

func (c *Coordinator) Owns(provider string) bool {
	if c == nil {
		return false
	}
	_, ok := c.adapters()[provider]
	return ok
}

func (c *Coordinator) Resolve(ctx context.Context, deployment domain.Deployment, targets []domain.Target) (routes.Snapshot, error) {
	if c == nil || c.Store == nil || c.Secrets == nil || c.Budgets == nil {
		return routes.Snapshot{}, errors.New("external fallback coordinator is not configured")
	}
	policy, err := c.Store.ExternalTargetPolicyForDeployment(ctx, deployment.TenantID, deployment.ID)
	if err != nil {
		return routes.Snapshot{}, err
	}
	if !policy.Enabled || !policy.PrivacyAcknowledged || !c.Owns(policy.Adapter) {
		return routes.Snapshot{}, errors.New("external fallback policy is disabled, unacknowledged, or unsupported")
	}
	var target domain.Target
	for _, candidate := range targets {
		if candidate.ID == policy.TargetID && candidate.Provider == policy.Adapter {
			target = candidate
			break
		}
	}
	if target.ID == "" {
		return routes.Snapshot{}, errors.New("external fallback target is not attached to the deployment")
	}
	reference, err := c.Store.SecretReferenceForTenant(ctx, deployment.TenantID, policy.SecretReferenceID)
	if err != nil {
		return routes.Snapshot{}, fmt.Errorf("load external secret reference: %w", err)
	}
	apiKey, err := c.Secrets.Resolve(ctx, reference)
	if err != nil {
		return routes.Snapshot{}, fmt.Errorf("resolve external credential: %w", err)
	}
	if err := c.healthy(ctx, target, apiKey); err != nil {
		return routes.Snapshot{}, err
	}
	batch := c.LeaseBatch
	if batch < 1 || batch > 256 {
		batch = 256
	}
	c.Budgets.RegisterRefill(policy.ID, batch/2, func(refillCtx context.Context) (domain.ExternalBudgetLease, error) {
		return c.Store.LeaseExternalBudget(refillCtx, deployment.TenantID, policy.ID, batch)
	})
	if c.Budgets.Remaining(policy.ID) == 0 {
		lease, leaseErr := c.Store.LeaseExternalBudget(ctx, deployment.TenantID, policy.ID, batch)
		if leaseErr != nil {
			return routes.Snapshot{}, leaseErr
		}
		if err := c.Budgets.Add(lease); err != nil {
			return routes.Snapshot{}, err
		}
	}
	upstreamModel := target.UpstreamModel
	if upstreamModel == "" {
		upstreamModel = deployment.Model
	}
	return routes.Snapshot{
		DeploymentID: deployment.ID, TargetID: target.ID, RevisionID: deployment.ActiveRevisionID, TenantID: deployment.TenantID,
		Alias: deployment.Name, UpstreamModel: upstreamModel, RouterURL: target.URL, Provider: policy.Adapter,
		ProviderResourceID: target.ProviderResourceID, Runtime: target.Runtime, ComputeMode: "external",
		UpstreamAPIKey: apiKey, ExternalPolicyID: policy.ID,
		SelectionReason: "all primary targets are unhealthy; explicit external fallback policy selected",
	}, nil
}

// ResolveBinding compiles an authenticated external model API into a stable
// endpoint route. The returned credential exists only in the in-memory route
// snapshot; persisted binding configuration contains a secret reference.
func (c *Coordinator) ResolveBinding(ctx context.Context, endpoint domain.Endpoint, binding domain.BackendBinding, target domain.Target) (routes.Snapshot, error) {
	store, ok := c.Store.(BindingStore)
	if !ok || c.Secrets == nil || c.Budgets == nil {
		return routes.Snapshot{}, errors.New("managed external binding coordinator is not configured")
	}
	policy, err := store.ManagedExternalBindingPolicy(ctx, endpoint.TenantID, binding.ID)
	if err != nil {
		return routes.Snapshot{}, err
	}
	if !policy.Enabled || !policy.PrivacyAcknowledged || !c.Owns(policy.Adapter) {
		return routes.Snapshot{}, errors.New("managed external binding is disabled, unacknowledged, or unsupported")
	}
	if policy.TargetID != target.ID || policy.Adapter != target.Provider {
		return routes.Snapshot{}, errors.New("managed external binding target does not match its immutable policy")
	}
	reference, err := c.Store.SecretReferenceForTenant(ctx, endpoint.TenantID, policy.SecretReferenceID)
	if err != nil {
		return routes.Snapshot{}, fmt.Errorf("load managed external secret reference: %w", err)
	}
	apiKey, err := c.Secrets.Resolve(ctx, reference)
	if err != nil {
		return routes.Snapshot{}, fmt.Errorf("resolve managed external credential: %w", err)
	}
	if err := c.healthy(ctx, target, apiKey); err != nil {
		return routes.Snapshot{}, err
	}
	batch := c.LeaseBatch
	if batch < 1 || batch > 256 {
		batch = 256
	}
	c.Budgets.RegisterRefill(binding.ID, batch/2, func(refillCtx context.Context) (domain.ExternalBudgetLease, error) {
		return store.LeaseManagedExternalBindingBudget(refillCtx, endpoint.TenantID, binding.ID, batch)
	})
	if c.Budgets.Remaining(binding.ID) == 0 {
		lease, leaseErr := store.LeaseManagedExternalBindingBudget(ctx, endpoint.TenantID, binding.ID, batch)
		if leaseErr != nil {
			return routes.Snapshot{}, leaseErr
		}
		if err := c.Budgets.Add(lease); err != nil {
			return routes.Snapshot{}, err
		}
	}
	return routes.Snapshot{
		TenantID: endpoint.TenantID, Alias: endpoint.Name, TargetID: target.ID,
		UpstreamModel: target.UpstreamModel, RouterURL: target.URL, Provider: policy.Adapter,
		ProviderResourceID: target.ProviderResourceID, Runtime: target.Runtime, ComputeMode: "external",
		UpstreamAPIKey: apiKey, ExternalPolicyID: binding.ID,
		SelectionReason: "explicit authenticated external endpoint binding selected",
	}, nil
}

func (c *Coordinator) ResolveHybrid(ctx context.Context, deployment domain.Deployment, targets []domain.Target, signal overflow.Signal, now time.Time) (routes.Snapshot, overflow.Decision, error) {
	store, ok := c.Store.(OverflowStore)
	if !ok {
		return routes.Snapshot{}, overflow.Decision{}, errors.New("hybrid overflow persistence is unavailable")
	}
	policy, err := c.Store.ExternalTargetPolicyForDeployment(ctx, deployment.TenantID, deployment.ID)
	if err != nil {
		return routes.Snapshot{}, overflow.Decision{}, err
	}
	budgetAvailable := c.Budgets != nil && c.Budgets.Remaining(policy.ID) > 0 || policy.RequestsReserved < policy.RequestLimit && policy.CostReservedMicrousd < policy.CostLimitMicrousd
	decision, err := store.EvaluateOverflow(ctx, deployment.TenantID, deployment.ID, signal, budgetAvailable, now)
	if err != nil {
		return routes.Snapshot{}, decision, err
	}
	if decision.Route != "external" {
		return routes.Snapshot{}, decision, nil
	}
	route, err := c.Resolve(ctx, deployment, targets)
	if err != nil {
		return routes.Snapshot{}, decision, err
	}
	route.SelectionReason = decision.Reason
	return route, decision, nil
}

func (c *Coordinator) OverflowMode(ctx context.Context, deployment domain.Deployment) (string, error) {
	policy, err := c.Store.ExternalTargetPolicyForDeployment(ctx, deployment.TenantID, deployment.ID)
	if err != nil {
		return "", err
	}
	if !policy.Enabled {
		return "", errors.New("external fallback policy is disabled")
	}
	return policy.OverflowMode, nil
}

func (c *Coordinator) healthy(ctx context.Context, target domain.Target, apiKey string) error {
	cacheKey := sha256.Sum256([]byte(target.ID + "\x00" + target.URL + "\x00" + target.UpstreamModel + "\x00" + apiKey))
	now := time.Now()
	c.healthMu.Lock()
	if until := c.healthUntil[cacheKey]; until.After(now) {
		c.healthMu.Unlock()
		return nil
	}
	c.healthMu.Unlock()

	endpoint, err := openaicompat.Endpoint(target.URL, "models")
	if err != nil {
		return errors.New("external target URL is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("external health request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("external target health check failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("external target health check returned status %d", response.StatusCode)
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&models); err != nil {
		return errors.New("external target returned an invalid model inventory")
	}
	expected := target.UpstreamModel
	if expected == "" {
		return errors.New("external target requires an explicit upstream model mapping")
	}
	for _, model := range models.Data {
		if model.ID == expected {
			ttl := c.HealthCacheTTL
			if ttl <= 0 {
				ttl = 30 * time.Second
			}
			c.healthMu.Lock()
			if c.healthUntil == nil {
				c.healthUntil = make(map[[32]byte]time.Time)
			}
			if len(c.healthUntil) >= 4096 {
				for key, until := range c.healthUntil {
					if !until.After(now) {
						delete(c.healthUntil, key)
					}
				}
			}
			if len(c.healthUntil) >= 4096 {
				c.healthMu.Unlock()
				return nil
			}
			c.healthUntil[cacheKey] = now.Add(ttl)
			c.healthMu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("external target does not expose configured model %q", expected)
}

func (c *Coordinator) adapters() map[string]struct{} {
	if len(c.Adapters) > 0 {
		return c.Adapters
	}
	return map[string]struct{}{"openai-compatible-external": {}, "openrouter": {}}
}
