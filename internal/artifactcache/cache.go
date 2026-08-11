// Package artifactcache defines provider-neutral observation and prefetch boundaries.
package artifactcache

import (
	"context"
	"errors"
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

func (r Request) Validate() error {
	if r.ArtifactID == "" || r.ModelIdentity == "" || r.Provider == "" || r.Location == "" {
		return errors.New("artifact identity, model identity, provider, and location are required")
	}
	return nil
}
