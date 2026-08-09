package authz

import "testing"

func TestPolicy(t *testing.T) {
	if !Allowed(Viewer, Read) || Allowed(Viewer, Deploy) {
		t.Fatal("viewer policy is incorrect")
	}
	if !Allowed(Operator, Delete) || Allowed(Operator, ManageTenant) {
		t.Fatal("operator policy is incorrect")
	}
	if !Allowed(Admin, ManageTenant) || Allowed(Role("unknown"), Read) {
		t.Fatal("admin or default policy is incorrect")
	}
}
