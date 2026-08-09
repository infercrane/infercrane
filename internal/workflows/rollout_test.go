package workflows

import (
	"context"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeRolloutStore struct {
	created, promoted, rejected, rolledBack int
	audit                                   domain.AuditEvent
}

func (f *fakeRolloutStore) ReleaseGuardAccepted(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func (f *fakeRolloutStore) EnsureCandidateRevision(context.Context, string, string, string, string) (domain.DeploymentRevision, error) {
	f.created++
	return domain.DeploymentRevision{ID: "rev-2", Number: 2}, nil
}
func (f *fakeRolloutStore) PromoteCandidateRevision(context.Context, string, string, string) error {
	f.promoted++
	return nil
}
func (f *fakeRolloutStore) RejectCandidateRevision(context.Context, string, string, string, string) error {
	f.rejected++
	return nil
}
func (f *fakeRolloutStore) RollbackRevision(context.Context, string, string, string, string) error {
	f.rolledBack++
	return nil
}
func (f *fakeRolloutStore) Audit(_ context.Context, event domain.AuditEvent) error {
	f.audit = event
	return nil
}

func TestRolloutHandlersPersistExplicitTransitions(t *testing.T) {
	store := &fakeRolloutStore{}
	handlers := RolloutHandlers(store)
	createResult, err := handlers[RolloutCreateKind](context.Background(), domain.Operation{ID: "operation-1", RequestJSON: `{"name":"prod","tenant_id":"global","actor":"alice","spec":{"model":"Qwen/Qwen3-8B"}}`})
	if err != nil || store.created != 1 || createResult != `{"candidate_id":"rev-2","deployment":"prod","revision_number":2}` || store.audit.Action != "rollout.create" {
		t.Fatalf("create result=%s store=%+v err=%v", createResult, store, err)
	}
	if _, err = handlers[RolloutPromoteKind](context.Background(), domain.Operation{RequestJSON: `{"name":"prod","tenant_id":"global","candidate_id":"rev-2"}`}); err != nil || store.promoted != 1 {
		t.Fatalf("promote count=%d err=%v", store.promoted, err)
	}
	if _, err = handlers[RolloutRejectKind](context.Background(), domain.Operation{RequestJSON: `{"name":"prod","tenant_id":"global","candidate_id":"rev-3","reason":"readiness failed"}`}); err != nil || store.rejected != 1 {
		t.Fatalf("reject count=%d err=%v", store.rejected, err)
	}
	if _, err = handlers[RolloutRollbackKind](context.Background(), domain.Operation{RequestJSON: `{"name":"prod","tenant_id":"global","revision_id":"rev-1","reason":"operator rollback"}`}); err != nil || store.rolledBack != 1 {
		t.Fatalf("rollback count=%d err=%v", store.rolledBack, err)
	}
}
