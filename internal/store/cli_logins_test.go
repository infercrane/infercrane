package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/clilogin"
	"github.com/infercrane/infercrane/internal/domain"
)

func startTestCLILogin(t *testing.T, ctx context.Context, s *Store, at time.Time, maxPending int) (string, string) {
	t.Helper()
	userCode, err := clilogin.NewUserCode()
	if err != nil {
		t.Fatal(err)
	}
	deviceCode, hash, err := clilogin.NewDeviceCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateCLILogin(ctx, domain.CLILoginStart{UserCode: userCode, DeviceCodeHash: hash, DeviceName: "ada-laptop", ClientOS: "darwin/arm64", ClientVersion: "infercrane 1.0.0", CreatedAt: at, ExpiresAt: at.Add(clilogin.RequestTTL)}, maxPending); err != nil {
		t.Fatal(err)
	}
	return deviceCode, userCode
}

func TestCLILoginMintsExactlyOneTenantScopedKey(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	approver, _, err := s.CreatePrincipalScoped(ctx, "acme", "dashboard-operator", authz.Operator, []authz.Action{authz.Read, authz.Deploy})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	deviceCode, userCode := startTestCLILogin(t, ctx, s, now, clilogin.MaxPending)
	hash := clilogin.HashDeviceCode(deviceCode)

	login, err := s.CLILoginByUserCode(ctx, userCode, now)
	if err != nil || login.Status != clilogin.StatusPending || login.DeviceName != "ada-laptop" || login.TenantID != "" {
		t.Fatalf("login=%#v err=%v", login, err)
	}
	if exchange, err := s.ExchangeCLILogin(ctx, hash, now, clilogin.MinPollGap, clilogin.SessionLifetime); err != nil || exchange.Status != clilogin.StatusPending {
		t.Fatalf("first poll=%#v err=%v", exchange, err)
	}
	if exchange, err := s.ExchangeCLILogin(ctx, hash, now.Add(time.Second), clilogin.MinPollGap, clilogin.SessionLifetime); err != nil || exchange.Status != clilogin.StatusSlowDown {
		t.Fatalf("fast poll=%#v err=%v", exchange, err)
	}

	decided, err := s.DecideCLILogin(ctx, userCode, approver, true, approver.Scopes, now.Add(2*time.Second))
	if err != nil || decided.Status != clilogin.StatusApproved || decided.TenantID != "acme" || decided.ApproverName != "dashboard-operator" || decided.DecidedAt == nil {
		t.Fatalf("decided=%#v err=%v", decided, err)
	}
	if _, err = s.DecideCLILogin(ctx, userCode, approver, false, nil, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second decision err=%v", err)
	}

	// Concurrent polls after approval mint one key between them.
	var wg sync.WaitGroup
	results := make(chan domain.CLILoginExchange, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exchange, exchangeErr := s.ExchangeCLILogin(ctx, hash, now.Add(10*time.Second), clilogin.MinPollGap, clilogin.SessionLifetime)
			if exchangeErr != nil {
				t.Error(exchangeErr)
			}
			results <- exchange
		}()
	}
	wg.Wait()
	close(results)
	var issued domain.CLILoginExchange
	approved := 0
	for result := range results {
		switch result.Status {
		case clilogin.StatusApproved:
			approved++
			issued = result
		case clilogin.StatusConsumed:
			if result.Credential != "" {
				t.Fatal("a consumed poll returned a credential")
			}
		default:
			t.Fatalf("unexpected concurrent status %q", result.Status)
		}
	}
	if approved != 1 || issued.Credential == "" || issued.OwnerName != "dashboard-operator" {
		t.Fatalf("approved=%d issued=%#v", approved, issued)
	}

	principal, err := s.AuthenticatePrincipal(ctx, issued.Credential)
	if err != nil || principal.ID != issued.Principal.ID || principal.TenantID != "acme" || principal.Kind != clilogin.PrincipalKind || principal.Role != "operator" || len(principal.Scopes) != 2 || principal.ExpiresAt == nil {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if !principal.ExpiresAt.After(now.Add(clilogin.SessionLifetime - time.Hour)) {
		t.Fatalf("key expires too early: %v", principal.ExpiresAt)
	}
	var storedPlaintext int
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM principals WHERE credential_hash=?`, issued.Credential).Scan(&storedPlaintext); err != nil || storedPlaintext != 0 {
		t.Fatalf("plaintext credential stored=%d err=%v", storedPlaintext, err)
	}

	sessions, err := s.CLISessionsForTenant(ctx, "acme", approver.ID, 50)
	if err != nil || len(sessions) != 1 || sessions[0].ID != principal.ID || sessions[0].LastUsedAt != nil || sessions[0].OwnerName != "dashboard-operator" {
		t.Fatalf("sessions=%#v err=%v", sessions, err)
	}
	if others, _ := s.CLISessionsForTenant(ctx, "acme", "someone-else", 50); len(others) != 0 {
		t.Fatalf("owner filter leaked %#v", others)
	}
	if other, _ := s.CLISessionsForTenant(ctx, "global", "", 50); len(other) != 0 {
		t.Fatalf("tenant filter leaked %#v", other)
	}
	used := now.Add(time.Minute)
	if err = s.TouchCLISession(ctx, principal.ID, used, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = s.TouchCLISession(ctx, principal.ID, used.Add(10*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	session, err := s.CLISession(ctx, "acme", principal.ID)
	if err != nil || session.LastUsedAt == nil || !session.LastUsedAt.Equal(used) {
		t.Fatalf("session=%#v err=%v", session, err)
	}
	if _, err = s.CLISession(ctx, "global", principal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant session err=%v", err)
	}
	if err = s.RevokeCLISession(ctx, "global", principal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant revoke err=%v", err)
	}
	if err = s.RevokeCLISession(ctx, "acme", approver.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a service account through CLI sessions err=%v", err)
	}
	if err = s.RevokeCLISession(ctx, "acme", principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticatePrincipal(ctx, issued.Credential); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key err=%v", err)
	}
	if sessions, _ = s.CLISessionsForTenant(ctx, "acme", "", 50); len(sessions) != 0 {
		t.Fatalf("revoked session still listed: %#v", sessions)
	}
}

func TestCLILoginDenialExpiryCapacityAndPruning(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	approver := domain.Principal{ID: "bootstrap", TenantID: "global", Name: "bootstrap", Role: "admin", Kind: "bootstrap"}
	now := time.Now().UTC().Truncate(time.Millisecond)

	deviceCode, userCode := startTestCLILogin(t, ctx, s, now, 10)
	if _, err := s.DecideCLILogin(ctx, userCode, approver, false, nil, now); err != nil {
		t.Fatal(err)
	}
	if exchange, err := s.ExchangeCLILogin(ctx, clilogin.HashDeviceCode(deviceCode), now, clilogin.MinPollGap, clilogin.SessionLifetime); err != nil || exchange.Status != clilogin.StatusDenied || exchange.Credential != "" {
		t.Fatalf("denied=%#v err=%v", exchange, err)
	}

	deviceCode, userCode = startTestCLILogin(t, ctx, s, now, 10)
	late := now.Add(clilogin.RequestTTL)
	if login, err := s.CLILoginByUserCode(ctx, userCode, late); err != nil || login.Status != clilogin.StatusExpired {
		t.Fatalf("expired view=%#v err=%v", login, err)
	}
	if _, err := s.DecideCLILogin(ctx, userCode, approver, true, []string{"read"}, late); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve expired err=%v", err)
	}
	if exchange, err := s.ExchangeCLILogin(ctx, clilogin.HashDeviceCode(deviceCode), late, clilogin.MinPollGap, clilogin.SessionLifetime); err != nil || exchange.Status != clilogin.StatusExpired {
		t.Fatalf("expired poll=%#v err=%v", exchange, err)
	}
	// An approval that the CLI never collects also expires.
	deviceCode, userCode = startTestCLILogin(t, ctx, s, now, 10)
	if _, err := s.DecideCLILogin(ctx, userCode, approver, true, []string{"read"}, now); err != nil {
		t.Fatal(err)
	}
	if exchange, err := s.ExchangeCLILogin(ctx, clilogin.HashDeviceCode(deviceCode), late, clilogin.MinPollGap, clilogin.SessionLifetime); err != nil || exchange.Status != clilogin.StatusExpired || exchange.Credential != "" {
		t.Fatalf("uncollected approval=%#v err=%v", exchange, err)
	}
	if _, err := s.ExchangeCLILogin(ctx, clilogin.HashDeviceCode("icd_"+"0000000000000000000000000000000000000000000000000000000000000000"), now, clilogin.MinPollGap, clilogin.SessionLifetime); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device code err=%v", err)
	}
	if _, err := s.DecideCLILogin(ctx, "BCDF-GHJK", approver, true, []string{"read"}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user code err=%v", err)
	}

	// The pending bound counts only unexpired pending requests.
	startTestCLILogin(t, ctx, s, now, 10)
	_, hash, _ := clilogin.NewDeviceCode()
	code, _ := clilogin.NewUserCode()
	if _, err := s.CreateCLILogin(ctx, domain.CLILoginStart{UserCode: code, DeviceCodeHash: hash, DeviceName: "x", ClientOS: "x", ClientVersion: "1", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}, 1); !errors.Is(err, domain.ErrCapacityExhausted) {
		t.Fatalf("over capacity err=%v", err)
	}
	_, collision, _ := clilogin.NewDeviceCode()
	if _, err := s.CreateCLILogin(ctx, domain.CLILoginStart{UserCode: userCode, DeviceCodeHash: collision, DeviceName: "x", ClientOS: "x", ClientVersion: "1", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}, 10); !errors.Is(err, ErrConflict) {
		t.Fatalf("user code collision err=%v", err)
	}

	// Requests older than the retention window are pruned by later starts.
	later := now.Add(clilogin.Retention + time.Hour)
	startTestCLILogin(t, ctx, s, later, 10)
	var remaining int
	if err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM cli_login_requests`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("remaining after prune=%d err=%v", remaining, err)
	}
}
