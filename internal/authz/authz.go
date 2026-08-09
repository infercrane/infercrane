// Package authz defines stable tenant roles and authorization policy.
package authz

type Role string
type Action string

const (
	Viewer       Role   = "viewer"
	Operator     Role   = "operator"
	Admin        Role   = "admin"
	Read         Action = "read"
	Deploy       Action = "deploy"
	Delete       Action = "delete"
	ManageTenant Action = "manage_tenant"
)

func Allowed(role Role, action Action) bool {
	switch role {
	case Admin:
		return true
	case Operator:
		return action == Read || action == Deploy || action == Delete
	case Viewer:
		return action == Read
	default:
		return false
	}
}
