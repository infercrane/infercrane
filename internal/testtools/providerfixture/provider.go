// Package providerfixture supplies deterministic, fault-injectable provider
// infrastructure for contract tests. It is never a production backend.
package providerfixture

import (
	"context"
	"errors"
	"sync"

	"github.com/infercrane/infercrane/internal/provision"
)

type Elastic struct {
	mu                   sync.Mutex
	resources            map[string]provision.Observation
	FailAfterCreateOnce  bool
	FailDeleteOnce       bool
	BlockEnsureUntilDone bool
	EnsureFailure        error
	EnsureCalls          int
	DeleteCalls          int
	CreatedResourceCount int
}

type Serverless struct {
	mu                   sync.Mutex
	endpoints            map[string]provision.ServerlessEndpoint
	FailAfterCreateOnce  bool
	EnsureCalls          int
	DeleteCalls          int
	CreatedEndpointCount int
}

func NewElastic() *Elastic {
	return &Elastic{resources: map[string]provision.Observation{}}
}

func NewServerless() *Serverless {
	return &Serverless{endpoints: map[string]provision.ServerlessEndpoint{}}
}

func (s *Serverless) EnsureEndpoint(_ context.Context, spec provision.ServerlessEndpointSpec) (provision.ServerlessEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.EnsureCalls++
	name := provision.ServerlessEndpointName(spec.ExternalKey)
	endpoint, exists := s.endpoints[name]
	if !exists {
		s.CreatedEndpointCount++
		endpoint = provision.ServerlessEndpoint{ID: "fixture-endpoint-" + spec.ExternalKey, Name: name, WorkersMin: 0, WorkersMax: spec.WorkersMax}
		s.endpoints[name] = endpoint
	}
	if s.FailAfterCreateOnce {
		s.FailAfterCreateOnce = false
		return provision.ServerlessEndpoint{}, errors.New("injected lost endpoint create response")
	}
	return endpoint, nil
}

func (s *Serverless) ListEndpoints(context.Context) ([]provision.ServerlessEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]provision.ServerlessEndpoint, 0, len(s.endpoints))
	for _, endpoint := range s.endpoints {
		out = append(out, endpoint)
	}
	return out, nil
}

func (s *Serverless) DeleteEndpoint(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, endpoint := range s.endpoints {
		if endpoint.ID == id {
			s.DeleteCalls++
			delete(s.endpoints, name)
			break
		}
	}
	return nil
}

func (s *Serverless) EndpointURL(id string) string {
	return "https://fixture.invalid/" + id + "/openai"
}

func (e *Elastic) Handle(externalKey string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: externalKey, ResourceID: "fixture-" + externalKey}
}

func (e *Elastic) EnsureReplica(ctx context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	if e.BlockEnsureUntilDone {
		<-ctx.Done()
		return provision.ProviderHandle{}, ctx.Err()
	}
	if e.EnsureFailure != nil {
		return provision.ProviderHandle{}, e.EnsureFailure
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.EnsureCalls++
	handle := e.Handle(spec.ExternalKey)
	if _, exists := e.resources[handle.ResourceID]; !exists {
		e.CreatedResourceCount++
		e.resources[handle.ResourceID] = provision.Observation{Exists: true, State: "ready", Endpoint: "http://fixture.invalid:8000"}
	}
	if e.FailAfterCreateOnce {
		e.FailAfterCreateOnce = false
		return provision.ProviderHandle{}, errors.New("injected lost create response")
	}
	return handle, nil
}

func (e *Elastic) ObserveReplica(_ context.Context, handle provision.ProviderHandle, _ int) (provision.Observation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	observation, exists := e.resources[handle.ResourceID]
	if !exists {
		return provision.Observation{}, nil
	}
	return observation, nil
}

func (e *Elastic) DeleteReplica(_ context.Context, handle provision.ProviderHandle) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.FailDeleteOnce {
		e.FailDeleteOnce = false
		return errors.New("injected partial delete failure")
	}
	if _, exists := e.resources[handle.ResourceID]; exists {
		e.DeleteCalls++
		delete(e.resources, handle.ResourceID)
	}
	return nil
}
