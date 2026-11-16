package apicontract

import "github.com/infercrane/infercrane/internal/notification"

// notificationSchemas describes workspace destinations for optimization run
// events. Event names come from internal/notification.
func notificationSchemas() map[string]any {
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	nullableTimestamp := map[string]any{"type": []string{"string", "null"}, "format": "date-time"}
	destination := map[string]any{"type": "string", "enum": []string{notification.DestinationWebhook, notification.DestinationSlack}}
	events := map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "enum": notification.CampaignEvents}}
	url := map[string]any{"type": "string", "format": "uri", "maxLength": 2048, "writeOnly": true, "description": "Absolute HTTPS URL. A Slack destination must be an incoming webhook URL on https://hooks.slack.com/services/. Sealed at rest and never returned."}
	name := map[string]any{"type": "string", "maxLength": 64, "description": "Label shown in the dashboard. Defaults to the URL host."}
	return map[string]any{
		"NotificationDelivery": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"event", "status", "attempts", "response_status", "error_code", "occurred_at", "updated_at", "delivered_at"},
			"properties": map[string]any{
				"event":           map[string]any{"type": "string", "description": "Event name, or test for a test event."},
				"status":          map[string]any{"type": "string", "enum": []string{"pending", "delivered", "failed"}},
				"attempts":        map[string]any{"type": "integer", "minimum": 0},
				"response_status": map[string]any{"type": []string{"integer", "null"}, "description": "HTTP status of the last attempt; null when no response arrived."},
				"error_code":      map[string]any{"type": []string{"string", "null"}, "description": "network_error, http_<status>, destination_rejected, destination_disabled, or destination_unreadable."},
				"occurred_at":     timestamp,
				"updated_at":      timestamp,
				"delivered_at":    nullableTimestamp,
			},
		},
		"NotificationDeliveryEnvelope": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"delivery"}, "properties": map[string]any{"delivery": ref("NotificationDelivery")}},
		"NotificationSubscription": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"id", "name", "destination", "url_hint", "events", "enabled", "created_by", "created_at", "updated_at", "last_delivery"},
			"properties": map[string]any{
				"id":            map[string]any{"type": "string"},
				"name":          map[string]any{"type": "string"},
				"destination":   destination,
				"url_hint":      map[string]any{"type": "string", "description": "Scheme, host, and the last characters of the path. The full URL is never returned."},
				"events":        events,
				"enabled":       map[string]any{"type": "boolean"},
				"created_by":    map[string]any{"type": "string"},
				"created_at":    timestamp,
				"updated_at":    timestamp,
				"last_delivery": map[string]any{"oneOf": []any{ref("NotificationDelivery"), map[string]any{"type": "null"}}},
			},
		},
		"NotificationSubscriptionCreate": map[string]any{
			"type": "object", "additionalProperties": false,
			"required":   []string{"destination", "url", "events"},
			"properties": map[string]any{"name": name, "destination": destination, "url": url, "events": events, "enabled": map[string]any{"type": "boolean", "default": true}},
		},
		"NotificationSubscriptionCreated": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"subscription"},
			"properties": map[string]any{
				"subscription":   ref("NotificationSubscription"),
				"signing_secret": map[string]any{"type": "string", "description": "Webhook destinations only. Verify InferCrane-Signature with it. Shown once; delete and recreate the destination to rotate it."},
			},
		},
		"NotificationSubscriptionEnvelope": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"subscription"}, "properties": map[string]any{"subscription": ref("NotificationSubscription")}},
		"NotificationSubscriptionList": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"data", "events"},
			"properties": map[string]any{
				"data":   map[string]any{"type": "array", "items": ref("NotificationSubscription")},
				"events": map[string]any{"type": "array", "description": "Every event a destination can subscribe to, in display order.", "items": map[string]any{"type": "string", "enum": notification.CampaignEvents}},
			},
		},
		"NotificationSubscriptionUpdate": map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"name": name, "url": url, "events": events, "enabled": map[string]any{"type": "boolean"}},
		},
	}
}
