package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/infercrane/infercrane/internal/spec"
)

// ComparisonAlgorithmVersion marks a proposal that measures one proposed
// setup against the current one, as a merge gate does. Nothing is planned or
// added: the run measures exactly the two setups it was given.
const ComparisonAlgorithmVersion = "setup-comparison-v1"

// Comparison is a proof of one change: the setup that serves today, the
// setup a change proposes, and how to measure them.
type Comparison struct {
	Current, Proposed   spec.Deployment
	Objective           string
	WorkloadProfile     string
	WorkloadFingerprint string
	QualityCheck        string
	QualityEvalSet      string
}

// CompareSetups builds the proposal of a two-setup run: the current setup is
// the run's control and the proposed setup is the one change it measures.
// Both must serve the same model artifact on the same accelerator, count,
// provider, and region, so the run compares them in turns on one kind of
// machine and the change is the only difference it measures.
func CompareSetups(comparison Comparison) (Proposal, error) {
	current, proposed := DraftFromSpec(comparison.Current), DraftFromSpec(comparison.Proposed)
	if err := sameMachine(current, proposed); err != nil {
		return Proposal{}, err
	}
	if current.Model.ID != proposed.Model.ID || current.Model.Revision != proposed.Model.Revision {
		return Proposal{}, fmt.Errorf("the proposed setup serves %s@%s and the current one %s@%s: a two-setup run compares setups of one model artifact; prove a model change with a migration Proof or your own eval", proposed.Model.ID, proposed.Model.Revision, current.Model.ID, current.Model.Revision)
	}
	if !immutableModelRevision(strings.ToLower(current.Model.Revision)) {
		return Proposal{}, fmt.Errorf("model.revision %q must be an immutable 40 to 64 character commit: a branch name can serve different weights to the two setups", current.Model.Revision)
	}
	if reflect.DeepEqual(measuredPart(current), measuredPart(proposed)) {
		return Proposal{}, errors.New("the proposed setup changes nothing a run measures: model, engine, settings, accelerator, and scaling are the same")
	}
	request := Request{
		ModelIdentity: current.Model.ID, ModelRevision: current.Model.Revision,
		Provider: current.Provider.Cloud, Region: current.Provider.Region, GPU: current.Resources.GPU, GPUCount: current.Resources.GPUCount,
		Runtimes:  []string{current.Runtime.Engine, proposed.Runtime.Engine},
		Objective: comparison.Objective, WorkloadProfile: comparison.WorkloadProfile, WorkloadFingerprint: comparison.WorkloadFingerprint,
		MaxCandidates: 2, QualityCheck: comparison.QualityCheck, QualityEvalSet: comparison.QualityEvalSet,
		ComputeConnectionID: current.ComputeConnectionID, ModelSecretReferenceID: current.ModelSecretReferenceID, ModelAdapters: current.Model.Adapters,
	}
	request = normalizeRequest(request)
	if err := validateRequest(request); err != nil {
		return Proposal{}, err
	}
	if err := ValidateMeasurable(request); err != nil {
		return Proposal{}, err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return Proposal{}, err
	}
	sum := sha256.Sum256(input)
	proposal := Proposal{
		SchemaVersion: SchemaVersion, AlgorithmVersion: ComparisonAlgorithmVersion, Input: request, InputDigest: hex.EncodeToString(sum[:]), Mutation: "none",
		SelectionBoundary: "the current setup and one proposed change only; the run measures both in turns on one kind of machine and checks the proposed setup's answers against the current one's",
	}
	applyWorkloadBoundary(&proposal)
	applyObjectiveMetric(&proposal)
	proposal.Candidates = []Candidate{
		comparisonCandidate(current, "current-setup", 1, request),
		comparisonCandidate(proposed, "proposed-change", 2, request),
	}
	proposal.StartingPoint = &StartingPoint{
		CandidateID: proposal.Candidates[0].ID, EvidenceState: EvidenceUnmeasured,
		Basis:         "the setup that serves today, named by the caller as the baseline of this comparison",
		ClaimBoundary: "a baseline to measure the proposed change against; neither setup is qualified until measured",
	}
	if err = ValidateProposal(proposal); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

// DraftFromSpec carries a validated DeploymentSpec into a proposal setup.
func DraftFromSpec(source spec.Deployment) DeploymentDraft {
	draft := DeploymentDraft{APIVersion: source.APIVersion, Kind: source.Kind, Name: source.Name, ModelSecretReferenceID: source.Model.SecretReferenceID}
	draft.Model.ID, draft.Model.Revision, draft.Model.Adapters = source.Model.ID, source.Model.Revision, source.Model.Adapters
	draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args = source.Runtime.Engine, source.Runtime.Version, append([]string{}, source.Runtime.Args...)
	if len(source.Runtime.Env) > 0 {
		draft.Runtime.Env = source.Runtime.Env
	}
	draft.Runtime.Workload = source.Runtime.Workload
	draft.Compute.Mode = source.Compute.Mode
	draft.Resources.GPU, draft.Resources.GPUCount = source.Resources.GPU, source.Resources.GPUCount
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = source.Provider.Cloud, source.Provider.Adapter, source.Provider.Region
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = source.Scaling.MinReplicas, source.Scaling.MaxReplicas
	draft.Routing.Strategy = source.Routing.Strategy
	draft.Serving = source.Serving
	return draft
}

func sameMachine(current, proposed DeploymentDraft) error {
	var changed []string
	if current.Resources.GPU != proposed.Resources.GPU {
		changed = append(changed, "resources.gpu")
	}
	if current.Resources.GPUCount != proposed.Resources.GPUCount {
		changed = append(changed, "resources.gpu_count")
	}
	if current.Provider != proposed.Provider {
		changed = append(changed, "provider")
	}
	if current.ComputeConnectionID != proposed.ComputeConnectionID {
		changed = append(changed, "compute connection")
	}
	if len(changed) > 0 {
		return fmt.Errorf("the proposed setup changes %s: a two-setup run measures both setups on one kind of machine, so it cannot prove a hardware or provider change; measure each setup with optimize create instead", strings.Join(changed, ", "))
	}
	return nil
}

// measuredPart is the setup without its name, which a run never measures.
func measuredPart(draft DeploymentDraft) DeploymentDraft {
	draft.Name = ""
	return draft
}

func comparisonCandidate(draft DeploymentDraft, profile string, rank int, request Request) Candidate {
	source := SourceInfo{Name: "infercrane-setup-comparison", Version: ComparisonAlgorithmVersion, EvidenceClass: "configuration-unverified"}
	identity, _ := json.Marshal(struct {
		Source     SourceInfo      `json:"source"`
		Deployment DeploymentDraft `json:"deployment"`
		Profile    string          `json:"profile"`
		Role       string          `json:"role"`
	}{source, draft, request.WorkloadProfile, profile})
	sum := sha256.Sum256(identity)
	required := []string{"runtime readiness and exact served-model identity", "AIPerf " + request.WorkloadProfile + " workload measured in turns with the other setup", "semantic quality evidence"}
	limitations := []string{"Both setups come from the caller's DeploymentSpecs; InferCrane has not inferred GPU memory fit, license, or runtime compatibility."}
	required, limitations = requireCustomerReplay(request, required, limitations)
	return Candidate{
		ID: hex.EncodeToString(sum[:]), Rank: rank, Status: "proposed-unmeasured", EvidenceState: EvidenceUnmeasured,
		Objective: request.Objective, ConfigurationProfile: profile, BenchmarkProfile: request.WorkloadProfile,
		CompatibilityState: "operator-supplied", Source: source,
		Features:         []Feature{{Name: "immutable_model_revision", State: "pinned", Source: draft.Model.Revision}},
		RequiredEvidence: required, Limitations: limitations, Deployment: draft,
	}
}
