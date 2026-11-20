package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

func TestAgentManagedEndpointIsCustomerCloudAndRefusesOtherSupply(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	server, _, err := s.RegisterAgentServer(ctx, "tenant-a", agent.ID, domain.AgentServerRegistration{Key: "vllm-models-svc-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Address: "http://vllm.models.svc:8000"})
	if err != nil {
		t.Fatal(err)
	}
	if bindings, bindErr := s.AgentDataPlaneBindings(ctx, "tenant-a", agent.ID); bindErr != nil || len(bindings) != 0 {
		t.Fatalf("an observe-only server must not be served: %#v err=%v", bindings, bindErr)
	}
	// traffic-managed stays refused: the control plane cannot reach it.
	if _, err = s.PromoteAdoptionOwnership(ctx, "tenant-a", "support-bot", "traffic-managed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("traffic-managed agent target: %v", err)
	}
	promoted, err := s.PromoteAdoptionOwnership(ctx, "tenant-a", "support-bot", "agent-managed")
	if err != nil || promoted.OwnershipMode != "agent-managed" {
		t.Fatalf("agent-managed promotion=%#v err=%v", promoted, err)
	}
	if residency, residencyErr := s.EndpointResidency(ctx, "tenant-a", "support-bot"); residencyErr != nil || residency != ResidencyCustomerCloud {
		t.Fatalf("residency=%q err=%v", residency, residencyErr)
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, "tenant-a", "support-bot")
	if err != nil || resolved.Endpoint.Residency != ResidencyCustomerCloud || resolved.Bindings[0].OwnershipMode != "agent-managed" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if err = s.ReportAgentServerHealth(ctx, "tenant-a", agent.ID, server.Key, "healthy", time.Now()); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.AgentDataPlaneBindings(ctx, "tenant-a", agent.ID)
	if err != nil || len(bindings) != 1 || bindings[0].ServerKey != server.Key || bindings[0].BindingID != server.BindingID || bindings[0].EndpointName != "support-bot" || bindings[0].Health != "healthy" || bindings[0].RoutingPolicy != "manual" {
		t.Fatalf("data plane bindings=%#v err=%v", bindings, err)
	}
	other, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-b", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if foreign, foreignErr := s.AgentDataPlaneBindings(ctx, "tenant-a", other.ID); foreignErr != nil || len(foreign) != 0 {
		t.Fatalf("another agent got this agent's routes: %#v err=%v", foreign, foreignErr)
	}

	// No other supply can join a customer-cloud endpoint, whichever path
	// adds it: a hosted external target, a deployment, or a managed fallback.
	external, err := s.AddTargetForTenant(ctx, "tenant-a", domain.Target{Name: "hosted-api", URL: "https://api.example.test/v1", Provider: "external", Runtime: "openai-compatible", UpstreamModel: "Qwen/Qwen3-8B", Health: "healthy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBackendBinding(ctx, "tenant-a", domain.BackendBinding{EndpointID: resolved.Endpoint.ID, Name: "hosted", Kind: "external", OwnershipMode: "traffic-managed", TargetID: external.ID}); !errors.Is(err, ErrResidency) {
		t.Fatalf("hosted binding on a customer-cloud endpoint: %v", err)
	}
	// Leaving customer-cloud is explicit, and then the binding is allowed.
	if _, err = s.SetEndpointResidency(ctx, "tenant-a", "support-bot", ResidencyAny); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBackendBinding(ctx, "tenant-a", domain.BackendBinding{EndpointID: resolved.Endpoint.ID, Name: "hosted", Kind: "external", OwnershipMode: "traffic-managed", TargetID: external.ID}); err != nil {
		t.Fatalf("binding after leaving customer-cloud: %v", err)
	}
	if _, err = s.SetEndpointResidency(ctx, "tenant-a", "support-bot", ResidencyCustomerCloud); !errors.Is(err, ErrResidency) {
		t.Fatalf("customer-cloud with a hosted binding present: %v", err)
	}
	if bindings, err = s.AgentDataPlaneBindings(ctx, "tenant-a", agent.ID); err != nil || len(bindings) != 0 {
		t.Fatalf("an endpoint that is not customer-cloud must not be served by the agent: %#v err=%v", bindings, err)
	}
	if _, err = s.SetEndpointResidency(ctx, "tenant-a", "support-bot", "everywhere"); err == nil {
		t.Fatal("an unknown residency was accepted")
	}

	// Demotion returns the server to observe-only.
	if demoted, demoteErr := s.PromoteAdoptionOwnership(ctx, "tenant-a", "support-bot", "observe-only"); demoteErr != nil || demoted.OwnershipMode != "observe-only" {
		t.Fatalf("demotion=%#v err=%v", demoted, demoteErr)
	}
	// A directly reached server cannot become agent-managed.
	if _, _, err = s.AdoptEndpoint(ctx, "tenant-a", "direct", "direct", "Qwen/Qwen3-8B", "http://10.0.0.5:8000/v1", "vllm", "observe-only", "vllm"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PromoteAdoptionOwnership(ctx, "tenant-a", "direct", "agent-managed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("agent-managed without an agent: %v", err)
	}
}

func TestAgentDataPlaneCredentialsAreDigestsOfLiveNonAgentKeys(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTenant(ctx, "tenant-b", "Tenant B"); err != nil {
		t.Fatal(err)
	}
	_, agentKey, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, appKey, err := s.CreatePrincipal(ctx, "tenant-a", "app", authz.Viewer)
	if err != nil {
		t.Fatal(err)
	}
	_, otherTenantKey, err := s.CreatePrincipal(ctx, "tenant-b", "app", authz.Viewer)
	if err != nil {
		t.Fatal(err)
	}
	records, err := s.AgentDataPlaneCredentials(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, record := range records {
		if record.Principal.TenantID != "tenant-a" || record.Principal.Kind == domain.AgentPrincipalKind {
			t.Fatalf("record=%#v", record)
		}
		found[record.Hash] = true
	}
	if !found[credentialHash(appKey)] || found[credentialHash(agentKey)] || found[credentialHash(otherTenantKey)] {
		t.Fatalf("credentials=%#v", records)
	}
	if limit, limited, quotaErr := s.RequestQuotaLimit(ctx, "tenant-a"); quotaErr != nil || limited || limit != 0 {
		t.Fatalf("no quota: %d %v %v", limit, limited, quotaErr)
	}
}

func TestAgentCommandsLeaseOnceExpireAndCancel(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.CreateAgent(ctx, "tenant-a", "cluster-a", "owner")
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(deadline time.Time) domain.AgentCommand {
		t.Helper()
		command, enqueueErr := s.EnqueueAgentCommand(ctx, domain.AgentCommand{TenantID: "tenant-a", AgentID: agent.ID, Kind: "run_candidate", SpecJSON: `{"run_candidate":{},"control":{}}`, CreatedBy: "owner", DeadlineAt: deadline})
		if enqueueErr != nil || command.State != "queued" {
			t.Fatalf("enqueue=%#v err=%v", command, enqueueErr)
		}
		return command
	}
	first := enqueue(time.Now().Add(time.Hour))
	second := enqueue(time.Now().Add(time.Hour))

	// Two replicas answering the same agent never lease one command twice.
	var mu sync.Mutex
	leased := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, leaseErr := s.LeaseAgentCommands(ctx, "tenant-a", agent.ID, time.Now(), time.Minute, 4)
			if leaseErr != nil {
				t.Error(leaseErr)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, item := range items {
				leased[item.ID]++
			}
		}()
	}
	wg.Wait()
	if len(leased) != 2 || leased[first.ID] != 1 || leased[second.ID] != 1 {
		t.Fatalf("leases=%v", leased)
	}
	running, err := s.UpdateAgentCommand(ctx, "tenant-a", agent.ID, first.ID, domain.AgentCommandUpdate{State: "running", Phase: "measuring_candidate", Lease: time.Minute})
	if err != nil || running.State != "running" || running.Phase != "measuring_candidate" || running.LeaseExpiresAt == nil {
		t.Fatalf("progress=%#v err=%v", running, err)
	}
	done, err := s.UpdateAgentCommand(ctx, "tenant-a", agent.ID, first.ID, domain.AgentCommandUpdate{State: "succeeded", Phase: "done", ResultJSON: `{"ready_seconds":12.5,"cleaned_up":true}`})
	if err != nil || done.State != "succeeded" || done.FinishedAt == nil || done.ResultJSON == "" {
		t.Fatalf("done=%#v err=%v", done, err)
	}
	// A finished command ignores late reports.
	if late, lateErr := s.UpdateAgentCommand(ctx, "tenant-a", agent.ID, first.ID, domain.AgentCommandUpdate{State: "failed", FailureCode: "late"}); lateErr != nil || late.State != "succeeded" {
		t.Fatalf("late report=%#v err=%v", late, lateErr)
	}
	cancelled, err := s.CancelAgentCommand(ctx, "tenant-a", agent.ID, second.ID)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel=%#v err=%v", cancelled, err)
	}
	if after, afterErr := s.UpdateAgentCommand(ctx, "tenant-a", agent.ID, second.ID, domain.AgentCommandUpdate{State: "running", Phase: "answer_check"}); afterErr != nil || after.State != "cancelled" {
		t.Fatalf("report after cancel=%#v err=%v", after, afterErr)
	}

	// Work past its deadline expires instead of running.
	late := enqueue(time.Now().Add(time.Minute))
	if items, leaseErr := s.LeaseAgentCommands(ctx, "tenant-a", agent.ID, time.Now().Add(2*time.Minute), time.Minute, 4); leaseErr != nil || len(items) != 0 {
		t.Fatalf("expired command leased: %#v err=%v", items, leaseErr)
	}
	if expired, readErr := s.AgentCommand(ctx, "tenant-a", agent.ID, late.ID); readErr != nil || expired.State != "expired" || expired.FailureCode != "deadline_exceeded" {
		t.Fatalf("expired=%#v err=%v", expired, readErr)
	}

	// A running command whose agent stopped reporting is handed out again,
	// a bounded number of times.
	stuck := enqueue(time.Now().Add(time.Hour))
	at := time.Now()
	for attempt := 1; attempt <= maxCommandAttempts; attempt++ {
		items, leaseErr := s.LeaseAgentCommands(ctx, "tenant-a", agent.ID, at, time.Minute, 4)
		if leaseErr != nil || len(items) != 1 || items[0].ID != stuck.ID || items[0].Attempts != attempt {
			t.Fatalf("attempt %d: %#v err=%v", attempt, items, leaseErr)
		}
		at = at.Add(2 * time.Minute)
	}
	if items, leaseErr := s.LeaseAgentCommands(ctx, "tenant-a", agent.ID, at, time.Minute, 4); leaseErr != nil || len(items) != 0 {
		t.Fatalf("leased past the attempt bound: %#v err=%v", items, leaseErr)
	}
	if failed, readErr := s.AgentCommand(ctx, "tenant-a", agent.ID, stuck.ID); readErr != nil || failed.State != "failed" || failed.FailureCode != "agent_stopped_reporting" {
		t.Fatalf("stuck=%#v err=%v", failed, readErr)
	}

	// The queue is bounded per agent, and a revoked agent takes no work.
	for index := 0; index <= MaxQueuedAgentCommands; index++ {
		if _, err = s.EnqueueAgentCommand(ctx, domain.AgentCommand{TenantID: "tenant-a", AgentID: agent.ID, Kind: "run_candidate", SpecJSON: `{}`, CreatedBy: "owner", DeadlineAt: time.Now().Add(time.Hour)}); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("unbounded queue: %v", err)
	}
	if _, err = s.RevokeAgent(ctx, "tenant-a", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueAgentCommand(ctx, domain.AgentCommand{TenantID: "tenant-a", AgentID: agent.ID, Kind: "run_candidate", SpecJSON: `{}`, CreatedBy: "owner", DeadlineAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked agent took a command: %v", err)
	}
}
