package domain

import (
	"encoding/json"
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

// ProviderConnection is reusable, tenant-scoped configuration for an
// authenticated external inference API. It contains only references: secret
// values remain in the configured resolver and endpoint bindings retain their
// own immutable privacy and budget policy.
type ProviderConnection struct {
	ID, TenantID, Name, Adapter, TargetID, TargetName string
	SecretReferenceID, SecretReferenceName            string
	CreatedAt, UpdatedAt                              time.Time
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
	ProviderAdapter    string                   `json:"provider_adapter,omitempty"`
	GPU                string                   `json:"gpu,omitempty"`
	Region             string                   `json:"region,omitempty"`
	Port               int                      `json:"port,omitempty"`
	Workload           runtimecontract.Workload `json:"workload,omitzero"`
}

// ControlPlaneInstance is an ephemeral HA membership observation. PostgreSQL
// remains the source of truth; a missed heartbeat expires membership rather
// than granting ownership of any operation.
type ControlPlaneInstance struct {
	ID            string    `json:"id"`
	BinaryVersion string    `json:"binary_version"`
	ProtocolMin   int       `json:"protocol_min"`
	ProtocolMax   int       `json:"protocol_max"`
	StartedAt     time.Time `json:"started_at"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
	Draining      bool      `json:"draining"`
}

type ModelArtifact struct {
	ID, TenantID, Source, Repository, RequestedRevision string
	ImmutableRevision, ModelIdentity, CacheState        string
	ApproximateSizeBytes                                *int64
	RuntimeCompatibilityJSON                            string
	ResolvedAt                                          time.Time
}

// SandboxReference records composition with an externally owned execution
// sandbox. InferCrane never stores commands, files, prompts, or outputs here.
type SandboxReference struct {
	ID, TenantID, Provider, ExternalID, ExternalRevision string
	EndpointName, PrincipalID, Status, MetadataJSON      string
	ExpiresAt, CreatedAt, UpdatedAt                      time.Time
}

// TrainingArtifactHandoff binds signed, content-free external training
// provenance to the immutable model artifact of one deployment revision.
type TrainingArtifactHandoff struct {
	ID, TenantID, DeploymentID, RevisionID, ModelArtifactID string
	Provider, ExternalRunID, Repository, ImmutableRevision  string
	ArtifactDigest, BaseModelIdentity, Method               string
	Framework, FrameworkVersion, DatasetFingerprint         string
	PayloadDigest, Signature, PublicKey, Algorithm, KeyID   string
	CreatedAt                                               time.Time
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

// EndpointMonitoringSnapshot is the bounded, tenant-scoped read model used by
// operator surfaces. It deliberately contains normalized aggregates and
// persisted lifecycle evidence rather than raw runtime/provider metrics.
type EndpointMonitoringSnapshot struct {
	Endpoint      string                `json:"endpoint"`
	LogicalModel  string                `json:"logical_model"`
	Environment   string                `json:"environment"`
	WindowStart   time.Time             `json:"window_start"`
	WindowEnd     time.Time             `json:"window_end"`
	BucketSeconds int                   `json:"bucket_seconds"`
	Summary       MonitoringSummary     `json:"summary"`
	Series        []MonitoringBucket    `json:"series"`
	Breakdowns    []MonitoringBreakdown `json:"breakdowns"`
	Events        []MonitoringEvent     `json:"events"`
	Evidence      MonitoringEvidence    `json:"evidence"`
}

type MonitoringSummary struct {
	Requests              int      `json:"requests"`
	Errors                int      `json:"errors"`
	Fallbacks             int      `json:"fallbacks"`
	Retried               int      `json:"retried"`
	Streaming             int      `json:"streaming"`
	TokenUsageSamples     int      `json:"token_usage_samples"`
	InputTokenSamples     int      `json:"input_token_samples"`
	OutputTokenSamples    int      `json:"output_token_samples"`
	InputTokens           int64    `json:"input_tokens"`
	OutputTokens          int64    `json:"output_tokens"`
	RequestsPerSecond     float64  `json:"requests_per_second"`
	InputTokensPerSecond  *float64 `json:"input_tokens_per_second"`
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second"`
	ErrorRate             *float64 `json:"error_rate"`
	FallbackRate          *float64 `json:"fallback_rate"`
	RetryRate             *float64 `json:"retry_rate"`
	P50LatencyMS          *float64 `json:"p50_latency_ms"`
	P95LatencyMS          *float64 `json:"p95_latency_ms"`
	P50TTFTMS             *float64 `json:"p50_ttft_ms"`
	P95TTFTMS             *float64 `json:"p95_ttft_ms"`
	P95QueueMS            *float64 `json:"p95_queue_ms"`
	P95GenerationMS       *float64 `json:"p95_generation_ms"`
}

type MonitoringBucket struct {
	StartedAt             time.Time `json:"started_at"`
	Requests              int       `json:"requests"`
	Errors                int       `json:"errors"`
	Fallbacks             int       `json:"fallbacks"`
	Retried               int       `json:"retried"`
	Streaming             int       `json:"streaming"`
	TokenUsageSamples     int       `json:"token_usage_samples"`
	InputTokenSamples     int       `json:"input_token_samples"`
	OutputTokenSamples    int       `json:"output_token_samples"`
	InputTokens           int64     `json:"input_tokens"`
	OutputTokens          int64     `json:"output_tokens"`
	RequestsPerSecond     float64   `json:"requests_per_second"`
	InputTokensPerSecond  *float64  `json:"input_tokens_per_second"`
	OutputTokensPerSecond *float64  `json:"output_tokens_per_second"`
	ErrorRate             *float64  `json:"error_rate"`
	FallbackRate          *float64  `json:"fallback_rate"`
	P50LatencyMS          *float64  `json:"p50_latency_ms"`
	P95LatencyMS          *float64  `json:"p95_latency_ms"`
	P50TTFTMS             *float64  `json:"p50_ttft_ms"`
	P95TTFTMS             *float64  `json:"p95_ttft_ms"`
	P95QueueMS            *float64  `json:"p95_queue_ms"`
	P95GenerationMS       *float64  `json:"p95_generation_ms"`
}

type MonitoringBreakdown struct {
	Binding      string    `json:"binding"`
	Deployment   string    `json:"deployment"`
	Revision     string    `json:"revision"`
	Provider     string    `json:"provider"`
	Runtime      string    `json:"runtime"`
	Requests     int       `json:"requests"`
	Errors       int       `json:"errors"`
	Fallbacks    int       `json:"fallbacks"`
	ErrorRate    *float64  `json:"error_rate"`
	P95LatencyMS *float64  `json:"p95_latency_ms"`
	P95TTFTMS    *float64  `json:"p95_ttft_ms"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

type MonitoringEvent struct {
	Kind        string          `json:"kind"`
	Type        string          `json:"type"`
	Summary     string          `json:"summary"`
	DetailsJSON string          `json:"-"`
	Details     json.RawMessage `json:"details"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

type MonitoringEvidence struct {
	Source                   string                `json:"source"`
	SemanticConventionSchema string                `json:"semantic_convention_schema"`
	SampleCount              int                   `json:"sample_count"`
	LatestRequestAt          *time.Time            `json:"latest_request_at"`
	Fresh                    bool                  `json:"fresh"`
	ContentRecorded          bool                  `json:"content_recorded"`
	Available                []string              `json:"available"`
	Unavailable              []string              `json:"unavailable"`
	Measurements             []MeasurementEvidence `json:"measurements"`
}

// MeasurementEvidence is a content-free, normalized measurement exposed to
// operators. Missing, stale, and unsupported evidence never carries a value.
type MeasurementEvidence struct {
	Name          string     `json:"name"`
	Value         *float64   `json:"value"`
	Unit          string     `json:"unit"`
	Availability  string     `json:"availability"`
	EvidenceClass string     `json:"evidence_class"`
	Source        string     `json:"source"`
	ObservedAt    *time.Time `json:"observed_at"`
	FreshUntil    *time.Time `json:"fresh_until"`
	SampleCount   int        `json:"sample_count"`
	Reason        string     `json:"reason,omitempty"`
}

// OperationalMeasurement is a content-free observation imported from a
// qualified runtime or infrastructure collector. ValidUntil is mandatory so a
// disconnected collector can never leave apparently-live evidence behind.
type OperationalMeasurement struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"-"`
	DeploymentID  string    `json:"deployment_id"`
	Deployment    string    `json:"deployment"`
	RevisionID    string    `json:"revision_id"`
	ReplicaID     string    `json:"replica_id,omitempty"`
	Name          string    `json:"name"`
	Value         float64   `json:"value"`
	Unit          string    `json:"unit"`
	EvidenceClass string    `json:"evidence_class"`
	Source        string    `json:"source"`
	SampleCount   int       `json:"sample_count"`
	ObservedAt    time.Time `json:"observed_at"`
	ValidUntil    time.Time `json:"valid_until"`
	CreatedAt     time.Time `json:"created_at"`
}

// CostEvidence is an immutable, revision-bound cost observation imported from
// a qualified external cost source. Amount is the normalized rate denominated
// by BillingUnit and retains its source window; InferCrane never assumes or
// converts currency.
type CostEvidence struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"-"`
	DeploymentID  string    `json:"deployment_id"`
	Deployment    string    `json:"deployment"`
	RevisionID    string    `json:"revision_id"`
	Source        string    `json:"source"`
	Scope         string    `json:"scope"`
	Resource      string    `json:"resource"`
	Currency      string    `json:"currency"`
	BillingUnit   string    `json:"billing_unit"`
	EvidenceClass string    `json:"evidence_class"`
	Amount        float64   `json:"amount"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	ObservedAt    time.Time `json:"observed_at"`
	ValidUntil    time.Time `json:"valid_until"`
	CreatedAt     time.Time `json:"created_at"`
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

// ModelRecipe is an immutable, content-addressed serving configuration captured
// from a real revision and benchmark. Payload and provenance are bounded JSON;
// the digest covers both canonical objects.
type ModelRecipe struct {
	ID, TenantID, Name, Version, Digest string
	PayloadJSON, ProvenanceJSON         string
	CreatedAt                           time.Time
}

// LabEvaluation is an immutable comparison of persisted evidence. Results
// label every row measured, modeled, or heuristic; v1.7 emits measured rows
// only and never synthesizes missing performance or cost.
type LabEvaluation struct {
	ID, TenantID, ModelIdentity, AlgorithmVersion string
	InputJSON, ResultsJSON, InputDigest           string
	CreatedAt                                     time.Time
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
	SessionIDHash, ParentSessionIDHash, SharedPrefixHash    string
	ToolPauseMS                                             *float64
}

type ReplayTrace struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"-"`
	DeploymentID   string    `json:"deployment_id"`
	DeploymentName string    `json:"deployment_name"`
	RevisionID     string    `json:"revision_id,omitempty"`
	SchemaVersion  string    `json:"schema_version"`
	ShapeJSON      string    `json:"shape"`
	SummaryJSON    string    `json:"summary"`
	ShapeDigest    string    `json:"shape_digest"`
	RequestCount   int       `json:"request_count"`
	WindowStart    time.Time `json:"window_start"`
	WindowEnd      time.Time `json:"window_end"`
	CreatedAt      time.Time `json:"created_at"`
}

type ArtifactCacheObservation struct {
	ID, TenantID, ModelArtifactID, Provider, Region, Location string
	State, Source, EvidenceJSON                               string
	ObservedAt, ExpiresAt, CreatedAt                          time.Time
}

type ArtifactPrefetch struct {
	ID, TenantID, ModelArtifactID, Provider, Region, Location string
	Status, IdempotencyKey, ProviderOperationID, ErrorCode    string
	CreatedAt, UpdatedAt                                      time.Time
}

type CapacityOperation struct {
	ID, TenantID, Provider, Runtime, ComputeMode, Region, GPU string
	Operation, ResourceKey, Outcome, ErrorCode                string
	StartedAt, CompletedAt, CreatedAt                         time.Time
	DurationSeconds                                           float64
}

type CapacitySummary struct {
	Provider           string    `json:"provider"`
	Runtime            string    `json:"runtime"`
	ComputeMode        string    `json:"compute_mode"`
	Region             string    `json:"region"`
	GPU                string    `json:"gpu"`
	Attempts           int       `json:"attempts"`
	Succeeded          int       `json:"succeeded"`
	CapacityFailures   int       `json:"capacity_failures"`
	RuntimeFailures    int       `json:"runtime_failures"`
	SuccessRate        float64   `json:"success_rate"`
	DurationP50Seconds *float64  `json:"duration_p50_seconds,omitempty"`
	DurationP95Seconds *float64  `json:"duration_p95_seconds,omitempty"`
	WindowStart        time.Time `json:"window_start"`
	WindowEnd          time.Time `json:"window_end"`
}

type FinOpsReport struct {
	ID, TenantID, DeploymentID, DeploymentName               string
	WindowStart, WindowEnd                                   time.Time
	Currency, Status, SummaryJSON, EvidenceJSON, InputDigest string
	KnownCost, EstimatedAvoidableCost                        *float64
	CreatedAt                                                time.Time
}

type AutopilotPlan struct {
	ID, TenantID, DeploymentID, DeploymentName, RecommendationID string
	Status, Objective, CandidateJSON, EvidenceJSON, InputDigest  string
	ApprovedBy                                                   string
	ApprovedAt                                                   *time.Time
	CreatedAt, UpdatedAt                                         time.Time
}

type ContextPassport struct {
	ID, TenantID, EndpointID, DeploymentID, DeploymentName, Status      string
	PreferredBindingID, PreferredTargetID, CacheHintsJSON, MetadataJSON string
	LastActivity, ExpiresAt, CreatedAt, UpdatedAt                       time.Time
}

type BurstGuardPolicy struct {
	ID, TenantID, DeploymentID, ExternalPolicyID                                             string
	Enabled                                                                                  bool
	QueueThreshold, BreachIntervals, RecoveryIntervals, CooldownSeconds, SignalMaxAgeSeconds int
	MaxIncrementalCostMicrousdHour                                                           int64
	CreatedAt, UpdatedAt                                                                     time.Time
}

type BurstGuardDecision struct {
	ID, TenantID, DeploymentID, PolicyID, Decision, Reason, EvidenceJSON string
	IncrementalCostMicrousdHour                                          int64
	CreatedAt                                                            time.Time
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

type AdmissionPolicy struct {
	EndpointID, TenantID                          string
	MaxConcurrency, MaxQueueDepth, QueueTimeoutMS int
	MaxRequestBytes, MaxOutputTokens, RetryBudget int
	AllowedPrioritiesJSON                         string
	Enabled                                       bool
	CreatedAt, UpdatedAt                          time.Time
}

type AsyncInferenceJob struct {
	ID, TenantID, EndpointID, RequestID, Protocol, Status string
	IdempotencyKey, EncryptionKeyReference                string
	PayloadDigest                                         string
	WebhookURL, WebhookSecretReferenceID                  string
	WebhookStatus, WebhookErrorCode                       string
	LeaseOwner, LeaseToken, ErrorCode, ErrorMessage       string
	PayloadCiphertext, PayloadNonce, ResultCiphertext     []byte
	ResultNonce                                           []byte
	Priority, Attempt, WebhookAttempts                    int
	ExecutionDeadline, ExpiresAt, CreatedAt, UpdatedAt    time.Time
	StartedAt, CompletedAt, LeaseExpiresAt                *time.Time
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
	RequireQualityEvidence         bool     `json:"require_quality_evidence"`
	MinimumQualityScore            *float64 `json:"minimum_quality_score,omitempty"`
	MaxQualityRegressionPercent    *float64 `json:"max_quality_regression_percent,omitempty"`
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
	QualityScore          *float64 `json:"quality_score,omitempty"`
	QualityPassed         *bool    `json:"quality_passed,omitempty"`
	QualityComparable     *bool    `json:"quality_comparable,omitempty"`
	QualityEvidenceID     string   `json:"quality_evidence_id,omitempty"`
	QualitySuite          string   `json:"quality_suite,omitempty"`
}

// QualityEvidence is signed, revision-bound output from a customer-selected
// semantic evaluator. It contains aggregate evidence only, never prompt or
// response bodies.
type QualityEvidence struct {
	ID, TenantID, DeploymentID, RevisionID                     string
	Suite, SuiteVersion, Evaluator, EvaluatorVersion           string
	ArtifactDigest, PayloadDigest, Signature, PublicKey, KeyID string
	Algorithm                                                  string
	Score                                                      float64
	Passed                                                     bool
	SampleCount                                                int
	EvaluatedAt, CreatedAt                                     time.Time
}

type EnvironmentPromotion struct {
	ID, TenantID, SourceEndpointID, SourcePlanID             string
	DestinationEndpointID, DestinationPlanID, IdempotencyKey string
	CreatedAt                                                time.Time
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
	EndpointNames                  []string
	Disabled                       bool
	CreatedAt                      time.Time
	ExpiresAt                      *time.Time
}
type CredentialRecord struct {
	Hash      string
	Principal Principal
}

type ExternalIdentity struct {
	Provider               string `json:"provider"`
	ExternalUserID         string `json:"external_user_id"`
	ExternalOrganizationID string `json:"external_organization_id"`
}

type ConsoleIdentityProvisioning struct {
	ExternalIdentity
	DisplayName string   `json:"display_name"`
	Role        string   `json:"role"`
	Scopes      []string `json:"scopes,omitempty"`
	Access      bool     `json:"access"`
}

type ConsoleIdentity struct {
	UserID      string   `json:"user_id"`
	TenantID    string   `json:"tenant_id"`
	DisplayName string   `json:"display_name"`
	Role        string   `json:"role"`
	Scopes      []string `json:"scopes"`
	Access      bool     `json:"access"`
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

// ManagedExternalBindingConfig is the immutable, secret-free policy attached
// to a stable endpoint binding backed by an authenticated external model API.
// Secret values remain behind SecretReferenceID and are resolved only by the
// control plane. Hard budgets are durably reserved before the gateway can send
// a request.
type ManagedExternalBindingConfig struct {
	Adapter                string `json:"adapter"`
	SecretReferenceID      string `json:"secret_reference_id"`
	Enabled                bool   `json:"enabled"`
	PrivacyAcknowledged    bool   `json:"privacy_acknowledged"`
	RequestLimit           int64  `json:"request_limit"`
	CostLimitMicrousd      int64  `json:"cost_limit_microusd"`
	MaxRequestCostMicrousd int64  `json:"max_request_cost_microusd"`
}

type ManagedExternalBindingPolicy struct {
	ID, TenantID, BindingID, TargetID         string
	Adapter, SecretReferenceID                string
	Enabled, PrivacyAcknowledged              bool
	RequestLimit, RequestsReserved            int64
	CostLimitMicrousd, MaxRequestCostMicrousd int64
	CostReservedMicrousd                      int64
	CreatedAt, UpdatedAt                      time.Time
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
