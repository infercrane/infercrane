// Package artifactcache defines provider-neutral observation and prefetch boundaries.
package artifactcache

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Request struct{ ArtifactID, ModelIdentity, Provider, Region, Location, IdempotencyKey string }
type Observation struct {
	State, Source, EvidenceJSON string
	ObservedAt, ExpiresAt       time.Time
}
type Operation struct{ ProviderOperationID, Status, ErrorCode string }
type Adapter interface {
	Observe(context.Context, Request) (Observation, error)
	Prefetch(context.Context, Request) (Operation, error)
}

// Failure distinguishes a definite rejection before any provider mutation
// from an ambiguous response that must be retried with the same identity.
// Untyped adapter errors remain outcome-unknown for backward-safe adoption.
type Failure struct {
	Code           string
	OutcomeUnknown bool
	Err            error
}

func (f Failure) Error() string {
	if f.Err != nil {
		return f.Err.Error()
	}
	return f.Code
}

func (f Failure) Unwrap() error { return f.Err }

func Definitive(code string, err error) error {
	if code == "" || err == nil {
		return errors.New("definitive artifact-cache failure requires code and cause")
	}
	return Failure{Code: code, Err: err}
}

func Classify(err error) (code string, outcomeUnknown bool) {
	if err == nil {
		return "", false
	}
	var failure Failure
	if errors.As(err, &failure) {
		if failure.Code == "" {
			return "artifact_cache_failed", failure.OutcomeUnknown
		}
		return failure.Code, failure.OutcomeUnknown
	}
	return "provider_result_unknown", true
}

func (r Request) Validate() error {
	if r.ArtifactID == "" || r.ModelIdentity == "" || r.Provider == "" || r.Location == "" || r.IdempotencyKey == "" {
		return errors.New("artifact identity, model identity, provider, location, and idempotency key are required")
	}
	for name, value := range map[string]string{"artifact": r.ArtifactID, "model": r.ModelIdentity, "provider": r.Provider, "region": r.Region, "location": r.Location, "idempotency_key": r.IdempotencyKey} {
		if len(value) > 1024 {
			return fmt.Errorf("artifact cache %s exceeds 1024 bytes", name)
		}
	}
	return nil
}
