// Package alert delivers deterministic signed webhook findings.
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
	now := d.Now
	if now == nil {
		now = time.Now
	}
	client := d.Client
	if client == nil {
		client = d.safeClient()
	}
	for delivery.Attempts < policy.MaxAttempts {
		timestamp := strconv.FormatInt(now().UTC().Unix(), 10)
		signature := sign(secret, timestamp, body)
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, policy.WebhookURL, bytes.NewReader(body))
		if requestErr != nil {
			return delivery, requestErr
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("InferCrane-Delivery", delivery.ID)
		request.Header.Set("InferCrane-Timestamp", timestamp)
		request.Header.Set("InferCrane-Signature", "v1="+signature)
		response, sendErr := client.Do(request)
		status, code, delivered := 0, "network_error", false
		if sendErr == nil {
			status = response.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			delivered = status >= 200 && status < 300
			if delivered {
				code = ""
			}
			if !delivered {
				code = "http_" + strconv.Itoa(status)
			}
		}
		maxAttempts := policy.MaxAttempts
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			maxAttempts = delivery.Attempts + 1
		}
		if err = d.Store.RecordAlertDeliveryAttempt(ctx, delivery.ID, delivered, status, code, maxAttempts); err != nil {
			return delivery, err
		}
		delivery.Attempts++
		delivery.ResponseStatus = status
		if delivered {
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

func (d Deliverer) safeClient() *http.Client {
	resolve := d.ResolveIP
	if resolve == nil {
		resolve = net.DefaultResolver.LookupIPAddr
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolve(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("webhook destination DNS could not be validated")
		}
		var lastErr error
		for _, resolved := range addresses {
			if !d.AllowPrivate && (resolved.IP.IsPrivate() || resolved.IP.IsLoopback() || resolved.IP.IsLinkLocalUnicast() || resolved.IP.IsUnspecified()) {
				continue
			}
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("webhook destination has no permitted address")
	}}
	return &http.Client{Timeout: 10 * time.Second, Transport: transport}
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
