package domain

import (
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

type Target struct {
	ID, Name, URL, Provider, Runtime, Health           string
	UpstreamModel, ProviderResourceID, ProviderDetails string
	CreatedAt, UpdatedAt                               time.Time
}

type Deployment struct {
	ID, TenantID, Name, Model, Runtime, RoutingStrategy string
	ActiveRevisionID, CandidateRevisionID               string
	DesiredState, ObservedState                         string
	MinReplicas, MaxReplicas                            int
	AutoscalingEnabled                                  bool
	CreatedAt, UpdatedAt                                time.Time
}

type DeploymentRevision struct {
	ID, DeploymentID, Status, SpecJSON, SourceRevisionID, Reason string
	Number                                                       int
	CreatedAt                                                    time.Time
	ActivatedAt, CompletedAt                                     *time.Time
}

type DeploymentRevisionSpec struct {
	Model              string   `json:"model"`
	ModelRevision      string   `json:"model_revision,omitempty"`
	Runtime            string   `json:"runtime"`
	RuntimeVersion     string   `json:"runtime_version,omitempty"`
	RuntimeArgs        []string `json:"runtime_args,omitempty"`
	RoutingStrategy    string   `json:"routing_strategy"`
	MinReplicas        int      `json:"min_replicas"`
	MaxReplicas        int      `json:"max_replicas"`
	AutoscalingEnabled bool     `json:"autoscaling_enabled"`
	ComputeMode        string   `json:"compute_mode,omitempty"`
	Cloud              string   `json:"cloud,omitempty"`
	GPU                string   `json:"gpu,omitempty"`
	Region             string   `json:"region,omitempty"`
	Port               int      `json:"port,omitempty"`
}

type ModelArtifact struct {
	ID, TenantID, Source, Repository, RequestedRevision string
	ImmutableRevision, ModelIdentity, CacheState        string
	ApproximateSizeBytes                                *int64
	RuntimeCompatibilityJSON                            string
	ResolvedAt                                          time.Time
}

type ResolvedDeployment struct {
	Deployment Deployment
	Targets    []Target
}

type Event struct {
	ID, DeploymentID, TargetID, Type, Summary, Payload string
	CreatedAt                                          time.Time
}

type ScalingPolicy struct {
	DeploymentID                         string
	Enabled                              bool
	MinReplicas, MaxReplicas             int
	QueueThreshold                       float64
	ScaleUpIntervals, ScaleDownIntervals int
	Cooldown                             time.Duration
	LowLoadThreshold                     float64
}

type ScalingDecision struct {
	ID, DeploymentID, Action, Reason, SignalsJSON string
	OldReplicas, NewReplicas                      int
	CreatedAt                                     time.Time
}

type Metrics struct {
	RequestsRunning, RequestsWaiting, KVCacheUsage *float64
	PrefixCacheQueries, PrefixCacheHits            *float64
	PromptTokensTotal, GenerationTokensTotal       *float64
	Raw                                            string
}

type RequestStats struct {
	RequestsPerSecond     float64  `json:"requests_per_second"`
	InputTokensPerSecond  float64  `json:"input_tokens_per_second"`
	OutputTokensPerSecond float64  `json:"output_tokens_per_second"`
	ErrorRate             float64  `json:"error_rate"`
	P50LatencyMS          *float64 `json:"p50_latency_ms"`
	P95LatencyMS          *float64 `json:"p95_latency_ms"`
	P50TTFTMS             *float64 `json:"p50_ttft_ms"`
	P95TTFTMS             *float64 `json:"p95_ttft_ms"`
}

// InferenceRecord contains request metadata and measurements only. Prompt and
// generated content are deliberately excluded.
type InferenceRecord struct {
	RequestID, DeploymentID, RevisionID, TargetID string
	Provider, Runtime, ComputeMode, OperationName string
	ResponseModel, ErrorType                      string
	StartedAt                                     time.Time
	StatusCode                                    int
	LatencyMS                                     float64
	TTFTMS                                        *float64
	InputTokens, OutputTokens                     *int
	Streaming                                     bool
}

type ReleaseGuardPolicy struct {
	DeploymentID                   string  `json:"deployment_id"`
	Enabled                        bool    `json:"enabled"`
	MinimumRequests                int     `json:"minimum_requests"`
	MaxTTFTRegressionPercent       float64 `json:"max_ttft_regression_percent"`
	MaxLatencyRegressionPercent    float64 `json:"max_latency_regression_percent"`
	MaxErrorRateIncrease           float64 `json:"max_error_rate_increase"`
	MaxOutputThroughputDropPercent float64 `json:"max_output_throughput_drop_percent"`
}

type RevisionMetrics struct {
	Requests              int      `json:"requests"`
	ReadyReplicas         int      `json:"ready_replicas"`
	ErrorRate             float64  `json:"error_rate"`
	P95TTFTMS             *float64 `json:"p95_ttft_ms"`
	P95LatencyMS          *float64 `json:"p95_latency_ms"`
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second"`
}

type ReleaseGuardEvaluation struct {
	ID                  string    `json:"id"`
	DeploymentID        string    `json:"deployment_id"`
	ActiveRevisionID    string    `json:"active_revision_id"`
	CandidateRevisionID string    `json:"candidate_revision_id"`
	Decision            string    `json:"decision"`
	ReasonCodesJSON     string    `json:"-"`
	MetricsJSON         string    `json:"-"`
	PolicyJSON          string    `json:"-"`
	CreatedAt           time.Time `json:"created_at"`
}

type RouterGeneration struct {
	ID, DeploymentID, OwnerID, Strategy, WorkerSetHash, InternalEndpoint, Status string
	Generation                                                                   int
	CreatedAt                                                                    time.Time
}

type Replica struct {
	ID, TenantID, DeploymentID, RevisionID, ExternalKey, LifecycleState string
	Provider, ProviderRequestID, ProviderResourceID                     string
	Endpoint, Health, ProviderDetails                                   string
	Ordinal                                                             int
	LastObservedAt                                                      *time.Time
	CreatedAt, UpdatedAt                                                time.Time
}

type Operation struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id,omitempty"`
	Kind            string     `json:"kind"`
	ResourceType    string     `json:"resource_type,omitempty"`
	ResourceName    string     `json:"resource_name,omitempty"`
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	Status          string     `json:"status"`
	Message         string     `json:"message,omitempty"`
	RequestJSON     string     `json:"request_json,omitempty"`
	ResultJSON      string     `json:"result_json,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
	Progress        int        `json:"progress"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	Retryable       bool       `json:"retryable"`
	CancelRequested bool       `json:"cancel_requested"`
	LeaseOwner      string     `json:"lease_owner,omitempty"`
	LeaseGeneration int64      `json:"lease_generation,omitempty"`
	CreatedAt       time.Time  `json:"created_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	NextAttemptAt   *time.Time `json:"next_attempt_at,omitempty"`
}

type OperationStep struct {
	OperationID, Name, Status, CheckpointJSON, ErrorCode string
	Attempt                                              int
	StartedAt, UpdatedAt                                 time.Time
	CompletedAt                                          *time.Time
}

type OperationEvent struct {
	OperationID, Level, Type, Message, Payload string
	Sequence                                   int64
	CreatedAt                                  time.Time
}

type Orphan struct {
	TargetID, Name, Provider, ProviderResourceID string
	CreatedAt                                    time.Time
}

type AuditEvent struct {
	ID, TenantID, Actor, Action, ResourceType, ResourceName string
	Outcome, RequestID, Payload                             string
	CreatedAt                                               time.Time
}

type Principal struct {
	ID, TenantID, Name, Role string
	Disabled                 bool
	CreatedAt                time.Time
}
type CredentialRecord struct {
	Hash      string
	Principal Principal
}

var RoutingStrategies = map[string]string{
	"round-robin":     "round_robin",
	"consistent-hash": "consistent_hash",
	"power-of-two":    "power_of_two",
	"cache-aware":     "cache_aware",
}
