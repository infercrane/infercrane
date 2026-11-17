package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/alert"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

type received struct {
	Path, Delivery, Timestamp, Signature string
	Body                                 []byte
}

// receiver is an HTTPS endpoint standing in for a customer webhook and for
// Slack. Every request to any host is routed to it.
type receiver struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	got     []received
	replies []int
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{t: t}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		r.mu.Lock()
		r.got = append(r.got, received{Path: request.URL.Path, Delivery: request.Header.Get("InferCrane-Delivery"), Timestamp: request.Header.Get("InferCrane-Timestamp"), Signature: request.Header.Get("InferCrane-Signature"), Body: body})
		status := http.StatusNoContent
		if len(r.replies) > 0 {
			status, r.replies = r.replies[0], r.replies[1:]
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) poster() alert.Deliverer {
	target, _ := url.Parse(r.server.URL)
	client := r.server.Client()
	base := client.Transport
	client.Transport = roundTripper(func(request *http.Request) (*http.Response, error) {
		request.URL.Host = target.Host
		return base.RoundTrip(request)
	})
	return alert.Deliverer{Client: client, AllowPrivate: true}
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func verifySignature(t *testing.T, secret string, request received) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(request.Timestamp + "."))
	_, _ = mac.Write(request.Body)
	if want := "v1=" + hex.EncodeToString(mac.Sum(nil)); !hmac.Equal([]byte(request.Signature), []byte(want)) || request.Delivery == "" {
		t.Fatalf("signature %q does not verify for %s", request.Signature, request.Body)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

func testService(t *testing.T, store *memoryStore, poster Poster, now func() time.Time) Service {
	t.Helper()
	cipher, err := secretcipher.New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	return Service{Store: store, Cipher: cipher, KeyReference: "environment:TEST_KEY", Poster: poster, ConsoleURL: "https://console.example.test/", Now: now}
}

func TestNotifierDeliversSignedApprovalAndDecisionEventsOnce(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	hook := newReceiver(t)
	service := testService(t, store, hook.poster(), clk.Now)

	webhook, secret, err := service.Create(ctx, "tenant-a", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/infercrane", Events: []string{"decided", "approval_needed"}})
	if err != nil || !strings.HasPrefix(secret, SecretPrefix) || webhook.Name != "hooks.example.test" || strings.Join(webhook.Events, ",") != "approval_needed,decided" {
		t.Fatalf("webhook=%+v secret=%q err=%v", webhook, secret, err)
	}
	if _, _, err = service.Create(ctx, "tenant-a", "owner", CreateRequest{Destination: "slack", URL: "https://hooks.slack.com/services/T000/B000/XXXXXXXXXXXXXXXX", Events: []string{"approval_needed"}}); err != nil {
		t.Fatal(err)
	}
	notifier := Notifier{Service: service, Store: store, Reports: store, Now: clk.Now}

	// A campaign created before the destinations existed is not announced.
	store.setCampaign(domain.OptimizationCampaign{ID: "old-run", TenantID: "tenant-a", State: optimizationcampaign.CampaignAwaitingApproval, CreatedAt: clk.Now().Add(-time.Minute), UpdatedAt: clk.Now()})
	clk.Advance(time.Minute)
	store.setCampaign(domain.OptimizationCampaign{ID: "run-1", TenantID: "tenant-a", ModelIdentity: "Qwen/Qwen3-8B", Objective: "interactive", Intent: optimizationcampaign.IntentNewEndpoint, State: optimizationcampaign.CampaignAwaitingApproval, CreatedAt: clk.Now(), UpdatedAt: clk.Now()})
	// Another workspace's campaign never reaches this destination.
	store.setCampaign(domain.OptimizationCampaign{ID: "run-other", TenantID: "tenant-b", State: optimizationcampaign.CampaignAwaitingApproval, CreatedAt: clk.Now(), UpdatedAt: clk.Now()})

	queued, err := notifier.Queue(ctx)
	if err != nil || queued != 2 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	if again, err := notifier.Queue(ctx); err != nil || again != 0 {
		t.Fatalf("second queue pass queued=%d err=%v", again, err)
	}
	if sent, err := notifier.Send(ctx); err != nil || sent != 2 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}

	clk.Advance(time.Hour)
	approved, expires := clk.Now(), clk.Now().Add(4*time.Hour)
	clk.Advance(30 * time.Minute)
	store.setCampaign(domain.OptimizationCampaign{ID: "run-1", TenantID: "tenant-a", ModelIdentity: "Qwen/Qwen3-8B", Objective: "interactive", Intent: optimizationcampaign.IntentNewEndpoint, State: optimizationcampaign.CampaignQualified, ApprovedAt: &approved, ApprovalExpiresAt: &expires, CreatedAt: approved, UpdatedAt: clk.Now(), Candidates: []domain.OptimizationCandidateRun{{ID: "winner", TenantID: "tenant-a", CampaignID: "run-1", State: optimizationcampaign.CandidateQualified, DeploymentSpecJSON: `{"name":"qwen-fast","runtime":{"engine":"vllm"},"target":{"gpu":"H100"}}`, UpdatedAt: clk.Now()}}})
	store.addTransition(domain.OptimizationCampaignTransition{TenantID: "tenant-a", CampaignID: "run-1", From: "running", To: "qualified", CreatedAt: clk.Now()})
	if queued, err = notifier.Queue(ctx); err != nil || queued != 1 {
		t.Fatalf("decision queued=%d err=%v", queued, err)
	}
	if sent, err := notifier.Send(ctx); err != nil || sent != 1 {
		t.Fatalf("decision sent=%d err=%v", sent, err)
	}

	requests := hook.requests()
	if len(requests) != 3 {
		t.Fatalf("requests=%d", len(requests))
	}
	var bodies []Body
	for _, request := range requests {
		if strings.HasPrefix(request.Path, "/services/") {
			var slack map[string]string
			if err = json.Unmarshal(request.Body, &slack); err != nil || !strings.Contains(slack["text"], "needs your spend approval") || !strings.Contains(slack["text"], "<https://console.example.test/optimization/run-1|Open the run>") {
				t.Fatalf("slack body=%s err=%v", request.Body, err)
			}
			continue
		}
		verifySignature(t, secret, request)
		var body Body
		if err = json.Unmarshal(request.Body, &body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	if len(bodies) != 2 || bodies[0].Event != "infercrane.optimization.approval_needed" || bodies[1].Event != "infercrane.optimization.decided" {
		t.Fatalf("bodies=%+v", bodies)
	}
	approval, decided := bodies[0], bodies[1]
	if approval.ID != "run-1:approval_needed" || approval.WorkspaceID != "tenant-a" || approval.Campaign == nil || approval.Campaign.Model != "Qwen/Qwen3-8B" || approval.Links.Dashboard != "https://console.example.test/optimization/run-1" || approval.Links.ReportAPI != "/api/v1/optimization/campaigns/run-1/report" {
		t.Fatalf("approval body=%+v", approval)
	}
	if decided.Outcome == nil || decided.Outcome.Kind != OutcomeWinner || decided.Outcome.WinnerCandidateID != "winner" || !strings.HasPrefix(decided.Summary, "The Qwen/Qwen3-8B run finished. Roll out") {
		t.Fatalf("decided body=%+v outcome=%+v", decided, decided.Outcome)
	}
	for _, delivery := range append(store.delivered(EventApprovalNeeded), store.delivered(EventDecided)...) {
		if delivery.Status != "delivered" || delivery.Attempts != 1 {
			t.Fatalf("delivery=%+v", delivery)
		}
	}
}

func TestQualityWaitIsAnnouncedOnlyWhenTheRunWaitsOnTheCustomer(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	service := testService(t, store, newReceiver(t).poster(), clk.Now)
	if _, _, err := service.Create(ctx, "tenant-a", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/infercrane", Events: []string{EventQualityEvidenceNeeded}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	uploaded := "sha256:" + strings.Repeat("a", 64)
	store.evalSets = map[string]bool{uploaded: true}
	run := func(id, proposal string) {
		store.setCampaign(domain.OptimizationCampaign{ID: id, TenantID: "tenant-a", ModelIdentity: "Qwen/Qwen3-8B", State: optimizationcampaign.CampaignRunning, ProposalJSON: proposal, CreatedAt: clk.Now().Add(-30 * time.Minute), UpdatedAt: clk.Now(), Candidates: []domain.OptimizationCandidateRun{{ID: id + "-a", State: optimizationcampaign.CandidateValidating, UpdatedAt: clk.Now().Add(-20 * time.Minute)}}})
	}
	run("builtin", proposalJSON(t, optimizer.QualityCheckBuiltin, ""))
	run("customer", proposalJSON(t, optimizer.QualityCheckCustomer, ""))
	run("eval-set-missing", proposalJSON(t, optimizer.QualityCheckBuiltin, testEvalSet))
	run("eval-set-uploaded", proposalJSON(t, optimizer.QualityCheckBuiltin, uploaded))

	queued, err := Notifier{Service: service, Store: store, Reports: store, Now: clk.Now}.Queue(ctx)
	if err != nil || queued != 2 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	summaries := map[string]string{}
	for _, delivery := range store.delivered(EventQualityEvidenceNeeded) {
		var body Body
		if err = json.Unmarshal([]byte(delivery.Body), &body); err != nil {
			t.Fatal(err)
		}
		summaries[delivery.CampaignID] = body.Summary
	}
	if !strings.Contains(summaries["customer"], "your signed evaluation of 1 setup") || !strings.Contains(summaries["eval-set-missing"], "eval set it names to be uploaded") || len(summaries) != 2 {
		t.Fatalf("summaries=%v", summaries)
	}
}

func TestNotifierRetriesWithBackoffAndStopsOnPermanentRefusal(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	hook := newReceiver(t)
	hook.replies = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusOK}
	service := testService(t, store, hook.poster(), clk.Now)
	if _, _, err := service.Create(ctx, "tenant-a", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/a", Events: []string{"failed"}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	store.setCampaign(domain.OptimizationCampaign{ID: "run", TenantID: "tenant-a", State: optimizationcampaign.CampaignCleaned, FailureCode: "approval_expired", UpdatedAt: clk.Now()})
	store.addTransition(domain.OptimizationCampaignTransition{TenantID: "tenant-a", CampaignID: "run", From: "running", To: "failed", CreatedAt: clk.Now()})
	notifier := Notifier{Service: service, Store: store, Now: clk.Now}
	if queued, err := notifier.Queue(ctx); err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	for attempt, wait := range []time.Duration{0, 30 * time.Second, 2 * time.Minute} {
		clk.Advance(wait)
		if sent, err := notifier.Send(ctx); err != nil || sent != 1 {
			t.Fatalf("attempt %d sent=%d err=%v", attempt+1, sent, err)
		}
		clk.Advance(time.Second)
		if sent, _ := notifier.Send(ctx); sent != 0 {
			t.Fatalf("attempt %d resent inside its retry delay", attempt+1)
		}
		clk.Advance(-time.Second)
	}
	delivery := store.delivered(EventFailed)[0]
	if delivery.Status != "delivered" || delivery.Attempts != 3 {
		t.Fatalf("delivery=%+v", delivery)
	}
	var body Body
	if err := json.Unmarshal([]byte(delivery.Body), &body); err != nil || body.Failure == nil || body.Failure.Code != "approval_expired" || !strings.Contains(body.Summary, "spend approval ended") {
		t.Fatalf("failed body=%s err=%v", delivery.Body, err)
	}

	hook.replies = []int{http.StatusGone}
	store.setCampaign(domain.OptimizationCampaign{ID: "run-2", TenantID: "tenant-a", State: optimizationcampaign.CampaignFailed, UpdatedAt: clk.Now()})
	store.addTransition(domain.OptimizationCampaignTransition{TenantID: "tenant-a", CampaignID: "run-2", From: "running", To: "failed", CreatedAt: clk.Now()})
	if _, err := notifier.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := notifier.Send(ctx); err != nil {
		t.Fatal(err)
	}
	gone := store.delivered(EventFailed)[1]
	if gone.Status != "failed" || gone.Attempts != 1 || gone.ErrorCode != "http_410" {
		t.Fatalf("permanent refusal delivery=%+v", gone)
	}
	var unknown Body
	if err := json.Unmarshal([]byte(gone.Body), &unknown); err != nil || unknown.Failure.Code != OutcomeUnknown || unknown.Failure.Reason != "reason unknown" {
		t.Fatalf("unknown failure body=%s", gone.Body)
	}
}

func TestDisabledDestinationsAndConcurrentNotifiersNeverDuplicate(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	store := newMemoryStore(clk.Now)
	hook := newReceiver(t)
	service := testService(t, store, hook.poster(), clk.Now)
	disabled := false
	if _, _, err := service.Create(ctx, "tenant-a", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/off", Events: []string{"approval_needed"}, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	enabled, _, err := service.Create(ctx, "tenant-a", "owner", CreateRequest{Destination: "webhook", URL: "https://hooks.example.test/on", Events: []string{"approval_needed"}})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	for index := range 5 {
		store.setCampaign(domain.OptimizationCampaign{ID: "run-" + string(rune('a'+index)), TenantID: "tenant-a", State: optimizationcampaign.CampaignAwaitingApproval, CreatedAt: clk.Now(), UpdatedAt: clk.Now()})
	}
	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			notifier := Notifier{Service: service, Store: store, Now: clk.Now}
			if _, err := notifier.Queue(ctx); err != nil {
				t.Error(err)
			}
			if _, err := notifier.Send(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	requests := hook.requests()
	deliveries := store.delivered(EventApprovalNeeded)
	if len(requests) != 5 || len(deliveries) != 5 {
		t.Fatalf("requests=%d deliveries=%d", len(requests), len(deliveries))
	}
	for _, delivery := range deliveries {
		if delivery.SubscriptionID != enabled.ID || delivery.Status != "delivered" {
			t.Fatalf("delivery=%+v", delivery)
		}
	}
}

func TestRetryDelayGrowsToAnHour(t *testing.T) {
	want := []time.Duration{30 * time.Second, 2 * time.Minute, 8 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for index, delay := range want {
		if got := RetryDelay(index + 1); got != delay {
			t.Fatalf("attempt %d delay=%s want %s", index+1, got, delay)
		}
	}
}
