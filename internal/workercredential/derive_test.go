package workercredential

import "testing"

func TestDeriveIsScopedAndStable(t *testing.T) {
	first := Derive("master-secret", "tenant-a", "deployment-a")
	if first == "" || first != Derive("master-secret", "tenant-a", "deployment-a") {
		t.Fatal("derived credential is not stable")
	}
	if first == Derive("master-secret", "tenant-b", "deployment-a") || first == Derive("master-secret", "tenant-a", "deployment-b") {
		t.Fatal("derived credential is not scoped")
	}
	if Derive("", "tenant-a", "deployment-a") != "" || Derive("master-secret", "", "deployment-a") != "" || Derive("master-secret", "tenant-a", "") != "" {
		t.Fatal("incomplete worker credential identity did not fail closed")
	}
}
