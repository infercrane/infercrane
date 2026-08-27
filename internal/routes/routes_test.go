package routes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSnapshotNeverSerializesUpstreamCredential(t *testing.T) {
	encoded, err := json.Marshal(Snapshot{Alias: "model", UpstreamAPIKey: "internal-router-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "internal-router-secret") || strings.Contains(string(encoded), "UpstreamAPIKey") {
		t.Fatalf("serialized route leaked upstream credential: %s", encoded)
	}
}

func TestWeightedEndpointDecaysMissingBindingAndRequiresBoundedRecovery(t *testing.T) {
	directory := New()
	directory.now = func() time.Time { return time.Unix(100, 0) }
	planned := []string{"aws", "gcp"}
	both := []Snapshot{{TenantID: "tenant", Alias: "model", BindingID: "aws", Provider: "aws", RoutingWeight: 1}, {TenantID: "tenant", Alias: "model", BindingID: "gcp", Provider: "gcp", RoutingWeight: 1}}
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "weighted", Routes: both, PlannedBindingIDs: planned})
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "weighted", Routes: both[:1], PlannedBindingIDs: planned})
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "weighted", Routes: both[:1], PlannedBindingIDs: planned})
	for range 4 {
		route, release, ok := directory.AcquireForTenant("tenant", "model")
		if !ok || route.BindingID != "aws" {
			t.Fatalf("decayed route selected: %#v ok=%t", route, ok)
		}
		release()
	}
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "weighted", Routes: both, PlannedBindingIDs: planned})
	for range 2 {
		route, release, _ := directory.AcquireForTenant("tenant", "model")
		if route.BindingID != "aws" {
			t.Fatalf("binding recovered without hysteresis: %#v", route)
		}
		release()
	}
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "weighted", Routes: both, PlannedBindingIDs: planned})
	seenGCP := false
	for range 4 {
		route, release, ok := directory.AcquireForTenant("tenant", "model")
		if ok && route.BindingID == "gcp" {
			seenGCP = true
		}
		release()
	}
	if !seenGCP {
		t.Fatal("healthy binding did not recover its configured weight")
	}
}

func TestDirectoryOrdersAndRemovesSnapshots(t *testing.T) {
	directory := New()
	directory.Put(Snapshot{Alias: "z"})
	directory.Put(Snapshot{Alias: "a"})
	if routes := directory.List(); len(routes) != 2 || routes[0].Alias != "a" {
		t.Fatalf("routes = %#v", routes)
	}
	directory.Remove("a")
	if _, ok := directory.Get("a"); ok {
		t.Fatal("removed route remains available")
	}
}

func TestDirectoryIsolatesSameAliasByTenant(t *testing.T) {
	directory := New()
	directory.Put(Snapshot{TenantID: "a", Alias: "model", RouterURL: "http://a"})
	directory.Put(Snapshot{TenantID: "b", Alias: "model", RouterURL: "http://b"})
	a, ok := directory.GetForTenant("a", "model")
	if !ok || a.RouterURL != "http://a" {
		t.Fatalf("tenant a route=%#v", a)
	}
	b, ok := directory.GetForTenant("b", "model")
	if !ok || b.RouterURL != "http://b" {
		t.Fatalf("tenant b route=%#v", b)
	}
}

func TestRetiredGenerationWaitsForPinnedRequest(t *testing.T) {
	directory := New()
	old := Snapshot{DeploymentID: "deployment", Alias: "model", RouterURL: "http://old", RouterProcessID: "deployment-g1"}
	directory.Put(old)
	selected, release, ok := directory.AcquireForTenant("global", "model")
	if !ok || selected.RouterProcessID != old.RouterProcessID {
		t.Fatalf("selected=%#v ok=%t", selected, ok)
	}
	directory.Put(Snapshot{DeploymentID: "deployment", Alias: "model", RouterURL: "http://new", RouterProcessID: "deployment-g2"})
	if pending := directory.RetiringInFlight("deployment"); pending != 1 || len(directory.RetiredReady()) != 0 {
		t.Fatalf("pending=%d ready=%#v", pending, directory.RetiredReady())
	}
	release()
	if pending := directory.RetiringInFlight("deployment"); pending != 0 || len(directory.RetiredReady()) != 1 {
		t.Fatalf("pending=%d ready=%#v", pending, directory.RetiredReady())
	}
}

func TestEndpointPromotionPinsExistingRequestGeneration(t *testing.T) {
	directory := New()
	old := Snapshot{DeploymentID: "old", EndpointID: "endpoint", ServingPlanID: "plan-1", BindingID: "binding-1", TenantID: "tenant", Alias: "coder-production", RouterURL: "http://old", RouterProcessID: "old-g1"}
	concreteOld := old
	concreteOld.Alias = "old-deployment"
	directory.Put(concreteOld)
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: old.Alias, RoutingPolicy: "manual", Routes: []Snapshot{old}})
	selected, release, ok := directory.AcquireForTenant("tenant", old.Alias)
	if !ok || selected.ServingPlanID != "plan-1" {
		t.Fatalf("selected = %#v", selected)
	}
	newRoute := Snapshot{DeploymentID: "new", EndpointID: "endpoint", ServingPlanID: "plan-2", BindingID: "binding-2", TenantID: "tenant", Alias: old.Alias, RouterURL: "http://new", RouterProcessID: "new-g1"}
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: old.Alias, RoutingPolicy: "manual", Routes: []Snapshot{newRoute}})
	if selected.RouterURL != "http://old" {
		t.Fatalf("pinned route changed=%#v", selected)
	}
	if pending := directory.RetiringInFlight(old.DeploymentID); pending != 1 {
		t.Fatalf("endpoint promotion did not fence old capacity: pending=%d", pending)
	}
	future, futureRelease, ok := directory.AcquireForTenant("tenant", old.Alias)
	if !ok || future.RouterURL != "http://new" {
		t.Fatalf("future route = %#v", future)
	}
	futureRelease()
	release()
	if pending := directory.RetiringInFlight(old.DeploymentID); pending != 0 {
		t.Fatalf("released endpoint generation remains in flight: pending=%d", pending)
	}
	if ready := directory.RetiredReady(); len(ready) != 0 {
		t.Fatalf("endpoint retirement attempted to stop a still-published concrete deployment: %#v", ready)
	}
	directory.RemoveForTenant("tenant", concreteOld.Alias)
	if ready := directory.RetiredReady(); len(ready) != 1 || ready[0].RouterProcessID != old.RouterProcessID {
		t.Fatalf("withdrawn concrete deployment was not reaped: %#v", ready)
	}
}

func TestWeightedEndpointSelectionIsDeterministic(t *testing.T) {
	directory := New()
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "weighted", Routes: []Snapshot{
		{TenantID: "tenant", Alias: "model", BindingID: "a", RoutingWeight: 2},
		{TenantID: "tenant", Alias: "model", BindingID: "b", RoutingWeight: 1},
	}})
	var selected []string
	for range 6 {
		route, release, ok := directory.AcquireForTenant("tenant", "model")
		if !ok {
			t.Fatal("endpoint route unavailable")
		}
		selected = append(selected, route.BindingID)
		release()
	}
	want := []string{"a", "a", "b", "a", "a", "b"}
	for index := range want {
		if selected[index] != want[index] {
			t.Fatalf("selection = %#v, want %#v", selected, want)
		}
	}
}

func TestRemovedEndpointDoesNotFallThroughToLegacyDeploymentAlias(t *testing.T) {
	directory := New()
	directory.Put(Snapshot{TenantID: "tenant", Alias: "model", DeploymentID: "deployment", RouterURL: "http://base"})
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "manual", Routes: []Snapshot{{TenantID: "tenant", Alias: "model", DeploymentID: "deployment", EndpointID: "endpoint", RouterURL: "http://base"}}})
	directory.RemoveEndpointForTenant("tenant", "model")
	if _, _, ok := directory.AcquireForTenant("tenant", "model"); ok {
		t.Fatal("removed endpoint fell through to concrete deployment alias")
	}
}

func TestPreferredRouteFallsBackWhenHintDisappears(t *testing.T) {
	directory := New()
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "manual", Routes: []Snapshot{{TenantID: "tenant", Alias: "model", BindingID: "active", TargetID: "one"}, {TenantID: "tenant", Alias: "model", BindingID: "preferred", TargetID: "two"}}})
	route, release, ok := directory.AcquirePreferredForTenant("tenant", "model", "preferred", "")
	if !ok || route.BindingID != "preferred" {
		t.Fatalf("preferred route = %#v", route)
	}
	release()
	directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "manual", Routes: []Snapshot{{TenantID: "tenant", Alias: "model", BindingID: "active", TargetID: "one"}}})
	route, release, ok = directory.AcquirePreferredForTenant("tenant", "model", "preferred", "")
	if !ok || route.BindingID != "active" {
		t.Fatalf("fallback route = %#v", route)
	}
	release()
}

func FuzzDirectoryGenerationSafety(f *testing.F) {
	f.Add([]byte{0, 1, 4, 2, 0, 3, 5, 2})
	f.Add([]byte{4, 1, 4, 1, 5, 0, 2, 2, 3})
	f.Fuzz(func(t *testing.T, sequence []byte) {
		if len(sequence) > 256 {
			sequence = sequence[:256]
		}
		directory := New()
		generation := 0
		var releases []func()
		publish := func(endpoint bool) {
			generation++
			route := Snapshot{TenantID: "tenant", Alias: "model", DeploymentID: "deployment", RevisionID: "revision", RouterURL: "http://router", RouterProcessID: "router-g" + string(rune(generation))}
			if endpoint {
				directory.PublishEndpoint(EndpointRoute{TenantID: "tenant", Alias: "model", RoutingPolicy: "manual", Routes: []Snapshot{route}})
			} else {
				directory.Put(route)
			}
		}
		for _, operation := range sequence {
			switch operation % 7 {
			case 0:
				publish(false)
			case 1:
				if _, release, ok := directory.AcquireForTenant("tenant", "model"); ok {
					releases = append(releases, release)
				}
			case 2:
				if len(releases) > 0 {
					releases[0]()
					releases[0]()
					releases = releases[1:]
				}
			case 3:
				directory.RemoveForTenant("tenant", "model")
			case 4:
				publish(true)
			case 5:
				directory.RemoveEndpointForTenant("tenant", "model")
			case 6:
				for _, route := range directory.RetiredReady() {
					directory.ForgetRetired(route)
				}
			}
			assertDirectoryGenerationInvariant(t, directory)
		}
		for _, release := range releases {
			release()
		}
		assertDirectoryGenerationInvariant(t, directory)
	})
}

func assertDirectoryGenerationInvariant(t *testing.T, directory *Directory) {
	t.Helper()
	directory.mu.RLock()
	current := map[string]struct{}{}
	inflight := make(map[string]int, len(directory.inflight))
	for _, route := range directory.items {
		current[routeID(route)] = struct{}{}
	}
	for _, endpoint := range directory.endpoints {
		for _, route := range endpoint.Routes {
			current[routeID(route)] = struct{}{}
		}
	}
	for id, count := range directory.inflight {
		inflight[id] = count
		if count <= 0 {
			directory.mu.RUnlock()
			t.Fatalf("route %q has non-positive in-flight count %d", id, count)
		}
	}
	directory.mu.RUnlock()
	for _, route := range directory.RetiredReady() {
		id := routeID(route)
		if inflight[id] != 0 {
			t.Fatalf("in-flight route %q became reapable", id)
		}
		if _, published := current[id]; published {
			t.Fatalf("published route %q became reapable", id)
		}
	}
}
