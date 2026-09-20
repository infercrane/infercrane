// Package sandboxprovider defines the customer-facing sandbox lifecycle
// boundary. Providers execute the lifecycle; InferCrane owns tenancy, policy,
// audit, and the stable product contract.
package sandboxprovider

import (
	"context"
	"errors"
	"time"
)

var (
	ErrUnavailable = errors.New("sandbox provider is unavailable")
	ErrForbidden   = errors.New("sandbox provider is not enabled for this tenant")
	ErrNotFound    = errors.New("sandbox was not found")
	ErrConflict    = errors.New("sandbox lifecycle conflict")
	ErrInvalid     = errors.New("invalid sandbox request")
	ErrUpstream    = errors.New("sandbox provider request failed")
)

// Template is a product-approved immutable sandbox environment. The provider
// revision stays visible as evidence, but customers select the stable ID.
type Template struct {
	ID                  string `json:"id"`
	Label               string `json:"label"`
	EnvironmentRevision string `json:"environment_revision"`
}

type Features struct {
	HostileCodeIsolation bool `json:"hostile_code_isolation"`
	DenyByDefaultEgress  bool `json:"deny_by_default_egress"`
	PauseResume          bool `json:"pause_resume"`
	FilesystemCheckpoint bool `json:"filesystem_checkpoint"`
	CommandStreaming     bool `json:"command_streaming"`
	FileReadWrite        bool `json:"file_read_write"`
	HTTPPreview          bool `json:"http_preview"`
	DurableWorkspaces    bool `json:"durable_workspaces"`
	InteractivePTY       bool `json:"interactive_pty"`
	GPU                  bool `json:"gpu"`
}

type Capabilities struct {
	Provider          string     `json:"provider"`
	Product           string     `json:"product"`
	State             string     `json:"state"`
	Assurance         string     `json:"assurance"`
	Runtime           string     `json:"runtime"`
	Qualification     string     `json:"qualification"`
	QualificationNote string     `json:"qualification_note,omitempty"`
	Templates         []Template `json:"templates"`
	Features          Features   `json:"features"`
}

type Lifecycle struct {
	ExpiresAfterSeconds int64 `json:"expires_after_seconds"`
	StandbyAfterSeconds int64 `json:"standby_after_seconds,omitempty"`
	AutoResume          bool  `json:"auto_resume,omitempty"`
}

type Network struct {
	Mode          string `json:"mode"`
	AllowInternet bool   `json:"allow_internet"`
}

type Failure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Sandbox struct {
	ID                  string    `json:"id"`
	Provider            string    `json:"provider"`
	TemplateID          string    `json:"template_id,omitempty"`
	EnvironmentRevision string    `json:"environment_revision"`
	State               string    `json:"state"`
	Lifecycle           Lifecycle `json:"lifecycle"`
	Network             Network   `json:"network"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	Failure             *Failure  `json:"failure,omitempty"`
}

type Operation struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	ResourceID string    `json:"resource_id"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Failure    *Failure  `json:"failure,omitempty"`
}

type Mutation struct {
	Resource  Sandbox   `json:"resource"`
	Operation Operation `json:"operation"`
}

type CreateRequest struct {
	TemplateID          string
	ExpiresAfterSeconds int64
	StandbyAfterSeconds int64
	AutoResume          bool
	NetworkMode         string
}

type Provider interface {
	Capabilities(context.Context, string) (Capabilities, error)
	List(context.Context, string, bool) ([]Sandbox, error)
	Get(context.Context, string, string) (Sandbox, error)
	Create(context.Context, string, string, CreateRequest) (Mutation, error)
	Pause(context.Context, string, string, string) (Mutation, error)
	Resume(context.Context, string, string, string) (Mutation, error)
	Delete(context.Context, string, string, string) (Mutation, error)
}
