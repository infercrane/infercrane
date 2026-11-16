package notification

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/infercrane/infercrane/internal/alert"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

// ErrInvalid marks a request the caller can fix.
var ErrInvalid = errors.New("invalid notification destination")

// SecretPrefix starts every generated signing secret.
const SecretPrefix = "icwh_"

const (
	maxURLLength = 2048
	// testClaim holds a test delivery for the request that sends it.
	testClaim = time.Minute
)

// Store is the persistence boundary of subscriptions and their deliveries.
type Store interface {
	CreateNotificationSubscription(context.Context, string, domain.NotificationSubscription) (domain.NotificationSubscription, error)
	UpdateNotificationSubscription(context.Context, string, domain.NotificationSubscription) (domain.NotificationSubscription, error)
	DeleteNotificationSubscription(context.Context, string, string) error
	NotificationSubscriptionForTenant(context.Context, string, string) (domain.NotificationSubscription, error)
	NotificationSubscriptionsForTenant(context.Context, string) ([]domain.NotificationSubscription, error)
	InsertNotificationDelivery(context.Context, domain.NotificationDelivery, time.Duration) (domain.NotificationDelivery, bool, error)
	RecordNotificationDeliveryAttempt(context.Context, string, bool, int, string, *time.Time) (domain.NotificationDelivery, error)
}

// Poster sends one signed body. alert.Deliverer implements it.
type Poster interface {
	Post(context.Context, string, string, string, []byte) (alert.Attempt, error)
}

// Service owns the write-only boundary of notification destinations: the URL
// and signing secret are sealed at rest and never returned after creation.
type Service struct {
	Store        Store
	Cipher       secretcipher.Cipher
	KeyReference string
	Poster       Poster
	ConsoleURL   string
	Now          func() time.Time
}

type CreateRequest struct {
	Name        string   `json:"name"`
	Destination string   `json:"destination"`
	URL         string   `json:"url"`
	Events      []string `json:"events"`
	Enabled     *bool    `json:"enabled"`
}

type UpdateRequest struct {
	Name    *string   `json:"name"`
	URL     *string   `json:"url"`
	Events  *[]string `json:"events"`
	Enabled *bool     `json:"enabled"`
}

// Create seals and stores a destination. The signing secret is returned once.
func (s Service) Create(ctx context.Context, tenant, actor string, request CreateRequest) (domain.NotificationSubscription, string, error) {
	if err := s.ready(); err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	if tenant == "" || actor == "" {
		return domain.NotificationSubscription{}, "", errors.New("tenant and actor are required")
	}
	destination := strings.TrimSpace(strings.ToLower(request.Destination))
	if destination != DestinationWebhook && destination != DestinationSlack {
		return domain.NotificationSubscription{}, "", fmt.Errorf("%w: destination must be webhook or slack", ErrInvalid)
	}
	target, hint, err := validateURL(destination, request.URL)
	if err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	events, err := normalizeEvents(request.Events)
	if err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	name, err := normalizeName(request.Name, hint)
	if err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	id, err := randomHex(16)
	if err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	secretBytes := make([]byte, 32)
	if _, err = rand.Read(secretBytes); err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	secret := SecretPrefix + base64.RawURLEncoding.EncodeToString(secretBytes)
	item := domain.NotificationSubscription{ID: id, Name: name, Destination: destination, URLHint: hint, KeyReference: s.KeyReference, Events: events, Enabled: request.Enabled == nil || *request.Enabled, CreatedBy: actor}
	if item.URLCiphertext, item.URLNonce, err = s.Cipher.Encrypt([]byte(target), associatedData(tenant, id, "url")); err != nil {
		return domain.NotificationSubscription{}, "", errors.New("destination encryption failed")
	}
	if item.SecretCiphertext, item.SecretNonce, err = s.Cipher.Encrypt([]byte(secret), associatedData(tenant, id, "secret")); err != nil {
		return domain.NotificationSubscription{}, "", errors.New("signing secret encryption failed")
	}
	created, err := s.Store.CreateNotificationSubscription(ctx, tenant, item)
	if err != nil {
		return domain.NotificationSubscription{}, "", err
	}
	return created, secret, nil
}

// Update changes the name, URL, events, or enabled flag. The destination kind
// and signing secret never change; delete and recreate to rotate the secret.
func (s Service) Update(ctx context.Context, tenant, id string, request UpdateRequest) (domain.NotificationSubscription, error) {
	if err := s.ready(); err != nil {
		return domain.NotificationSubscription{}, err
	}
	item, err := s.Store.NotificationSubscriptionForTenant(ctx, tenant, id)
	if err != nil {
		return domain.NotificationSubscription{}, err
	}
	if request.URL != nil {
		target, hint, validateErr := validateURL(item.Destination, *request.URL)
		if validateErr != nil {
			return domain.NotificationSubscription{}, validateErr
		}
		if item.URLCiphertext, item.URLNonce, err = s.Cipher.Encrypt([]byte(target), associatedData(tenant, id, "url")); err != nil {
			return domain.NotificationSubscription{}, errors.New("destination encryption failed")
		}
		item.URLHint = hint
	}
	if request.Events != nil {
		if item.Events, err = normalizeEvents(*request.Events); err != nil {
			return domain.NotificationSubscription{}, err
		}
	}
	if request.Name != nil {
		if item.Name, err = normalizeName(*request.Name, item.URLHint); err != nil {
			return domain.NotificationSubscription{}, err
		}
	}
	if request.Enabled != nil {
		item.Enabled = *request.Enabled
	}
	return s.Store.UpdateNotificationSubscription(ctx, tenant, item)
}

func (s Service) Delete(ctx context.Context, tenant, id string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.Store.DeleteNotificationSubscription(ctx, tenant, id)
}

func (s Service) List(ctx context.Context, tenant string) ([]domain.NotificationSubscription, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	return s.Store.NotificationSubscriptionsForTenant(ctx, tenant)
}

// Test sends one test event now, whether or not the destination is enabled,
// and records it like any other delivery.
func (s Service) Test(ctx context.Context, tenant, id string) (domain.NotificationDelivery, error) {
	if err := s.ready(); err != nil {
		return domain.NotificationDelivery{}, err
	}
	item, err := s.Store.NotificationSubscriptionForTenant(ctx, tenant, id)
	if err != nil {
		return domain.NotificationDelivery{}, err
	}
	target, secret, err := s.open(item)
	if err != nil {
		return domain.NotificationDelivery{}, err
	}
	suffix, err := randomHex(8)
	if err != nil {
		return domain.NotificationDelivery{}, err
	}
	at := s.now()
	links := Links{}
	if base := strings.TrimRight(strings.TrimSpace(s.ConsoleURL), "/"); base != "" {
		links.Dashboard = base + "/settings/notifications"
	}
	encoded, err := Encode(item.Destination, TestBody(tenant, "test:"+suffix, at, links))
	if err != nil {
		return domain.NotificationDelivery{}, err
	}
	delivery, _, err := s.Store.InsertNotificationDelivery(ctx, domain.NotificationDelivery{TenantID: tenant, SubscriptionID: item.ID, EventID: "test:" + suffix, EventType: EventTest, Body: string(encoded), BodyDigest: digest(encoded), MaxAttempts: 1, OccurredAt: at}, testClaim)
	if err != nil {
		return domain.NotificationDelivery{}, err
	}
	return s.attempt(ctx, delivery, target, secret, nil)
}

// attempt sends a delivery once and records the result. retryAt is when to
// try again after a retryable failure; nil makes any failure final.
func (s Service) attempt(ctx context.Context, delivery domain.NotificationDelivery, target, secret string, retryAt *time.Time) (domain.NotificationDelivery, error) {
	result, err := s.Poster.Post(ctx, target, secret, delivery.ID, []byte(delivery.Body))
	if err != nil {
		// The destination failed the public-network check. DNS can change, so
		// it is retried within the delivery's attempt budget.
		return s.Store.RecordNotificationDeliveryAttempt(context.WithoutCancel(ctx), delivery.ID, false, 0, "destination_rejected", retryAt)
	}
	if result.Permanent {
		retryAt = nil
	}
	return s.Store.RecordNotificationDeliveryAttempt(context.WithoutCancel(ctx), delivery.ID, result.Delivered, result.Status, result.Code, retryAt)
}

func (s Service) open(item domain.NotificationSubscription) (string, string, error) {
	if item.KeyReference != s.KeyReference {
		return "", "", errors.New("notification destination uses an unavailable encryption key")
	}
	target, err := s.Cipher.Decrypt(item.URLCiphertext, item.URLNonce, associatedData(item.TenantID, item.ID, "url"))
	if err != nil {
		return "", "", errors.New("notification destination decryption failed")
	}
	secret, err := s.Cipher.Decrypt(item.SecretCiphertext, item.SecretNonce, associatedData(item.TenantID, item.ID, "secret"))
	if err != nil {
		return "", "", errors.New("notification signing secret decryption failed")
	}
	return string(target), string(secret), nil
}

func (s Service) ready() error {
	if s.Store == nil || s.Poster == nil || s.KeyReference == "" {
		return errors.New("notification storage is not configured")
	}
	return nil
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// validateURL returns the normalized URL and a hint that names its host and
// the last characters of its path, which is enough to tell two apart.
func validateURL(destination, raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > maxURLLength || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", "", fmt.Errorf("%w: url must be an absolute HTTPS URL without credentials or a fragment", ErrInvalid)
	}
	if destination == DestinationSlack && (!strings.EqualFold(parsed.Hostname(), "hooks.slack.com") || parsed.Port() != "" || !strings.HasPrefix(parsed.Path, "/services/")) {
		return "", "", fmt.Errorf("%w: a Slack destination must be an incoming webhook URL that starts with https://hooks.slack.com/services/", ErrInvalid)
	}
	hint := "https://" + parsed.Host
	if path := strings.Trim(parsed.EscapedPath(), "/"); path != "" {
		hint += "/…"
		if len(path) > 8 {
			hint += path[len(path)-4:]
		}
	}
	return parsed.String(), hint, nil
}

func normalizeEvents(requested []string) ([]string, error) {
	wanted := map[string]bool{}
	for _, event := range requested {
		event = strings.TrimSpace(strings.ToLower(event))
		known := false
		for _, candidate := range CampaignEvents {
			known = known || candidate == event
		}
		if !known {
			return nil, fmt.Errorf("%w: unknown event %q; use %s", ErrInvalid, event, strings.Join(CampaignEvents, ", "))
		}
		wanted[event] = true
	}
	events := make([]string, 0, len(wanted))
	for _, event := range CampaignEvents {
		if wanted[event] {
			events = append(events, event)
		}
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: choose at least one event", ErrInvalid)
	}
	return events, nil
}

func normalizeName(name, hint string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimPrefix(hint, "https://")
		if index := strings.Index(name, "/"); index > 0 {
			name = name[:index]
		}
	}
	if len([]rune(name)) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: name must be at most 64 printable characters", ErrInvalid)
	}
	return name, nil
}

func associatedData(tenant, id, field string) []byte {
	return []byte("infercrane.notification-destination.v1\x00" + tenant + "\x00" + id + "\x00" + field)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
