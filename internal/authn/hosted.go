package authn

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	clerk "github.com/clerk/clerk-sdk-go/v2"
	clerkjwt "github.com/clerk/clerk-sdk-go/v2/jwt"
	"github.com/infercrane/infercrane/internal/domain"
)

type PrincipalAuthenticator interface {
	AuthenticatePrincipal(context.Context, string) (domain.Principal, error)
}

type ExternalPrincipalResolver interface {
	ResolveExternalPrincipal(context.Context, domain.ExternalIdentity, string) (domain.Principal, error)
}

type ExternalPrincipalProvisioner interface {
	ResolveOrProvisionExternalPrincipal(context.Context, domain.ExternalIdentity, string, string) (domain.Principal, error)
}

type Chain struct {
	Authenticators []PrincipalAuthenticator
}

func (c Chain) AuthenticatePrincipal(ctx context.Context, token string) (domain.Principal, error) {
	for _, authenticator := range c.Authenticators {
		if authenticator == nil {
			continue
		}
		principal, err := authenticator.AuthenticatePrincipal(ctx, token)
		if err == nil && principal.ID != "" {
			return principal, nil
		}
	}
	return domain.Principal{}, domain.ErrNotFound
}

type ClerkVerifierConfig struct {
	JWTKey, Issuer, Audience string
	AuthorizedParties        []string
	AutoProvision            bool
}

type ClerkAuthenticator struct {
	key               *clerk.JSONWebKey
	issuer, audience  string
	authorizedParties []string
	autoProvision     bool
	Resolver          ExternalPrincipalResolver
	Entitlement       string
}

type clerkV2Claims struct {
	Organization struct {
		ID   string `json:"id"`
		Role string `json:"rol"`
	} `json:"o"`
}

func NewClerkAuthenticator(config ClerkVerifierConfig, resolver ExternalPrincipalResolver, entitlement string) (*ClerkAuthenticator, error) {
	if strings.TrimSpace(config.JWTKey) == "" || strings.TrimSpace(config.Issuer) == "" {
		return nil, errors.New("Clerk JWT public key and issuer are required")
	}
	if len(config.AuthorizedParties) == 0 {
		return nil, errors.New("at least one Clerk authorized party is required")
	}
	if resolver == nil || entitlement == "" {
		return nil, errors.New("hosted identity resolver and entitlement are required")
	}
	key, err := clerk.JSONWebKeyFromPEM(config.JWTKey)
	if err != nil {
		return nil, fmt.Errorf("parse Clerk JWT public key: %w", err)
	}
	if config.AutoProvision {
		if _, ok := resolver.(ExternalPrincipalProvisioner); !ok {
			return nil, errors.New("hosted identity resolver does not support automatic provisioning")
		}
	}
	return &ClerkAuthenticator{key: key, issuer: strings.TrimSuffix(config.Issuer, "/"), audience: config.Audience, authorizedParties: append([]string(nil), config.AuthorizedParties...), autoProvision: config.AutoProvision, Resolver: resolver, Entitlement: entitlement}, nil
}

func (a *ClerkAuthenticator) AuthenticatePrincipal(ctx context.Context, token string) (domain.Principal, error) {
	if a == nil || a.key == nil || token == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	claims, err := clerkjwt.Verify(ctx, &clerkjwt.VerifyParams{
		Token: token,
		JWK:   a.key,
		AuthorizedPartyHandler: func(actual string) bool {
			for _, expected := range a.authorizedParties {
				if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1 {
					return true
				}
			}
			return false
		},
		CustomClaimsConstructor: func(context.Context) any { return &clerkV2Claims{} },
	})
	if err != nil || claims == nil {
		return domain.Principal{}, domain.ErrNotFound
	}
	if strings.TrimSuffix(claims.Issuer, "/") != a.issuer || claims.Subject == "" || claims.SessionID == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	if a.audience != "" && !containsString(claims.Audience, a.audience) {
		return domain.Principal{}, domain.ErrNotFound
	}
	organizationID := claims.ActiveOrganizationID
	organizationRole := ""
	if organizationID == "" {
		if custom, ok := claims.Custom.(*clerkV2Claims); ok {
			organizationID = custom.Organization.ID
			organizationRole = custom.Organization.Role
		}
	} else if custom, ok := claims.Custom.(*clerkV2Claims); ok {
		organizationRole = custom.Organization.Role
	}
	if organizationID == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	identity := domain.ExternalIdentity{Provider: "clerk", ExternalUserID: claims.Subject, ExternalOrganizationID: organizationID}
	principal, err := a.Resolver.ResolveExternalPrincipal(ctx, identity, a.Entitlement)
	if err == nil || !a.autoProvision || !errors.Is(err, domain.ErrNotFound) {
		return principal, err
	}
	provisioner := a.Resolver.(ExternalPrincipalProvisioner)
	return provisioner.ResolveOrProvisionExternalPrincipal(ctx, identity, a.Entitlement, clerkMembershipRole(organizationRole))
}

func clerkMembershipRole(clerkRole string) string {
	clerkRole = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(clerkRole)), "org:")
	if clerkRole == "admin" {
		return "admin"
	}
	return "operator"
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
