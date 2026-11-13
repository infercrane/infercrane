package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/clilogin"
	"github.com/infercrane/infercrane/internal/domain"
)

type memoryLogin struct {
	request    domain.CLILoginRequest
	deviceHash string
	role       string
	scopes     []string
	lastPolled time.Time
}

// memoryCLILogins mirrors the PostgreSQL semantics the API relies on.
type memoryCLILogins struct {
	*fakeStore
	mu        sync.Mutex
	logins    map[string]*memoryLogin
	sessions  map[string]domain.CLISession
	keys      map[string]domain.Principal
	revoked   map[string]bool
	touches   int
	refreshes int
}

func newMemoryCLILogins() *memoryCLILogins {
	return &memoryCLILogins{fakeStore: &fakeStore{}, logins: map[string]*memoryLogin{}, sessions: map[string]domain.CLISession{}, keys: map[string]domain.Principal{}, revoked: map[string]bool{}}
}

func (m *memoryCLILogins) CreateCLILogin(_ context.Context, start domain.CLILoginStart, maxPending int) (domain.CLILoginRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := 0
	for _, login := range m.logins {
		if login.request.Status == clilogin.StatusPending && login.request.ExpiresAt.After(start.CreatedAt) {
			pending++
		}
	}
	if pending >= maxPending {
		return domain.CLILoginRequest{}, domain.ErrCapacityExhausted
	}
	if _, exists := m.logins[start.UserCode]; exists {
		return domain.CLILoginRequest{}, domain.ErrConflict
	}
	request := domain.CLILoginRequest{ID: "login-" + start.UserCode, UserCode: start.UserCode, DeviceName: start.DeviceName, ClientOS: start.ClientOS, ClientVersion: start.ClientVersion, Status: clilogin.StatusPending, CreatedAt: start.CreatedAt, ExpiresAt: start.ExpiresAt}
	m.logins[start.UserCode] = &memoryLogin{request: request, deviceHash: start.DeviceCodeHash}
	return request, nil
}

func (m *memoryCLILogins) view(login *memoryLogin, now time.Time) domain.CLILoginRequest {
	out := login.request
	if out.Status == clilogin.StatusPending && !out.ExpiresAt.After(now) {
		out.Status = clilogin.StatusExpired
	}
	return out
}

func (m *memoryCLILogins) CLILoginByUserCode(_ context.Context, code string, now time.Time) (domain.CLILoginRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	login, ok := m.logins[code]
	if !ok {
		return domain.CLILoginRequest{}, domain.ErrNotFound
	}
	return m.view(login, now), nil
}

func (m *memoryCLILogins) DecideCLILogin(_ context.Context, code string, approver domain.Principal, approve bool, scopes []string, now time.Time) (domain.CLILoginRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	login, ok := m.logins[code]
	if !ok {
		return domain.CLILoginRequest{}, domain.ErrNotFound
	}
	if login.request.Status != clilogin.StatusPending || !login.request.ExpiresAt.After(now) {
		return domain.CLILoginRequest{}, domain.ErrConflict
	}
	login.request.Status = clilogin.StatusDenied
	if approve {
		login.request.Status = clilogin.StatusApproved
		login.role, login.scopes = approver.Role, scopes
	}
	login.request.TenantID, login.request.ApproverID, login.request.ApproverName = approver.TenantID, approver.ID, approver.Name
	login.request.DecidedAt = &now
	return m.view(login, now), nil
}

func (m *memoryCLILogins) ExchangeCLILogin(_ context.Context, hash string, now time.Time, gap, lifetime time.Duration) (domain.CLILoginExchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, login := range m.logins {
		if login.deviceHash != hash {
			continue
		}
		switch {
		case login.request.Status == clilogin.StatusDenied || login.request.Status == clilogin.StatusConsumed:
			return domain.CLILoginExchange{Status: login.request.Status}, nil
		case !login.request.ExpiresAt.After(now):
			return domain.CLILoginExchange{Status: clilogin.StatusExpired}, nil
		case login.request.Status == clilogin.StatusPending:
			status := clilogin.StatusPending
			if !login.lastPolled.IsZero() && now.Sub(login.lastPolled) < gap {
				status = clilogin.StatusSlowDown
			}
			login.lastPolled = now
			return domain.CLILoginExchange{Status: status}, nil
		}
		login.request.Status = clilogin.StatusConsumed
		id := "key-" + login.request.UserCode
		expires := now.Add(lifetime)
		principal := domain.Principal{ID: id, TenantID: login.request.TenantID, Name: clilogin.PrincipalName(login.request.DeviceName, id), Role: login.role, Kind: clilogin.PrincipalKind, Scopes: login.scopes, ExpiresAt: &expires}
		credential := "ic_" + strings.Repeat("c", 60) + login.request.UserCode[:4]
		m.keys[credential] = principal
		m.sessions[id] = domain.CLISession{ID: id, TenantID: principal.TenantID, OwnerID: login.request.ApproverID, OwnerName: login.request.ApproverName, DeviceName: login.request.DeviceName, ClientOS: login.request.ClientOS, ClientVersion: login.request.ClientVersion, CreatedAt: now, ExpiresAt: &expires}
		return domain.CLILoginExchange{Status: clilogin.StatusApproved, Principal: principal, Credential: credential, OwnerName: login.request.ApproverName}, nil
	}
	return domain.CLILoginExchange{}, domain.ErrNotFound
}

func (m *memoryCLILogins) CLISessionsForTenant(_ context.Context, tenant, owner string, _ int) ([]domain.CLISession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.CLISession{}
	for id, session := range m.sessions {
		if session.TenantID == tenant && (owner == "" || session.OwnerID == owner) && !m.revoked[id] {
			out = append(out, session)
		}
	}
	return out, nil
}

func (m *memoryCLILogins) CLISession(_ context.Context, tenant, id string) (domain.CLISession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok || session.TenantID != tenant || m.revoked[id] {
		return domain.CLISession{}, domain.ErrNotFound
	}
	return session, nil
}

func (m *memoryCLILogins) RevokeCLISession(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	if !ok || session.TenantID != tenant || m.revoked[id] {
		return domain.ErrNotFound
	}
	m.revoked[id] = true
	return nil
}

func (m *memoryCLILogins) TouchCLISession(context.Context, string, time.Time, time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touches++
	return nil
}

// AuthenticatePrincipal resolves dashboard tokens and issued CLI keys.
func (m *memoryCLILogins) AuthenticatePrincipal(_ context.Context, token string) (domain.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if principal, ok := dashboardPrincipals[token]; ok {
		return principal, nil
	}
	if principal, ok := m.keys[token]; ok && !m.revoked[principal.ID] {
		return principal, nil
	}
	return domain.Principal{}, domain.ErrNotFound
}

var dashboardPrincipals = map[string]domain.Principal{
	"ada-session":   {ID: "user-ada", TenantID: "acme", Name: "Ada", Role: "operator", Kind: "human", Scopes: []string{"read", "deploy"}},
	"grace-session": {ID: "user-grace", TenantID: "acme", Name: "Grace", Role: "admin", Kind: "human", Scopes: authz.DefaultScopeNames(authz.Admin)},
	"bob-session":   {ID: "user-bob", TenantID: "other", Name: "Bob", Role: "admin", Kind: "human", Scopes: authz.DefaultScopeNames(authz.Admin)},
	"endpoint-only": {ID: "token-1", TenantID: "acme", Name: "token", Role: "viewer", Kind: "inference_token", Scopes: []string{"read"}},
}

type cliLoginHarness struct {
	t     *testing.T
	store *memoryCLILogins
	h     http.Handler
	now   time.Time
}

func newCLILoginHarness(t *testing.T) *cliLoginHarness {
	t.Helper()
	harness := &cliLoginHarness{t: t, store: newMemoryCLILogins(), now: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	harness.h = API{
		Store: harness.store, Authenticator: harness.store, ConsoleURL: "https://console.example/",
		CLISessionUsage: &CLISessionUsage{Store: harness.store, Now: func() time.Time { return harness.now }},
		CredentialRefresh: func(context.Context) error {
			harness.store.mu.Lock()
			harness.store.refreshes++
			harness.store.mu.Unlock()
			return nil
		},
		Now: func() time.Time { return harness.now },
	}.Handler()
	return harness
}

func (h *cliLoginHarness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	h.h.ServeHTTP(response, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response.Code, decoded
}

func (h *cliLoginHarness) start() (deviceCode, userCode string) {
	h.t.Helper()
	status, body := h.do(http.MethodPost, "/api/v1/cli/logins", "", map[string]string{"device_name": "ada-laptop", "client_os": "darwin/arm64", "client_version": "infercrane 1.0.0"})
	if status != http.StatusCreated {
		h.t.Fatalf("start status=%d body=%v", status, body)
	}
	return body["device_code"].(string), body["user_code"].(string)
}

func (h *cliLoginHarness) poll(deviceCode string) map[string]any {
	h.t.Helper()
	status, body := h.do(http.MethodPost, "/api/v1/cli/logins/token", "", map[string]string{"device_code": deviceCode})
	if status != http.StatusOK {
		h.t.Fatalf("poll status=%d body=%v", status, body)
	}
	return body
}

func TestCLILoginIsApprovedInTheDashboardAndYieldsOneScopedKey(t *testing.T) {
	h := newCLILoginHarness(t)
	status, started := h.do(http.MethodPost, "/api/v1/cli/logins", "", map[string]string{"device_name": "ada-laptop", "client_os": "darwin/arm64", "client_version": "infercrane 1.0.0"})
	if status != http.StatusCreated {
		t.Fatalf("start=%d %v", status, started)
	}
	deviceCode, userCode := started["device_code"].(string), started["user_code"].(string)
	if started["verification_uri_complete"] != "https://console.example/cli/authorize?code="+userCode || started["interval"] != float64(3) || started["expires_in"] != float64(600) {
		t.Fatalf("start=%v", started)
	}
	for _, stored := range h.store.logins {
		if stored.deviceHash == deviceCode || strings.Contains(stored.deviceHash, deviceCode[4:]) {
			t.Fatal("the device code itself was stored")
		}
	}

	if body := h.poll(deviceCode); body["status"] != "pending" || body["credential"] != nil {
		t.Fatalf("first poll=%v", body)
	}
	if body := h.poll(deviceCode); body["status"] != "slow_down" {
		t.Fatalf("immediate second poll=%v", body)
	}

	status, view := h.do(http.MethodGet, "/api/v1/cli/logins/"+strings.ToLower(strings.ReplaceAll(userCode, "-", "")), "ada-session", nil)
	login, _ := view["login"].(map[string]any)
	if status != http.StatusOK || login["device_name"] != "ada-laptop" || login["status"] != "pending" || login["user_code"] != userCode {
		t.Fatalf("view=%d %v", status, view)
	}
	status, decided := h.do(http.MethodPost, "/api/v1/cli/logins/"+userCode+"/approve", "ada-session", nil)
	if login, _ = decided["login"].(map[string]any); status != http.StatusOK || login["status"] != "approved" || login["decided_by"] != "Ada" {
		t.Fatalf("approve=%d %v", status, decided)
	}
	if status, _ = h.do(http.MethodPost, "/api/v1/cli/logins/"+userCode+"/deny", "ada-session", nil); status != http.StatusConflict {
		t.Fatalf("deny after approve=%d", status)
	}

	h.now = h.now.Add(5 * time.Second)
	exchanged := h.poll(deviceCode)
	credential, _ := exchanged["credential"].(string)
	principal, _ := exchanged["principal"].(map[string]any)
	if exchanged["status"] != "approved" || credential == "" || exchanged["owner_name"] != "Ada" || exchanged["console_url"] != "https://console.example" {
		t.Fatalf("exchange=%v", exchanged)
	}
	if principal["tenant_id"] != "acme" || principal["role"] != "operator" || principal["kind"] != clilogin.PrincipalKind || len(principal["scopes"].([]any)) != 2 || principal["expires_at"] == nil {
		t.Fatalf("issued principal=%v", principal)
	}
	if h.store.refreshes == 0 {
		t.Fatal("credential cache was not refreshed after issuing the key")
	}
	if again := h.poll(deviceCode); again["status"] != "consumed" || again["credential"] != nil {
		t.Fatalf("second exchange=%v", again)
	}

	// The key works, records its use once per minute, and cannot approve.
	for i := 0; i < 3; i++ {
		if status, _ = h.do(http.MethodGet, "/api/v1/whoami", credential, nil); status != http.StatusOK {
			t.Fatalf("whoami with issued key=%d", status)
		}
	}
	if h.store.touches != 1 {
		t.Fatalf("last-use writes=%d, want 1 within a minute", h.store.touches)
	}
	_, other := h.start()
	if status, _ = h.do(http.MethodPost, "/api/v1/cli/logins/"+other+"/approve", credential, nil); status != http.StatusForbidden {
		t.Fatalf("CLI key approving another login=%d", status)
	}

	// Ada sees her session; Grace (admin) sees it too and may revoke it.
	status, listed := h.do(http.MethodGet, "/api/v1/cli/sessions", "ada-session", nil)
	sessions, _ := listed["data"].([]any)
	if status != http.StatusOK || len(sessions) != 1 || sessions[0].(map[string]any)["device_name"] != "ada-laptop" || sessions[0].(map[string]any)["owner_name"] != "Ada" {
		t.Fatalf("sessions=%d %v", status, listed)
	}
	status, listed = h.do(http.MethodGet, "/api/v1/cli/sessions", credential, nil)
	if sessions, _ = listed["data"].([]any); status != http.StatusOK || len(sessions) != 1 || sessions[0].(map[string]any)["current"] != true {
		t.Fatalf("sessions from the key itself=%d %v", status, listed)
	}
	if status, _ = h.do(http.MethodGet, "/api/v1/cli/sessions", "bob-session", nil); status != http.StatusOK {
		t.Fatalf("other tenant list=%d", status)
	}
	sessionID := principal["id"].(string)
	if status, _ = h.do(http.MethodDelete, "/api/v1/cli/sessions/"+sessionID, "bob-session", nil); status != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke=%d", status)
	}
	if status, _ = h.do(http.MethodDelete, "/api/v1/cli/sessions/"+sessionID, "grace-session", nil); status != http.StatusNoContent {
		t.Fatalf("admin revoke=%d", status)
	}
	if status, _ = h.do(http.MethodGet, "/api/v1/whoami", credential, nil); status != http.StatusUnauthorized {
		t.Fatalf("revoked key=%d", status)
	}
}

func TestCLILoginDenialExpiryAndBounds(t *testing.T) {
	h := newCLILoginHarness(t)
	deviceCode, userCode := h.start()
	if status, _ := h.do(http.MethodPost, "/api/v1/cli/logins/"+userCode+"/deny", "ada-session", nil); status != http.StatusOK {
		t.Fatalf("deny=%d", status)
	}
	if body := h.poll(deviceCode); body["status"] != "denied" || body["credential"] != nil {
		t.Fatalf("denied poll=%v", body)
	}
	// A decided request is hidden from other tenants.
	if status, _ := h.do(http.MethodGet, "/api/v1/cli/logins/"+userCode, "bob-session", nil); status != http.StatusNotFound {
		t.Fatalf("other tenant view=%d", status)
	}

	deviceCode, userCode = h.start()
	h.now = h.now.Add(clilogin.RequestTTL)
	status, view := h.do(http.MethodGet, "/api/v1/cli/logins/"+userCode, "ada-session", nil)
	if login, _ := view["login"].(map[string]any); status != http.StatusOK || login["status"] != "expired" {
		t.Fatalf("expired view=%d %v", status, view)
	}
	if status, _ = h.do(http.MethodPost, "/api/v1/cli/logins/"+userCode+"/approve", "ada-session", nil); status != http.StatusConflict {
		t.Fatalf("approve expired=%d", status)
	}
	if body := h.poll(deviceCode); body["status"] != "expired" {
		t.Fatalf("expired poll=%v", body)
	}

	for _, test := range []struct {
		path, token string
		body        any
		want        int
	}{
		{"/api/v1/cli/logins", "", map[string]string{"device_name": "", "client_os": "x", "client_version": "1"}, http.StatusUnprocessableEntity},
		{"/api/v1/cli/logins", "", map[string]any{"device_name": "x", "client_os": "x", "client_version": "1", "role": "admin"}, http.StatusBadRequest},
		{"/api/v1/cli/logins/token", "", map[string]string{"device_code": "ic_not-a-device-code"}, http.StatusBadRequest},
		{"/api/v1/cli/logins/token", "", map[string]string{"device_code": "icd_" + strings.Repeat("0", 64)}, http.StatusNotFound},
	} {
		if status, body := h.do(http.MethodPost, test.path, test.token, test.body); status != test.want {
			t.Fatalf("POST %s %v = %d %v, want %d", test.path, test.body, status, body, test.want)
		}
	}
	for _, path := range []string{"/api/v1/cli/logins/NOT-REAL", "/api/v1/cli/logins/BCDF-GHJK"} {
		if status, _ := h.do(http.MethodGet, path, "ada-session", nil); status != http.StatusNotFound {
			t.Fatalf("GET %s=%d", path, status)
		}
	}
	if status, _ := h.do(http.MethodGet, "/api/v1/cli/logins/"+userCode, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous view=%d", status)
	}
	if status, _ := h.do(http.MethodGet, "/api/v1/cli/logins/"+userCode, "endpoint-only", nil); status != http.StatusForbidden {
		t.Fatalf("inference token view=%d", status)
	}
}

func TestCLILoginStartIsBoundedAndLogoutKeepsSharedKeys(t *testing.T) {
	h := newCLILoginHarness(t)
	for i := 0; i < clilogin.MaxPending; i++ {
		code := fmt.Sprintf("seeded-%d", i)
		h.store.logins[code] = &memoryLogin{request: domain.CLILoginRequest{UserCode: code, Status: clilogin.StatusPending, ExpiresAt: h.now.Add(time.Minute)}}
	}
	if status, body := h.do(http.MethodPost, "/api/v1/cli/logins", "", map[string]string{"device_name": "x", "client_os": "x", "client_version": "1"}); status != http.StatusTooManyRequests {
		t.Fatalf("over the pending bound=%d %v", status, body)
	}
	if status, body := h.do(http.MethodDelete, "/api/v1/cli/sessions/current", "grace-session", nil); status != http.StatusConflict || body["error"].(map[string]any)["code"] != "not_cli_session" {
		t.Fatalf("logout with a dashboard session=%d %v", status, body)
	}
}

func TestCLILogoutRevokesOnlyTheCallingKey(t *testing.T) {
	h := newCLILoginHarness(t)
	issue := func() (string, string) {
		deviceCode, userCode := h.start()
		if status, _ := h.do(http.MethodPost, "/api/v1/cli/logins/"+userCode+"/approve", "ada-session", nil); status != http.StatusOK {
			t.Fatalf("approve=%d", status)
		}
		h.now = h.now.Add(5 * time.Second)
		body := h.poll(deviceCode)
		return body["credential"].(string), body["principal"].(map[string]any)["id"].(string)
	}
	laptop, _ := issue()
	desktop, desktopID := issue()
	if status, _ := h.do(http.MethodDelete, "/api/v1/cli/sessions/"+desktopID, "endpoint-only", nil); status != http.StatusForbidden {
		t.Fatalf("inference token revoke=%d", status)
	}
	if status, _ := h.do(http.MethodDelete, "/api/v1/cli/sessions/current", laptop, nil); status != http.StatusNoContent {
		t.Fatalf("logout=%d", status)
	}
	if status, _ := h.do(http.MethodGet, "/api/v1/whoami", laptop, nil); status != http.StatusUnauthorized {
		t.Fatalf("logged-out key=%d", status)
	}
	if status, _ := h.do(http.MethodGet, "/api/v1/whoami", desktop, nil); status != http.StatusOK {
		t.Fatalf("other key after logout=%d", status)
	}
}
