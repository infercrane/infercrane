package dashboard

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesShellAndAssetsWithSecurityHeaders(t *testing.T) {
	server := httptest.NewServer(Handler())
	defer server.Close()
	for _, route := range []string{"/dashboard/", "/dashboard/app.mjs", "/dashboard/model.mjs", "/dashboard/style.css"} {
		response, err := http.Get(server.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK || len(body) == 0 {
			t.Fatalf("%s: status=%d body=%q", route, response.StatusCode, body)
		}
		for _, header := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
			if response.Header.Get(header) == "" {
				t.Errorf("%s omitted %s", route, header)
			}
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s can be cached", route)
		}
	}
}

func TestHandlerRedirectsCanonicalPathAndRejectsUnsafeRequests(t *testing.T) {
	h := Handler()
	redirect := httptest.NewRecorder()
	h.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != "/dashboard/" {
		t.Fatalf("redirect=%d %q", redirect.Code, redirect.Header().Get("Location"))
	}
	for method, route := range map[string]string{http.MethodPost: "/dashboard/", http.MethodGet: "/dashboard/missing.js"} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(method, route, nil))
		if response.Code != map[string]int{http.MethodPost: http.StatusMethodNotAllowed, http.MethodGet: http.StatusNotFound}[method] {
			t.Fatalf("%s %s=%d", method, route, response.Code)
		}
	}
}

func TestShellAccessibilityAndCredentialSafetyContract(t *testing.T) {
	index, err := fs.ReadFile(assets, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	for _, required := range []string{`href="#connection-panel"`, `<header`, `<nav`, `<main id="main"`, `aria-live="polite"`, `autocomplete="current-password"`, `type="password"`} {
		if !strings.Contains(html, required) {
			t.Errorf("shell is missing %q", required)
		}
	}
	for _, forbidden := range []string{"localStorage", "innerHTML", "http://", "https://"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("shell contains forbidden content %q", forbidden)
		}
	}
	app, _ := fs.ReadFile(assets, "static/app.mjs")
	if strings.Contains(string(app), "localStorage") || strings.Contains(string(app), "innerHTML") {
		t.Fatal("application may not persist credentials or use unsafe HTML sinks")
	}
}
