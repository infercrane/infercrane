package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/alert"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/notification"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

// TestCampaignNotificationsDeliverSignedApprovalAndDecisionEvents runs the
// whole path against PostgreSQL: a destination is created through the
// service, a campaign waits for approval and then qualifies a winner, and the
// notifier delivers both events, signed, exactly once to an HTTPS receiver.
func TestCampaignNotificationsDeliverSignedApprovalAndDecisionEvents(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)

	var mu sync.Mutex
	var received [][]byte
	var secret string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(r.Header.Get("InferCrane-Timestamp") + "."))
		_, _ = mac.Write(body)
		if r.Header.Get("InferCrane-Signature") != "v1="+hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("InferCrane-Delivery") == "" {
			t.Errorf("unsigned delivery: %#v", r.Header)
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		received = append(received, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cipher, err := secretcipher.New(strings.Repeat("n", 32))
	if err != nil {
		t.Fatal(err)
	}
	service := notification.Service{Store: s, Cipher: cipher, KeyReference: "environment:TEST", Poster: alert.Deliverer{Client: server.Client(), AllowPrivate: true}, ConsoleURL: "https://console.example.test"}
	subscription, signingSecret, err := service.Create(ctx, "global", "owner", notification.CreateRequest{Destination: "webhook", URL: server.URL + "/infercrane", Events: []string{"approval_needed", "decided"}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	secret = signingSecret
	mu.Unlock()
	if listed, listErr := s.NotificationSubscriptionsForTenant(ctx, "global"); listErr != nil || len(listed) != 1 || listed[0].LastDelivery != nil || strings.Join(listed[0].Events, ",") != "approval_needed,decided" {
		t.Fatalf("listed=%+v err=%v", listed, listErr)
	}
	// Subscriptions only see events after they exist; give the clock a tick.
	time.Sleep(10 * time.Millisecond)

	suffix := time.Now().UTC().Format("150405.000000000")
	model := "Qwen/Qwen3-8B"
	target, err := s.AddTarget(ctx, domain.Target{Name: "notify-target-" + suffix, URL: "http://notify-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: model})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "notify-" + suffix, Model: model}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "notify-" + suffix, InputDigest: strings.Repeat("1", 64), ModelIdentity: model, Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`}, []domain.OptimizationCandidateRun{{ProposalCandidateID: strings.Repeat("2", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"qwen-fast"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`}})
	if err != nil {
		t.Fatal(err)
	}
	notifier := notification.Notifier{Service: service, Store: s, Reports: s}
	if queued, queueErr := notifier.Queue(ctx); queueErr != nil || queued != 1 {
		t.Fatalf("approval queued=%d err=%v", queued, queueErr)
	}
	if queued, queueErr := notifier.Queue(ctx); queueErr != nil || queued != 0 {
		t.Fatalf("approval queued again=%d err=%v", queued, queueErr)
	}
	if sent, sendErr := notifier.Send(ctx); sendErr != nil || sent != 1 {
		t.Fatalf("approval sent=%d err=%v", sent, sendErr)
	}

	campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	quality, _, err := s.RecordQualityEvidence(ctx, "global", deployment.Name, domain.QualityEvidence{RevisionID: deployment.ActiveRevisionID, Suite: "notify", SuiteVersion: "git:abc", Evaluator: "offline-evaluator", EvaluatorVersion: "1.0.0", Score: .99, Passed: true, SampleCount: 50, ArtifactDigest: "sha256:" + strings.Repeat("3", 64), PayloadDigest: "sha256:" + strings.Repeat("4", 64), Signature: "signature", PublicKey: "public-key", Algorithm: "Ed25519-SHA256", KeyID: "sha256:key", EvaluatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	lab, err := s.RecordLabEvaluation(ctx, "global", domain.LabEvaluation{ModelIdentity: model, AlgorithmVersion: "inference-lab-v3", InputJSON: `{"objective":"interactive"}`, ResultsJSON: `[{"selected":true}]`, InputDigest: strings.Repeat("5", 64)})
	if err != nil {
		t.Fatal(err)
	}
	candidateID, state := campaign.Candidates[0].ID, optimizationcampaign.CandidateProposed
	for _, step := range []struct {
		to      string
		updates domain.OptimizationCandidateRun
	}{
		{optimizationcampaign.CandidateProvisioning, domain.OptimizationCandidateRun{}},
		{optimizationcampaign.CandidateReady, domain.OptimizationCandidateRun{DeploymentName: deployment.Name, RevisionID: deployment.ActiveRevisionID}},
		{optimizationcampaign.CandidateMeasuring, domain.OptimizationCandidateRun{}},
		{optimizationcampaign.CandidateValidating, domain.OptimizationCandidateRun{BenchmarkID: "bench-notify"}},
		{optimizationcampaign.CandidateRanked, domain.OptimizationCandidateRun{BenchmarkID: "bench-notify", QualityEvidenceID: quality.ID}},
		{optimizationcampaign.CandidateQualified, domain.OptimizationCandidateRun{LabEvaluationID: lab.ID}},
	} {
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidateID, state, step.to, step.updates); err != nil {
			t.Fatalf("transition %s -> %s: %v", state, step.to, err)
		}
		state = step.to
	}
	transitions, err := s.OptimizationCampaignTransitionsSince(ctx, "global", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var path []string
	for _, transition := range transitions {
		if transition.CampaignID == campaign.ID {
			path = append(path, transition.From+">"+transition.To)
		}
	}
	if strings.Join(path, ",") != "approved>running,running>ranked,ranked>qualified" {
		t.Fatalf("recorded transitions=%v", path)
	}

	if queued, queueErr := notifier.Queue(ctx); queueErr != nil || queued != 1 {
		t.Fatalf("decision queued=%d err=%v", queued, queueErr)
	}
	if sent, sendErr := notifier.Send(ctx); sendErr != nil || sent != 1 {
		t.Fatalf("decision sent=%d err=%v", sent, sendErr)
	}
	mu.Lock()
	bodies := append([][]byte(nil), received...)
	mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("received %d deliveries", len(bodies))
	}
	var approval, decided notification.Body
	if err = json.Unmarshal(bodies[0], &approval); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(bodies[1], &decided); err != nil {
		t.Fatal(err)
	}
	if approval.Event != "infercrane.optimization.approval_needed" || approval.Campaign.ID != campaign.ID || approval.Links.Dashboard != "https://console.example.test/optimization/"+campaign.ID {
		t.Fatalf("approval=%s", bodies[0])
	}
	if decided.Event != "infercrane.optimization.decided" || decided.Outcome == nil || decided.Outcome.Kind != notification.OutcomeWinner || decided.Outcome.WinnerCandidateID != candidateID || decided.Links.ReportAPI != "/api/v1/optimization/campaigns/"+campaign.ID+"/report" {
		t.Fatalf("decided=%s", bodies[1])
	}
	t.Logf("approval_needed delivery: %s", bodies[0])
	t.Logf("decided delivery: %s", bodies[1])

	listed, err := s.NotificationSubscriptionForTenant(ctx, "global", subscription.ID)
	if err != nil || listed.LastDelivery == nil || listed.LastDelivery.EventType != notification.EventDecided || listed.LastDelivery.Status != "delivered" || listed.LastDelivery.DeliveredAt == nil {
		t.Fatalf("last delivery=%+v err=%v", listed.LastDelivery, err)
	}
	if err = s.DeleteNotificationSubscription(ctx, "global", subscription.ID); err != nil {
		t.Fatal(err)
	}
	if keys, keysErr := s.NotificationEventKeysSince(ctx, "global", time.Now().Add(-time.Hour)); keysErr != nil || len(keys) != 0 {
		t.Fatalf("deliveries outlived their destination: %v %v", keys, keysErr)
	}
}

func TestFailedCampaignKeepsItsFailureCodeAfterCleanup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "failed-" + suffix, InputDigest: strings.Repeat("6", 64), ModelIdentity: "org/model", Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`}, []domain.OptimizationCandidateRun{{ProposalCandidateID: strings.Repeat("7", 64), Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: `{"name":"candidate"}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`}})
	if err != nil {
		t.Fatal(err)
	}
	if campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	candidate := campaign.Candidates[0].ID
	if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidate, optimizationcampaign.CandidateProposed, optimizationcampaign.CandidateProvisioning, domain.OptimizationCandidateRun{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidate, optimizationcampaign.CandidateProvisioning, optimizationcampaign.CandidateFailed, domain.OptimizationCandidateRun{FailureCode: "provider_capacity_unavailable"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidate, optimizationcampaign.CandidateFailed, optimizationcampaign.CandidateCleaned, domain.OptimizationCandidateRun{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.OptimizationCampaign(ctx, "global", campaign.ID)
	if err != nil || loaded.State != optimizationcampaign.CampaignCleaned || loaded.FailureCode != "provider_capacity_unavailable" {
		t.Fatalf("campaign=%+v err=%v", loaded, err)
	}
	transitions, err := s.OptimizationCampaignTransitionsSince(ctx, "global", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	notices := notification.CampaignNotices(loaded, transitions, time.Now(), notification.QualityWaitNone)
	if len(notices) != 1 || notices[0].Type != notification.EventFailed {
		t.Fatalf("notices=%+v", notices)
	}
}

func TestNotificationDeliveryClaimsAndAttemptsAreFenced(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	stored, err := s.CreateNotificationSubscription(ctx, "global", domain.NotificationSubscription{Name: "fenced", Destination: "webhook", URLHint: "https://h.example", URLCiphertext: []byte{1}, URLNonce: []byte{2}, SecretCiphertext: []byte{3}, SecretNonce: []byte{4}, KeyReference: "k", Events: []string{"decided"}, Enabled: true, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, created, err := s.InsertNotificationDelivery(ctx, domain.NotificationDelivery{TenantID: "global", SubscriptionID: stored.ID, EventID: "run:decided", EventType: "decided", Body: `{}`, BodyDigest: "sha256:x", MaxAttempts: 2, OccurredAt: time.Now()}, 0)
	if err != nil || !created || delivery.Status != "pending" {
		t.Fatalf("delivery=%+v created=%v err=%v", delivery, created, err)
	}
	if _, created, err = s.InsertNotificationDelivery(ctx, domain.NotificationDelivery{TenantID: "global", SubscriptionID: stored.ID, EventID: "run:decided", EventType: "decided", Body: `{"other":1}`, BodyDigest: "sha256:y", MaxAttempts: 2, OccurredAt: time.Now()}, 0); err != nil || created {
		t.Fatalf("duplicate event created=%v err=%v", created, err)
	}
	first, err := s.ClaimNotificationDeliveries(ctx, time.Minute, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	if second, claimErr := s.ClaimNotificationDeliveries(ctx, time.Minute, 10); claimErr != nil || len(second) != 0 {
		t.Fatalf("a held delivery was claimed twice: %+v err=%v", second, claimErr)
	}
	retryAt := time.Now().Add(-time.Second)
	if row, recordErr := s.RecordNotificationDeliveryAttempt(ctx, delivery.ID, false, 503, "http_503", &retryAt); recordErr != nil || row.Status != "pending" || row.Attempts != 1 {
		t.Fatalf("first attempt=%+v err=%v", row, recordErr)
	}
	again, err := s.ClaimNotificationDeliveries(ctx, time.Minute, 10)
	if err != nil || len(again) != 1 {
		t.Fatalf("retry claim=%+v err=%v", again, err)
	}
	if row, recordErr := s.RecordNotificationDeliveryAttempt(ctx, delivery.ID, false, 503, "http_503", &retryAt); recordErr != nil || row.Status != "failed" || row.Attempts != 2 || row.ResponseStatus != 503 {
		t.Fatalf("budget exhausted attempt=%+v err=%v", row, recordErr)
	}
	if _, recordErr := s.RecordNotificationDeliveryAttempt(ctx, delivery.ID, true, 200, "", nil); !errors.Is(recordErr, ErrConflict) {
		t.Fatalf("a finished delivery accepted another attempt: %v", recordErr)
	}
	for index := 1; index < MaxNotificationSubscriptions; index++ {
		if _, err = s.CreateNotificationSubscription(ctx, "global", domain.NotificationSubscription{Name: "more", Destination: "slack", URLHint: "https://h", URLCiphertext: []byte{1}, URLNonce: []byte{2}, SecretCiphertext: []byte{3}, SecretNonce: []byte{4}, KeyReference: "k", Events: []string{"failed"}, Enabled: index%2 == 0, CreatedBy: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.CreateNotificationSubscription(ctx, "global", domain.NotificationSubscription{Name: "one-too-many", Destination: "slack", URLHint: "https://h", URLCiphertext: []byte{1}, URLNonce: []byte{2}, SecretCiphertext: []byte{3}, SecretNonce: []byte{4}, KeyReference: "k", Events: []string{"failed"}, Enabled: true, CreatedBy: "owner"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("eleventh destination err=%v", err)
	}
	if tenants, tenantsErr := s.NotificationTenants(ctx); tenantsErr != nil || strings.Join(tenants, ",") != "global" {
		t.Fatalf("tenants=%v err=%v", tenants, tenantsErr)
	}
}
