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

// A hosted OpenAI-compatible API has no /health route; its model list is the
// probe. A failing /health still fails.
func TestInspectAcceptsAHostedAPIWithoutAHealthRoute(t *testing.T) {
	for _, test := range []struct {
		health int
		ready  bool
	}{{http.StatusNotFound, true}, {http.StatusOK, true}, {http.StatusServiceUnavailable, false}, {http.StatusUnauthorized, false}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer sk-closed" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/health":
				w.WriteHeader(test.health)
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1-mini"}]}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		ready, models := OpenAI{}.InspectWithCredential(context.Background(), server.URL, "sk-closed")
		server.Close()
		if _, listed := models["gpt-4.1-mini"]; ready != test.ready || ready && !listed {
			t.Fatalf("health %d: ready=%v models=%v", test.health, ready, models)
		}
	}
}
