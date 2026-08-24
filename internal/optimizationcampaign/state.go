// Package optimizationcampaign owns legal durable optimization transitions.
// It intentionally does not own provisioning, benchmarking, or promotion.
package optimizationcampaign

import (
	"errors"
	"fmt"
)

const (
	CampaignAwaitingApproval = "awaiting_approval"
	CampaignApproved         = "approved"
	CampaignRunning          = "running"
	CampaignRanked           = "ranked"
	CampaignGuardPassed      = "guard_passed"
	CampaignRejected         = "rejected"
	CampaignInconclusive     = "inconclusive"
	CampaignPromoted         = "promoted"
	CampaignObserved         = "observed"
	CampaignCancelled        = "cancelled"
	CampaignFailed           = "failed"
	CampaignCleaned          = "cleaned"

	CandidateProposed     = "proposed"
	CandidateProvisioning = "provisioning"
	CandidateReady        = "ready"
	CandidateMeasuring    = "measuring"
	CandidateValidating   = "validating"
	CandidateRanked       = "ranked"
	CandidateGuarding     = "guarding"
	CandidateGuardPassed  = "guard_passed"
	CandidateRejected     = "rejected"
	CandidateInconclusive = "inconclusive"
	CandidatePromoted     = "promoted"
	CandidateObserved     = "observed"
	CandidateFailed       = "failed"
	CandidateCancelled    = "cancelled"
	CandidateCleaned      = "cleaned"
)

var candidateTransitions = map[string]map[string]struct{}{
	CandidateProposed:     set(CandidateProvisioning, CandidateRejected, CandidateCancelled, CandidateFailed),
	CandidateProvisioning: set(CandidateReady, CandidateRejected, CandidateCancelled, CandidateFailed),
	CandidateReady:        set(CandidateMeasuring, CandidateRejected, CandidateCancelled, CandidateFailed),
	CandidateMeasuring:    set(CandidateValidating, CandidateInconclusive, CandidateRejected, CandidateCancelled, CandidateFailed),
	CandidateValidating:   set(CandidateRanked, CandidateRejected, CandidateInconclusive, CandidateCancelled, CandidateFailed),
	CandidateRanked:       set(CandidateGuarding, CandidateRejected, CandidateInconclusive, CandidateCancelled, CandidateFailed),
	CandidateGuarding:     set(CandidateGuardPassed, CandidateRejected, CandidateInconclusive, CandidateCancelled, CandidateFailed),
	CandidateGuardPassed:  set(CandidatePromoted, CandidateRejected, CandidateCancelled, CandidateFailed),
	CandidatePromoted:     set(CandidateObserved, CandidateFailed),
	CandidateObserved:     set(CandidateCleaned),
	CandidateRejected:     set(CandidateCleaned),
	CandidateInconclusive: set(CandidateCleaned),
	CandidateFailed:       set(CandidateCleaned),
	CandidateCancelled:    set(CandidateCleaned),
	CandidateCleaned:      {},
}

func ValidateCandidateTransition(from, to string) error {
	if from == to {
		return nil
	}
	allowed, ok := candidateTransitions[from]
	if !ok {
		return fmt.Errorf("unknown candidate state %q", from)
	}
	if _, ok = allowed[to]; !ok {
		return fmt.Errorf("illegal optimization candidate transition %s -> %s", from, to)
	}
	return nil
}

func EvidenceForState(state string) (string, error) {
	switch state {
	case CandidateProposed, CandidateProvisioning, CandidateReady:
		return "unmeasured", nil
	case CandidateMeasuring:
		return "unmeasured", nil
	case CandidateValidating, CandidateRanked, CandidateGuarding:
		return "measured", nil
	case CandidateGuardPassed, CandidatePromoted, CandidateObserved:
		return "qualified", nil
	case CandidateRejected:
		return "rejected", nil
	case CandidateInconclusive, CandidateFailed, CandidateCancelled:
		return "stale", nil
	case CandidateCleaned:
		return "stale", nil
	default:
		return "", errors.New("unknown candidate state")
	}
}

func TerminalCandidate(state string) bool {
	return state == CandidateRejected || state == CandidateInconclusive || state == CandidateFailed || state == CandidateCancelled || state == CandidateCleaned || state == CandidateObserved
}

// AggregateState derives the campaign summary from candidate truth. It is
// deliberately deterministic so API, CLI, and workers cannot invent a second
// workflow state machine. A mixed campaign remains running until all remaining
// candidates reach a meaningful proof or terminal boundary.
func AggregateState(states []string) (string, error) {
	if len(states) == 0 {
		return "", errors.New("optimization campaign requires candidate state")
	}
	counts := map[string]int{}
	allTerminal := true
	for _, state := range states {
		if _, known := candidateTransitions[state]; !known {
			return "", fmt.Errorf("unknown candidate state %q", state)
		}
		counts[state]++
		allTerminal = allTerminal && TerminalCandidate(state)
	}
	if counts[CandidatePromoted] > 0 {
		return CampaignPromoted, nil
	}
	if counts[CandidateObserved] > 0 {
		return CampaignObserved, nil
	}
	if counts[CandidateGuardPassed] > 0 {
		return CampaignGuardPassed, nil
	}
	if counts[CandidateRanked] > 0 || counts[CandidateGuarding] > 0 {
		return CampaignRanked, nil
	}
	if counts[CandidateCleaned] == len(states) {
		return CampaignCleaned, nil
	}
	if !allTerminal {
		return CampaignRunning, nil
	}
	if counts[CandidateFailed] > 0 {
		return CampaignFailed, nil
	}
	if counts[CandidateInconclusive] > 0 {
		return CampaignInconclusive, nil
	}
	if counts[CandidateCancelled] == len(states) {
		return CampaignCancelled, nil
	}
	return CampaignRejected, nil
}

func set(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
