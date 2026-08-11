package routes

import "testing"

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
	future, futureRelease, ok := directory.AcquireForTenant("tenant", old.Alias)
	if !ok || future.RouterURL != "http://new" {
		t.Fatalf("future route = %#v", future)
	}
	futureRelease()
	release()
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
