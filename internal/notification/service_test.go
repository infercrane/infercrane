package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/alert"
)

func TestCreateValidatesDestinationsAndSealsURLAndSecret(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	service := testService(t, store, newReceiver(t).poster(), clk.Now)
	for _, tc := range []CreateRequest{
		{Destination: "email", URL: "https://hooks.example.test/a", Events: []string{"decided"}},
		{Destination: "webhook", URL: "http://hooks.example.test/a", Events: []string{"decided"}},
		{Destination: "webhook", URL: "https://user:pass@hooks.example.test/a", Events: []string{"decided"}},
		{Destination: "webhook", URL: "https://hooks.example.test/a#fragment", Events: []string{"decided"}},
		{Destination: "webhook", URL: "https://hooks.example.test/a", Events: nil},
		{Destination: "webhook", URL: "https://hooks.example.test/a", Events: []string{"test"}},
		{Destination: "webhook", URL: "https://hooks.example.test/a", Events: []string{"decided"}, Name: strings.Repeat("n", 65)},
		{Destination: "slack", URL: "https://hooks.example.test/services/T/B/X", Events: []string{"decided"}},
		{Destination: "slack", URL: "https://hooks.slack.com/workflows/T/A/X", Events: []string{"decided"}},
	} {
		if _, _, err := service.Create(ctx, "tenant", "owner", tc); !errors.Is(err, ErrInvalid) {
			t.Fatalf("request %+v err=%v, want ErrInvalid", tc, err)
		}
	}
	url := "https://hooks.slack.com/services/T0001/B0001/abcdefghijklmnopqrstuvwx"
	item, secret, err := service.Create(ctx, "tenant", "owner", CreateRequest{Name: "Team alerts", Destination: "slack", URL: url, Events: []string{"failed", "decided", "decided"}})
	if err != nil {
		t.Fatal(err)
	}
	if item.Name != "Team alerts" || item.URLHint != "https://hooks.slack.com/…uvwx" || strings.Join(item.Events, ",") != "decided,failed" || !item.Enabled || item.CreatedBy != "owner" {
		t.Fatalf("item=%+v", item)
	}
	if bytes.Contains(item.URLCiphertext, []byte("abcdefghijklmnop")) || bytes.Contains(item.SecretCiphertext, []byte(secret)) {
		t.Fatal("destination URL or signing secret was stored in plaintext")
	}
	opened, openedSecret, err := service.open(item)
	if err != nil || opened != url || openedSecret != secret {
		t.Fatalf("open=%q,%q err=%v", opened, openedSecret, err)
	}
	moved := item
	moved.TenantID = "other-tenant"
	if _, _, err = service.open(moved); err == nil {
		t.Fatal("a sealed destination opened under another workspace")
	}
	rotated := service
	rotated.KeyReference = "environment:OTHER_KEY"
	if _, _, err = rotated.open(item); err == nil {
		t.Fatal("a sealed destination opened under a different key reference")
	}
}

func TestUpdateChangesOnlyRequestedFields(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	service := testService(t, store, newReceiver(t).poster(), clk.Now)
	item, secret, err := service.Create(ctx, "tenant", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/first", Events: []string{"decided"}})
	if err != nil {
		t.Fatal(err)
	}
	off, events, url := false, []string{"approval_needed", "rolled_out"}, "https://receiver.example.test/second-path"
	updated, err := service.Update(ctx, "tenant", item.ID, UpdateRequest{Enabled: &off, Events: &events, URL: &url})
	if err != nil || updated.Enabled || strings.Join(updated.Events, ",") != "approval_needed,rolled_out" || updated.URLHint != "https://receiver.example.test/…path" || updated.Name != item.Name {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	opened, openedSecret, err := service.open(updated)
	if err != nil || opened != url || openedSecret != secret {
		t.Fatalf("after update open=%q secret kept=%v err=%v", opened, openedSecret == secret, err)
	}
	bad := "https://hooks.slack.com/services/x"
	if _, err = service.Update(ctx, "tenant", item.ID, UpdateRequest{URL: &bad, Events: &[]string{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid update err=%v", err)
	}
	if _, err = service.Update(ctx, "other-tenant", item.ID, UpdateRequest{Enabled: &off}); err == nil {
		t.Fatal("another workspace updated the destination")
	}
}

func TestTestEventIsSignedRecordedAndReportsRefusals(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	hook := newReceiver(t)
	service := testService(t, store, hook.poster(), clk.Now)
	off := false
	item, secret, err := service.Create(ctx, "tenant", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/test", Events: []string{"decided"}, Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := service.Test(ctx, "tenant", item.ID)
	if err != nil || delivery.Status != "delivered" || delivery.Attempts != 1 || delivery.EventType != EventTest {
		t.Fatalf("test delivery=%+v err=%v", delivery, err)
	}
	requests := hook.requests()
	if len(requests) != 1 {
		t.Fatalf("requests=%d", len(requests))
	}
	verifySignature(t, secret, requests[0])
	var body Body
	if err = json.Unmarshal(requests[0].Body, &body); err != nil || body.Event != "infercrane.notification.test" || body.Campaign != nil || body.Links.Dashboard != "https://console.example.test/settings/notifications" {
		t.Fatalf("test body=%s err=%v", requests[0].Body, err)
	}
	hook.replies = []int{http.StatusNotFound}
	refused, err := service.Test(ctx, "tenant", item.ID)
	if err != nil || refused.Status != "failed" || refused.ErrorCode != "http_404" || refused.ResponseStatus != http.StatusNotFound {
		t.Fatalf("refused=%+v err=%v", refused, err)
	}
	// The background sender never picks up a test event the request holds.
	if claimed, _ := store.ClaimNotificationDeliveries(ctx, time.Minute, 10); len(claimed) != 0 {
		t.Fatalf("claimed=%+v", claimed)
	}
	private := service
	private.Poster = privateRejecting{}
	rejected, err := private.Test(ctx, "tenant", item.ID)
	if err != nil || rejected.Status != "failed" || rejected.ErrorCode != "destination_rejected" {
		t.Fatalf("rejected=%+v err=%v", rejected, err)
	}
}

type privateRejecting struct{}

func (privateRejecting) Post(context.Context, string, string, string, []byte) (alert.Attempt, error) {
	return alert.Attempt{}, errors.New("webhook destination resolves to a private or local address")
}
