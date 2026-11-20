package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

// keysetAudit serves events newest first by (created_at, id), the store's
// order, strictly before the cursor.
func keysetAudit(events []domain.AuditEvent) func(domain.AuditCursor, int) []domain.AuditEvent {
	return func(before domain.AuditCursor, limit int) []domain.AuditEvent {
		var page []domain.AuditEvent
		for _, event := range events {
			if !before.CreatedAt.IsZero() && !(event.CreatedAt.Before(before.CreatedAt) || (event.CreatedAt.Equal(before.CreatedAt) && event.ID < before.ID)) {
				continue
			}
			if page = append(page, event); len(page) == limit {
				break
			}
		}
		return page
	}
}

func TestAuditEventsFollowNextThroughEventsThatShareATimestamp(t *testing.T) {
	tie := time.Date(2026, 11, 20, 12, 0, 0, 123456000, time.UTC)
	var events []domain.AuditEvent
	for _, id := range []string{"event-4", "event-3", "event-2", "event-1", "event-0"} {
		events = append(events, domain.AuditEvent{ID: id, Action: "test.event", Payload: "{}", CreatedAt: tie})
	}
	events = append(events, domain.AuditEvent{ID: "event-9", Action: "test.event", Payload: "{}", CreatedAt: tie.Add(-time.Second)})
	store := &fakeStore{principal: domain.Principal{ID: "admin-1", TenantID: "tenant-1", Role: "admin", Scopes: authz.DefaultScopeNames(authz.Admin)}, auditEvents: keysetAudit(events)}
	handler := (API{Store: store, Authenticator: store}).Handler()
	get := func(query url.Values) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events?"+query.Encode(), nil)
		request.Header.Set("Authorization", "Bearer admin-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var body map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		return response, body
	}

	query := url.Values{"limit": {"2"}}
	var seen []string
	for range 10 {
		response, body := get(query)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		assertMatchesDocumentedSchema(t, "AuditEventList", body)
		for _, row := range body["data"].([]any) {
			seen = append(seen, row.(map[string]any)["id"].(string))
		}
		next, ok := body["next"].(map[string]any)
		if !ok {
			break
		}
		query = url.Values{"limit": {"2"}, "before": {next["before"].(string)}, "before_id": {next["before_id"].(string)}}
	}
	if want := []string{"event-4", "event-3", "event-2", "event-1", "event-0", "event-9"}; !slices.Equal(seen, want) {
		t.Fatalf("paged events = %v, want %v", seen, want)
	}
	if store.auditBefore.ID != "event-9" || !store.auditBefore.CreatedAt.Equal(tie.Add(-time.Second)) {
		t.Fatalf("last cursor = %+v", store.auditBefore)
	}
}

func TestAuditEventsClampTheLimitAndCheckTheCursor(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "admin-1", TenantID: "tenant-1", Role: "admin", Scopes: authz.DefaultScopeNames(authz.Admin)}}
	handler := (API{Store: store, Authenticator: store}).Handler()
	get := func(query string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/audit-events"+query, nil)
		request.Header.Set("Authorization", "Bearer admin-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for query, want := range map[string]int{"?limit=1000": 500, "?limit=501": 500, "": 100, "?limit=0": 100, "?limit=-2": 100, "?limit=abc": 100, "?limit=25": 25} {
		response := get(query)
		if response.Code != http.StatusOK || store.auditLimit != want || !strings.Contains(response.Body.String(), `"limit":`) {
			t.Fatalf("%q: status=%d store limit=%d, want %d: %s", query, response.Code, store.auditLimit, want, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"next":null`) {
			t.Fatalf("%q: a short page has a next cursor: %s", query, response.Body.String())
		}
	}
	if response := get("?before=2026-11-20T12:00:00.123456Z"); response.Code != http.StatusOK || store.auditBefore.ID != "" || !store.auditBefore.CreatedAt.Equal(time.Date(2026, 11, 20, 12, 0, 0, 123456000, time.UTC)) {
		t.Fatalf("before alone: status=%d cursor=%+v", response.Code, store.auditBefore)
	}
	for _, query := range []string{"?before_id=event-1", "?before=yesterday", "?before=yesterday&before_id=event-1"} {
		if response := get(query); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_cursor") {
			t.Fatalf("%q: status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}
