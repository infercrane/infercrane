package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

func TestConsoleIdentityIsMappedTenantSafeAndRevocable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	tenantA, tenantB := "console-a-"+suffix, "console-b-"+suffix
	if err := s.CreateTenant(ctx, tenantA, tenantA); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTenant(ctx, tenantB, tenantB); err != nil {
		t.Fatal(err)
	}
	request := domain.ConsoleIdentityProvisioning{
		ExternalIdentity: domain.ExternalIdentity{Provider: "clerk", ExternalUserID: "user_" + suffix, ExternalOrganizationID: "org_" + suffix},
		DisplayName:      "Ada Operator",
		Role:             "operator",
		Scopes:           []string{"read", "deploy"},
		Access:           true,
	}
	created, err := s.ProvisionConsoleIdentity(ctx, tenantA, request)
	if err != nil || created.UserID == "" || created.TenantID != tenantA || !created.Access {
		t.Fatalf("provision=%#v err=%v", created, err)
	}
	principal, err := s.ResolveExternalPrincipal(ctx, request.ExternalIdentity, WebConsoleAccess)
	if err != nil || principal.ID != created.UserID || principal.TenantID != tenantA || principal.Kind != "human" || principal.Role != "operator" {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if _, err = s.ProvisionConsoleIdentity(ctx, tenantB, request); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-tenant organization remap error=%v, want conflict", err)
	}
	request.Access = false
	if _, err = s.ProvisionConsoleIdentity(ctx, tenantA, request); err != nil {
		t.Fatalf("revoke access: %v", err)
	}
	if _, err = s.ResolveExternalPrincipal(ctx, request.ExternalIdentity, WebConsoleAccess); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked identity error=%v, want not found", err)
	}
}

func TestConsoleAdministrativeListsExposeOnlyTenantSafeMetadata(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)

	principal, _, err := s.CreatePrincipal(ctx, "global", "console-ci", authz.Operator)
	if err != nil {
		t.Fatal(err)
	}
	principals, err := s.PrincipalsForTenant(ctx, "global")
	if err != nil || len(principals) == 0 || principals[0].ID != principal.ID {
		t.Fatalf("principals=%#v err=%v", principals, err)
	}

	operation, _, err := s.EnqueueOperation(ctx, domain.Operation{TenantID: "global", Kind: "test", ResourceType: "deployment", ResourceName: "console", IdempotencyKey: "console-list"})
	if err != nil {
		t.Fatal(err)
	}
	operations, err := s.OperationsForTenant(ctx, "global", time.Time{}, 10)
	if err != nil || len(operations) == 0 || operations[0].ID != operation.ID {
		t.Fatalf("operations=%#v err=%v", operations, err)
	}

	request := domain.ConsoleIdentityProvisioning{ExternalIdentity: domain.ExternalIdentity{Provider: "clerk", ExternalUserID: "user_list", ExternalOrganizationID: "org_list"}, DisplayName: "Ada", Role: "viewer", Access: true}
	created, err := s.ProvisionConsoleIdentity(ctx, "global", request)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.ConsoleIdentitiesForTenant(ctx, "global")
	if err != nil || len(members) != 1 || members[0].UserID != created.UserID || !members[0].Access {
		t.Fatalf("members=%#v err=%v", members, err)
	}
}
