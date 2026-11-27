package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/workflows"
)

const (
	// MaxMachineReplacements bounds how many lost machines of one setup are
	// replaced during a run. The next loss stops the run.
	MaxMachineReplacements = 2
	// replacementBillingReserve is what each replacement adds to a setup's
	// worst-case spend. A replacement starts only after the lost machine is
	// gone, so machines of one setup never bill at the same time and the
	// approved window already covers their running time. What the window
	// does not cover is a provider's minimum charge for starting a machine.
	// InferCrane has no sourced minimum billing period for any provider, so
	// it reserves one full hour, the unit of every quoted rate.
	replacementBillingReserve = time.Hour
	// machineReplacementMarkerLimit bounds a setup's replacement list,
	// stop markers included.
	machineReplacementMarkerLimit = 128

	FailureMachineLostRepeatedly    = "optimization_machine_lost_repeatedly"
	FailureReplacementNotCovered    = "optimization_replacement_not_covered"
	FailureMachineReplacementFailed = "optimization_machine_replacement_failed"
	FailureMeasurementInterrupted   = "optimization_measurement_interrupted_repeatedly"
	FailureControlLost              = "optimization_control_lost"

	RecoveryMachineServing  = "machine_serving"
	RecoveryMachineReplaced = "machine_replaced"
	RecoveryProviderManaged = "provider_managed"

	ReplacementProvisioning = "provisioning"
	ReplacementReplaced     = "replaced"
	ReplacementFailed       = "failed"
	ReplacementGivenUp      = "given_up"

	stageRemoving  = "removing"
	stageReplacing = "replacing"
)

// MachineReplacement is one entry of a setup's machine_replacements: a lost
// machine and what was done about it. A given_up entry is a stop marker: the
// run stopped because this setup, as the control, could not be recovered.
type MachineReplacement struct {
	LostReplicaID       string `json:"lost_replica_id"`
	Number              int    `json:"number,omitempty"`
	State               string `json:"state"`
	MeasuredCandidateID string `json:"measured_candidate_id"`
	// Purpose is "confirmation" for a server retired so a gain is measured
	// again on a fresh one; empty for a lost machine.
	Purpose     string    `json:"purpose,omitempty"`
	OperationID string    `json:"operation_id,omitempty"`
	FailureCode string    `json:"failure_code,omitempty"`
	At          time.Time `json:"at"`
}

// MachineReplacementRecorder keeps a setup's replacement list. The store
// refuses a new entry once limit entries other than stop markers exist.
type MachineReplacementRecorder interface {
	RecordOptimizationMachineReplacement(context.Context, string, string, string, string, int) (string, error)
}

// DecodeMachineReplacements reads a setup's replacement list; malformed
// storage reads as empty.
func DecodeMachineReplacements(raw string) []MachineReplacement {
	var entries []MachineReplacement
	if json.Unmarshal([]byte(raw), &entries) != nil {
		return nil
	}
	return entries
}

// lostMachineReplacements counts replacements of lost machines, the ones
// MaxMachineReplacements bounds; retirements for a confirmation are not.
func lostMachineReplacements(entries []MachineReplacement) int {
	used := 0
	for _, entry := range entries {
		if entry.State != ReplacementGivenUp && entry.Purpose != ReplacementPurposeConfirmation {
			used++
		}
	}
	return used
}

// MachineReplacementsUsed counts replacements started for a setup, lost
// machines and confirmations alike; stop markers are not replacements. It
// numbers the next replacement.
func MachineReplacementsUsed(entries []MachineReplacement) int {
	used := 0
	for _, entry := range entries {
		if entry.State != ReplacementGivenUp {
			used++
		}
	}
	return used
}

// ControlLost reports whether a run stopped because its control's machine
// could not be recovered. No later setup can be compared, so none is started.
func ControlLost(campaign domain.OptimizationCampaign) bool {
	control, found := CampaignControl(campaign)
	if !found {
		return false
	}
	for _, entry := range DecodeMachineReplacements(control.MachineReplacementsJSON) {
		if entry.State == ReplacementGivenUp {
			return true
		}
	}
	return false
}

func stoppedForLostControl(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) error {
	if control, found := CampaignControl(campaign); found && control.ID != candidate.ID && ControlLost(campaign) {
		return operations.Permanent(FailureControlLost, errors.New("the run's control machine was lost and could not be recovered, so this setup cannot be compared"))
	}
	return nil
}

// ReplacementWorstCase is the most a setup can be billed after its
// replacements-th replacement: its machines billing one after another from
// approval until the approval ends, plus one billing reserve per replacement.
func ReplacementWorstCase(quote CostQuote, since, expires time.Time, replacements int) float64 {
	worst := quote.HourlyUSD * (expires.Sub(since).Hours() + float64(replacements)*replacementBillingReserve.Hours())
	if quote.HardMaximumCostUSD > 0 && quote.HardMaximumCostUSD < worst {
		worst = quote.HardMaximumCostUSD
	}
	return worst
}

// ReplacementsCovered is how many lost machines of a setup its share of the
// approved cap can pay to replace, at most MaxMachineReplacements.
func ReplacementsCovered(quote CostQuote, since, expires time.Time, share float64) int {
	for replacements := MaxMachineReplacements; replacements > 0; replacements-- {
		if ReplacementWorstCase(quote, since, expires, replacements) <= share {
			return replacements
		}
	}
	return 0
}

// AuthorizeCostCoverage is AuthorizeCost that also reports how many lost
// machines the candidate's share covers, counted from now.
func AuthorizeCostCoverage(ctx context.Context, costs CostAuthority, tenant string, draft optimizer.DeploymentDraft, budget Budget, now time.Time) (int, error) {
	quote, err := authorizeQuote(ctx, costs, tenant, draft, budget, now)
	if err != nil {
		return 0, err
	}
	return ReplacementsCovered(quote, now.UTC(), budget.ExpiresAt, budget.MaxCostUSD), nil
}

// authorizeReplacement re-authorizes a setup's spend immediately before a
// replacement machine is requested, with a fresh sourced quote. Its share
// must cover the setup billing from approval until expiry plus the reserve
// for this and every earlier replacement, at the higher of the fresh rate and
// the rate its first servers were bound to.
func (d CompositeDriver) authorizeReplacement(ctx context.Context, campaign domain.OptimizationCampaign, owner domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft, budget Budget, number int) error {
	quote, err := authorizeQuote(ctx, d.Costs, owner.TenantID, draft, budget, d.now())
	if err != nil {
		return err
	}
	// The setup's earlier servers billed at the rate that first authorized
	// them. A cheaper fresh quote must not make that time look cheaper.
	if bound, ok := decodeCostQuote(owner.CostQuoteJSON); ok && bound.HourlyUSD > quote.HourlyUSD {
		quote.HourlyUSD = bound.HourlyUSD
	}
	since := owner.CreatedAt
	if campaign.ApprovedAt != nil {
		since = campaign.ApprovedAt.UTC()
	}
	if worst := ReplacementWorstCase(quote, since, budget.ExpiresAt, number); worst > budget.MaxCostUSD {
		return operations.Permanent(FailureReplacementNotCovered, fmt.Errorf("replacing machine %d of this setup needs a worst case of $%.2f; its share of the approved cap is $%.2f", number, worst, budget.MaxCostUSD))
	}
	return nil
}

// recoverInterruption resolves the measured candidate's pending
// interruption. It asks the provider whether the machine is lost, replaces a
// lost one through the setup's own provisioning path, and records every step
// in the checkpoint, so each retry continues where the last one stopped. It
// returns a retryable error while a child operation runs and a permanent one
// when the run must stop. The caller holds the updated checkpoint.
func (d CompositeDriver) recoverInterruption(ctx context.Context, campaign domain.OptimizationCampaign, measured domain.OptimizationCandidateRun, checkpoint *MeasurementCheckpoint, budget Budget) error {
	sink, sinkOK := d.Store.(MeasurementCheckpointSink)
	recorder, recorderOK := d.Store.(MachineReplacementRecorder)
	pending := checkpoint.Pending()
	if pending == nil {
		return nil
	}
	if !sinkOK || !recorderOK {
		return operations.Permanent("optimization_recovery_unavailable", errors.New("measurement recovery storage is not configured"))
	}
	save := func() error {
		encoded, err := json.Marshal(checkpoint)
		if err != nil {
			return operations.Permanent("optimization_checkpoint_invalid", err)
		}
		written, err := sink.RecordOptimizationMeasurementCheckpoint(ctx, measured.TenantID, measured.CampaignID, measured.ID, string(encoded))
		if err != nil {
			return operations.Retryable("optimization_checkpoint_failed", err)
		}
		if !written {
			return operations.Retryable("optimization_checkpoint_fenced", errors.New("candidate is no longer measuring"))
		}
		return nil
	}
	owner, found := candidateByID(campaign.Candidates, pending.OwnerCandidateID)
	if !found {
		return operations.Permanent("optimization_recovery_invalid", errors.New("interrupted machine belongs to no candidate of this run"))
	}
	// Giving up on the run's control after its server was confirmed lost
	// stops the whole run: the marker on the control makes every later setup
	// fail before it spends. Giving up for any other reason stops only the
	// measured setup.
	control, hasControl := CampaignControl(campaign)
	giveUp := func(code string, cause error, confirmedLoss bool) error {
		pending.GaveUp, pending.Stage = code, ""
		if confirmedLoss && hasControl && control.ID == owner.ID {
			marker, _ := json.Marshal(MachineReplacement{LostReplicaID: "stopped:" + measured.ID, State: ReplacementGivenUp, MeasuredCandidateID: measured.ID, FailureCode: code, At: d.now()})
			if _, err := recorder.RecordOptimizationMachineReplacement(ctx, owner.TenantID, owner.CampaignID, owner.ID, string(marker), machineReplacementMarkerLimit); err != nil {
				return operations.Retryable("optimization_recovery_record_failed", err)
			}
			if owner.ID != measured.ID {
				code = FailureControlLost
			}
		}
		if err := save(); err != nil {
			return err
		}
		return operations.Permanent(code, cause)
	}
	// A confirmation retires a serving machine on purpose; it is neither a
	// loss nor an interruption the limits below count.
	confirming := pending.Kind == RunKindConfirmation
	resolve := func(recovery string) error {
		resolved := d.now()
		pending.Recovery, pending.Stage, pending.ResolvedAt = recovery, "", &resolved
		if confirming {
			advanceConfirmation(checkpoint, pending.Arm)
		}
		return save()
	}
	if !confirming && lostInterruptions(checkpoint) > maxMeasurementInterruptions {
		return giveUp(FailureMeasurementInterrupted, fmt.Errorf("the measurement stopped %d times; the limit is %d", lostInterruptions(checkpoint), maxMeasurementInterruptions), false)
	}
	draft, err := candidateDraft(owner)
	if err != nil {
		return operations.Permanent("optimization_candidate_spec_invalid", err)
	}
	if draft.Compute.Mode == "serverless" || pending.ReplicaID == "" {
		return resolve(RecoveryProviderManaged)
	}
	if owner.DeploymentName == "" || owner.RevisionID == "" {
		return operations.Permanent("optimization_recovery_invalid", errors.New("interrupted machine's setup has no deployment"))
	}
	if confirming && pending.Stage == "" {
		// Retire nothing the approved cap cannot replace.
		if err = d.authorizeReplacement(ctx, campaign, owner, draft, budget, MachineReplacementsUsed(DecodeMachineReplacements(owner.MachineReplacementsJSON))+1); err != nil {
			var failure operations.Failure
			if errors.As(err, &failure) && !failure.Retryable {
				return giveUp(FailureConfirmationNotCovered, err, false)
			}
			return err
		}
		// The setup's first servers answer the task set before the
		// candidate's is retired, so the verdict can pool both pairs.
		if pending.Arm == ArmCandidate {
			if err = d.askFirstAnswers(ctx, campaign, measured, checkpoint, save); err != nil {
				return err
			}
		}
	}
	if pending.Stage == "" || pending.Stage == stageRemoving {
		key := fmt.Sprintf("remove-lost-%s-%d", shortID(measured.ID), pending.Sequence)
		if confirming {
			key = fmt.Sprintf("retire-%s-%d", shortID(measured.ID), pending.Sequence)
		}
		request, _ := json.Marshal(workflows.RemoveLostReplicaRequest{TenantID: owner.TenantID, Name: owner.DeploymentName, ReplicaID: pending.ReplicaID, Actor: "optimization-campaign", Retire: confirming})
		removal, _, enqueueErr := d.Store.EnqueueOperation(ctx, domain.Operation{TenantID: owner.TenantID, Kind: workflows.ReplicaRemoveLostKind, ResourceType: "deployment", ResourceName: owner.DeploymentName, IdempotencyKey: childKey(owner.ID, key), RequestJSON: string(request), MaxAttempts: 120})
		if enqueueErr != nil {
			return executionFailure(enqueueErr)
		}
		if pending.Stage != stageRemoving || pending.RemovalOperationID != removal.ID {
			pending.Stage, pending.RemovalOperationID = stageRemoving, removal.ID
			if err = save(); err != nil {
				return err
			}
		}
		if err = childComplete(removal, "lost machine check"); err != nil {
			var failure operations.Failure
			if errors.As(err, &failure) && !failure.Retryable {
				return giveUp(FailureMachineReplacementFailed, err, false)
			}
			return err
		}
		var removed workflows.RemoveLostReplicaResult
		if json.Unmarshal([]byte(removal.ResultJSON), &removed) != nil || removed.ReplicaID != pending.ReplicaID {
			return giveUp(FailureMachineReplacementFailed, errors.New("lost machine check returned no result for the interrupted machine"), false)
		}
		if !removed.Lost && !removed.Retired {
			return resolve(RecoveryMachineServing)
		}
		pending.Stage = stageReplacing
		if err = save(); err != nil {
			return err
		}
	}
	entries := DecodeMachineReplacements(owner.MachineReplacementsJSON)
	var entry *MachineReplacement
	for index := range entries {
		if entries[index].LostReplicaID == pending.ReplicaID {
			entry = &entries[index]
		}
	}
	if entry == nil {
		number := MachineReplacementsUsed(entries) + 1
		if !confirming {
			if lost := lostMachineReplacements(entries); lost >= MaxMachineReplacements {
				return giveUp(FailureMachineLostRepeatedly, fmt.Errorf("this setup already replaced %d lost machines; the limit is %d", lost, MaxMachineReplacements), true)
			}
			if err = d.authorizeReplacement(ctx, campaign, owner, draft, budget, number); err != nil {
				var failure operations.Failure
				if errors.As(err, &failure) && !failure.Retryable {
					return giveUp(FailureReplacementNotCovered, err, true)
				}
				return err
			}
		}
		entry = &MachineReplacement{LostReplicaID: pending.ReplicaID, Number: number, State: ReplacementProvisioning, MeasuredCandidateID: measured.ID, At: d.now()}
		if confirming {
			entry.Purpose = ReplacementPurposeConfirmation
		}
		if err = d.recordReplacement(ctx, recorder, owner, *entry); errors.Is(err, domain.ErrConflict) {
			return giveUp(FailureMachineLostRepeatedly, fmt.Errorf("this setup reached its limit of %d replaced machines", MaxMachineReplacements), true)
		} else if err != nil {
			return err
		}
	}
	if entry.State == ReplacementFailed {
		return giveUp(FailureMachineReplacementFailed, errors.New("the replacement machine could not be provisioned"), true)
	}
	replacement, err := d.enqueueReplacement(ctx, campaign, owner, draft, entry.Number)
	if err != nil {
		return executionFailure(err)
	}
	if pending.ReplacementNumber != entry.Number || pending.ReplaceOperationID != replacement.ID {
		pending.ReplacementNumber, pending.ReplaceOperationID = entry.Number, replacement.ID
		if err = save(); err != nil {
			return err
		}
	}
	if err = childComplete(replacement, "machine replacement"); err != nil {
		var failure operations.Failure
		if errors.As(err, &failure) && !failure.Retryable {
			entry.State, entry.OperationID, entry.FailureCode = ReplacementFailed, replacement.ID, failure.Code
			if recordErr := d.recordReplacement(ctx, recorder, owner, *entry); recordErr != nil {
				return recordErr
			}
			return giveUp(FailureMachineReplacementFailed, err, true)
		}
		return err
	}
	if entry.State != ReplacementReplaced {
		entry.State, entry.OperationID = ReplacementReplaced, replacement.ID
		if err = d.recordReplacement(ctx, recorder, owner, *entry); err != nil {
			return err
		}
	}
	if confirming {
		return resolve(RecoveryMachineRetired)
	}
	return resolve(RecoveryMachineReplaced)
}

// lostInterruptions counts the stops a measurement did not choose: every
// interruption except retirements for a confirmation.
func lostInterruptions(checkpoint *MeasurementCheckpoint) int {
	count := 0
	for _, interruption := range checkpoint.Interruptions {
		if interruption.Kind != RunKindConfirmation {
			count++
		}
	}
	return count
}

func (d CompositeDriver) recordReplacement(ctx context.Context, recorder MachineReplacementRecorder, owner domain.OptimizationCandidateRun, entry MachineReplacement) error {
	encoded, _ := json.Marshal(entry)
	// The store refuses a new entry once limit entries exist. Retirements for
	// a confirmation do not use up the lost-machine limit, so they raise it.
	entries := DecodeMachineReplacements(owner.MachineReplacementsJSON)
	limit := MaxMachineReplacements + MachineReplacementsUsed(entries) - lostMachineReplacements(entries)
	if entry.Purpose == ReplacementPurposeConfirmation {
		limit = MachineReplacementsUsed(entries) + 1
	}
	if _, err := recorder.RecordOptimizationMachineReplacement(ctx, owner.TenantID, owner.CampaignID, owner.ID, string(encoded), limit); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return err
		}
		return operations.Retryable("optimization_recovery_record_failed", err)
	}
	return nil
}

// enqueueReplacement provisions the setup's revision again through the same
// child operation that provisioned it first, under a key unique to this
// replacement. The removed replica's ordinal is free, so provisioning creates
// one new machine with a new provider identity and adopts every live one.
func (d CompositeDriver) enqueueReplacement(ctx context.Context, campaign domain.OptimizationCampaign, owner domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft, number int) (domain.Operation, error) {
	key := childKey(owner.ID, fmt.Sprintf("replace-%d", number))
	if campaign.Intent == IntentEvolveEndpoint {
		request, _ := json.Marshal(workflows.RolloutRequest{Name: campaign.TargetDeployment, CandidateID: owner.RevisionID, TenantID: owner.TenantID, Actor: "optimization-campaign"})
		operation, _, err := d.Store.EnqueueOperation(ctx, domain.Operation{TenantID: owner.TenantID, Kind: workflows.RolloutProvisionKind, ResourceType: "deployment", ResourceName: campaign.TargetDeployment, IdempotencyKey: key, RequestJSON: string(request), MaxAttempts: 120})
		return operation, err
	}
	resolved, err := d.Store.ResolveForTenant(ctx, owner.TenantID, owner.DeploymentName)
	if err != nil {
		return domain.Operation{}, err
	}
	request := draftCloudRequest(draft)
	request.Name, request.DeploymentID, request.RevisionID = owner.DeploymentName, resolved.Deployment.ID, owner.RevisionID
	request.TenantID, request.Actor, request.NoEndpoint = owner.TenantID, "optimization-campaign", true
	encoded, _ := json.Marshal(request)
	operation, _, err := d.Store.EnqueueOperation(ctx, domain.Operation{TenantID: owner.TenantID, Kind: workflows.ConvergeKind, ResourceType: "deployment", ResourceName: owner.DeploymentName, IdempotencyKey: key, RequestJSON: string(encoded), MaxAttempts: 120})
	return operation, err
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
