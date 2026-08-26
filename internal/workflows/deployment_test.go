package workflows

import (
	"context"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeDeploymentStore struct {
	applied      bool
	publishCalls int
	endpoint     string
	event        domain.AuditEvent
}

func (f *fakeDeploymentStore) PublishDeploymentEndpoint(_ context.Context, _, endpointName, _ string) (domain.ResolvedEndpoint, error) {
	f.publishCalls++
	f.endpoint = endpointName
	return domain.ResolvedEndpoint{Endpoint: domain.Endpoint{Name: endpointName}}, nil
}

func (f *fakeDeploymentStore) ApplyDeploymentForTenant(_ context.Context, _ string, d domain.Deployment, targets []string) (domain.Deployment, error) {
	f.applied = true
	d.ID = "deployment-id"
	return d, nil
}
func (f *fakeDeploymentStore) Audit(_ context.Context, event domain.AuditEvent) error {
	f.event = event
	return nil
}
func TestApplyExistingHandlerIsDeterministic(t *testing.T) {
	store := &fakeDeploymentStore{}
	handler := DeploymentHandlers(store)[ApplyExistingKind]
	result, err := handler(context.Background(), domain.Operation{RequestJSON: `{"name":"prod","model":"model","targets":["gpu-a"],"actor":"alice"}`})
	if err != nil || !store.applied || store.publishCalls != 1 || store.endpoint != "prod" || store.event.Actor != "alice" || result != `{"deployment_id":"deployment-id","endpoint_name":"prod","name":"prod"}` {
		t.Fatalf("result=%s applied=%t event=%#v err=%v", result, store.applied, store.event, err)
	}
}
func TestApplyExistingHandlerPublishesExplicitStableEndpoint(t *testing.T) {
	store := &fakeDeploymentStore{}
	handler := DeploymentHandlers(store)[ApplyExistingKind]
	result, err := handler(context.Background(), domain.Operation{RequestJSON: `{"name":"prod-v1","endpoint_name":"support-production","model":"model","targets":["gpu-a"]}`})
	if err != nil || store.endpoint != "support-production" || result != `{"deployment_id":"deployment-id","endpoint_name":"support-production","name":"prod-v1"}` {
		t.Fatalf("result=%s endpoint=%q err=%v", result, store.endpoint, err)
	}
}
func TestApplyExistingHandlerRejectsInvalidPayload(t *testing.T) {
	handler := DeploymentHandlers(&fakeDeploymentStore{})[ApplyExistingKind]
	if _, err := handler(context.Background(), domain.Operation{RequestJSON: `{}`}); err == nil {
		t.Fatal("expected validation error")
	}
}
