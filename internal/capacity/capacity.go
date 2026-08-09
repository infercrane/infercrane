// Package capacity defines provider-neutral placement and lifecycle contracts.
package capacity

import (
	"context"
	"errors"
	"sort"
)

type Requirement struct {
	GPU, Region, Model string
	Count              int
	Warm               bool
}
type Candidate struct {
	Provider, Region, GPU, Pool string
	Available                   int
	WarmModels                  map[string]bool
	HourlyCost                  *float64
}
type Placement struct {
	Candidate Candidate
	Count     int
	Reason    string
}

func Choose(requirement Requirement, candidates []Candidate) (Placement, error) {
	if requirement.Count < 1 || requirement.GPU == "" {
		return Placement{}, errors.New("GPU and positive count are required")
	}
	eligible := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.GPU == requirement.GPU && c.Available >= requirement.Count && (requirement.Region == "" || c.Region == requirement.Region) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return Placement{}, errors.New("no eligible capacity")
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		iWarm, jWarm := eligible[i].WarmModels[requirement.Model], eligible[j].WarmModels[requirement.Model]
		if iWarm != jWarm {
			return iWarm
		}
		if eligible[i].HourlyCost == nil {
			return false
		}
		if eligible[j].HourlyCost == nil {
			return true
		}
		return *eligible[i].HourlyCost < *eligible[j].HourlyCost
	})
	chosen := eligible[0]
	reason := "eligible capacity selected deterministically"
	if chosen.WarmModels[requirement.Model] {
		reason = "warm model cache preferred"
	}
	return Placement{Candidate: chosen, Count: requirement.Count, Reason: reason}, nil
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
