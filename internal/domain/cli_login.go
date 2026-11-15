package domain

import (
	"errors"
	"time"
)

// ErrCapacityExhausted reports that a bounded queue is full.
var ErrCapacityExhausted = errors.New("capacity exhausted")

// CLILoginStart is a new CLI sign-in waiting for approval in the dashboard.
type CLILoginStart struct {
	UserCode       string
	DeviceCodeHash string
	DeviceName     string
	ClientOS       string
	ClientVersion  string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// CLILoginRequest is what the dashboard shows the person approving a sign-in.
// It never contains the device code or a credential.
type CLILoginRequest struct {
	ID            string
	UserCode      string
	DeviceName    string
	ClientOS      string
	ClientVersion string
	Status        string
	TenantID      string
	ApproverID    string
	ApproverName  string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	DecidedAt     *time.Time
}

// CLILoginExchange is the result of the CLI polling with its device code.
// Credential is set once, on the poll that consumes an approved request.
type CLILoginExchange struct {
	Status     string
	Principal  Principal
	Credential string
	// OwnerName is the person who approved the sign-in.
	OwnerName string
}

// CLISession is a key issued through a CLI sign-in. ID is the key's principal.
type CLISession struct {
	ID            string
	TenantID      string
	OwnerID       string
	OwnerName     string
	DeviceName    string
	ClientOS      string
	ClientVersion string
	CreatedAt     time.Time
	LastUsedAt    *time.Time
	ExpiresAt     *time.Time
}
