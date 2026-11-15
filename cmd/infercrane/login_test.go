package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

const issuedTestKey = "ic_1111111111111111111111111111111111111111111111111111111111111111"

type fakeLoginServer struct {
	mu          sync.Mutex
	polls       int
	approveOn   int
	finalStatus string
	advertise   string
	revokedWith string
	created     map[string]string
}

func (f *fakeLoginServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/cli/logins", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("sign-in start sent credentials: %q", r.Header.Get("Authorization"))
		}
		var client map[string]string
		_ = json.NewDecoder(r.Body).Decode(&client)
		f.mu.Lock()
		f.created = client
		f.mu.Unlock()
		response := map[string]any{"device_code": "icd_" + strings.Repeat("a", 64), "user_code": "BCDF-GHJK", "expires_in": 600, "interval": 3, "expires_at": time.Now().Add(10 * time.Minute)}
		if f.advertise != "" {
			response["verification_uri"] = f.advertise + "/cli/authorize"
			response["verification_uri_complete"] = f.advertise + "/cli/authorize?code=BCDF-GHJK"
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("POST /api/v1/cli/logins/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.polls++
		if f.polls < f.approveOn {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending", "interval": 3})
			return
		}
		if f.finalStatus != "approved" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": f.finalStatus})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "approved", "credential": issuedTestKey, "owner_name": "Ada Operator",
			"principal": map[string]any{"id": "key-1", "tenant_id": "acme", "name": "cli:ada-laptop:key-1", "role": "operator", "kind": "cli_session", "scopes": []string{"read", "deploy"}, "expires_at": "2027-02-11T09:00:00Z"},
		})
	})
	mux.HandleFunc("GET /api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+issuedTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"principal": map[string]any{"id": "key-1", "tenant_id": "acme", "name": "cli:ada-laptop:key-1", "role": "operator", "kind": "cli_session", "scopes": []string{"read", "deploy"}}})
	})
	mux.HandleFunc("DELETE /api/v1/cli/sessions/current", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revokedWith = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"endpoint": map[string]any{"name": "support-bot"}})
	})
	return mux
}

type loginHarness struct {
	server  *httptest.Server
	fake    *fakeLoginServer
	stderr  *bytes.Buffer
	opened  []string
	config  string
	browser error
}

func newLoginHarness(t *testing.T, fake *fakeLoginServer) *loginHarness {
	t.Helper()
	h := &loginHarness{fake: fake, stderr: &bytes.Buffer{}, config: filepath.Join(t.TempDir(), "config.json")}
	h.server = httptest.NewServer(fake.handler(t))
	t.Cleanup(h.server.Close)
	t.Setenv("INFERCRANE_CONFIG", h.config)
	t.Setenv("INFERCRANE_CREDENTIAL_STORE", "file")
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_URL", "")
	t.Setenv("INFERCRANE_CONSOLE_URL", "")
	t.Setenv("INFERCRANE_CONTEXT", "")
	t.Setenv("NO_COLOR", "1")
	previousOutput, previousOpen, previousSleep := loginOutput, openBrowser, loginSleep
	loginOutput = h.stderr
	openBrowser = func(target string) error {
		h.opened = append(h.opened, target)
		return h.browser
	}
	loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { loginOutput, openBrowser, loginSleep = previousOutput, previousOpen, previousSleep })
	return h
}

func TestLoginShowsTheCodeWaitsForApprovalAndStoresTheKeyPrivately(t *testing.T) {
	h := newLoginHarness(t, &fakeLoginServer{approveOn: 3, finalStatus: "approved"})
	output, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"login", "--url", h.server.URL, "--console-url", "http://127.0.0.1:3200/", "--no-browser"})
	})
	if err != nil {
		t.Fatalf("login: %v\n%s", err, h.stderr.String())
	}
	progress := h.stderr.String()
	for _, want := range []string{"Your one-time code: BCDF-GHJK", "http://127.0.0.1:3200/cli/authorize?code=BCDF-GHJK", "expires in 10 minutes"} {
		if !strings.Contains(progress, want) {
			t.Fatalf("progress is missing %q:\n%s", want, progress)
		}
	}
	for _, want := range []string{"Signed in to " + h.server.URL + " as Ada Operator (operator, tenant acme)", "Expires    2027-02-11", "(mode 0600); INFERCRANE_CREDENTIAL_STORE=file", "View in dashboard → http://127.0.0.1:3200/settings/cli-sessions"} {
		if !strings.Contains(output, want) {
			t.Fatalf("result is missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output+progress, issuedTestKey) {
		t.Fatal("login printed the key")
	}
	if len(h.opened) != 0 {
		t.Fatalf("--no-browser opened %v", h.opened)
	}
	if h.fake.polls != 3 {
		t.Fatalf("polls=%d", h.fake.polls)
	}
	if h.fake.created["device_name"] == "" || !strings.HasPrefix(h.fake.created["client_version"], "infercrane ") {
		t.Fatalf("client description=%v", h.fake.created)
	}
	info, err := os.Stat(h.config)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%v err=%v", info.Mode().Perm(), err)
	}

	status, err := captureStdout(t, func() error { return run(context.Background(), []string{"whoami"}) })
	if err != nil || !strings.Contains(status, "Key from       config file") || !strings.Contains(status, "Signed in by   infercrane login") || !strings.Contains(status, "View in dashboard → http://127.0.0.1:3200") {
		t.Fatalf("whoami=%q err=%v", status, err)
	}
	opened, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"open", "model", "Qwen/Qwen3-8B", "--no-browser"})
	})
	if err != nil || strings.TrimSpace(opened) != "http://127.0.0.1:3200/models/Qwen%2FQwen3-8B" {
		t.Fatalf("open=%q err=%v", opened, err)
	}

	signedOut, err := captureStdout(t, func() error { return run(context.Background(), []string{"logout"}) })
	if err != nil || !strings.Contains(signedOut, "The key was revoked on the control plane.") || !strings.Contains(signedOut, "Removed the key from config file.") {
		t.Fatalf("logout=%q err=%v", signedOut, err)
	}
	if h.fake.revokedWith != "Bearer "+issuedTestKey {
		t.Fatalf("logout revoked with %q", h.fake.revokedWith)
	}
	body, _ := os.ReadFile(h.config)
	if strings.Contains(string(body), issuedTestKey) || !strings.Contains(string(body), h.server.URL) {
		t.Fatalf("config after logout=%s", body)
	}
	if _, err = captureStdout(t, func() error { return run(context.Background(), []string{"auth", "status"}) }); err == nil || !strings.Contains(err.Error(), "infercrane login") {
		t.Fatalf("status after logout err=%v", err)
	}
}

func TestLoginOpensTheAdvertisedDashboardAndReportsDenial(t *testing.T) {
	h := newLoginHarness(t, &fakeLoginServer{approveOn: 1, finalStatus: "denied", advertise: "https://console.example"})
	_, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"auth", "login", "--url", h.server.URL})
	})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied login err=%v", err)
	}
	if len(h.opened) != 1 || h.opened[0] != "https://console.example/cli/authorize?code=BCDF-GHJK" {
		t.Fatalf("opened=%v", h.opened)
	}
	if _, statErr := os.Stat(h.config); !os.IsNotExist(statErr) {
		t.Fatalf("a denied login wrote config: %v", statErr)
	}
}

func TestLoginPrintsTheURLWhenNoBrowserOpensAndNeedsADashboard(t *testing.T) {
	h := newLoginHarness(t, &fakeLoginServer{approveOn: 1, finalStatus: "expired"})
	_, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"login", "--url", h.server.URL})
	})
	if err == nil || !strings.Contains(err.Error(), "--console-url") {
		t.Fatalf("login without a dashboard err=%v", err)
	}
	h.browser = os.ErrNotExist
	t.Setenv("INFERCRANE_CONSOLE_URL", "http://127.0.0.1:3200")
	_, err = captureStdout(t, func() error {
		return run(context.Background(), []string{"login", "--url", h.server.URL, "--output", "json"})
	})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired login err=%v", err)
	}
	if !strings.Contains(h.stderr.String(), "Open this page in a browser and approve the sign-in:\n  http://127.0.0.1:3200/cli/authorize?code=BCDF-GHJK") {
		t.Fatalf("fallback instructions=%s", h.stderr.String())
	}
}

func TestLoginCancelSavesNothing(t *testing.T) {
	h := newLoginHarness(t, &fakeLoginServer{approveOn: 100, finalStatus: "approved"})
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	loginSleep = func(context.Context, time.Duration) error {
		polls++
		if polls == 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	_, err := captureStdout(t, func() error {
		return run(ctx, []string{"login", "--url", h.server.URL, "--console-url", "http://127.0.0.1:3200"})
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled login err=%v", err)
	}
	if _, statErr := os.Stat(h.config); !os.IsNotExist(statErr) {
		t.Fatalf("a cancelled login wrote config: %v", statErr)
	}
}

func TestResultsLinkToTheDashboardOnlyWhenOneIsConfigured(t *testing.T) {
	h := newLoginHarness(t, &fakeLoginServer{})
	t.Setenv("INFERCRANE_API_KEY", issuedTestKey)
	t.Setenv("INFERCRANE_URL", h.server.URL)
	output, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"endpoint", "create", "support-bot", "--model", "support"})
	})
	if err != nil || strings.Contains(output, "View in dashboard") {
		t.Fatalf("local stack without a dashboard printed a link: %q err=%v", output, err)
	}
	t.Setenv("INFERCRANE_CONSOLE_URL", "https://console.example")
	output, err = captureStdout(t, func() error {
		return run(context.Background(), []string{"endpoint", "create", "support-bot", "--model", "support"})
	})
	if err != nil || !strings.Contains(output, "View in dashboard → https://console.example/endpoints/support-bot") {
		t.Fatalf("endpoint create=%q err=%v", output, err)
	}

	cfg := config.Config{ConsoleURL: "https://console.example"}
	for _, test := range []struct{ kind, name, want string }{
		{"deployment", "qwen prod", "https://console.example/workloads/qwen%20prod"},
		{"run", "4f1c", "https://console.example/optimization/4f1c"},
		{"operation", "op-1", "https://console.example/activity/operations/op-1"},
		{"model", "Qwen/Qwen3-8B", "https://console.example/models/Qwen%2FQwen3-8B"},
	} {
		if got := dashboardURL(cfg, dashboardPath(test.kind, test.name)); got != test.want {
			t.Fatalf("%s %s → %q, want %q", test.kind, test.name, got, test.want)
		}
	}
	if got := dashboardURL(config.Config{}, "/endpoints/x"); got != "" {
		t.Fatalf("no dashboard produced %q", got)
	}
	if _, err = captureStdout(t, func() error { return run(context.Background(), []string{"open", "widget", "x"}) }); err == nil {
		t.Fatal("open accepted an unknown page kind")
	}
}
