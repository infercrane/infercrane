package controlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/notification"
)

type fakeNotifications struct {
	tenants []string
	items   map[string]domain.NotificationSubscription
	err     error
}

func (f *fakeNotifications) Create(_ context.Context, tenant, actor string, request notification.CreateRequest) (domain.NotificationSubscription, string, error) {
	f.tenants = append(f.tenants, tenant)
	if f.err != nil {
		return domain.NotificationSubscription{}, "", f.err
	}
	item := domain.NotificationSubscription{ID: fmt.Sprintf("sub-%d", len(f.items)+1), TenantID: tenant, Name: "hooks.example.test", Destination: request.Destination, URLHint: "https://hooks.example.test/…", URLCiphertext: []byte("sealed-url"), SecretCiphertext: []byte("sealed-secret"), Events: request.Events, Enabled: true, CreatedBy: actor, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	f.items[item.ID] = item
	return item, notification.SecretPrefix + "generated", nil
}

func (f *fakeNotifications) Update(_ context.Context, tenant, id string, request notification.UpdateRequest) (domain.NotificationSubscription, error) {
	item, ok := f.items[id]
	if !ok || item.TenantID != tenant {
		return domain.NotificationSubscription{}, domain.ErrNotFound
	}
	if request.Enabled != nil {
		item.Enabled = *request.Enabled
	}
	f.items[id] = item
	return item, nil
}

func (f *fakeNotifications) Delete(_ context.Context, tenant, id string) error {
	item, ok := f.items[id]
	if !ok || item.TenantID != tenant {
		return domain.ErrNotFound
	}
	delete(f.items, id)
	return nil
}

func (f *fakeNotifications) List(_ context.Context, tenant string) ([]domain.NotificationSubscription, error) {
	out := []domain.NotificationSubscription{}
	for _, item := range f.items {
		if item.TenantID == tenant {
			out = append(out, item)
		}
	}
	return out, nil
}

func (f *fakeNotifications) Test(_ context.Context, tenant, id string) (domain.NotificationDelivery, error) {
	if item, ok := f.items[id]; !ok || item.TenantID != tenant {
		return domain.NotificationDelivery{}, domain.ErrNotFound
	}
	delivered := time.Now().UTC()
	return domain.NotificationDelivery{EventType: notification.EventTest, Status: "delivered", Attempts: 1, ResponseStatus: 204, OccurredAt: delivered, UpdatedAt: delivered, DeliveredAt: &delivered}, nil
}

func TestNotificationRoutesAreScopedAndNeverReturnSealedValues(t *testing.T) {
	admin := &fakeStore{principal: domain.Principal{ID: "admin", TenantID: "tenant-a", Name: "owner", Role: string(authz.Admin), Scopes: authz.DefaultScopeNames(authz.Admin)}}
	notifications := &fakeNotifications{items: map[string]domain.NotificationSubscription{}}
	handler := (API{Store: admin, Authenticator: admin, Notifications: notifications}).Handler()

	response := serve(handler, http.MethodPost, "/api/v1/notifications/subscriptions", "admin", `{"destination":"webhook","url":"https://hooks.example.test/x","events":["decided"]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Subscription  map[string]any `json:"subscription"`
		SigningSecret string         `json:"signing_secret"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.SigningSecret != notification.SecretPrefix+"generated" || created.Subscription["created_by"] != "owner" || created.Subscription["last_delivery"] != nil {
		t.Fatalf("created=%s err=%v", response.Body.String(), err)
	}
	if body := response.Body.String(); strings.Contains(body, "sealed") || strings.Contains(body, "/x\"") {
		t.Fatalf("response leaked a sealed value: %s", body)
	}
	if notifications.tenants[0] != "tenant-a" {
		t.Fatalf("created for tenant %v", notifications.tenants)
	}
	response = serve(handler, http.MethodPost, "/api/v1/notifications/subscriptions", "admin", `{"destination":"slack","url":"https://hooks.slack.com/services/T/B/X","events":["decided"]}`)
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "signing_secret") {
		t.Fatalf("a Slack destination returned a signing secret: %d %s", response.Code, response.Body.String())
	}
	response = serve(handler, http.MethodPost, "/api/v1/notifications/subscriptions", "admin", `{"destination":"webhook","url":"https://hooks.example.test/x","events":["decided"],"extra":true}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", response.Code)
	}
	response = serve(handler, http.MethodGet, "/api/v1/notifications/subscriptions", "admin", "")
	var listed struct {
		Data   []map[string]any `json:"data"`
		Events []string         `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || response.Code != http.StatusOK || len(listed.Data) != 2 || strings.Join(listed.Events, ",") != strings.Join(notification.CampaignEvents, ",") {
		t.Fatalf("list status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	response = serve(handler, http.MethodPatch, "/api/v1/notifications/subscriptions/sub-1", "admin", `{"enabled":false}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"enabled":false`) {
		t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
	}
	response = serve(handler, http.MethodPost, "/api/v1/notifications/subscriptions/sub-1/test", "admin", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"delivered"`) || !strings.Contains(response.Body.String(), `"error_code":null`) {
		t.Fatalf("test status=%d body=%s", response.Code, response.Body.String())
	}
	if response = serve(handler, http.MethodDelete, "/api/v1/notifications/subscriptions/sub-1", "admin", ""); response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", response.Code)
	}
	if response = serve(handler, http.MethodDelete, "/api/v1/notifications/subscriptions/sub-1", "admin", ""); response.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d", response.Code)
	}

	viewer := &fakeStore{principal: domain.Principal{ID: "viewer", TenantID: "tenant-a", Name: "viewer", Role: string(authz.Viewer), Scopes: []string{"read"}}}
	viewerHandler := (API{Store: viewer, Authenticator: viewer, Notifications: notifications}).Handler()
	if response = serve(viewerHandler, http.MethodGet, "/api/v1/notifications/subscriptions", "viewer", ""); response.Code != http.StatusOK {
		t.Fatalf("viewer list status=%d", response.Code)
	}
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/notifications/subscriptions", `{"destination":"webhook","url":"https://hooks.example.test/x","events":["decided"]}`},
		{http.MethodPatch, "/api/v1/notifications/subscriptions/sub-2", `{"enabled":false}`},
		{http.MethodDelete, "/api/v1/notifications/subscriptions/sub-2", ""},
		{http.MethodPost, "/api/v1/notifications/subscriptions/sub-2/test", ""},
	} {
		if response = serve(viewerHandler, request.method, request.path, "viewer", request.body); response.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s status=%d", request.method, request.path, response.Code)
		}
	}
	other := &fakeStore{principal: domain.Principal{ID: "other", TenantID: "tenant-b", Name: "other", Role: string(authz.Admin), Scopes: authz.DefaultScopeNames(authz.Admin)}}
	otherHandler := (API{Store: other, Authenticator: other, Notifications: notifications}).Handler()
	if response = serve(otherHandler, http.MethodPost, "/api/v1/notifications/subscriptions/sub-2/test", "other", ""); response.Code != http.StatusNotFound {
		t.Fatalf("another workspace tested a destination: status=%d", response.Code)
	}
}

func TestNotificationRoutesReportUnavailableAndInvalidRequests(t *testing.T) {
	admin := &fakeStore{principal: domain.Principal{ID: "admin", TenantID: "tenant-a", Name: "owner", Role: string(authz.Admin), Scopes: authz.DefaultScopeNames(authz.Admin)}}
	unconfigured := (API{Store: admin, Authenticator: admin}).Handler()
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/notifications/subscriptions"},
		{http.MethodPost, "/api/v1/notifications/subscriptions"},
		{http.MethodPatch, "/api/v1/notifications/subscriptions/x"},
		{http.MethodDelete, "/api/v1/notifications/subscriptions/x"},
		{http.MethodPost, "/api/v1/notifications/subscriptions/x/test"},
	} {
		response := serve(unconfigured, request.method, request.path, "admin", `{}`)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "capability_unavailable") {
			t.Fatalf("%s %s status=%d body=%s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	invalid := &fakeNotifications{items: map[string]domain.NotificationSubscription{}, err: fmt.Errorf("%w: url must be HTTPS", notification.ErrInvalid)}
	handler := (API{Store: admin, Authenticator: admin, Notifications: invalid}).Handler()
	response := serve(handler, http.MethodPost, "/api/v1/notifications/subscriptions", "admin", `{"destination":"webhook","url":"http://x","events":["decided"]}`)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_notification_destination") {
		t.Fatalf("invalid status=%d body=%s", response.Code, response.Body.String())
	}
	full := &fakeNotifications{items: map[string]domain.NotificationSubscription{}, err: fmt.Errorf("%w: at most 10", domain.ErrConflict)}
	response = serve((API{Store: admin, Authenticator: admin, Notifications: full}).Handler(), http.MethodPost, "/api/v1/notifications/subscriptions", "admin", `{"destination":"webhook","url":"https://x.test","events":["decided"]}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("limit status=%d", response.Code)
	}
}
