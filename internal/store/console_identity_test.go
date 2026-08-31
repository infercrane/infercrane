package store

import (
	"context"
	"errors"
	"sync"
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

func TestHostedIdentityBootstrapCreatesIsolatedTenantAndPreservesMemberships(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	organizationA := domain.ExternalIdentity{Provider: "clerk", ExternalUserID: "user_owner_" + suffix, ExternalOrganizationID: "org_a_" + suffix}

	owner, err := s.ResolveOrProvisionExternalPrincipal(ctx, organizationA, WebConsoleAccess, "operator")
	if err != nil || owner.Role != "admin" || owner.TenantID == "" || owner.Kind != "human" {
		t.Fatalf("owner=%#v err=%v", owner, err)
	}
	again, err := s.ResolveOrProvisionExternalPrincipal(ctx, organizationA, WebConsoleAccess, "viewer")
	if err != nil || again.ID != owner.ID || again.TenantID != owner.TenantID || again.Role != "admin" {
		t.Fatalf("idempotent owner=%#v err=%v", again, err)
	}

	memberIdentity := domain.ExternalIdentity{Provider: "clerk", ExternalUserID: "user_member_" + suffix, ExternalOrganizationID: organizationA.ExternalOrganizationID}
	member, err := s.ResolveOrProvisionExternalPrincipal(ctx, memberIdentity, WebConsoleAccess, "operator")
	if err != nil || member.TenantID != owner.TenantID || member.Role != "operator" {
		t.Fatalf("member=%#v err=%v", member, err)
	}
	memberAgain, err := s.ResolveOrProvisionExternalPrincipal(ctx, memberIdentity, WebConsoleAccess, "admin")
	if err != nil || memberAgain.Role != "operator" {
		t.Fatalf("membership changed during login: member=%#v err=%v", memberAgain, err)
	}

	organizationB := domain.ExternalIdentity{Provider: "clerk", ExternalUserID: organizationA.ExternalUserID, ExternalOrganizationID: "org_b_" + suffix}
	secondWorkspace, err := s.ResolveOrProvisionExternalPrincipal(ctx, organizationB, WebConsoleAccess, "operator")
	if err != nil || secondWorkspace.ID != owner.ID || secondWorkspace.TenantID == owner.TenantID || secondWorkspace.Role != "admin" {
		t.Fatalf("second workspace=%#v err=%v", secondWorkspace, err)
	}

	_, err = s.ProvisionConsoleIdentity(ctx, owner.TenantID, domain.ConsoleIdentityProvisioning{
		ExternalIdentity: organizationA,
		DisplayName:      owner.Name,
		Role:             "admin",
		Access:           false,
	})
	if err != nil {
		t.Fatalf("revoke bootstrapped owner: %v", err)
	}
	if _, err = s.ResolveOrProvisionExternalPrincipal(ctx, organizationA, WebConsoleAccess, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("login restored explicitly revoked access: %v", err)
	}
}

func TestHostedIdentityBootstrapIsConcurrentAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	identity := domain.ExternalIdentity{Provider: "clerk", ExternalUserID: "user_concurrent_" + suffix, ExternalOrganizationID: "org_concurrent_" + suffix}

	const requests = 8
	principals := make([]domain.Principal, requests)
	errorsByRequest := make([]error, requests)
	var wait sync.WaitGroup
	wait.Add(requests)
	for index := range requests {
		go func() {
			defer wait.Done()
			principals[index], errorsByRequest[index] = s.ResolveOrProvisionExternalPrincipal(ctx, identity, WebConsoleAccess, "operator")
		}()
	}
	wait.Wait()
	for index := range requests {
		if errorsByRequest[index] != nil || principals[index].ID == "" || principals[index].Role != "admin" {
			t.Fatalf("request %d principal=%#v err=%v", index, principals[index], errorsByRequest[index])
		}
		if principals[index].ID != principals[0].ID || principals[index].TenantID != principals[0].TenantID {
			t.Fatalf("request %d produced a second identity: first=%#v current=%#v", index, principals[0], principals[index])
		}
	}
}
