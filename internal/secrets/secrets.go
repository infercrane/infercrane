// Package secrets resolves reference-only credentials at the final process
// boundary. Resolved values must never be persisted or returned by public APIs.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

var environmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,127}$`)

type Resolver interface {
	Resolve(context.Context, domain.SecretReference) (string, error)
}

// BootstrapTenant is the workspace of the operator's own API key. It may always
// read the control plane's environment.
const BootstrapTenant = "global"

// ErrTenantNotAllowed refuses an environment reference from a workspace that
// may not read the control plane's environment. That environment holds the
// operator's own keys, so on a control plane shared by many workspaces only
// the operator's workspaces may name it.
var ErrTenantNotAllowed = errors.New("this workspace may not reference the control plane's environment; environment secret references are reserved for the operator's workspaces")

// TenantPolicy names the workspaces whose secret references may read the
// control plane's environment. The zero value allows none.
type TenantPolicy struct {
	all     bool
	tenants map[string]struct{}
}

// NewTenantPolicy allows the named workspace IDs; "*" allows every workspace
// and is only for a control plane that serves one customer.
func NewTenantPolicy(ids ...string) TenantPolicy {
	policy := TenantPolicy{tenants: map[string]struct{}{}}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		switch {
		case id == "*":
			policy.all = true
		case id != "":
			policy.tenants[id] = struct{}{}
		}
	}
	return policy
}

// Allows reports whether the workspace may create or resolve environment
// references.
func (p TenantPolicy) Allows(tenant string) bool {
	if p.all {
		return true
	}
	if tenant == "" {
		return false
	}
	_, ok := p.tenants[tenant]
	return ok
}

// Environment resolves "env" references from the process environment. It
// resolves only references whose workspace Tenants allows: a workspace that
// could name any variable could read the operator's keys.
type Environment struct {
	Lookup func(string) (string, bool)
	// Tenants are the workspaces whose references may be resolved. The zero
	// value resolves none.
	Tenants TenantPolicy
}

// AllowsTenant reports whether the workspace may create environment references.
func (e Environment) AllowsTenant(tenant string) bool {
	return e.Tenants.Allows(tenant)
}

// ValidateReference checks the stored, non-secret part of a reference for its
// resolver. A sealed reference stores no reference text: its value is sealed
// separately and never leaves the control plane.
func ValidateReference(reference domain.SecretReference) error {
	switch reference.Resolver {
	case ResolverEnvironment:
		if !environmentName.MatchString(reference.Reference) {
			return errors.New("secret reference is not a valid environment reference")
		}
	case ResolverSealed:
		if reference.Reference != "" {
			return errors.New("a sealed secret takes a value, not a reference")
		}
	case ResolverKubernetes:
		if _, _, ok := KubernetesSecretKey(reference.Reference); !ok {
			return errors.New("a kubernetes secret reference must be SECRET-NAME/KEY in the cluster's InferCrane namespace")
		}
	case ResolverAWSSecretsManager:
		if !awsSecretARN.MatchString(reference.Reference) {
			return errors.New("an aws-secrets-manager reference must be a Secrets Manager secret ARN")
		}
	default:
		return fmt.Errorf("unsupported secret resolver %q", reference.Resolver)
	}
	return nil
}

func (e Environment) Resolve(_ context.Context, reference domain.SecretReference) (string, error) {
	if reference.Resolver != ResolverEnvironment {
		return "", errors.New("secret reference is not a valid environment reference")
	}
	if err := ValidateReference(reference); err != nil {
		return "", err
	}
	if !e.Tenants.Allows(reference.TenantID) {
		return "", ErrTenantNotAllowed
	}
	lookup := e.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, found := lookup(reference.Reference)
	if !found || value == "" {
		return "", fmt.Errorf("secret environment reference %q is unavailable", reference.Reference)
	}
	return value, nil
}
