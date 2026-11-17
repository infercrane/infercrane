// Package alert delivers signed webhooks: deterministic diagnostic findings,
// and through Post, any caller-built event such as an optimization campaign
// notification.
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/safehttp"
	"github.com/infercrane/infercrane/internal/secrets"
)

type Store interface {
	SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error)
	BeginAlertDelivery(context.Context, domain.AlertPolicy, domain.DiagnosticFinding, string) (domain.AlertDelivery, bool, error)
	RecordAlertDeliveryAttempt(context.Context, string, bool, int, string, int) error
}

type Deliverer struct {
	Store        Store
	Secrets      secrets.Resolver
	Client       *http.Client
	ResolveIP    func(context.Context, string) ([]net.IPAddr, error)
	AllowPrivate bool
	Now          func() time.Time
}

type payload struct {
	SchemaVersion int                      `json:"schema_version"`
	Event         string                   `json:"event"`
	Finding       domain.DiagnosticFinding `json:"finding"`
}

func (d Deliverer) Deliver(ctx context.Context, policy domain.AlertPolicy, finding domain.DiagnosticFinding) (domain.AlertDelivery, error) {
	if !policy.Enabled || !severityAtLeast(finding.Severity, policy.MinimumSeverity) {
		return domain.AlertDelivery{}, nil
	}
	if d.Store == nil || d.Secrets == nil {
		return domain.AlertDelivery{}, errors.New("alert delivery dependencies are unavailable")
	}
	if err := d.validateDestination(ctx, policy.WebhookURL); err != nil {
		return domain.AlertDelivery{}, err
	}
	body, err := json.Marshal(payload{SchemaVersion: 1, Event: "infercrane.diagnostic_finding", Finding: finding})
	if err != nil {
		return domain.AlertDelivery{}, err
	}
	digestBytes := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	delivery, _, err := d.Store.BeginAlertDelivery(ctx, policy, finding, digest)
	if err != nil || delivery.Status == "delivered" || delivery.Status == "failed" {
		return delivery, err
	}
	reference, err := d.Store.SecretReferenceForTenant(ctx, policy.TenantID, policy.SecretReferenceID)
	if err != nil {
		return delivery, err
	}
	secret, err := d.Secrets.Resolve(ctx, reference)
	if err != nil {
		return delivery, errors.New("alert signing secret is unavailable")
	}
	client := d.client()
	for delivery.Attempts < policy.MaxAttempts {
		attempt, sendErr := d.send(ctx, client, policy.WebhookURL, secret, delivery.ID, body)
		if sendErr != nil {
			return delivery, sendErr
		}
		maxAttempts := policy.MaxAttempts
		if attempt.Permanent {
			maxAttempts = delivery.Attempts + 1
		}
		if err = d.Store.RecordAlertDeliveryAttempt(ctx, delivery.ID, attempt.Delivered, attempt.Status, attempt.Code, maxAttempts); err != nil {
			return delivery, err
		}
		delivery.Attempts++
		delivery.ResponseStatus = attempt.Status
		if attempt.Delivered {
			delivery.Status = "delivered"
			return delivery, nil
		}
		if maxAttempts <= delivery.Attempts {
			delivery.Status = "failed"
			break
		}
	}
	delivery.Status = "failed"
	return delivery, fmt.Errorf("alert delivery failed after %d attempts", delivery.Attempts)
}

// Attempt is the outcome of one signed POST.
type Attempt struct {
	// Status is the HTTP response status, or 0 when no response arrived.
	Status int
	// Code is empty on delivery, otherwise network_error or http_<status>.
	Code      string
	Delivered bool
	// Permanent is true for a 4xx other than 429: retrying the same body
	// cannot succeed.
	Permanent bool
}

// Post validates the destination and sends one exact body once, signed with
// the same InferCrane headers as alert deliveries. The caller owns retries and
// the delivery ledger. Only the destination check returns an error; a refused
// or failed request is reported in the Attempt.
func (d Deliverer) Post(ctx context.Context, destination, secret, deliveryID string, body []byte) (Attempt, error) {
	if secret == "" || deliveryID == "" {
		return Attempt{}, errors.New("a signing secret and delivery ID are required")
	}
	if err := d.validateDestination(ctx, destination); err != nil {
		return Attempt{}, err
	}
	return d.send(ctx, d.client(), destination, secret, deliveryID, body)
}

func (d Deliverer) send(ctx context.Context, client *http.Client, destination, secret, deliveryID string, body []byte) (Attempt, error) {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	timestamp := strconv.FormatInt(now().UTC().Unix(), 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination, bytes.NewReader(body))
	if err != nil {
		return Attempt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("InferCrane-Delivery", deliveryID)
	request.Header.Set("InferCrane-Timestamp", timestamp)
	request.Header.Set("InferCrane-Signature", "v1="+sign(secret, timestamp, body))
	response, err := client.Do(request)
	if err != nil {
		return Attempt{Code: "network_error"}, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	attempt := Attempt{Status: response.StatusCode, Delivered: response.StatusCode >= 200 && response.StatusCode < 300}
	if !attempt.Delivered {
		attempt.Code = "http_" + strconv.Itoa(response.StatusCode)
		attempt.Permanent = response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests
	}
	return attempt, nil
}

func (d Deliverer) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return safehttp.WebhookClient(d.ResolveIP, d.AllowPrivate)
}

func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func severityAtLeast(actual, minimum string) bool {
	levels := map[string]int{"info": 0, "warning": 1, "critical": 2}
	return levels[actual] >= levels[minimum]
}

func (d Deliverer) validateDestination(ctx context.Context, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("webhook destination must be HTTPS")
	}
	if d.AllowPrivate {
		return nil
	}
	resolve := d.ResolveIP
	if resolve == nil {
		resolver := net.DefaultResolver
		resolve = resolver.LookupIPAddr
	}
	addresses, err := resolve(ctx, parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		return errors.New("webhook destination DNS could not be validated")
	}
	for _, address := range addresses {
		if address.IP.IsPrivate() || address.IP.IsLoopback() || address.IP.IsLinkLocalUnicast() || address.IP.IsUnspecified() {
			return errors.New("webhook destination resolves to a private or local address")
		}
	}
	return nil
}
