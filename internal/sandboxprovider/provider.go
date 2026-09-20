// Package sandboxprovider defines the customer-facing sandbox lifecycle
// boundary. Providers execute the lifecycle; InferCrane owns tenancy, policy,
// audit, and the stable product contract.
package sandboxprovider

import (
	"context"
	"errors"
	"io"
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

// Workspace is the provider-side durable storage attached at /workspace.
// Its ID is a backend reference and must never be returned from InferCrane's
// customer API.
type Workspace struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Failure   *Failure  `json:"failure,omitempty"`
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

type WorkspaceMutation struct {
	Resource  Workspace `json:"resource"`
	Operation Operation `json:"operation"`
}

type CreateRequest struct {
	TemplateID          string
	WorkspaceID         string
	ConnectorRevision   string
	ExpiresAfterSeconds int64
	StandbyAfterSeconds int64
	AutoResume          bool
	NetworkMode         string
}

type CommandRequest struct {
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty"`
}

type CommandEvent struct {
	ExecutionID string `json:"execution_id,omitempty"`
	Type        string `json:"type"`
	PID         uint32 `json:"pid,omitempty"`
	Data        []byte `json:"data,omitempty"`
	ExitCode    *int32 `json:"exit_code,omitempty"`
	Exited      bool   `json:"exited,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

type FileInfo struct {
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type FileDownload struct {
	Body io.ReadCloser
	FileInfo
}

type PortLease struct {
	Path      string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PreviewRequest and PreviewResponse carry an authenticated preview through
// InferCrane. The provider lease path is never returned to the browser.
type PreviewRequest struct {
	Method   string
	Path     string
	RawQuery string
}

type PreviewResponse struct {
	StatusCode int
	Header     map[string][]string
	Body       io.ReadCloser
}

type Event struct {
	ID          string         `json:"id"`
	Sequence    int64          `json:"sequence"`
	OperationID string         `json:"operation_id,omitempty"`
	Type        string         `json:"type"`
	State       string         `json:"state,omitempty"`
	At          time.Time      `json:"at"`
	Details     map[string]any `json:"details,omitempty"`
}

// ReceiptEvidence is the customer-safe projection of a provider receipt. The
// signed envelope remains server-side because it contains backend resource and
// project identities.
type ReceiptEvidence struct {
	Digest              string `json:"digest"`
	Signed              bool   `json:"signed"`
	KeyID               string `json:"key_id,omitempty"`
	State               string `json:"state,omitempty"`
	EnvironmentRevision string `json:"environment_revision,omitempty"`
	Assurance           string `json:"assurance,omitempty"`
}

type Provider interface {
	Capabilities(context.Context, string) (Capabilities, error)
	Get(context.Context, string, string) (Sandbox, error)
	CreateWorkspace(context.Context, string, string, string) (WorkspaceMutation, error)
	DeleteWorkspace(context.Context, string, string, string) (WorkspaceMutation, error)
	Create(context.Context, string, string, CreateRequest) (Mutation, error)
	Pause(context.Context, string, string, string) (Mutation, error)
	Resume(context.Context, string, string, string) (Mutation, error)
	Delete(context.Context, string, string, string) (Mutation, error)
	RunCommand(context.Context, string, string, CommandRequest, func(CommandEvent) error) (string, error)
	WriteFile(context.Context, string, string, string, io.Reader, int64) (FileInfo, error)
	ReadFile(context.Context, string, string, string) (FileDownload, error)
	CreatePortLease(context.Context, string, string, uint16, int64) (PortLease, error)
	ProxyPreview(context.Context, string, string, PreviewRequest) (PreviewResponse, error)
	Events(context.Context, string, string) ([]Event, error)
	Receipt(context.Context, string, string) (ReceiptEvidence, error)
}
