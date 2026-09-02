// Package authz defines stable tenant roles and authorization policy.
package authz

import "fmt"

type Role string
type Action string

const (
	Viewer         Role   = "viewer"
	Operator       Role   = "operator"
	Admin          Role   = "admin"
	Read           Action = "read"
	Deploy         Action = "deploy"
	Delete         Action = "delete"
	ManageTenant   Action = "manage_tenant"
	ManageSecrets  Action = "manage_secrets"
	ManageExternal Action = "manage_external"
	ManageModelAPI Action = "manage_model_api"
)

func Allowed(role Role, action Action) bool {
	switch role {
	case Admin:
		return true
	case Operator:
		return action == Read || action == Deploy || action == Delete || action == ManageExternal
	case Viewer:
		return action == Read
	default:
		return false
	}
}

// AllowedScoped applies a role ceiling and then an optional explicit scope
// restriction. Empty scopes preserve only the legacy non-sensitive action set
// and never grant secret or external-target management. Newly created service
// accounts always persist explicit scopes.
func AllowedScoped(role Role, scopes []string, action Action) bool {
	if !Allowed(role, action) {
		return false
	}
	if len(scopes) == 0 {
		return action != ManageSecrets && action != ManageExternal
	}
	for _, scope := range scopes {
		if Action(scope) == action {
			return true
		}
	}
	return false
}

func DefaultScopeNames(role Role) []string {
	actions := DefaultScopes(role)
	names := make([]string, len(actions))
	for i, action := range actions {
		names[i] = string(action)
	}
	return names
}

func ValidateScopes(role Role, scopes []Action) error {
	seen := map[Action]struct{}{}
	for _, scope := range scopes {
		if scope != Read && scope != Deploy && scope != Delete && scope != ManageTenant && scope != ManageSecrets && scope != ManageExternal && scope != ManageModelAPI {
			return fmt.Errorf("unknown scope %q", scope)
		}
		if !Allowed(role, scope) {
			return fmt.Errorf("scope %q exceeds role %q", scope, role)
		}
		if _, duplicate := seen[scope]; duplicate {
			return fmt.Errorf("scope %q is duplicated", scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func DefaultScopes(role Role) []Action {
	candidates := []Action{Read, Deploy, Delete, ManageTenant, ManageSecrets, ManageExternal, ManageModelAPI}
	out := make([]Action, 0, len(candidates))
	for _, candidate := range candidates {
		if Allowed(role, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}
