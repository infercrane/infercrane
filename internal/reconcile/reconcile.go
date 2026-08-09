package reconcile

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
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
type Runtime interface {
	Inspect(context.Context, string) (bool, map[string]struct{})
}
type ServerlessStatus interface {
	EndpointHealth(context.Context, string) (provision.ServerlessHealth, error)
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
	Store           Store
	Routes          *routes.Directory
	Router          router.Backend
	Runtimes        map[string]Runtime
	Interval        time.Duration
	RouterStartPort int
	InstanceID      string
	Logger          *slog.Logger
	DirectTargets   map[string]DirectTargetBackend
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
	deployments, err := r.Store.Deployments(ctx)
	if err != nil {
		return err
	}
	active := make(map[string]struct{}, len(deployments))
	for _, d := range deployments {
		active[d.TenantID+"\x00"+d.Name] = struct{}{}
	}
	for _, stale := range r.Routes.List() {
		if _, ok := active[stale.TenantID+"\x00"+stale.Alias]; !ok {
			r.Routes.RemoveForTenant(stale.TenantID, stale.Alias)
			processID := stale.RouterProcessID
			if processID == "" {
				processID = stale.DeploymentID
			}
			_ = r.Router.Stop(processID)
		}
	}
	for _, d := range deployments {
		resolved, err := r.Store.ResolveForTenant(ctx, d.TenantID, d.Name)
		if err != nil {
			return err
		}
		type inspection struct {
			target     domain.Target
			ok         bool
			models     map[string]struct{}
			workers    *int
			observedAt time.Time
		}
		inspections := make([]inspection, len(resolved.Targets))
		var wg sync.WaitGroup
		semaphore := make(chan struct{}, 16)
		for i, target := range resolved.Targets {
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
			runtimeInspector, registered := r.Runtimes[runtimeName]
			if !registered {
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
				ok, models := runtimeInspector.Inspect(ctx, target.URL)
				inspections[i] = inspection{target: target, ok: ok, models: models}
			}()
		}
		wg.Wait()
		healthy := make([]domain.Target, 0, len(resolved.Targets))
		for _, result := range inspections {
			target, ok, models := result.target, result.ok, result.models
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
		if len(healthy) == 0 {
			r.Routes.RemoveForTenant(d.TenantID, d.Name)
			_ = r.Store.SetDeploymentState(ctx, d.ID, "unhealthy")
			continue
		}
		direct, isDirect := r.DirectTargets[healthy[0].Provider]
		if len(healthy) == 1 && isDirect {
			oldRoute, hadOldRoute := r.Routes.GetForTenant(d.TenantID, d.Name)
			if hadOldRoute && oldRoute.RouterProcessID != "" {
				_ = r.Router.Stop(oldRoute.RouterProcessID)
			}
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
			r.Routes.Put(routes.Snapshot{DeploymentID: d.ID, RevisionID: d.ActiveRevisionID, TenantID: d.TenantID, Alias: d.Name, UpstreamModel: upstream, RouterURL: healthy[0].URL, Provider: direct.Provider, Runtime: d.Runtime, ComputeMode: "serverless", UpstreamAPIKey: direct.APIKey, ProviderWorkers: workers, ProviderObservedAt: observedAt})
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
			return gerr
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
				return e
			}
			oldRoute, hadOldRoute := r.Routes.GetForTenant(d.TenantID, d.Name)
			upstream := healthy[0].UpstreamModel
			if upstream == "" {
				upstream = d.Model
			}
			r.Routes.Put(routeSnapshot(d, healthy, upstream, generation.InternalEndpoint, candidateID))
			if hadOldRoute && oldRoute.RouterProcessID != "" && oldRoute.RouterProcessID != candidateID {
				_ = r.Router.Stop(oldRoute.RouterProcessID)
			}
		}
		upstream := healthy[0].UpstreamModel
		if upstream == "" {
			upstream = d.Model
		}
		if _, published := r.Routes.GetForTenant(d.TenantID, d.Name); !published {
			r.Routes.Put(routeSnapshot(d, healthy, upstream, generation.InternalEndpoint, routerProcessID(d.ID, generation.Generation)))
		}
		state := "degraded"
		if len(healthy) == len(resolved.Targets) {
			state = "healthy"
		}
		_ = r.Store.SetDeploymentState(ctx, d.ID, state)
	}
	return nil
}

func routeSnapshot(deployment domain.Deployment, targets []domain.Target, upstream, endpoint, processID string) routes.Snapshot {
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
	return routes.Snapshot{DeploymentID: deployment.ID, RevisionID: deployment.ActiveRevisionID, TenantID: deployment.TenantID, Alias: deployment.Name, UpstreamModel: upstream, RouterURL: endpoint, RouterProcessID: processID, Provider: provider, Runtime: deployment.Runtime, ComputeMode: computeMode}
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
