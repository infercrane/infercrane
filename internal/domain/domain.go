package domain

import (
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/runtimecontract"
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
	ComputeMode                                         string
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
	Model              string                   `json:"model"`
	ModelRevision      string                   `json:"model_revision,omitempty"`
	Runtime            string                   `json:"runtime"`
	RuntimeVersion     string                   `json:"runtime_version,omitempty"`
	RuntimeArgs        []string                 `json:"runtime_args,omitempty"`
	RoutingStrategy    string                   `json:"routing_strategy"`
	MinReplicas        int                      `json:"min_replicas"`
	MaxReplicas        int                      `json:"max_replicas"`
	AutoscalingEnabled bool                     `json:"autoscaling_enabled"`
	ComputeMode        string                   `json:"compute_mode,omitempty"`
	Cloud              string                   `json:"cloud,omitempty"`
	GPU                string                   `json:"gpu,omitempty"`
	Region             string                   `json:"region,omitempty"`
	Port               int                      `json:"port,omitempty"`
	Workload           runtimecontract.Workload `json:"workload,omitzero"`
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

// LogicalModel is a stable product-level identity. It intentionally contains
// no provider or runtime fields.
type LogicalModel struct {
	ID, TenantID, Name, Description string
	CreatedAt, UpdatedAt            time.Time
}

// Environment scopes endpoint policy without encoding a cloud environment.
type Environment struct {
	ID, TenantID, Name, PolicyJSON string
	CreatedAt, UpdatedAt           time.Time
}

type Endpoint struct {
	ID, TenantID, LogicalModelID, EnvironmentID string
	Name, DesiredState, ObservedState           string
	ActiveServingPlanID, CandidateServingPlanID string
	CreatedAt, UpdatedAt                        time.Time
}

type BackendBinding struct {
	ID, TenantID, EndpointID, Name, Kind, OwnershipMode string
	DeploymentID, TargetID, ConfigJSON                  string
	CreatedAt, UpdatedAt                                time.Time
}

type ServingPlanBinding struct {
	BindingID string `json:"binding_id"`
	Priority  int    `json:"priority"`
	Weight    int    `json:"weight"`
}

// ServingPlan is immutable after creation. SpecDigest identifies the canonical
// routing policy and ordered binding set.
type ServingPlan struct {
	ID, TenantID, EndpointID, RoutingPolicy string
	SpecJSON, SpecDigest                    string
	Version                                 int
	Bindings                                []ServingPlanBinding
	CreatedAt                               time.Time
}

type ResolvedEndpoint struct {
	Endpoint      Endpoint
	LogicalModel  LogicalModel
	Environment   Environment
	ActivePlan    ServingPlan
	CandidatePlan *ServingPlan
	Bindings      []BackendBinding
}

type EndpointReleaseGuardPolicy struct {
	EndpointID                     string  `json:"endpoint_id"`
	Enabled                        bool    `json:"enabled"`
	MinimumRequests                int     `json:"minimum_requests"`
	MaxTTFTRegressionPercent       float64 `json:"max_ttft_regression_percent"`
	MaxLatencyRegressionPercent    float64 `json:"max_latency_regression_percent"`
	MaxErrorRateIncrease           float64 `json:"max_error_rate_increase"`
	MaxOutputThroughputDropPercent float64 `json:"max_output_throughput_drop_percent"`
	RequireCompatibilityEvidence   bool    `json:"require_compatibility_evidence"`
}

type EndpointReleaseGuardEvaluation struct {
	ID, TenantID, EndpointID                    string
	ActiveServingPlanID, CandidateServingPlanID string
	Decision, ReasonCodesJSON, MetricsJSON      string
	PolicyJSON                                  string
	CreatedAt                                   time.Time
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

type ColdStartStats struct {
	ClassifiedRequests    int      `json:"classified_requests"`
	ColdStarts            int      `json:"cold_starts"`
	WarmRequests          int      `json:"warm_requests"`
	ColdTTFTP50MS         *float64 `json:"cold_ttft_p50_ms"`
	ColdTTFTP95MS         *float64 `json:"cold_ttft_p95_ms"`
	WarmTTFTP50MS         *float64 `json:"warm_ttft_p50_ms"`
	WarmTTFTP95MS         *float64 `json:"warm_ttft_p95_ms"`
	TimeToReadyP50MS      *float64 `json:"time_to_ready_p50_ms"`
	TimeToReadyP95MS      *float64 `json:"time_to_ready_p95_ms"`
	AvailableBoundaries   []string `json:"available_boundaries"`
	UnavailableBoundaries []string `json:"unavailable_boundaries"`
	BottleneckCode        string   `json:"bottleneck_code,omitempty"`
	Evidence              string   `json:"evidence"`
}

// BenchmarkResult is the persisted, reproducible summary of one AIPerf run.
// Raw prompts and generated content are intentionally not part of this schema.
type BenchmarkResult struct {
	ID, TenantID, DeploymentID, DeploymentName, RevisionID  string
	ModelArtifactID, ModelIdentity, Runtime, RuntimeVersion string
	RuntimeConfigJSON, Provider, Region, GPU, ComputeMode   string
	GPUCount                                                *int
	Tool, ToolVersion, WorkloadJSON, ReproductionCommand    string
	RequestCount, Succeeded, Failed                         int
	DurationSeconds                                         float64
	RequestThroughput, OutputTokenThroughput                *float64
	TTFTP50MS, TTFTP95MS, TPOTP50MS, TPOTP95MS              *float64
	LatencyP50MS, LatencyP95MS, Goodput, GPUUtilization     *float64
	CostMetadataJSON                                        string
	CreatedAt                                               time.Time
}

type SLOPolicy struct {
	DeploymentID          string    `json:"deployment_id"`
	MaxTTFTP95MS          *float64  `json:"max_ttft_p95_ms,omitempty"`
	MaxLatencyP95MS       *float64  `json:"max_latency_p95_ms,omitempty"`
	MaxErrorRate          *float64  `json:"max_error_rate,omitempty"`
	MinOutputTokensSecond *float64  `json:"min_output_tokens_second,omitempty"`
	MaxHourlyCost         *float64  `json:"max_hourly_cost,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type InferenceRecommendation struct {
	ID, TenantID, DeploymentID, Status, AlgorithmVersion    string
	SelectedEvidenceID, Reason, MissingJSON, CandidatesJSON string
	InputSnapshotJSON, InputDigest                          string
	CreatedAt                                               time.Time
}

type CapacityEvidence struct {
	ID, TenantID, Provider, Runtime, ComputeMode, Region, GPU string
	State, Source, EvidenceJSON                               string
	ObservedAt, ExpiresAt, CreatedAt                          time.Time
}

// InferenceRecord contains request metadata and measurements only. Prompt and
// generated content are deliberately excluded.
type InferenceRecord struct {
	RequestID, TenantID, DeploymentID, RevisionID, TargetID string
	LogicalModelID, EnvironmentID, EndpointID               string
	ServingPlanID, BindingID                                string
	Provider, Runtime, ComputeMode, OperationName           string
	RequestModel, ResponseModel, ErrorType                  string
	SemanticConventionSchema                                string
	StartedAt                                               time.Time
	StatusCode                                              int
	LatencyMS                                               float64
	TTFTMS                                                  *float64
	InputTokens, OutputTokens                               *int
	ColdStart                                               *bool
	ProviderWorkersAtArrival                                *int
	ProviderCapacityObservedAt                              *time.Time
	Streaming                                               bool
	RetryCount                                              int
	QueueMS, GenerationMS                                   *float64
	FallbackReason                                          string
}

type AdoptedWorkload struct {
	ID, TenantID, EndpointID, BindingID, TargetID string
	OwnershipMode, Source, ImmutableIdentity      string
	CreatedAt, UpdatedAt                          time.Time
}

type RequestInspection struct {
	InferenceRecord
	LogicalModel, Environment, Endpoint, ServingPlan, Binding string `json:"-"`
	Deployment, Revision, Target                              string `json:"-"`
}

type DiagnosticFinding struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	EndpointID     string    `json:"endpoint_id"`
	Code           string    `json:"code"`
	Severity       string    `json:"severity"`
	Confidence     string    `json:"confidence"`
	Summary        string    `json:"summary"`
	EvidenceJSON   string    `json:"evidence_json"`
	EvidenceDigest string    `json:"evidence_digest"`
	ObservedAt     time.Time `json:"observed_at"`
	CreatedAt      time.Time `json:"created_at"`
}

type AlertPolicy struct {
	ID, TenantID, EndpointID, Name, WebhookURL, SecretReferenceID string
	MinimumSeverity                                               string
	Enabled                                                       bool
	MaxAttempts                                                   int
	CreatedAt, UpdatedAt                                          time.Time
}

type AlertDelivery struct {
	ID, TenantID, PolicyID, FindingID, Status, ErrorCode, BodyDigest string
	Attempts, ResponseStatus                                         int
	CreatedAt, UpdatedAt, DeliveredAt                                time.Time
}

type ReleaseGuardPolicy struct {
	DeploymentID                   string   `json:"deployment_id"`
	Enabled                        bool     `json:"enabled"`
	MinimumRequests                int      `json:"minimum_requests"`
	MaxTTFTRegressionPercent       float64  `json:"max_ttft_regression_percent"`
	MaxLatencyRegressionPercent    float64  `json:"max_latency_regression_percent"`
	MaxErrorRateIncrease           float64  `json:"max_error_rate_increase"`
	MaxOutputThroughputDropPercent float64  `json:"max_output_throughput_drop_percent"`
	RequireCompatibilityEvidence   bool     `json:"require_compatibility_evidence"`
	RequireSyntheticEvidence       bool     `json:"require_synthetic_evidence"`
	MaxCostRegressionPercent       *float64 `json:"max_cost_regression_percent,omitempty"`
	AutoRollbackEnabled            bool     `json:"auto_rollback_enabled"`
	AutoRollbackWindowSeconds      int      `json:"auto_rollback_window_seconds"`
	ValidationMaxRequests          int      `json:"validation_max_requests"`
	ValidationMaxConcurrency       int      `json:"validation_max_concurrency"`
}

type RevisionMetrics struct {
	EvidenceSource        string   `json:"evidence_source,omitempty"`
	EvidenceID            string   `json:"evidence_id,omitempty"`
	Requests              int      `json:"requests"`
	ReadyReplicas         int      `json:"ready_replicas"`
	ErrorRate             float64  `json:"error_rate"`
	P95TTFTMS             *float64 `json:"p95_ttft_ms"`
	P95LatencyMS          *float64 `json:"p95_latency_ms"`
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second"`
	SourcedHourlyCost     *float64 `json:"sourced_hourly_cost,omitempty"`
	Compatible            *bool    `json:"compatible,omitempty"`
	CompatibilityEvidence string   `json:"compatibility_evidence,omitempty"`
	SyntheticValidation   bool     `json:"synthetic_validation"`
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

// InferencePassport is an immutable, signed statement assembled exclusively
// from persisted release evidence. The payload never contains credentials or
// prompt/output content.
type InferencePassport struct {
	ID, TenantID, DeploymentID, RevisionID string
	PayloadJSON, PayloadDigest             string
	Signature, PublicKey, Algorithm, KeyID string
	CreatedAt                              time.Time
}

type ReleaseGuardMonitor struct {
	ID, TenantID, DeploymentID, PromotedRevisionID, RollbackRevisionID string
	Status, EvaluationID, Reason, PolicyJSON                           string
	Deadline, CreatedAt, UpdatedAt                                     time.Time
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
	ID, TenantID, Name, Role, Kind string
	Scopes                         []string
	Disabled                       bool
	CreatedAt                      time.Time
}
type CredentialRecord struct {
	Hash      string
	Principal Principal
}

type SecretReference struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	Resolver  string    `json:"resolver"`
	Reference string    `json:"reference"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ExternalTargetPolicy struct {
	ID                     string    `json:"id"`
	TenantID               string    `json:"tenant_id"`
	DeploymentID           string    `json:"deployment_id"`
	TargetID               string    `json:"target_id"`
	Adapter                string    `json:"adapter"`
	SecretReferenceID      string    `json:"secret_reference_id"`
	Enabled                bool      `json:"enabled"`
	PrivacyAcknowledged    bool      `json:"privacy_acknowledged"`
	RequestLimit           int64     `json:"request_limit"`
	RequestsReserved       int64     `json:"requests_reserved"`
	CostLimitMicrousd      int64     `json:"cost_limit_microusd"`
	MaxRequestCostMicrousd int64     `json:"max_request_cost_microusd"`
	CostReservedMicrousd   int64     `json:"cost_reserved_microusd"`
	OverflowMode           string    `json:"overflow_mode"`
	QueueThreshold         *float64  `json:"queue_threshold,omitempty"`
	BreachIntervals        int       `json:"breach_intervals"`
	RecoveryIntervals      int       `json:"recovery_intervals"`
	CooldownSeconds        int       `json:"cooldown_seconds"`
	SignalMaxAgeSeconds    int       `json:"signal_max_age_seconds"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type ExternalBudgetLease struct {
	PolicyID               string `json:"policy_id"`
	Requests               int64  `json:"requests"`
	ReservedCostMicrousd   int64  `json:"reserved_cost_microusd"`
	MaxRequestCostMicrousd int64  `json:"max_request_cost_microusd"`
}

var RoutingStrategies = map[string]string{
	"round-robin":     "round_robin",
	"consistent-hash": "consistent_hash",
	"power-of-two":    "power_of_two",
	"cache-aware":     "cache_aware",
}
