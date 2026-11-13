package controlapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/clilogin"
	"github.com/infercrane/infercrane/internal/domain"
)

// cliLoginStore is the persistence boundary for signing the CLI in through
// the dashboard. Only hashes of device codes and keys are stored.
type cliLoginStore interface {
	CreateCLILogin(context.Context, domain.CLILoginStart, int) (domain.CLILoginRequest, error)
	CLILoginByUserCode(context.Context, string, time.Time) (domain.CLILoginRequest, error)
	DecideCLILogin(context.Context, string, domain.Principal, bool, []string, time.Time) (domain.CLILoginRequest, error)
	ExchangeCLILogin(context.Context, string, time.Time, time.Duration, time.Duration) (domain.CLILoginExchange, error)
	CLISessionsForTenant(context.Context, string, string, int) ([]domain.CLISession, error)
	CLISession(context.Context, string, string) (domain.CLISession, error)
	RevokeCLISession(context.Context, string, string) error
}

// CLISessionUsage records when a CLI key was last used on the control API. It
// writes at most once per Gap per key, and never runs on the inference path.
type CLISessionUsage struct {
	Store interface {
		TouchCLISession(context.Context, string, time.Time, time.Duration) error
	}
	Gap time.Duration
	Now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

const cliSessionUsageMaxTracked = 10_000

func (u *CLISessionUsage) Touch(ctx context.Context, id string) {
	if u == nil || u.Store == nil || id == "" {
		return
	}
	now := time.Now().UTC()
	if u.Now != nil {
		now = u.Now()
	}
	gap := u.Gap
	if gap <= 0 {
		gap = time.Minute
	}
	u.mu.Lock()
	if last, ok := u.seen[id]; ok && now.Sub(last) < gap {
		u.mu.Unlock()
		return
	}
	if u.seen == nil || len(u.seen) >= cliSessionUsageMaxTracked {
		u.seen = make(map[string]time.Time)
	}
	u.seen[id] = now
	u.mu.Unlock()
	touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = u.Store.TouchCLISession(touchCtx, id, now, gap)
}

func (a API) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a API) cliLogins(w http.ResponseWriter) (cliLoginStore, bool) {
	store, ok := a.Store.(cliLoginStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "CLI sign-in is not configured on this control plane")
	}
	return store, ok
}

// startCLILogin is unauthenticated: the CLI has no credential yet. It returns
// the device code once; the control plane keeps only its hash.
func (a API) startCLILogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	var request clilogin.Client
	if !decodeMutationBody(w, r, &request) {
		return
	}
	client, err := request.Normalize()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		userCode, codeErr := clilogin.NewUserCode()
		deviceCode, deviceHash, deviceErr := clilogin.NewDeviceCode()
		if codeErr != nil || deviceErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "CLI sign-in codes could not be generated")
			return
		}
		created := a.now()
		login, createErr := store.CreateCLILogin(r.Context(), domain.CLILoginStart{UserCode: userCode, DeviceCodeHash: deviceHash, DeviceName: client.DeviceName, ClientOS: client.OS, ClientVersion: client.Version, CreatedAt: created, ExpiresAt: created.Add(clilogin.RequestTTL)}, clilogin.MaxPending)
		if errors.Is(createErr, domain.ErrConflict) {
			continue
		}
		if errors.Is(createErr, domain.ErrCapacityExhausted) {
			writeError(w, http.StatusTooManyRequests, "too_many_pending_logins", "Too many CLI sign-ins are waiting for approval. Try again in a few minutes.")
			return
		}
		if createErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "CLI sign-in could not be started")
			return
		}
		response := map[string]any{
			"device_code": deviceCode,
			"user_code":   login.UserCode,
			"expires_at":  login.ExpiresAt,
			"expires_in":  int(clilogin.RequestTTL / time.Second),
			"interval":    int(clilogin.PollInterval / time.Second),
		}
		if verification := a.cliVerificationURL(""); verification != "" {
			response["verification_uri"] = verification
			response["verification_uri_complete"] = a.cliVerificationURL(login.UserCode)
		}
		writeJSON(w, http.StatusCreated, response)
		return
	}
	writeError(w, http.StatusServiceUnavailable, "code_collision", "CLI sign-in codes collided repeatedly; retry the request")
}

func (a API) cliVerificationURL(userCode string) string {
	base := strings.TrimRight(strings.TrimSpace(a.ConsoleURL), "/")
	if base == "" {
		return ""
	}
	if userCode == "" {
		return base + "/cli/authorize"
	}
	return base + "/cli/authorize?code=" + url.QueryEscape(userCode)
}

// exchangeCLILogin is the CLI's unauthenticated poll. The device code is the
// only proof, and a consumed code never yields a second credential.
func (a API) exchangeCLILogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	var request struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	hash := clilogin.HashDeviceCode(request.DeviceCode)
	if hash == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "device_code is not a CLI sign-in code")
		return
	}
	exchange, err := store.ExchangeCLILogin(r.Context(), hash, a.now(), clilogin.MinPollGap, clilogin.SessionLifetime)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "CLI sign-in was not found; run infercrane login again")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "CLI sign-in could not be checked")
		return
	}
	switch exchange.Status {
	case clilogin.StatusApproved:
		cacheSynchronized := a.refreshCredentialCache(r.Context())
		principal := exchange.Principal
		_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "cli_session.create", ResourceType: "principal", ResourceName: principal.ID, Outcome: "succeeded"})
		response := map[string]any{
			"status":     exchange.Status,
			"credential": exchange.Credential,
			"owner_name": exchange.OwnerName,
			"principal": map[string]any{
				"id": principal.ID, "tenant_id": principal.TenantID, "name": principal.Name, "role": principal.Role,
				"kind": principal.Kind, "scopes": principal.Scopes, "expires_at": principal.ExpiresAt,
			},
			"credential_cache_synchronized": cacheSynchronized,
		}
		if console := strings.TrimRight(strings.TrimSpace(a.ConsoleURL), "/"); console != "" {
			response["console_url"] = console
		}
		writeJSON(w, http.StatusOK, response)
	case clilogin.StatusPending:
		writeJSON(w, http.StatusOK, map[string]any{"status": exchange.Status, "interval": int(clilogin.PollInterval / time.Second)})
	case clilogin.StatusSlowDown:
		writeJSON(w, http.StatusOK, map[string]any{"status": exchange.Status, "interval": int((clilogin.PollInterval + 2*time.Second) / time.Second)})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": exchange.Status})
	}
}

func cliLoginResponse(login domain.CLILoginRequest, viewer domain.Principal) map[string]any {
	response := map[string]any{
		"user_code": login.UserCode, "device_name": login.DeviceName, "client_os": login.ClientOS,
		"client_version": login.ClientVersion, "status": login.Status,
		"created_at": login.CreatedAt, "expires_at": login.ExpiresAt,
	}
	if login.DecidedAt != nil {
		response["decided_at"] = login.DecidedAt
	}
	if login.ApproverName != "" && login.TenantID == viewer.TenantID {
		response["decided_by"] = login.ApproverName
	}
	return response
}

// approverOnly keeps CLI keys and endpoint tokens from approving sign-ins.
func approverOnly(w http.ResponseWriter, principal domain.Principal) bool {
	if clilogin.ApproverAllowed(principal.Kind) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "CLI sign-ins are approved from the dashboard")
	return false
}

func (a API) cliLoginForViewer(w http.ResponseWriter, r *http.Request, store cliLoginStore, principal domain.Principal) (domain.CLILoginRequest, bool) {
	code, valid := clilogin.NormalizeUserCode(r.PathValue("code"))
	if !valid {
		writeError(w, http.StatusNotFound, "not_found", "CLI sign-in code was not found")
		return domain.CLILoginRequest{}, false
	}
	login, err := store.CLILoginByUserCode(r.Context(), code, a.now())
	if err == nil && login.TenantID != "" && login.TenantID != principal.TenantID {
		err = domain.ErrNotFound
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "CLI sign-in code was not found")
		return domain.CLILoginRequest{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "CLI sign-in lookup failed")
		return domain.CLILoginRequest{}, false
	}
	return login, true
}

func (a API) cliLogin(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if !approverOnly(w, principal) {
		return
	}
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	login, ok := a.cliLoginForViewer(w, r, store, principal)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": cliLoginResponse(login, principal)})
}

func (a API) approveCLILogin(w http.ResponseWriter, r *http.Request) {
	a.decideCLILogin(w, r, true)
}

func (a API) denyCLILogin(w http.ResponseWriter, r *http.Request) {
	a.decideCLILogin(w, r, false)
}

func (a API) decideCLILogin(w http.ResponseWriter, r *http.Request, approve bool) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if !approverOnly(w, principal) {
		return
	}
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	login, ok := a.cliLoginForViewer(w, r, store, principal)
	if !ok {
		return
	}
	var scopes []string
	if approve {
		issued, err := clilogin.IssuedScopes(authz.Role(principal.Role), principal.Scopes)
		if err != nil {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
			return
		}
		for _, scope := range issued {
			scopes = append(scopes, string(scope))
		}
	}
	decided, err := store.DecideCLILogin(r.Context(), login.UserCode, principal, approve, scopes, a.now())
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "CLI sign-in code was not found")
		return
	}
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "not_pending", "This sign-in was already approved, denied, or has expired. Run infercrane login again.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "CLI sign-in decision could not be saved")
		return
	}
	action := "cli_login.deny"
	if approve {
		action = "cli_login.approve"
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: action, ResourceType: "cli_login", ResourceName: decided.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"login": cliLoginResponse(decided, principal)})
}

// cliSessionOwner is the person a principal acts for. A CLI key acts for the
// person who approved it.
func (a API) cliSessionOwner(ctx context.Context, store cliLoginStore, principal domain.Principal) (string, error) {
	if principal.Kind != clilogin.PrincipalKind {
		return principal.ID, nil
	}
	session, err := store.CLISession(ctx, principal.TenantID, principal.ID)
	if err != nil {
		return "", err
	}
	return session.OwnerID, nil
}

func cliSessionResponse(session domain.CLISession, current string) map[string]any {
	return map[string]any{
		"id": session.ID, "device_name": session.DeviceName, "client_os": session.ClientOS,
		"client_version": session.ClientVersion, "owner_name": session.OwnerName,
		"created_at": session.CreatedAt, "last_used_at": session.LastUsedAt, "expires_at": session.ExpiresAt,
		"current": session.ID == current,
	}
}

// cliSessions lists the caller's own CLI keys. Administrators see every
// member's keys so they can revoke them.
func (a API) cliSessions(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	owner := ""
	if !authz.AllowedScoped(authz.Role(principal.Role), principal.Scopes, authz.ManageTenant) {
		var err error
		if owner, err = a.cliSessionOwner(r.Context(), store, principal); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "CLI session owner lookup failed")
			return
		}
	}
	items, err := store.CLISessionsForTenant(r.Context(), principal.TenantID, owner, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "CLI sessions could not be listed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, cliSessionResponse(item, principal.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a API) revokeCLISession(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	session, err := store.CLISession(r.Context(), principal.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "CLI session was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "CLI session lookup failed")
		return
	}
	if !authz.AllowedScoped(authz.Role(principal.Role), principal.Scopes, authz.ManageTenant) {
		owner, ownerErr := a.cliSessionOwner(r.Context(), store, principal)
		if ownerErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "CLI session owner lookup failed")
			return
		}
		if owner != session.OwnerID {
			writeError(w, http.StatusForbidden, "forbidden", "Only the person who signed in, or an administrator, can revoke this CLI session")
			return
		}
	}
	a.finishCLISessionRevoke(w, r, store, principal, session.ID)
}

// revokeCurrentCLISession is infercrane logout. It only revokes keys that a
// CLI sign-in issued; a pasted service-account key may be shared and stays
// active.
func (a API) revokeCurrentCLISession(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if principal.Kind != clilogin.PrincipalKind {
		writeError(w, http.StatusConflict, "not_cli_session", "This credential was not issued by infercrane login, so it stays active. Revoke service accounts with infercrane principal revoke.")
		return
	}
	store, ok := a.cliLogins(w)
	if !ok {
		return
	}
	a.finishCLISessionRevoke(w, r, store, principal, principal.ID)
}

func (a API) finishCLISessionRevoke(w http.ResponseWriter, r *http.Request, store cliLoginStore, actor domain.Principal, id string) {
	if err := store.RevokeCLISession(r.Context(), actor.TenantID, id); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "CLI session was not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "CLI session could not be revoked")
		return
	}
	a.refreshCredentialCache(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "cli_session.revoke", ResourceType: "principal", ResourceName: id, Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}
