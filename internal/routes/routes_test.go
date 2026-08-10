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
