package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
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
type Reconciler struct {
	Store           Store
	Routes          *routes.Directory
	Router          router.Backend
	Runtime         Runtime
	Interval        time.Duration
	RouterStartPort int
	InstanceID      string
	Logger          *slog.Logger
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
			_ = r.Router.Stop(stale.DeploymentID)
		}
	}
	for _, d := range deployments {
		resolved, err := r.Store.ResolveForTenant(ctx, d.TenantID, d.Name)
		if err != nil {
			return err
		}
		type inspection struct {
			target domain.Target
			ok     bool
			models map[string]struct{}
		}
		inspections := make([]inspection, len(resolved.Targets))
		var wg sync.WaitGroup
		semaphore := make(chan struct{}, 16)
		for i, target := range resolved.Targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case semaphore <- struct{}{}:
					defer func() { <-semaphore }()
				case <-ctx.Done():
					return
				}
				ok, models := r.Runtime.Inspect(ctx, target.URL)
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
		hash := workerHash(d.RoutingStrategy, healthy)
		generation, gerr := r.Store.ActiveGeneration(ctx, d.ID, r.InstanceID)
		if gerr != nil && !errors.Is(gerr, domain.ErrNotFound) {
			return gerr
		}
		restart := errors.Is(gerr, domain.ErrNotFound) || generation.WorkerSetHash != hash || !r.Router.Running(d.ID)
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
			endpoint, e := r.Router.Start(ctx, router.Spec{DeploymentID: d.ID, Workers: workers, Strategy: d.RoutingStrategy, Host: "127.0.0.1", Port: routerPort(r.RouterStartPort, d.ID)})
			if e != nil {
				r.Routes.RemoveForTenant(d.TenantID, d.Name)
				_ = r.Store.Event(ctx, d.ID, "", "router_failed", e.Error(), "")
				_ = r.Store.SetDeploymentState(ctx, d.ID, "degraded")
				continue
			}
			generation, e = r.Store.RecordGeneration(ctx, domain.RouterGeneration{DeploymentID: d.ID, OwnerID: r.InstanceID, Generation: next, Strategy: d.RoutingStrategy, WorkerSetHash: hash, InternalEndpoint: endpoint})
			if e != nil {
				return e
			}
		}
		upstream := healthy[0].UpstreamModel
		if upstream == "" {
			upstream = d.Model
		}
		r.Routes.Put(routes.Snapshot{DeploymentID: d.ID, TenantID: d.TenantID, Alias: d.Name, UpstreamModel: upstream, RouterURL: generation.InternalEndpoint})
		state := "degraded"
		if len(healthy) == len(resolved.Targets) {
			state = "healthy"
		}
		_ = r.Store.SetDeploymentState(ctx, d.ID, state)
	}
	return nil
}
func routerPort(start int, deploymentID string) int {
	sum := sha256.Sum256([]byte(deploymentID))
	offset := int(sum[0])<<8 | int(sum[1])
	return start + offset%10000
}
func workerHash(strategy string, targets []domain.Target) string {
	urls := make([]string, len(targets))
	for i, t := range targets {
		urls[i] = t.URL
	}
	sort.Strings(urls)
	sum := sha256.Sum256([]byte(strategy + "\x00" + strings.Join(urls, "\x00")))
	return hex.EncodeToString(sum[:])
}
