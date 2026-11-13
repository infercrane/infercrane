// Package clilogin holds the policy for signing the CLI in through the
// dashboard: code formats, lifetimes, request bounds, and which permissions an
// approved sign-in may carry. It has no I/O so the control API, the store, and
// the CLI agree on one definition.
package clilogin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/infercrane/infercrane/internal/authz"
)

const (
	// RequestTTL bounds how long a sign-in waits for approval in the browser.
	RequestTTL = 10 * time.Minute
	// PollInterval is the slowest rate the CLI is told to poll at. Polls that
	// arrive faster than MinPollGap are told to slow down.
	PollInterval = 3 * time.Second
	MinPollGap   = 2 * time.Second
	// SessionLifetime is how long a key issued to the CLI stays valid. The
	// person signs in again after it expires, and can revoke it at any time.
	SessionLifetime = 90 * 24 * time.Hour
	// MaxPending bounds unauthenticated sign-in requests waiting for approval
	// across the control plane.
	MaxPending = 500
	// Retention is how long finished requests are kept before they are pruned.
	Retention = 24 * time.Hour

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"
	StatusConsumed = "consumed"
	StatusExpired  = "expired"
	StatusSlowDown = "slow_down"

	// PrincipalKind marks keys issued through a CLI sign-in.
	PrincipalKind = "cli_session"

	// DeviceCodePrefix makes a leaked device code recognizable in scans.
	DeviceCodePrefix = "icd_"
)

// userCodeAlphabet has no vowels, so codes never spell words, and no digits or
// letters that are easy to confuse when read aloud or retyped (RFC 8628 §6.1).
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

var ErrInvalidClient = errors.New("invalid CLI client description")

// Client describes the device asking to sign in. It is shown to the person
// approving the request, so it is bounded and stripped of control characters.
type Client struct {
	DeviceName string `json:"device_name"`
	OS         string `json:"client_os"`
	Version    string `json:"client_version"`
}

// Normalize trims the client description and rejects values that cannot be
// shown safely.
func (c Client) Normalize() (Client, error) {
	out := Client{DeviceName: clean(c.DeviceName), OS: clean(c.OS), Version: clean(c.Version)}
	switch {
	case out.DeviceName == "" || utf8.RuneCountInString(out.DeviceName) > 120:
		return Client{}, fmt.Errorf("%w: device_name must be 1 to 120 characters", ErrInvalidClient)
	case out.OS == "" || utf8.RuneCountInString(out.OS) > 64:
		return Client{}, fmt.Errorf("%w: client_os must be 1 to 64 characters", ErrInvalidClient)
	case out.Version == "" || utf8.RuneCountInString(out.Version) > 64:
		return Client{}, fmt.Errorf("%w: client_version must be 1 to 64 characters", ErrInvalidClient)
	}
	return out, nil
}

func clean(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

// NewUserCode returns a code such as "WXZB-KLMN" drawn from a crypto RNG.
func NewUserCode() (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(userCodeAlphabet)))
	for i := 0; i < 8; i++ {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate user code: %w", err)
		}
		b.WriteByte(userCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// NormalizeUserCode accepts what a person may retype (lower case, spaces, a
// missing dash) and returns the canonical form.
func NormalizeUserCode(value string) (string, bool) {
	compact := strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, value)
	if len(compact) != 8 {
		return "", false
	}
	for _, r := range compact {
		if !strings.ContainsRune(userCodeAlphabet, r) {
			return "", false
		}
	}
	return compact[:4] + "-" + compact[4:], true
}

// NewDeviceCode returns the secret the CLI polls with and the hash stored by
// the control plane. The code itself is never persisted.
func NewDeviceCode() (code, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate device code: %w", err)
	}
	code = DeviceCodePrefix + hex.EncodeToString(raw)
	return code, HashDeviceCode(code), nil
}

// HashDeviceCode returns the stored form of a device code, or "" when the
// value cannot be a device code.
func HashDeviceCode(code string) string {
	if !strings.HasPrefix(code, DeviceCodePrefix) || len(code) != len(DeviceCodePrefix)+64 {
		return ""
	}
	if _, err := hex.DecodeString(code[len(DeviceCodePrefix):]); err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// ApproverAllowed reports whether a principal kind may approve a CLI sign-in.
// People approve through the dashboard. Local dashboards sign in with a
// service account or the bootstrap key. A CLI key and an endpoint-scoped
// inference token can never mint another key.
func ApproverAllowed(kind string) bool {
	return kind == "human" || kind == "service_account" || kind == "bootstrap"
}

// IssuedScopes returns the explicit scopes for a key issued to the CLI. The key
// carries exactly the approver's permissions and never more. A legacy
// principal without explicit scopes keeps its legacy ceiling: no secret or
// external-target management.
func IssuedScopes(role authz.Role, scopes []string) ([]authz.Action, error) {
	var out []authz.Action
	if len(scopes) == 0 {
		for _, action := range authz.DefaultScopes(role) {
			if authz.AllowedScoped(role, nil, action) {
				out = append(out, action)
			}
		}
	} else {
		for _, scope := range scopes {
			if authz.AllowedScoped(role, scopes, authz.Action(scope)) {
				out = append(out, authz.Action(scope))
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the approving principal has no permissions to delegate")
	}
	if err := authz.ValidateScopes(role, out); err != nil {
		return nil, err
	}
	return out, nil
}

// PrincipalName names an issued key so audit trails show which device used it.
func PrincipalName(deviceName, id string) string {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	name := []rune(deviceName)
	if len(name) > 80 {
		name = name[:80]
	}
	return "cli:" + string(name) + ":" + short
}
