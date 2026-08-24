// Package capacity defines provider-neutral placement and lifecycle contracts.
package capacity

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

type Requirement struct {
	GPU, Region, Model, Objective string
	Count                         int
	Warm                          bool
	MaxReadyP95Seconds            *float64
}
type CacheEvidence struct {
	State     string
	Source    string
	Samples   int
	ExpiresAt time.Time
}
type Candidate struct {
	Provider, Region, GPU, Pool string
	Available                   int
	WarmModels                  map[string]bool
	CacheObservations           map[string]CacheEvidence
	HourlyCost, SuccessRate     *float64
	ReadyP95Seconds             *float64
}
type Placement struct {
	Candidate Candidate
	Count     int
	Reason    string
	Evidence  []string
}

func Choose(requirement Requirement, candidates []Candidate) (Placement, error) {
	if requirement.Count < 1 || requirement.GPU == "" {
		return Placement{}, errors.New("GPU and positive count are required")
	}
	requirement.Objective = strings.ToLower(strings.TrimSpace(requirement.Objective))
	if requirement.Objective == "" {
		requirement.Objective = "readiness"
	}
	if requirement.Objective != "readiness" && requirement.Objective != "reliability" && requirement.Objective != "cost" {
		return Placement{}, errors.New("capacity objective must be readiness, reliability, or cost")
	}
	now := time.Now().UTC()
	eligible := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.GPU != requirement.GPU || c.Available < requirement.Count || (requirement.Region != "" && c.Region != requirement.Region) {
			continue
		}
		if requirement.Warm && !warm(c, requirement.Model, now) {
			continue
		}
		if requirement.MaxReadyP95Seconds != nil && (c.ReadyP95Seconds == nil || *c.ReadyP95Seconds > *requirement.MaxReadyP95Seconds) {
			continue
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		return Placement{}, errors.New("no eligible capacity")
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		iWarm, jWarm := warm(eligible[i], requirement.Model, now), warm(eligible[j], requirement.Model, now)
		if iWarm != jWarm {
			return iWarm
		}
		compareReliability := func() int { return compareHigher(eligible[i].SuccessRate, eligible[j].SuccessRate) }
		compareReadiness := func() int { return compareLower(eligible[i].ReadyP95Seconds, eligible[j].ReadyP95Seconds) }
		compareCost := func() int { return compareLower(eligible[i].HourlyCost, eligible[j].HourlyCost) }
		comparisons := []func() int{compareReadiness, compareReliability, compareCost}
		if requirement.Objective == "reliability" {
			comparisons = []func() int{compareReliability, compareReadiness, compareCost}
		} else if requirement.Objective == "cost" {
			comparisons = []func() int{compareCost, compareReliability, compareReadiness}
		}
		for _, compare := range comparisons {
			if result := compare(); result != 0 {
				return result < 0
			}
		}
		left := eligible[i].Provider + "\x00" + eligible[i].Region + "\x00" + eligible[i].Pool
		right := eligible[j].Provider + "\x00" + eligible[j].Region + "\x00" + eligible[j].Pool
		return left < right
	})
	chosen := eligible[0]
	reason := "eligible capacity selected deterministically for " + requirement.Objective
	evidence := []string{}
	if warm(chosen, requirement.Model, now) {
		reason = "fresh model cache preferred, then ranked for " + requirement.Objective
		if observation, ok := chosen.CacheObservations[requirement.Model]; ok && observation.State == "present" && observation.ExpiresAt.After(now) {
			evidence = append(evidence, "cache:"+observation.Source)
		} else {
			evidence = append(evidence, "cache:legacy-inventory")
		}
	}
	if chosen.ReadyP95Seconds != nil {
		evidence = append(evidence, "readiness:p95-observed")
	}
	if chosen.SuccessRate != nil {
		evidence = append(evidence, "capacity:success-rate-observed")
	}
	if chosen.HourlyCost != nil {
		evidence = append(evidence, "cost:sourced")
	}
	return Placement{Candidate: chosen, Count: requirement.Count, Reason: reason, Evidence: evidence}, nil
}

func warm(candidate Candidate, model string, now time.Time) bool {
	if observation, ok := candidate.CacheObservations[model]; ok {
		return observation.State == "present" && observation.Samples > 0 && observation.ExpiresAt.After(now)
	}
	return candidate.WarmModels[model]
}

func compareLower(left, right *float64) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return 1
	}
	if right == nil {
		return -1
	}
	if *left < *right {
		return -1
	}
	if *left > *right {
		return 1
	}
	return 0
}

func compareHigher(left, right *float64) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return 1
	}
	if right == nil {
		return -1
	}
	if *left > *right {
		return -1
	}
	if *left < *right {
		return 1
	}
	return 0
}

type Resource struct{ ID, Endpoint, Provider string }
type Provider interface {
	Provision(context.Context, Requirement) ([]Resource, error)
	Resize(context.Context, string, int) error
	Destroy(context.Context, string) error
	Inventory(context.Context) ([]Resource, error)
}
type Runtime interface {
	Start(context.Context, Resource, string, []string) error
	Ready(context.Context, Resource, string) error
	Stop(context.Context, Resource) error
}
