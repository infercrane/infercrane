package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestMigrateCommandsUseTheMigrationRoutes(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}
	guardRefuses := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies[r.Method+" "+r.URL.Path] = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/endpoints/support-bot/replays":
			_, _ = io.WriteString(w, `{"replay":{"id":"r1","request_count":12,"shape_digest":"abc"}}`)
		case "PUT /api/v1/endpoints/support-bot/bindings/primary/rate-card":
			_, _ = io.WriteString(w, `{"rate_card":{"id":"c1","valid_until":"2026-12-31T00:00:00Z"}}`)
		case "GET /api/v1/optimization/campaigns/run-1/migration-verdicts":
			_, _ = io.WriteString(w, `{"data":[{"id":"v1","verdict":"PASS","candidate_id":"fast","reference_score":0.95,"candidate_score":0.94}]}`)
		case "POST /api/v1/endpoints/support-bot/migration":
			if guardRefuses {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"migration":{"step":"primary","activated":false,"evaluation":{"decision":"REJECT","reasons":["ttft_regression"]}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"migration":{"step":"canary","activated":true,"routing":"converged"}}`)
		default:
			t.Fatalf("unexpected route %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	ctx := context.Background()
	out, err := captureStdout(t, func() error {
		return migrateCommand(ctx, cfg, []string{"capture", "support-bot", "--binding", "primary", "--window", "2h"})
	})
	if err != nil || !strings.Contains(out, "sha256:abc") || bodies["POST /api/v1/endpoints/support-bot/replays"]["binding"] != "primary" || bodies["POST /api/v1/endpoints/support-bot/replays"]["window_seconds"] != float64(7200) {
		t.Fatalf("capture out=%q err=%v bodies=%v", out, err, bodies)
	}
	if _, err = captureStdout(t, func() error {
		return migrateCommand(ctx, cfg, []string{"price", "support-bot", "primary", "--authority", "list_price", "--input-usd", "0.40", "--cached-input-usd", "0.1", "--output-usd", "1.6", "--valid-until", "2026-12-31"})
	}); err != nil {
		t.Fatal(err)
	}
	card := bodies["PUT /api/v1/endpoints/support-bot/bindings/primary/rate-card"]
	if card["input_microusd_per_million"] != float64(400000) || card["cached_input_microusd_per_million"] != float64(100000) || card["output_microusd_per_million"] != float64(1600000) || card["valid_until"] != "2026-12-31T00:00:00Z" {
		t.Fatalf("rate card body=%v", card)
	}
	if out, err = captureStdout(t, func() error { return migrateCommand(ctx, cfg, []string{"verdicts", "run-1"}) }); err != nil || !strings.Contains(out, "v1  PASS  setup fast  current API 0.950  setup 0.940") {
		t.Fatalf("verdicts out=%q err=%v", out, err)
	}
	if err = migrateCommand(ctx, cfg, []string{"canary", "support-bot"}); err == nil || !strings.Contains(err.Error(), "--verdict") {
		t.Fatalf("canary without a verdict: %v", err)
	}
	if err = migrateCommand(ctx, cfg, []string{"canary", "support-bot", "--verdict", "v1", "--percent", "20"}); err == nil {
		t.Fatal("a 20% canary was sent")
	}
	if out, err = captureStdout(t, func() error {
		return migrateCommand(ctx, cfg, []string{"canary", "support-bot", "--verdict", "v1", "--percent", "2.5"})
	}); err != nil || !strings.Contains(out, "the canary is live") || bodies["POST /api/v1/endpoints/support-bot/migration"]["canary_basis_points"] != float64(250) {
		t.Fatalf("canary out=%q err=%v body=%v", out, err, bodies["POST /api/v1/endpoints/support-bot/migration"])
	}
	guardRefuses = true
	if err = migrateCommand(ctx, cfg, []string{"primary", "support-bot"}); err == nil || !strings.Contains(err.Error(), "REJECT") || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("refused primary step: %v", err)
	}
	if err = migrateCommand(ctx, cfg, []string{"price", "support-bot", "primary", "--authority", "guess", "--input-usd", "1", "--output-usd", "1", "--valid-until", "2026-12-31"}); err == nil {
		t.Fatal("a rate card without authority was sent")
	}
}
