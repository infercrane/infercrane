package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInspectRejectsRedirectedReadinessWithoutForwardingCredential(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("worker credential followed readiness redirect: %q", authorization)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		http.NotFound(w, r)
	}))
	defer origin.Close()

	ready, _ := (OpenAI{APIKey: "worker-secret"}).Inspect(context.Background(), origin.URL)
	if ready || redirected {
		t.Fatalf("redirected readiness was accepted: ready=%t redirected=%t", ready, redirected)
	}
}
