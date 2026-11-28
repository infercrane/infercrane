package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/workflows"
)

// cleanupStore holds one deployment with, optionally, one unfinished
// operation, and answers a delete the way the store's cancel rule does.
type cleanupStore struct {
	mu     sync.Mutex
	open   *domain.Operation
	leased bool
	submit []domain.Operation
	kinds  [][]string
	audits []domain.AuditEvent
}

func (s *cleanupStore) ResolveForTenant(_ context.Context, tenant, name string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep-" + name, TenantID: tenant, Name: name}}, nil
}

func (s *cleanupStore) ActiveOperationForResource(context.Context, string, string, string) (domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		return domain.Operation{}, domain.ErrNotFound
	}
	return *s.open, nil
}

func (s *cleanupStore) SubmitDeploymentDeleteCancelling(_ context.Context, _, _, _ string, operation domain.Operation, cancelKinds []string) (store.DeploymentDeletion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submit, s.kinds = append(s.submit, operation), append(s.kinds, cancelKinds)
	deletion := store.DeploymentDeletion{Operation: operation, Created: true}
	if s.open != nil {
		if s.leased || !slices.Contains(cancelKinds, s.open.Kind) {
			return store.DeploymentDeletion{}, &store.OpenOperationError{ID: s.open.ID, Kind: s.open.Kind, Status: s.open.Status, Running: s.leased}
		}
		cancelled := *s.open
		cancelled.Status = "cancelled"
		deletion.Cancelled, s.open = &cancelled, nil
	}
	return deletion, nil
}

func (s *cleanupStore) Audit(_ context.Context, event domain.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, event)
	return nil
}

func runCleanup(t *testing.T, s *cleanupStore) string {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))
	queueManagedDeploymentCleanup(context.Background(), s, []domain.ManagedSpendReservation{{ID: "res-1", TenantID: "tenant", ResourceName: "qwen"}}, "managed-expiry:", logger)
	return logs.String()
}

// A managed deployment whose deploy keeps waiting between attempts is
// cleaned up at expiry: the delete cancels the deploy, audited as operation
// cancel records it, and queues the cleanup.
func TestManagedExpiryCancelsADeployThatNeverFinished(t *testing.T) {
	for _, test := range []struct{ deploy, delete string }{
		{workflows.ConvergeKind, workflows.DeleteKind},
		{workflows.ServerlessConvergeKind, workflows.ServerlessDeleteKind},
	} {
		s := &cleanupStore{open: &domain.Operation{ID: "deploy-1", Kind: test.deploy, Status: "waiting"}}
		if logs := runCleanup(t, s); logs != "" {
			t.Fatalf("%s: logged %q", test.deploy, logs)
		}
		var request workflows.DeleteRequest
		if len(s.submit) != 1 || s.submit[0].Kind != test.delete || s.submit[0].IdempotencyKey != "managed-expiry:res-1" || json.Unmarshal([]byte(s.submit[0].RequestJSON), &request) != nil || request.Actor != managedCleanupActor || request.DeploymentID != "dep-qwen" {
			t.Fatalf("%s: submitted %+v", test.deploy, s.submit)
		}
		if !slices.Equal(s.kinds[0], workflows.DeleteCancelsKinds()) {
			t.Fatalf("%s: delete may cancel %v", test.deploy, s.kinds[0])
		}
		if len(s.audits) != 1 || s.audits[0].Action != "operation.cancel" || s.audits[0].ResourceName != "deploy-1" || s.audits[0].Actor != managedCleanupActor {
			t.Fatalf("%s: audits %+v", test.deploy, s.audits)
		}
	}
}

// A deploy a worker holds, or another kind of unfinished operation, refuses
// the delete and changes nothing; the next pass tries again.
func TestManagedExpiryLeavesADeployAWorkerHolds(t *testing.T) {
	for _, s := range []*cleanupStore{
		{open: &domain.Operation{ID: "deploy-1", Kind: workflows.ConvergeKind, Status: "running"}, leased: true},
		{open: &domain.Operation{ID: "scale-1", Kind: workflows.ScaleKind, Status: "waiting"}},
	} {
		if logs := runCleanup(t, s); logs != "" {
			t.Fatalf("%s: a refused delete logged %q", s.open.ID, logs)
		}
		if len(s.audits) != 0 || s.open == nil {
			t.Fatalf("%s: a refused delete changed something: audits %+v", s.open.ID, s.audits)
		}
	}
	s := &cleanupStore{}
	if logs := runCleanup(t, s); logs != "" || len(s.submit) != 1 || len(s.audits) != 0 {
		t.Fatalf("no open operation: logs %q, submitted %+v, audits %+v", logs, s.submit, s.audits)
	}
}
