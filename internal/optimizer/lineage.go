package optimizer

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Lineage states of the model artifact one proposal candidate serves,
// relative to the proposal's base model.
const (
	// LineageBase marks a candidate that serves the base model artifact.
	LineageBase = "base"
	// LineageExact marks a derived artifact, such as a quantized checkpoint,
	// whose recorded source is the base model's repository and immutable
	// revision.
	LineageExact = "exact"
	// LineagePublisherDeclared marks a derived artifact whose publisher names
	// the base model's repository but not the source commit. The source
	// revision stays unknown.
	LineagePublisherDeclared = "publisher_declared"
)

var (
	// ErrDifferentBaseModel means the candidates do not all serve one base
	// model or an artifact derived from it, so no measurement can compare
	// them.
	ErrDifferentBaseModel = errors.New("proposal candidates do not derive from one base model")
	// ErrDerivedControl means the starting point serves a derived artifact.
	// The control's answers are the reference for every candidate's answer
	// check, so it must serve the base model when a derived artifact competes.
	ErrDerivedControl = errors.New("the starting point must serve the base model when a derived artifact competes with it")
)

// CandidateArtifact is the exact model artifact one candidate serves.
type CandidateArtifact struct {
	// Identity is repository@revision, the identity measured evidence records.
	Identity string
	Lineage  string
}

// ArtifactLineage relates the artifact every candidate serves to the
// proposal's base model.
type ArtifactLineage struct {
	// Base is the base model's repository@revision.
	Base string
	// Candidates is keyed by proposal candidate ID.
	Candidates map[string]CandidateArtifact
}

// Derived returns the distinct identities of derived artifacts, sorted.
func (l ArtifactLineage) Derived() []string {
	seen := map[string]struct{}{}
	var derived []string
	for _, artifact := range l.Candidates {
		if _, found := seen[artifact.Identity]; found || artifact.Lineage == LineageBase {
			continue
		}
		seen[artifact.Identity] = struct{}{}
		derived = append(derived, artifact.Identity)
	}
	sort.Strings(derived)
	return derived
}

// ProposalLineage proves which base model a proposal's candidates share.
// Candidates that serve one artifact share it trivially. Otherwise every
// candidate without an artifact seed must serve one base model, and every
// other artifact must be a seed built from that base: the same repository,
// and for exact lineage the same immutable revision. A publisher that names
// only the base repository leaves the source revision unknown and is labelled
// LineagePublisherDeclared, never exact. Anything else is a different model.
//
// Lineage makes measurements comparable. It is not quality evidence: a
// derived artifact changes the model's numbers, and only its answer check
// against the base-model control decides whether its answers held.
func ProposalLineage(proposal Proposal) (ArtifactLineage, error) {
	if len(proposal.Candidates) == 0 {
		return ArtifactLineage{}, errors.New("proposal has no candidates")
	}
	identities := make(map[string]string, len(proposal.Candidates))
	single, baseModel, baseRevision := "", "", ""
	shared := true
	for _, candidate := range proposal.Candidates {
		model, revision := strings.TrimSpace(candidate.Deployment.Model.ID), strings.TrimSpace(candidate.Deployment.Model.Revision)
		if model == "" || revision == "" {
			return ArtifactLineage{}, fmt.Errorf("proposal candidate %s lacks an exact model artifact", candidate.ID)
		}
		identity := model + "@" + revision
		identities[candidate.ID] = identity
		if single == "" {
			single = identity
		}
		shared = shared && identity == single
		if candidate.ArtifactSeed != nil {
			continue
		}
		if baseModel != "" && identity != baseModel+"@"+baseRevision {
			return ArtifactLineage{}, fmt.Errorf("%w: candidates serve %s@%s and %s", ErrDifferentBaseModel, baseModel, baseRevision, identity)
		}
		baseModel, baseRevision = model, revision
	}
	lineage := ArtifactLineage{Candidates: make(map[string]CandidateArtifact, len(proposal.Candidates))}
	if shared {
		lineage.Base = single
		for _, candidate := range proposal.Candidates {
			lineage.Candidates[candidate.ID] = CandidateArtifact{Identity: single, Lineage: LineageBase}
		}
		return lineage, nil
	}
	if baseModel == "" {
		return ArtifactLineage{}, fmt.Errorf("%w: no candidate serves the unmodified base model", ErrDifferentBaseModel)
	}
	lineage.Base = baseModel + "@" + baseRevision
	for _, candidate := range proposal.Candidates {
		identity := identities[candidate.ID]
		if identity == lineage.Base {
			lineage.Candidates[candidate.ID] = CandidateArtifact{Identity: identity, Lineage: LineageBase}
			continue
		}
		state, err := seedLineage(*candidate.ArtifactSeed, baseModel, baseRevision)
		if err != nil {
			return ArtifactLineage{}, fmt.Errorf("%w: candidate %s serves %s: %v", ErrDifferentBaseModel, candidate.ID, identity, err)
		}
		lineage.Candidates[candidate.ID] = CandidateArtifact{Identity: identity, Lineage: state}
	}
	if proposal.StartingPoint != nil {
		if control, found := lineage.Candidates[proposal.StartingPoint.CandidateID]; found && control.Lineage != LineageBase {
			return ArtifactLineage{}, fmt.Errorf("%w: it serves %s, not %s", ErrDerivedControl, control.Identity, lineage.Base)
		}
	}
	return lineage, nil
}

func seedLineage(seed ArtifactSeed, baseModel, baseRevision string) (string, error) {
	source := strings.TrimSpace(seed.BaseRepository)
	if !strings.EqualFold(source, baseModel) {
		return "", fmt.Errorf("it was built from %s, not from %s", source, baseModel)
	}
	declared := strings.TrimSpace(seed.BaseRevision)
	switch seed.LineageState {
	case LineageExact:
		if !strings.EqualFold(declared, baseRevision) {
			return "", fmt.Errorf("it was built from revision %s, not from %s", declared, baseRevision)
		}
		return LineageExact, nil
	case LineagePublisherDeclared:
		if declared != "" && !strings.EqualFold(declared, baseRevision) {
			return "", fmt.Errorf("its publisher declares revision %s, not %s", declared, baseRevision)
		}
		return LineagePublisherDeclared, nil
	default:
		return "", fmt.Errorf("its lineage state %q is unsupported", seed.LineageState)
	}
}
