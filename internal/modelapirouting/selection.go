package modelapirouting

import (
	"crypto/sha256"
	"encoding/binary"
)

// CandidatesForRequest returns one deterministic weighted candidate followed
// by the other exactly-compatible candidates in publication order. The tail
// is available to admission controls (for example an open circuit); request
// execution still makes at most one possibly billable supplier transmission.
func (l Lease) CandidatesForRequest(requestID, operation string) []Candidate {
	eligible := make([]Candidate, 0, len(l.Candidates))
	for _, candidate := range l.Candidates {
		if candidate.supports(operation) {
			eligible = append(eligible, candidate)
		}
	}
	if len(eligible) < 2 {
		return eligible
	}

	total := 0
	for _, candidate := range eligible {
		total += candidate.TrafficWeightBPS
	}
	selected := 0
	if total == 10_000 {
		digest := sha256.Sum256([]byte(l.Entitlement.CustomerTenantID + "\x00" + l.Entitlement.ProductID + "\x00" + l.Publication.SupplyPlanID + "\x00" + requestID))
		bucket := int(binary.BigEndian.Uint64(digest[:8]) % 10_000)
		cumulative := 0
		for index, candidate := range eligible {
			cumulative += candidate.TrafficWeightBPS
			if bucket < cumulative {
				selected = index
				break
			}
		}
	}
	if selected == 0 {
		return eligible
	}
	ordered := make([]Candidate, 0, len(eligible))
	ordered = append(ordered, eligible[selected])
	ordered = append(ordered, eligible[:selected]...)
	ordered = append(ordered, eligible[selected+1:]...)
	return ordered
}
