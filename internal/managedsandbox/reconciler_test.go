package managedsandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/sandboxprovider"
)

type memoryRepository struct {
	reservation domain.ManagedSpendReservation
	row         domain.NativeSandbox
	cleanup     []domain.NativeSandbox
	events      []domain.SandboxUsageEvent
	released    bool
	settledMS   int64
}

func (m *memoryRepository) ManagedSandboxesForReconciliation(context.Context, int) ([]domain.ManagedSpendReservation, error) {
	if m.reservation.ID == "" {
		return nil, nil
	}
	return []domain.ManagedSpendReservation{m.reservation}, nil
}
func (m *memoryRepository) CleanupPendingSandboxesForReconciliation(context.Context, int) ([]domain.NativeSandbox, error) {
	return m.cleanup, nil
}
func (m *memoryRepository) NativeSandbox(context.Context, string, string) (domain.NativeSandbox, error) {
	return m.row, nil
}
func (m *memoryRepository) RecordNativeSandboxTransition(_ context.Context, _, _, status, failure, operationID, eventType string, occurredAt time.Time) (domain.NativeSandbox, error) {
	if m.row.Status != status {
		elapsed := occurredAt.Sub(m.row.BillingStateSince).Milliseconds()
		if elapsed < 0 {
			elapsed = 0
		}
		event := domain.SandboxUsageEvent{EventID: operationID + eventType, EventType: eventType, OccurredAt: occurredAt}
		if m.row.Status == "running" || m.row.Status == "pausing" || m.row.Status == "resuming" {
			event.RunningMilliseconds = elapsed
		}
		if m.row.Status == "standby" {
			event.StandbyMilliseconds = elapsed
		}
		m.events = append(m.events, event)
		m.row.BillingStateSince = occurredAt
	}
	m.row.Status, m.row.FailureCode = status, failure
	return m.row, nil
}
func (m *memoryRepository) SandboxRunningMilliseconds(context.Context, string, string) (int64, error) {
	var total int64
	for _, event := range m.events {
		total += event.RunningMilliseconds
	}
	return total, nil
}
func (m *memoryRepository) ReleaseManagedSandboxSpend(context.Context, string, string, string) error {
	m.released = true
	return nil
}
func (m *memoryRepository) SettleManagedSandboxSpend(_ context.Context, _, _ string, milliseconds int64, _ string) error {
	m.settledMS = milliseconds
	return nil
}

type memoryProvider struct {
	sandbox            sandboxprovider.Sandbox
	deletedSandbox     bool
	deletedWorkspace   bool
	deleteSandboxErr   error
	deleteWorkspaceErr error
}

func (m *memoryProvider) Capabilities(context.Context, string) (sandboxprovider.Capabilities, error) {
	return sandboxprovider.Capabilities{}, nil
}
func (m *memoryProvider) List(context.Context, string, bool) ([]sandboxprovider.Sandbox, error) {
	return nil, nil
}
func (m *memoryProvider) CreateWorkspace(context.Context, string, string, string) (sandboxprovider.WorkspaceMutation, error) {
	return sandboxprovider.WorkspaceMutation{}, nil
}
func (m *memoryProvider) DeleteWorkspace(context.Context, string, string, string) (sandboxprovider.WorkspaceMutation, error) {
	m.deletedWorkspace = true
	return sandboxprovider.WorkspaceMutation{}, m.deleteWorkspaceErr
}
func (m *memoryProvider) Get(context.Context, string, string) (sandboxprovider.Sandbox, error) {
	return m.sandbox, nil
}
func (m *memoryProvider) Create(context.Context, string, string, sandboxprovider.CreateRequest) (sandboxprovider.Mutation, error) {
	return sandboxprovider.Mutation{}, nil
}
func (m *memoryProvider) Pause(context.Context, string, string, string) (sandboxprovider.Mutation, error) {
	return sandboxprovider.Mutation{}, nil
}
func (m *memoryProvider) Resume(context.Context, string, string, string) (sandboxprovider.Mutation, error) {
	return sandboxprovider.Mutation{}, nil
}
func (m *memoryProvider) Delete(context.Context, string, string, string) (sandboxprovider.Mutation, error) {
	m.deletedSandbox = true
	return sandboxprovider.Mutation{}, m.deleteSandboxErr
}
func (m *memoryProvider) RunCommand(context.Context, string, string, sandboxprovider.CommandRequest, func(sandboxprovider.CommandEvent) error) (string, error) {
	return "", nil
}
func (m *memoryProvider) WriteFile(context.Context, string, string, string, io.Reader, int64) (sandboxprovider.FileInfo, error) {
	return sandboxprovider.FileInfo{}, nil
}
func (m *memoryProvider) ReadFile(context.Context, string, string, string) (sandboxprovider.FileDownload, error) {
	return sandboxprovider.FileDownload{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
func (m *memoryProvider) CreatePortLease(context.Context, string, string, uint16, int64) (sandboxprovider.PortLease, error) {
	return sandboxprovider.PortLease{}, nil
}
func (m *memoryProvider) ProxyPreview(context.Context, string, string, sandboxprovider.PreviewRequest) (sandboxprovider.PreviewResponse, error) {
	return sandboxprovider.PreviewResponse{}, nil
}
func (m *memoryProvider) Events(context.Context, string, string) ([]sandboxprovider.Event, error) {
	return nil, nil
}
func (m *memoryProvider) Receipt(context.Context, string, string) (sandboxprovider.ReceiptEvidence, error) {
	return sandboxprovider.ReceiptEvidence{}, nil
}

func TestReconcilerExpiresComputeAndSettlesOnlyRunningTime(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	activated := now.Add(-time.Hour)
	repository := &memoryRepository{
		reservation: domain.ManagedSpendReservation{ID: "reservation-1", TenantID: "tenant", ResourceName: "sandbox-1", ActivatedAt: &activated, ExpiresAt: now.Add(-30 * time.Minute)},
		row:         domain.NativeSandbox{ID: "sandbox-1", TenantID: "tenant", TemplateID: "base", BrezelSandboxID: "provider-1", BrezelWorkspaceID: "workspace-1", Status: "running", BillingStateSince: activated, LastActiveAt: now.Add(-time.Minute)},
	}
	provider := &memoryProvider{sandbox: sandboxprovider.Sandbox{ID: "provider-1", State: "running", UpdatedAt: activated}}
	if err := (Reconciler{Store: repository, Provider: provider, Now: func() time.Time { return now }}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !provider.deletedSandbox || !provider.deletedWorkspace || repository.row.Status != "deleted" {
		t.Fatalf("cleanup sandbox=%t workspace=%t row=%+v", provider.deletedSandbox, provider.deletedWorkspace, repository.row)
	}
	if repository.settledMS != int64((30*time.Minute)/time.Millisecond) {
		t.Fatalf("settled milliseconds=%d", repository.settledMS)
	}
}

func TestReconcilerReleasesUnactivatedHoldOnlyAfterCleanup(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	repository := &memoryRepository{
		reservation: domain.ManagedSpendReservation{ID: "reservation-2", TenantID: "tenant", ResourceName: "sandbox-2", ExpiresAt: now.Add(time.Hour)},
		row:         domain.NativeSandbox{ID: "sandbox-2", TenantID: "tenant", TemplateID: "base", BrezelWorkspaceID: "workspace-2", Status: "creating", BillingStateSince: now.Add(-time.Minute)},
	}
	provider := &memoryProvider{}
	if err := (Reconciler{Store: repository, Provider: provider, Now: func() time.Time { return now }}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !provider.deletedWorkspace || !repository.released || repository.settledMS != 0 {
		t.Fatalf("workspace_deleted=%t released=%t settled=%d", provider.deletedWorkspace, repository.released, repository.settledMS)
	}
}

func TestReconcilerCleansUnreservedCleanupPendingSandbox(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	row := domain.NativeSandbox{ID: "sandbox-orphan", TenantID: "tenant", TemplateID: "base", BrezelWorkspaceID: "workspace-orphan", Status: "cleanup_pending", BillingStateSince: now.Add(-time.Hour)}
	repository := &memoryRepository{cleanup: []domain.NativeSandbox{row}, row: row}
	provider := &memoryProvider{}
	if err := (Reconciler{Store: repository, Provider: provider, Now: func() time.Time { return now }}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !provider.deletedWorkspace || repository.row.Status != "deleted" || repository.released || repository.settledMS != 0 {
		t.Fatalf("workspace_deleted=%t row=%+v released=%t settled=%d", provider.deletedWorkspace, repository.row, repository.released, repository.settledMS)
	}
}

func TestReconcilerAcceptsAlreadyTerminalCleanupRetry(t *testing.T) {
	now := time.Date(2026, 10, 6, 21, 0, 0, 0, time.UTC)
	row := domain.NativeSandbox{ID: "sandbox-orphan", TenantID: "tenant", TemplateID: "base", BrezelSandboxID: "sandbox-terminal", BrezelWorkspaceID: "workspace-terminal", Status: "cleanup_pending", BillingStateSince: now.Add(-time.Hour)}
	repository := &memoryRepository{cleanup: []domain.NativeSandbox{row}, row: row}
	provider := &memoryProvider{
		deleteSandboxErr:   fmt.Errorf("%w: sandbox is already terminal", sandboxprovider.ErrConflict),
		deleteWorkspaceErr: fmt.Errorf("%w: workspace is already terminal", sandboxprovider.ErrConflict),
	}
	if err := (Reconciler{Store: repository, Provider: provider, Now: func() time.Time { return now }}).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.row.Status != "deleted" {
		t.Fatalf("row status = %q", repository.row.Status)
	}
}

func TestReconcilerRetriesNonTerminalCleanupConflict(t *testing.T) {
	now := time.Date(2026, 10, 6, 21, 0, 0, 0, time.UTC)
	row := domain.NativeSandbox{ID: "sandbox-orphan", TenantID: "tenant", TemplateID: "base", BrezelWorkspaceID: "workspace-active", Status: "cleanup_pending", BillingStateSince: now.Add(-time.Hour)}
	repository := &memoryRepository{cleanup: []domain.NativeSandbox{row}, row: row}
	provider := &memoryProvider{deleteWorkspaceErr: fmt.Errorf("%w: workspace has an active backend mutation", sandboxprovider.ErrConflict)}
	if err := (Reconciler{Store: repository, Provider: provider, Now: func() time.Time { return now }}).Once(context.Background()); err == nil {
		t.Fatal("expected active cleanup conflict")
	}
	if repository.row.Status != "cleanup_pending" {
		t.Fatalf("row status = %q", repository.row.Status)
	}
}
