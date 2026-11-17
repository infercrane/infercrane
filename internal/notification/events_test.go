package notification

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
)

const testEvalSet = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// proposalJSON is a stored proposal that asks for the given answer check.
func proposalJSON(t *testing.T, check, evalSet string) string {
	t.Helper()
	encoded, err := json.Marshal(optimizer.Proposal{Input: optimizer.Request{QualityCheck: check, QualityEvalSet: evalSet}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func at(base time.Time, offset time.Duration) *time.Time {
	value := base.Add(offset)
	return &value
}

func noticeTypes(notices []Notice) string {
	types := make([]string, 0, len(notices))
	for _, notice := range notices {
		types = append(types, notice.Type)
	}
	return strings.Join(types, ",")
}

func TestCampaignNoticesFollowWaitingStatesAndRecordedTransitions(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	transition := func(from, to string, offset time.Duration) domain.OptimizationCampaignTransition {
		return domain.OptimizationCampaignTransition{CampaignID: "run", From: from, To: to, CreatedAt: now.Add(offset)}
	}
	for _, tc := range []struct {
		name        string
		campaign    domain.OptimizationCampaign
		transitions []domain.OptimizationCampaignTransition
		wait        string
		want        string
	}{
		{name: "awaiting approval", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignAwaitingApproval, CreatedAt: now.Add(-time.Minute)}, want: EventApprovalNeeded},
		{name: "cancelled produces nothing", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignAwaitingApproval, CancelRequested: true}, transitions: []domain.OptimizationCampaignTransition{transition("running", "failed", -time.Minute)}, want: ""},
		{name: "running far from expiry", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning, ApprovedAt: at(now, -time.Hour), ApprovalExpiresAt: at(now, 3*time.Hour)}, want: ""},
		{name: "running inside the warning window", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning, ApprovedAt: at(now, -3*time.Hour), ApprovalExpiresAt: at(now, 30*time.Minute)}, want: EventApprovalExpiring},
		{name: "expired approval warns no more", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignQualified, ApprovedAt: at(now, -3*time.Hour), ApprovalExpiresAt: at(now, -time.Second)}, want: ""},
		{name: "waiting on the customer inside grace", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning, Candidates: []domain.OptimizationCandidateRun{{ID: "a", State: optimizationcampaign.CandidateValidating, UpdatedAt: now.Add(-time.Minute)}}}, wait: QualityWaitEvidence, want: ""},
		{name: "waiting on the customer past grace", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning, Candidates: []domain.OptimizationCandidateRun{{ID: "a", State: optimizationcampaign.CandidateValidating, UpdatedAt: now.Add(-10 * time.Minute)}}}, wait: QualityWaitEvidence, want: EventQualityEvidenceNeeded},
		{name: "waiting for an eval set past grace", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning, Candidates: []domain.OptimizationCandidateRun{{ID: "a", State: optimizationcampaign.CandidateValidating, UpdatedAt: now.Add(-10 * time.Minute)}}}, wait: QualityWaitEvalSet, want: EventQualityEvidenceNeeded},
		{name: "a slow built-in answer check needs no one", campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning, Candidates: []domain.OptimizationCandidateRun{{ID: "a", State: optimizationcampaign.CandidateValidating, UpdatedAt: now.Add(-time.Hour)}}}, want: ""},
		{
			name:     "a rejected decision survives cleanup",
			campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignCleaned},
			transitions: []domain.OptimizationCampaignTransition{
				transition("rejected", "cleaned", -time.Second), transition("running", "rejected", -2*time.Second), transition("approved", "running", -time.Hour),
			},
			want: EventDecided,
		},
		{
			name:     "winner then rollout",
			campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignPromoted},
			transitions: []domain.OptimizationCampaignTransition{
				transition("running", "qualified", -time.Hour), transition("qualified", "promoted", -time.Minute),
			},
			want: EventDecided + "," + EventRolledOut,
		},
		{
			name:     "winner discarded when the approval ends",
			campaign: domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignCleaned, FailureCode: "approval_expired_before_activation"},
			transitions: []domain.OptimizationCampaignTransition{
				transition("running", "guard_passed", -2*time.Hour), transition("guard_passed", "failed", -time.Minute), transition("failed", "cleaned", -time.Second),
			},
			want: EventDecided + "," + EventFailed,
		},
		{
			name:        "another campaign's transitions are ignored",
			campaign:    domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignRunning},
			transitions: []domain.OptimizationCampaignTransition{{CampaignID: "other", To: optimizationcampaign.CampaignFailed, CreatedAt: now}},
			want:        "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notices := CampaignNotices(tc.campaign, tc.transitions, now, tc.wait)
			if got := noticeTypes(notices); got != tc.want {
				t.Fatalf("notices=%s want %s (%+v)", got, tc.want, notices)
			}
			for _, notice := range notices {
				if notice.ID != "run:"+notice.Type || notice.CampaignID != "run" || notice.OccurredAt.IsZero() {
					t.Fatalf("notice identity=%+v", notice)
				}
				if notice.Type == EventQualityEvidenceNeeded && notice.QualityWait != tc.wait {
					t.Fatalf("quality wait=%q want %q", notice.QualityWait, tc.wait)
				}
			}
		})
	}
}

func TestNoticeTimesAreStableAcrossPasses(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	campaign := domain.OptimizationCampaign{ID: "run", State: optimizationcampaign.CampaignQualified, ApprovedAt: at(now, -4*time.Hour), ApprovalExpiresAt: at(now, 20*time.Minute), Candidates: []domain.OptimizationCandidateRun{{ID: "winner", State: optimizationcampaign.CandidateQualified}}}
	first := CampaignNotices(campaign, nil, now, QualityWaitNone)
	second := CampaignNotices(campaign, nil, now.Add(5*time.Minute), QualityWaitNone)
	if len(first) != 1 || len(second) != 1 || !first[0].OccurredAt.Equal(second[0].OccurredAt) || !first[0].OccurredAt.Equal(now.Add(-40*time.Minute)) {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if strings.Join(first[0].CandidateIDs, ",") != "winner" {
		t.Fatalf("expiring winner candidates=%v", first[0].CandidateIDs)
	}
}

func TestQualityWaitFollowsTheRunsAnswerCheck(t *testing.T) {
	for _, tc := range []struct {
		name           string
		proposal       string
		evalSetMissing bool
		want           string
	}{
		{"built-in check", proposalJSON(t, optimizer.QualityCheckBuiltin, ""), false, QualityWaitNone},
		{"no stored proposal", "", true, QualityWaitNone},
		{"customer evidence", proposalJSON(t, optimizer.QualityCheckCustomer, ""), false, QualityWaitEvidence},
		{"customer evidence wins over a missing eval set", proposalJSON(t, optimizer.QualityCheckCustomer, testEvalSet), true, QualityWaitEvidence},
		{"eval set not uploaded", proposalJSON(t, optimizer.QualityCheckBuiltin, testEvalSet), true, QualityWaitEvalSet},
		{"eval set uploaded or unknown", proposalJSON(t, optimizer.QualityCheckBuiltin, testEvalSet), false, QualityWaitNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := QualityWait(domain.OptimizationCampaign{ID: "run", ProposalJSON: tc.proposal}, tc.evalSetMissing); got != tc.want {
				t.Fatalf("wait=%q want %q", got, tc.want)
			}
		})
	}
}

func TestExpiryWarningIsAQuarterOfTheWindowAtMostAnHour(t *testing.T) {
	base := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		window, want time.Duration
	}{
		{time.Hour, 15 * time.Minute},
		{2 * time.Hour, 30 * time.Minute},
		{24 * time.Hour, time.Hour},
		{-time.Hour, 0},
	} {
		if got := ExpiryWarning(base, base.Add(tc.window)); got != tc.want {
			t.Fatalf("window %s warning=%s want %s", tc.window, got, tc.want)
		}
	}
}

func TestCampaignAttentionMatchesNotificationRules(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	codes := func(items []Attention) string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Severity+":"+item.Code)
		}
		return strings.Join(out, ",")
	}
	validating := []domain.OptimizationCandidateRun{{ID: "a", State: optimizationcampaign.CandidateValidating, UpdatedAt: now.Add(-10 * time.Minute)}}
	for _, tc := range []struct {
		name     string
		campaign domain.OptimizationCampaign
		wait     string
		want     string
	}{
		{"approval", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignAwaitingApproval}, "", "warning:optimization_approval_needed"},
		{"winner with time left", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignGuardPassed, ApprovedAt: at(now, -time.Hour), ApprovalExpiresAt: at(now, 5*time.Hour)}, "", "warning:optimization_decision_waiting"},
		{"winner about to be discarded", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignQualified, ApprovedAt: at(now, -time.Hour), ApprovalExpiresAt: at(now, 5*time.Minute)}, "", "critical:optimization_decision_waiting"},
		{"running near expiry", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignRunning, ApprovedAt: at(now, -time.Hour), ApprovalExpiresAt: at(now, 5*time.Minute)}, "", "warning:optimization_approval_expiring"},
		{"failed, still recorded after cleanup", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignCleaned, FailureCode: "provider_capacity", UpdatedAt: now.Add(-time.Hour)}, "", "critical:optimization_failed"},
		{"old failure leaves the inbox", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignCleaned, FailureCode: "provider_capacity", UpdatedAt: now.Add(-48 * time.Hour)}, "", ""},
		{"finished with no failure", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignCleaned}, "", ""},
		{"cancelled", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignAwaitingApproval, CancelRequested: true}, "", ""},
		{"signed evaluation needed", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignRunning, Candidates: validating}, QualityWaitEvidence, "warning:optimization_quality_evidence_needed"},
		{"eval set needed", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignRunning, Candidates: validating}, QualityWaitEvalSet, "warning:optimization_eval_set_needed"},
		{"built-in check running", domain.OptimizationCampaign{ID: "r", State: optimizationcampaign.CampaignRunning, Candidates: validating}, QualityWaitNone, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codes(CampaignAttention(tc.campaign, now, tc.wait)); got != tc.want {
				t.Fatalf("attention=%s want %s", got, tc.want)
			}
		})
	}
}

func TestFailureReasonKeepsUnknownCodesVisible(t *testing.T) {
	if FailureReason("") != "reason unknown" || FailureReason("approval_expired") != "the spend approval ended before the run finished" || FailureReason("provider_capacity_unavailable") != "provider capacity unavailable" {
		t.Fatal("unexpected failure reasons")
	}
}
