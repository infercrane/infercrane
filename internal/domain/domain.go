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
	DesiredState, ObservedState                         string
	MinReplicas, MaxReplicas                            int
	AutoscalingEnabled                                  bool
	CreatedAt, UpdatedAt                                time.Time
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
	RequestsPerSecond float64
	ErrorRate         float64
	P50LatencyMS      *float64
	P95LatencyMS      *float64
}

type RouterGeneration struct {
	ID, DeploymentID, OwnerID, Strategy, WorkerSetHash, InternalEndpoint, Status string
	Generation                                                                   int
	CreatedAt                                                                    time.Time
}

type ProvisionedTarget struct {
	Name, URL, ProviderResourceID, UpstreamModel string
	Details                                      string
}

type Operation struct {
	ID, TenantID, Kind, ResourceType, ResourceName, IdempotencyKey string
	Status, Message, RequestJSON, ResultJSON, ErrorCode            string
	Progress, Attempt, MaxAttempts                                 int
	Retryable, CancelRequested                                     bool
	LeaseOwner                                                     string
	CreatedAt, UpdatedAt                                           time.Time
	CompletedAt, LeaseExpiresAt, NextAttemptAt                     *time.Time
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
