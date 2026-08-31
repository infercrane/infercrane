package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
	"github.com/infercrane/infercrane/internal/domain"
)

type hostedResolver struct {
	identity        domain.ExternalIdentity
	provisioned     bool
	provisionedRole string
	err             error
}

func (r *hostedResolver) ResolveExternalPrincipal(_ context.Context, identity domain.ExternalIdentity, entitlement string) (domain.Principal, error) {
	r.identity = identity
	if r.err != nil {
		return domain.Principal{}, r.err
	}
	if entitlement != "web_console_access" {
		return domain.Principal{}, domain.ErrNotFound
	}
	return domain.Principal{ID: "user-internal", TenantID: "tenant-internal", Name: "Ada", Role: "operator", Kind: "human", Scopes: []string{"read"}}, nil
}

func (r *hostedResolver) ResolveOrProvisionExternalPrincipal(_ context.Context, identity domain.ExternalIdentity, entitlement, role string) (domain.Principal, error) {
	r.identity = identity
	r.provisioned = true
	r.provisionedRole = role
	if entitlement != "web_console_access" {
		return domain.Principal{}, domain.ErrNotFound
	}
	return domain.Principal{ID: "user-provisioned", TenantID: "tenant-provisioned", Name: "Ada", Role: role, Kind: "human", Scopes: []string{"read"}}, nil
}

func TestClerkAuthenticatorVerifiesAndResolvesV2OrganizationClaims(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustMarshalPublicKey(t, &privateKey.PublicKey)})
	resolver := &hostedResolver{}
	authenticator, err := NewClerkAuthenticator(ClerkVerifierConfig{JWTKey: string(publicPEM), Issuer: "https://infercrane.clerk.accounts.dev", Audience: "infercrane-control", AuthorizedParties: []string{"https://app.infercrane.ai"}}, resolver, "web_console_access")
	if err != nil {
		t.Fatal(err)
	}
	token := signClerkToken(t, privateKey, map[string]any{
		"iss": "https://infercrane.clerk.accounts.dev", "sub": "user_external", "sid": "sess_external",
		"azp": "https://app.infercrane.ai", "aud": []string{"infercrane-control"}, "v": 2,
		"o":   map[string]any{"id": "org_external", "rol": "admin"},
		"iat": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	})
	principal, err := authenticator.AuthenticatePrincipal(context.Background(), token)
	if err != nil || principal.ID != "user-internal" {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if resolver.identity.ExternalUserID != "user_external" || resolver.identity.ExternalOrganizationID != "org_external" || resolver.identity.Provider != "clerk" {
		t.Fatalf("resolved identity=%#v", resolver.identity)
	}
}

func TestClerkAuthenticatorRejectsWrongAuthorizedPartyAndMissingEntitlement(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustMarshalPublicKey(t, &privateKey.PublicKey)})
	resolver := &hostedResolver{err: domain.ErrNotFound}
	authenticator, err := NewClerkAuthenticator(ClerkVerifierConfig{JWTKey: string(publicPEM), Issuer: "https://issuer.example", AuthorizedParties: []string{"https://app.infercrane.ai"}}, resolver, "web_console_access")
	if err != nil {
		t.Fatal(err)
	}
	for _, party := range []string{"https://evil.example", "https://app.infercrane.ai"} {
		token := signClerkToken(t, privateKey, map[string]any{
			"iss": "https://issuer.example", "sub": "user_external", "sid": "sess_external", "azp": party,
			"o": map[string]any{"id": "org_external"}, "iat": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
		})
		if _, err = authenticator.AuthenticatePrincipal(context.Background(), token); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("party=%s error=%v, want not found", party, err)
		}
	}
}

func TestClerkAuthenticatorAutoProvisionsOnlyVerifiedMissingIdentities(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustMarshalPublicKey(t, &privateKey.PublicKey)})
	resolver := &hostedResolver{err: domain.ErrNotFound}
	authenticator, err := NewClerkAuthenticator(ClerkVerifierConfig{JWTKey: string(publicPEM), Issuer: "https://infercrane.clerk.accounts.dev", Audience: "infercrane-control", AuthorizedParties: []string{"https://app.infercrane.ai"}, AutoProvision: true}, resolver, "web_console_access")
	if err != nil {
		t.Fatal(err)
	}
	token := signClerkToken(t, privateKey, map[string]any{
		"iss": "https://infercrane.clerk.accounts.dev", "sub": "user_external", "sid": "sess_external",
		"azp": "https://app.infercrane.ai", "aud": []string{"infercrane-control"}, "v": 2,
		"o":   map[string]any{"id": "org_external", "rol": "admin"},
		"iat": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	})
	principal, err := authenticator.AuthenticatePrincipal(context.Background(), token)
	if err != nil || principal.ID != "user-provisioned" || !resolver.provisioned || resolver.provisionedRole != "admin" {
		t.Fatalf("principal=%#v identity=%#v auto=%v provisioned=%v role=%q err=%v", principal, resolver.identity, authenticator.autoProvision, resolver.provisioned, resolver.provisionedRole, err)
	}

	resolver.err = errors.New("database unavailable")
	resolver.provisioned = false
	if _, err = authenticator.AuthenticatePrincipal(context.Background(), token); err == nil || resolver.provisioned {
		t.Fatalf("non-not-found error=%v provisioned=%v", err, resolver.provisioned)
	}
}

func mustMarshalPublicKey(t *testing.T, key *rsa.PublicKey) []byte {
	t.Helper()
	encoded, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func signClerkToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(claims).CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
