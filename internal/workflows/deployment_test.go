package workflows

import (
	"context"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeDeploymentStore struct {
	applied bool
	event   domain.AuditEvent
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
	if err != nil || !store.applied || store.event.Actor != "alice" || result != `{"deployment_id":"deployment-id","name":"prod"}` {
		t.Fatalf("result=%s applied=%t event=%#v err=%v", result, store.applied, store.event, err)
	}
}
func TestApplyExistingHandlerRejectsInvalidPayload(t *testing.T) {
	handler := DeploymentHandlers(&fakeDeploymentStore{})[ApplyExistingKind]
	if _, err := handler(context.Background(), domain.Operation{RequestJSON: `{}`}); err == nil {
		t.Fatal("expected validation error")
	}
}
