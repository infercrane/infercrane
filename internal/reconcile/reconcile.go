package reconcile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/overflow"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type Store interface {
	Deployments(context.Context) ([]domain.Deployment, error)
	Resolve(context.Context, string) (domain.ResolvedDeployment, error)
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	SetTargetHealth(context.Context, string, string) error
	SetDeploymentState(context.Context, string, string) error
	Event(context.Context, string, string, string, string, string) error
	ActiveGeneration(context.Context, string, string) (domain.RouterGeneration, error)
	RecordGeneration(context.Context, domain.RouterGeneration) (domain.RouterGeneration, error)
}
type ServerlessStatus interface {
	EndpointHealth(context.Context, string) (provision.ServerlessHealth, error)
}
type ExternalFallback interface {
	Owns(string) bool
	Resolve(context.Context, domain.Deployment, []domain.Target) (routes.Snapshot, error)
	ResolveBinding(context.Context, domain.Endpoint, domain.BackendBinding, domain.Target) (routes.Snapshot, error)
}
type HybridFallback interface {
	OverflowMode(context.Context, domain.Deployment) (string, error)
	ResolveHybrid(context.Context, domain.Deployment, []domain.Target, overflow.Signal, time.Time) (routes.Snapshot, overflow.Decision, error)
}
type QueueSignals interface {
	Waiting(context.Context, string) (float64, error)
}

type endpointStore interface {
	TenantsWithEndpoints(context.Context) ([]string, error)
	EndpointsForTenant(context.Context, string) ([]domain.Endpoint, error)
	ResolveEndpointForTenant(context.Context, string, string) (domain.ResolvedEndpoint, error)
	TargetForTenantByID(context.Context, string, string) (domain.Target, error)
	SetEndpointState(context.Context, string, string, string) error
}

// DirectTargetBackend describes a provider-native endpoint that bypasses the
// standalone replica router. The reconciler depends on this metadata rather
// than a RunPod-specific branch, so another serverless adapter can register the
// same lifecycle contract without changing reconciliation.
type DirectTargetBackend struct {
	Provider string
	APIKey   string
	Status   ServerlessStatus
}

type Reconciler struct {
	Store  Store
	Routes *routes.Directory
	Router router.Backend
	// RouterAPIKey authenticates gateway requests to the internal replica
	// router. It is kept only in the in-memory route snapshot and is never
	// persisted as deployment or provider state.
	RouterAPIKey     string
	Runtimes         integration.RuntimeBackends
	Interval         time.Duration
	RouterStartPort  int
	InstanceID       string
	Logger           *slog.Logger
	DirectTargets    map[string]DirectTargetBackend
	ExternalFallback ExternalFallback
	QueueSignals     QueueSignals
}

func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		if err := r.Once(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if r.Logger != nil {
				r.Logger.Error("reconciliation failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (r *Reconciler) Once(ctx context.Context) error {
	var failures []error
	r.reapRetiredRoutes()
	deployments, err := r.Store.Deployments(ctx)
	if err != nil {
		return err
	}
	active := make(map[string]struct{}, len(deployments))
	for _, d := range deployments {
		if d.DesiredState == "deleted" {
			continue
		}
		active[d.TenantID+"\x00"+d.Name] = struct{}{}
	}
	for _, stale := range r.Routes.ConcreteRoutes() {
		if _, ok := active[stale.TenantID+"\x00"+stale.Alias]; !ok {
			r.Routes.RemoveForTenant(stale.TenantID, stale.Alias)
		}
	}
	for _, d := range deployments {
		// SubmitDeploymentDelete persists desired_state=deleted before the
		// cleanup worker touches provider capacity. Withdraw the concrete route
		// immediately and never let an ordinary reconciliation pass republish it;
		// the delete worker uses this in-memory boundary as its drain fence.
		if d.DesiredState == "deleted" {
			r.Routes.RemoveForTenant(d.TenantID, d.Name)
			continue
		}
		resolved, err := r.Store.ResolveForTenant(ctx, d.TenantID, d.Name)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures = append(failures, fmt.Errorf("reconcile deployment %s/%s: %w", d.TenantID, d.Name, err))
			continue
		}
		type inspection struct {
			target     domain.Target
			ok         bool
			models     map[string]struct{}
			workers    *int
			observedAt time.Time
		}
		inspections := make([]inspection, len(resolved.Targets))
		primaryCount := 0
		var wg sync.WaitGroup
		semaphore := make(chan struct{}, 16)
		for i, target := range resolved.Targets {
			if r.ExternalFallback != nil && r.ExternalFallback.Owns(target.Provider) {
				inspections[i] = inspection{target: target}
				continue
			}
			primaryCount++
			direct, isDirect := r.DirectTargets[target.Provider]
			if isDirect {
				if direct.APIKey == "" || direct.Provider == "" {
					inspections[i] = inspection{target: target}
					continue
				}
				expected := target.UpstreamModel
				if expected == "" {
					expected = d.Model
				}
				result := inspection{target: target, ok: true, models: map[string]struct{}{expected: {}}}
				if direct.Status != nil && target.ProviderResourceID != "" {
					if health, healthErr := direct.Status.EndpointHealth(ctx, target.ProviderResourceID); healthErr == nil {
						workers := health.WorkersIdle + health.WorkersRunning
						result.workers, result.observedAt = &workers, time.Now()
					}
				}
				inspections[i] = result
				continue
			}
			runtimeName := target.Runtime
			if runtimeName == "" {
				runtimeName = d.Runtime
			}
			runtimeBackend, runtimeErr := r.Runtimes.ForRuntime(runtimeName)
			if runtimeErr != nil {
				inspections[i] = inspection{target: target}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case semaphore <- struct{}{}:
					defer func() { <-semaphore }()
				case <-ctx.Done():
					return
				}
				ok, models := runtimeBackend.Inspector.Inspect(ctx, target.URL)
				inspections[i] = inspection{target: target, ok: ok, models: models}
			}()
		}
		wg.Wait()
		healthy := make([]domain.Target, 0, len(resolved.Targets))
		for _, result := range inspections {
			target, ok, models := result.target, result.ok, result.models
			if r.ExternalFallback != nil && r.ExternalFallback.Owns(target.Provider) {
				continue
			}
			expected := target.UpstreamModel
			if expected == "" {
				expected = d.Model
			}
			health := "unhealthy"
			if _, found := models[expected]; ok && found {
				health = "healthy"
				healthy = append(healthy, target)
			}
			if target.Health != health {
				_ = r.Store.SetTargetHealth(ctx, target.ID, health)
				_ = r.Store.Event(ctx, d.ID, target.ID, "replica_"+health, "Target "+target.Name+" became "+health, "")
			}
		}
		if hybrid, ok := r.ExternalFallback.(HybridFallback); ok {
			mode, modeErr := hybrid.OverflowMode(ctx, d)
			var waiting *float64
			observedAt := time.Time{}
			if modeErr == nil && mode == "health_and_queue" && r.QueueSignals != nil {
				if value, signalErr := r.QueueSignals.Waiting(ctx, d.ID); signalErr == nil {
					waiting = &value
					observedAt = time.Now().UTC()
				}
			}
			if modeErr == nil {
				fallback, decision, hybridErr := hybrid.ResolveHybrid(ctx, d, resolved.Targets, overflow.Signal{PrimaryHealthy: len(healthy) > 0, Waiting: waiting, ObservedAt: observedAt}, time.Now().UTC())
				if hybridErr == nil && decision.Route == "external" {
					r.Routes.Put(fallback)
					_ = r.Store.SetDeploymentState(ctx, d.ID, "degraded")
					if decision.Action == "overflow" {
						_ = r.Store.Event(ctx, d.ID, fallback.TargetID, "external_overflow_selected", decision.Reason, "")
					}
					continue
				}
				if hybridErr == nil && decision.Action == "recover" {
					_ = r.Store.Event(ctx, d.ID, "", "external_overflow_recovered", decision.Reason, "")
				}
				if hybridErr == nil && decision.Route == "unavailable" && len(healthy) == 0 {
					r.Routes.RemoveForTenant(d.TenantID, d.Name)
					_ = r.Store.SetDeploymentState(ctx, d.ID, "unhealthy")
					_ = r.Store.Event(ctx, d.ID, "", "external_overflow_denied", decision.Reason, "")
					continue
				}
			}
		}
		if len(healthy) == 0 {
			if r.ExternalFallback != nil {
				fallback, fallbackErr := r.ExternalFallback.Resolve(ctx, d, resolved.Targets)
				if fallbackErr == nil {
					r.Routes.Put(fallback)
					_ = r.Store.SetDeploymentState(ctx, d.ID, "degraded")
					for _, target := range resolved.Targets {
						if target.ID == fallback.TargetID && target.Health != "healthy" {
							_ = r.Store.SetTargetHealth(ctx, target.ID, "healthy")
							_ = r.Store.Event(ctx, d.ID, target.ID, "external_fallback_available", fallback.SelectionReason, "")
						}
					}
					continue
				}
				for _, target := range resolved.Targets {
					if r.ExternalFallback.Owns(target.Provider) && target.Health != "unhealthy" {
						_ = r.Store.SetTargetHealth(ctx, target.ID, "unhealthy")
						_ = r.Store.Event(ctx, d.ID, target.ID, "external_fallback_unavailable", "Configured external fallback is unavailable: "+fallbackErr.Error(), "")
					}
				}
			}
			r.Routes.RemoveForTenant(d.TenantID, d.Name)
			_ = r.Store.SetDeploymentState(ctx, d.ID, "unhealthy")
			continue
		}
		direct, isDirect := r.DirectTargets[healthy[0].Provider]
		if len(healthy) == 1 && isDirect {
			upstream := healthy[0].UpstreamModel
			if upstream == "" {
				upstream = d.Model
			}
			var workers *int
			var observedAt time.Time
			for _, result := range inspections {
				if result.target.ID == healthy[0].ID {
					workers, observedAt = result.workers, result.observedAt
					break
				}
			}
			r.Routes.Put(routes.Snapshot{DeploymentID: d.ID, TargetID: healthy[0].ID, RevisionID: d.ActiveRevisionID, TenantID: d.TenantID, Alias: d.Name, UpstreamModel: upstream, RouterURL: healthy[0].URL, Provider: direct.Provider, ProviderResourceID: healthy[0].ProviderResourceID, Runtime: d.Runtime, ComputeMode: "serverless", UpstreamAPIKey: direct.APIKey, ProviderWorkers: workers, ProviderObservedAt: observedAt, ProtocolCapabilities: r.protocolCapabilities(d.Runtime)})
			_ = r.Store.SetDeploymentState(ctx, d.ID, "healthy")
			continue
		}
		workerURLs := make([]string, len(healthy))
		for i, target := range healthy {
			workerURLs[i] = target.URL
		}
		hash := router.WorkerSetHash(d.RoutingStrategy, workerURLs)
		generation, gerr := r.Store.ActiveGeneration(ctx, d.ID, r.InstanceID)
		if gerr != nil && !errors.Is(gerr, domain.ErrNotFound) {
			failures = append(failures, fmt.Errorf("read router generation for %s/%s: %w", d.TenantID, d.Name, gerr))
			continue
		}
		running := false
		if gerr == nil {
			running = r.Router.Running(routerProcessID(d.ID, generation.Generation))
		}
		restart := errors.Is(gerr, domain.ErrNotFound) || generation.WorkerSetHash != hash || !running
		if restart {
			next := 1
			if gerr == nil {
				next = generation.Generation + 1
			}
			workers := make([]string, len(healthy))
			for i, t := range healthy {
				workers[i] = t.URL
			}
			sort.Strings(workers)
			candidateID := routerProcessID(d.ID, next)
			endpoint, e := r.Router.Start(ctx, router.Spec{DeploymentID: d.ID, ProcessID: candidateID, Model: d.Model, Workers: workers, Strategy: d.RoutingStrategy, Host: "127.0.0.1", Port: routerGenerationPort(r.RouterStartPort, d.ID, next)})
			if e != nil {
				_ = r.Store.Event(ctx, d.ID, "", "router_failed", e.Error(), "")
				_ = r.Store.SetDeploymentState(ctx, d.ID, "degraded")
				continue
			}
			generation, e = r.Store.RecordGeneration(ctx, domain.RouterGeneration{DeploymentID: d.ID, OwnerID: r.InstanceID, Generation: next, Strategy: d.RoutingStrategy, WorkerSetHash: hash, InternalEndpoint: endpoint})
			if e != nil {
				_ = r.Router.Stop(candidateID)
				failures = append(failures, fmt.Errorf("record router generation for %s/%s: %w", d.TenantID, d.Name, e))
				continue
			}
			upstream := healthy[0].UpstreamModel
			if upstream == "" {
				upstream = d.Model
			}
			r.Routes.Put(routeSnapshot(d, healthy, upstream, generation.InternalEndpoint, candidateID, r.RouterAPIKey, r.protocolCapabilities(d.Runtime)))
		}
		upstream := healthy[0].UpstreamModel
		if upstream == "" {
			upstream = d.Model
		}
		if _, published := r.Routes.GetForTenant(d.TenantID, d.Name); !published {
			r.Routes.Put(routeSnapshot(d, healthy, upstream, generation.InternalEndpoint, routerProcessID(d.ID, generation.Generation), r.RouterAPIKey, r.protocolCapabilities(d.Runtime)))
		}
		state := "degraded"
		if len(healthy) == primaryCount {
			state = "healthy"
		}
		_ = r.Store.SetDeploymentState(ctx, d.ID, state)
	}
	if err := r.observeAdoptedTargets(ctx); err != nil {
		failures = append(failures, err)
	}
	if err := r.compileEndpoints(ctx, deployments); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (r *Reconciler) observeAdoptedTargets(ctx context.Context) error {
	store, ok := r.Store.(endpointStore)
	if !ok {
		return nil
	}
	tenants, err := store.TenantsWithEndpoints(ctx)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		endpoints, listErr := store.EndpointsForTenant(ctx, tenant)
		if listErr != nil {
			return listErr
		}
		for _, endpoint := range endpoints {
			resolved, resolveErr := store.ResolveEndpointForTenant(ctx, tenant, endpoint.Name)
			if resolveErr != nil {
				return resolveErr
			}
			for _, binding := range resolved.Bindings {
				if binding.Kind != "external" || binding.TargetID == "" {
					continue
				}
				_, managed, configErr := external.ParseManagedBindingConfig(binding.ConfigJSON)
				if configErr == nil && managed {
					// Authenticated external APIs are observed by the external
					// coordinator because the generic runtime inspector must never
					// receive their credential.
					continue
				}
				target, targetErr := store.TargetForTenantByID(ctx, tenant, binding.TargetID)
				if targetErr != nil {
					return targetErr
				}
				backend, runtimeErr := r.Runtimes.ForRuntime(target.Runtime)
				if runtimeErr != nil {
					continue
				}
				health := "unhealthy"
				if ok, models := backend.Inspector.Inspect(ctx, target.URL); ok {
					if _, found := models[target.UpstreamModel]; found {
						health = "healthy"
					}
				}
				if target.Health != health {
					_ = r.Store.SetTargetHealth(ctx, target.ID, health)
				}
			}
		}
	}
	return nil
}

func (r *Reconciler) compileEndpoints(ctx context.Context, deployments []domain.Deployment) error {
	store, ok := r.Store.(endpointStore)
	if !ok {
		return nil
	}
	byTenant := make(map[string]map[string]domain.Deployment)
	for _, deployment := range deployments {
		if deployment.DesiredState == "deleted" {
			continue
		}
		if byTenant[deployment.TenantID] == nil {
			byTenant[deployment.TenantID] = make(map[string]domain.Deployment)
		}
		byTenant[deployment.TenantID][deployment.ID] = deployment
	}
	// Include tenants that currently have only endpoint snapshots so deleting
	// their last concrete deployment cannot leave a stale routable alias.
	for _, route := range r.Routes.List() {
		if byTenant[route.TenantID] == nil {
			byTenant[route.TenantID] = make(map[string]domain.Deployment)
		}
	}
	tenants, err := store.TenantsWithEndpoints(ctx)
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		if byTenant[tenant] == nil {
			byTenant[tenant] = make(map[string]domain.Deployment)
		}
	}
	for tenant, concrete := range byTenant {
		endpoints, err := store.EndpointsForTenant(ctx, tenant)
		if err != nil {
			return err
		}
		active := make(map[string]struct{}, len(endpoints))
		for _, endpoint := range endpoints {
			active[endpoint.Name] = struct{}{}
			if endpoint.DesiredState != "serving" || endpoint.ActiveServingPlanID == "" {
				r.Routes.RemoveEndpointForTenant(tenant, endpoint.Name)
				continue
			}
			resolved, resolveErr := store.ResolveEndpointForTenant(ctx, tenant, endpoint.Name)
			if resolveErr != nil {
				return resolveErr
			}
			bindings := make(map[string]domain.BackendBinding, len(resolved.Bindings))
			for _, binding := range resolved.Bindings {
				bindings[binding.ID] = binding
			}
			compiled := make([]routes.Snapshot, 0, len(resolved.ActivePlan.Bindings))
			for _, planned := range resolved.ActivePlan.Bindings {
				binding, found := bindings[planned.BindingID]
				if !found || binding.OwnershipMode == "observe-only" {
					continue
				}
				if binding.Kind == "external" {
					target, targetErr := store.TargetForTenantByID(ctx, tenant, binding.TargetID)
					if targetErr != nil {
						continue
					}
					_, managed, configErr := external.ParseManagedBindingConfig(binding.ConfigJSON)
					if configErr != nil {
						continue
					}
					if managed {
						if r.ExternalFallback == nil || !r.ExternalFallback.Owns(target.Provider) {
							continue
						}
						base, resolveErr := r.ExternalFallback.ResolveBinding(ctx, endpoint, binding, target)
						health := "unhealthy"
						if resolveErr == nil {
							health = "healthy"
							base.LogicalModelID = endpoint.LogicalModelID
							base.EnvironmentID = endpoint.EnvironmentID
							base.EndpointID = endpoint.ID
							base.ServingPlanID = resolved.ActivePlan.ID
							base.BindingID = binding.ID
							base.RoutingWeight = planned.Weight
							base.ProtocolCapabilities = r.protocolCapabilities(target.Runtime)
							compiled = append(compiled, base)
						}
						if target.Health != health {
							_ = r.Store.SetTargetHealth(ctx, target.ID, health)
						}
						continue
					}
					if target.Health != "healthy" {
						continue
					}
					compiled = append(compiled, routes.Snapshot{TenantID: tenant, Alias: endpoint.Name, TargetID: target.ID, UpstreamModel: target.UpstreamModel, RouterURL: target.URL, Provider: target.Provider, Runtime: target.Runtime, ComputeMode: "external", LogicalModelID: endpoint.LogicalModelID, EnvironmentID: endpoint.EnvironmentID, EndpointID: endpoint.ID, ServingPlanID: resolved.ActivePlan.ID, BindingID: binding.ID, RoutingWeight: planned.Weight, ProtocolCapabilities: r.protocolCapabilities(target.Runtime)})
					continue
				}
				if binding.Kind != "deployment" {
					continue
				}
				deployment, found := concrete[binding.DeploymentID]
				if !found {
					continue
				}
				base, available := r.Routes.GetDeployment(deployment.ID)
				if !available {
					continue
				}
				base.Alias = endpoint.Name
				base.LogicalModelID = endpoint.LogicalModelID
				base.EnvironmentID = endpoint.EnvironmentID
				base.EndpointID = endpoint.ID
				base.ServingPlanID = resolved.ActivePlan.ID
				base.BindingID = binding.ID
				base.RoutingWeight = planned.Weight
				compiled = append(compiled, base)
			}
			state := "serving"
			if len(compiled) == 0 || resolved.ActivePlan.RoutingPolicy == "manual" && len(compiled) != 1 {
				state = "degraded"
				r.Routes.RemoveEndpointForTenant(tenant, endpoint.Name)
			} else {
				r.Routes.PublishEndpoint(routes.EndpointRoute{TenantID: tenant, Alias: endpoint.Name, RoutingPolicy: resolved.ActivePlan.RoutingPolicy, Routes: compiled})
			}
			if endpoint.ObservedState != state {
				_ = store.SetEndpointState(ctx, tenant, endpoint.ID, state)
			}
		}
		for _, alias := range r.Routes.EndpointAliasesForTenant(tenant) {
			if _, found := active[alias]; !found {
				r.Routes.RemoveEndpointForTenant(tenant, alias)
			}
		}
	}
	return nil
}

// RefreshEndpoints recompiles only the stable endpoint layer from already
// published concrete deployment routes. It avoids provider probes and is safe
// to invoke after an endpoint control-plane mutation.
func (r *Reconciler) RefreshEndpoints(ctx context.Context) error {
	deployments, err := r.Store.Deployments(ctx)
	if err != nil {
		return err
	}
	return r.compileEndpoints(ctx, deployments)
}

func (r *Reconciler) reapRetiredRoutes() {
	for _, retired := range r.Routes.RetiredReady() {
		if retired.RouterProcessID != "" {
			_ = r.Router.Stop(retired.RouterProcessID)
		}
		r.Routes.ForgetRetired(retired)
	}
}

func routeSnapshot(deployment domain.Deployment, targets []domain.Target, upstream, endpoint, processID, routerAPIKey string, capabilities runtimecontract.ProtocolCapabilities) routes.Snapshot {
	provider := ""
	computeMode := ""
	for _, target := range targets {
		if provider == "" {
			provider = target.Provider
		} else if provider != target.Provider {
			provider = ""
			break
		}
	}
	if provider != "" {
		computeMode = "elastic"
		if provider == "existing" {
			computeMode = "existing"
		}
	}
	return routes.Snapshot{DeploymentID: deployment.ID, RevisionID: deployment.ActiveRevisionID, TenantID: deployment.TenantID, Alias: deployment.Name, UpstreamModel: upstream, RouterURL: endpoint, RouterProcessID: processID, Provider: provider, Runtime: deployment.Runtime, ComputeMode: computeMode, UpstreamAPIKey: routerAPIKey, ProtocolCapabilities: capabilities}
}

func (r *Reconciler) protocolCapabilities(runtime string) runtimecontract.ProtocolCapabilities {
	backend, err := r.Runtimes.ForRuntime(runtime)
	if err != nil {
		return runtimecontract.ProtocolCapabilities{}
	}
	return backend.Profile.ProtocolCapabilities()
}
func routerPort(start int, deploymentID string) int {
	sum := sha256.Sum256([]byte(deploymentID))
	offset := int(sum[0])<<8 | int(sum[1])
	return start + offset%10000
}
func routerGenerationPort(start int, deploymentID string, generation int) int {
	base := routerPort(start, deploymentID) - start
	return start + (base+generation*7919)%10000
}
func routerProcessID(deploymentID string, generation int) string {
	return deploymentID + "-g" + strconv.Itoa(generation)
}
