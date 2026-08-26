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

func TestExplicitScopesCannotEscalateRole(t *testing.T) {
	if err := ValidateScopes(Viewer, []Action{Deploy}); err == nil {
		t.Fatal("viewer deploy scope was accepted")
	}
	if err := ValidateScopes(Operator, []Action{Read, Deploy}); err != nil {
		t.Fatal(err)
	}
	if !AllowedScoped(Operator, []string{"read"}, Read) || AllowedScoped(Operator, []string{"read"}, Deploy) {
		t.Fatal("explicit scopes did not restrict operator role")
	}
	if AllowedScoped(Role("unknown"), nil, Read) {
		t.Fatal("unknown role was allowed")
	}
	if AllowedScoped(Operator, nil, ManageExternal) || AllowedScoped(Admin, nil, ManageSecrets) {
		t.Fatal("legacy empty scopes gained a sensitive permission")
	}
}
