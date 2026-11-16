package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/notification"
)

// notificationService is the workspace notification destination boundary.
// Nil means sealed destination storage is not configured on this control plane.
type notificationService interface {
	Create(context.Context, string, string, notification.CreateRequest) (domain.NotificationSubscription, string, error)
	Update(context.Context, string, string, notification.UpdateRequest) (domain.NotificationSubscription, error)
	Delete(context.Context, string, string) error
	List(context.Context, string) ([]domain.NotificationSubscription, error)
	Test(context.Context, string, string) (domain.NotificationDelivery, error)
}

const notificationsUnavailable = "notification destinations need INFERCRANE_CREDENTIAL_ENCRYPTION_KEY on the control plane"

func (a API) notificationSubscriptions(w http.ResponseWriter, r *http.Request) {
	if a.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", notificationsUnavailable)
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := a.Notifications.List(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "notification destinations could not be listed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, notificationSubscriptionResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "events": notification.CampaignEvents})
}

func (a API) createNotificationSubscription(w http.ResponseWriter, r *http.Request) {
	if a.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", notificationsUnavailable)
		return
	}
	var request notification.CreateRequest
	if !decodeNotificationRequest(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, secret, err := a.Notifications.Create(r.Context(), actor.TenantID, actor.Name, request)
	if writeNotificationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "notification_subscription.create", ResourceType: "notification_subscription", ResourceName: item.ID, Outcome: "succeeded"})
	response := map[string]any{"subscription": notificationSubscriptionResponse(item)}
	// Slack does not verify signatures, so only a webhook receives the secret.
	if item.Destination == notification.DestinationWebhook {
		response["signing_secret"] = secret
	}
	writeJSON(w, http.StatusCreated, response)
}

func (a API) updateNotificationSubscription(w http.ResponseWriter, r *http.Request) {
	if a.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", notificationsUnavailable)
		return
	}
	var request notification.UpdateRequest
	if !decodeNotificationRequest(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := a.Notifications.Update(r.Context(), actor.TenantID, r.PathValue("id"), request)
	if writeNotificationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "notification_subscription.update", ResourceType: "notification_subscription", ResourceName: item.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"subscription": notificationSubscriptionResponse(item)})
}

func (a API) deleteNotificationSubscription(w http.ResponseWriter, r *http.Request) {
	if a.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", notificationsUnavailable)
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	id := r.PathValue("id")
	if writeNotificationError(w, a.Notifications.Delete(r.Context(), actor.TenantID, id)) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "notification_subscription.delete", ResourceType: "notification_subscription", ResourceName: id, Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}

func (a API) testNotificationSubscription(w http.ResponseWriter, r *http.Request) {
	if a.Notifications == nil {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", notificationsUnavailable)
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	delivery, err := a.Notifications.Test(r.Context(), actor.TenantID, r.PathValue("id"))
	if writeNotificationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "notification_subscription.test", ResourceType: "notification_subscription", ResourceName: r.PathValue("id"), Outcome: delivery.Status})
	writeJSON(w, http.StatusOK, map[string]any{"delivery": notificationDeliveryResponse(delivery)})
}

func decodeNotificationRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return false
	}
	return true
}

// writeNotificationError writes err and reports whether it did.
func writeNotificationError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, notification.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, "invalid_notification_destination", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "notification destination was not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "notification destination request failed")
	}
	return true
}

func notificationSubscriptionResponse(item domain.NotificationSubscription) map[string]any {
	response := map[string]any{"id": item.ID, "name": item.Name, "destination": item.Destination, "url_hint": item.URLHint, "events": item.Events, "enabled": item.Enabled, "created_by": item.CreatedBy, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "last_delivery": nil}
	if item.LastDelivery != nil {
		response["last_delivery"] = notificationDeliveryResponse(*item.LastDelivery)
	}
	return response
}

func notificationDeliveryResponse(item domain.NotificationDelivery) map[string]any {
	return map[string]any{"event": item.EventType, "status": item.Status, "attempts": item.Attempts, "response_status": nullableInt(item.ResponseStatus), "error_code": nullableString(item.ErrorCode), "occurred_at": item.OccurredAt, "updated_at": item.UpdatedAt, "delivered_at": item.DeliveredAt}
}

func nullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
