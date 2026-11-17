// Package notification tells a workspace, outside the dashboard, when an
// optimization run needs a person or reaches a result. Events are derived from
// persisted campaign state, queued once per destination, and sent through the
// signed webhook deliverer in internal/alert.
package notification

import (
	"sort"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// Subscribable campaign events.
const (
	EventApprovalNeeded        = "approval_needed"
	EventApprovalExpiring      = "approval_expiring"
	EventQualityEvidenceNeeded = "quality_evidence_needed"
	EventDecided               = "decided"
	EventFailed                = "failed"
	EventRolledOut             = "rolled_out"

	// EventTest is sent only on request and cannot be subscribed to.
	EventTest = "test"
)

// CampaignEvents is every subscribable event, in the order people read them.
var CampaignEvents = []string{EventApprovalNeeded, EventApprovalExpiring, EventQualityEvidenceNeeded, EventDecided, EventFailed, EventRolledOut}

// Decision outcomes carried by EventDecided. They follow the outcome report:
// winner_selected, baseline_kept, and no_winner. When the report cannot be
// read the outcome is unknown, never guessed from state.
const (
	OutcomeWinner        = "winner"
	OutcomeNoClearWinner = "no_clear_winner"
	OutcomeNothingPassed = "nothing_passed"
	OutcomeUnknown       = "unknown"
)

const (
	// Lookback bounds how old an event may be and still be sent. It equals the
	// longest approval window, so no live run falls outside it.
	Lookback = 24 * time.Hour
	// QualityEvidenceGrace is how long a setup may wait on the customer in
	// validation before the run says so. Evidence or an eval set provided
	// ahead of time is picked up on the run's next pass, so that case stays
	// quiet.
	QualityEvidenceGrace = 5 * time.Minute
	// MaxExpiryWarning caps how early the approval-expiring event fires.
	MaxExpiryWarning = time.Hour
	// RecentFailure is how long a failed run stays in the inbox.
	RecentFailure = 24 * time.Hour
)

// What a validating setup waits for from the customer. The built-in answer
// check needs no one, so a run that uses it never waits on the customer.
const (
	QualityWaitNone     = ""
	QualityWaitEvidence = "evidence"
	QualityWaitEvalSet  = "eval_set"
)

// Notice is one event a campaign has produced. ID is stable for the campaign
// and event, so queuing it again is a no-op.
type Notice struct {
	Type         string
	ID           string
	CampaignID   string
	OccurredAt   time.Time
	CandidateIDs []string
	// QualityWait is set on EventQualityEvidenceNeeded.
	QualityWait string
}

// QualityWait returns what the run's validating setups wait for from the
// customer: their signed evaluation when the run asked for customer
// evidence, or the eval set the run names when evalSetMissing says it is not
// uploaded. Pass false when the upload state is unknown, so an unread store
// never says a run needs someone.
func QualityWait(campaign domain.OptimizationCampaign, evalSetMissing bool) string {
	switch {
	case optimizationcampaign.QualityWaitsForCustomer(campaign):
		return QualityWaitEvidence
	case evalSetMissing && optimizationcampaign.QualityEvalSetDigest(campaign) != "":
		return QualityWaitEvalSet
	}
	return QualityWaitNone
}

// ExpiryWarning is how long before an approval ends the run warns about it:
// a quarter of the approved window, at most MaxExpiryWarning.
func ExpiryWarning(approvedAt, expiresAt time.Time) time.Duration {
	window := expiresAt.Sub(approvedAt) / 4
	if window > MaxExpiryWarning {
		return MaxExpiryWarning
	}
	if window < 0 {
		return 0
	}
	return window
}

// CampaignNotices returns every event the campaign has produced by now.
// Waiting states are read from the campaign itself, with qualityWait from
// QualityWait; decisions, failures, and rollouts are read from its recorded
// transitions, because cleanup rewrites the campaign state seconds after a
// decision. A cancelled run produces none.
func CampaignNotices(campaign domain.OptimizationCampaign, transitions []domain.OptimizationCampaignTransition, now time.Time, qualityWait string) []Notice {
	if campaign.CancelRequested || campaign.ID == "" {
		return nil
	}
	notice := func(kind string, at time.Time, candidates []string) Notice {
		return Notice{Type: kind, ID: campaign.ID + ":" + kind, CampaignID: campaign.ID, OccurredAt: at.UTC(), CandidateIDs: candidates}
	}
	notices := make([]Notice, 0, 2)
	if campaign.State == optimizationcampaign.CampaignAwaitingApproval {
		notices = append(notices, notice(EventApprovalNeeded, campaign.CreatedAt, nil))
	}
	if warnAt, ok := expiryWarningAt(campaign, now); ok {
		notices = append(notices, notice(EventApprovalExpiring, warnAt, waitingForRollout(campaign)))
	}
	if since, waiting := waitingOnQualityEvidence(campaign, qualityWait, now); len(waiting) > 0 {
		waitNotice := notice(EventQualityEvidenceNeeded, since.Add(QualityEvidenceGrace), waiting)
		waitNotice.QualityWait = qualityWait
		notices = append(notices, waitNotice)
	}
	ordered := append([]domain.OptimizationCampaignTransition(nil), transitions...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.Before(ordered[j].CreatedAt) })
	seen := map[string]bool{}
	for _, transition := range ordered {
		if transition.CampaignID != campaign.ID {
			continue
		}
		kind := ""
		switch transition.To {
		case optimizationcampaign.CampaignQualified, optimizationcampaign.CampaignGuardPassed, optimizationcampaign.CampaignRejected, optimizationcampaign.CampaignInconclusive:
			kind = EventDecided
		case optimizationcampaign.CampaignFailed:
			kind = EventFailed
		case optimizationcampaign.CampaignPromoted:
			kind = EventRolledOut
		}
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		notices = append(notices, notice(kind, transition.CreatedAt, nil))
	}
	return notices
}

// holdsAuthority reports the states in which an approval end changes what
// happens next: running work stops, and a waiting winner is discarded.
func holdsAuthority(state string) bool {
	switch state {
	case optimizationcampaign.CampaignApproved, optimizationcampaign.CampaignRunning, optimizationcampaign.CampaignRanked,
		optimizationcampaign.CampaignQualified, optimizationcampaign.CampaignGuardPassed:
		return true
	}
	return false
}

// AwaitingRollout reports a run whose winner waits for a person to roll it out.
func AwaitingRollout(state string) bool {
	return state == optimizationcampaign.CampaignQualified || state == optimizationcampaign.CampaignGuardPassed
}

func expiryWarningAt(campaign domain.OptimizationCampaign, now time.Time) (time.Time, bool) {
	if !holdsAuthority(campaign.State) || campaign.ApprovalExpiresAt == nil || campaign.ApprovedAt == nil {
		return time.Time{}, false
	}
	expires := campaign.ApprovalExpiresAt.UTC()
	warnAt := expires.Add(-ExpiryWarning(*campaign.ApprovedAt, expires))
	if now.Before(warnAt) || !now.Before(expires) {
		return time.Time{}, false
	}
	return warnAt, true
}

func waitingForRollout(campaign domain.OptimizationCampaign) []string {
	if !AwaitingRollout(campaign.State) {
		return nil
	}
	ids := []string{}
	for _, candidate := range campaign.Candidates {
		if candidate.State == optimizationcampaign.CandidateQualified || candidate.State == optimizationcampaign.CandidateGuardPassed {
			ids = append(ids, candidate.ID)
		}
	}
	return ids
}

// waitingOnQualityEvidence returns the setups that have waited on the
// customer in validation past the grace period, and since when the first of
// them has waited. Without a customer wait there are none.
func waitingOnQualityEvidence(campaign domain.OptimizationCampaign, qualityWait string, now time.Time) (time.Time, []string) {
	var since time.Time
	if qualityWait == QualityWaitNone {
		return since, nil
	}
	waiting := []string{}
	for _, candidate := range campaign.Candidates {
		if candidate.State != optimizationcampaign.CandidateValidating || candidate.QualityEvidenceID != "" || now.Sub(candidate.UpdatedAt) < QualityEvidenceGrace {
			continue
		}
		if since.IsZero() || candidate.UpdatedAt.Before(since) {
			since = candidate.UpdatedAt
		}
		waiting = append(waiting, candidate.ID)
	}
	return since, waiting
}
