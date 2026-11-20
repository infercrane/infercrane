package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// Migration switch steps.
const (
	MigrationStepCanary   = "canary"
	MigrationStepPrimary  = "primary"
	MigrationStepRollback = "rollback"
	// MaxMigrationCanaryBasisPoints is the largest share of an endpoint's
	// traffic a canary sends to the owned deployment before Release Guard
	// has measured it on that endpoint: 5%.
	MaxMigrationCanaryBasisPoints = 500
)

// migrationVerdictEnvelope rebuilds the signed envelope of a stored verdict.
func migrationVerdictEnvelope(verdict domain.MigrationQualityVerdict) passport.Envelope {
	return passport.Envelope{PayloadJSON: verdict.PayloadJSON, Digest: verdict.PayloadDigest, Signature: verdict.Signature, PublicKey: verdict.PublicKey, Algorithm: verdict.Algorithm, KeyID: verdict.KeyID}
}

// checkMigrationVerdict verifies a stored verdict's signature, that the
// signed payload says what the row says, and that it is a PASS for this
// endpoint. It returns why not otherwise.
func checkMigrationVerdict(verdict domain.MigrationQualityVerdict, endpointID string) error {
	payload, err := qualityevidence.DecodeMigrationVerdict(migrationVerdictEnvelope(verdict))
	if err != nil {
		return err
	}
	if payload.BindingID != verdict.BindingID || payload.CandidateRevisionID != verdict.RevisionID || payload.Verdict != verdict.Verdict || payload.CampaignID != verdict.CampaignID {
		return errors.New("the stored migration verdict does not match its signed payload")
	}
	if verdict.EndpointID != endpointID {
		return errors.New("the migration verdict belongs to another endpoint")
	}
	if verdict.Verdict != releaseguard.QualityPass {
		return fmt.Errorf("the migration verdict is %s, not PASS", verdict.Verdict)
	}
	return nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// migrationVerdictRow reads a verdict through a transaction or the store.
func migrationVerdictRow(ctx context.Context, q queryRower, tenant, id string) (domain.MigrationQualityVerdict, error) {
	return scanMigrationVerdict(q.QueryRowContext(ctx, `SELECT `+migrationVerdictColumns+` FROM migration_quality_verdicts WHERE tenant_id=? AND id=?`, tenant, id))
}

// validateMigrationPlan admits a plan that routes between a closed API's
// external binding and an owned deployment binding only when it cites a
// signed PASS verdict for exactly that binding and the deployment's active
// revision. In a primary-fallback plan the owned deployment is the primary.
func validateMigrationPlan(ctx context.Context, tx *tx, tenant string, plan domain.ServingPlan) error {
	verdict, err := migrationVerdictRow(ctx, tx, tenant, plan.MigrationVerdictID)
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("%w: migration verdict", ErrNotFound)
	}
	if err != nil {
		return err
	}
	if err = checkMigrationVerdict(verdict, plan.EndpointID); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	external, owned := -1, -1
	for index, planned := range plan.Bindings {
		var kind, deploymentID, targetID, activeRevision string
		err = tx.QueryRowContext(ctx, `SELECT b.kind,COALESCE(b.deployment_id,''),COALESCE(b.target_id,''),COALESCE(d.active_revision_id,'') FROM backend_bindings b LEFT JOIN deployments d ON d.id=b.deployment_id AND d.desired_state<>'deleted' WHERE b.id=? AND b.endpoint_id=? AND b.tenant_id=?`, planned.BindingID, plan.EndpointID, tenant).Scan(&kind, &deploymentID, &targetID, &activeRevision)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: binding %s", ErrNotFound, planned.BindingID)
		}
		if err != nil {
			return err
		}
		switch {
		case kind == "external" && planned.BindingID == verdict.BindingID:
			external = index
		case kind == "deployment" && deploymentID == verdict.DeploymentID && activeRevision == verdict.RevisionID:
			owned = index
		}
	}
	if external < 0 || owned < 0 {
		return fmt.Errorf("%w: a migration plan routes between the verdict's closed API binding and the verdict's revision, active on its deployment", ErrConflict)
	}
	if plan.RoutingPolicy == "primary-fallback" && plan.Bindings[owned].Priority > plan.Bindings[external].Priority {
		return fmt.Errorf("%w: in a migration primary-fallback plan the owned deployment is the primary and the closed API the fallback", ErrConflict)
	}
	return nil
}

// migrationTransition classifies the change from the active plan to the
// candidate plan as a step of a staged migration, or returns "" when it is
// none.
//
//   - canary:   [closed API] -> weighted [owned <= 5%, closed API], citing a
//     PASS verdict. No metric can exist for the owned binding on this
//     endpoint yet; the canary is how they are made.
//   - primary:  canary -> primary-fallback [owned, closed API] over the same
//     two bindings, guarded by per-binding metrics from the canary.
//   - rollback: any migration plan -> [closed API] on its own.
func migrationTransition(resolved domain.ResolvedEndpoint) (step, owned, external string) {
	active, candidate := resolved.ActivePlan, resolved.CandidatePlan
	if candidate == nil {
		return "", "", ""
	}
	kind := func(id string) string {
		for _, binding := range resolved.Bindings {
			if binding.ID == id {
				return binding.Kind
			}
		}
		return ""
	}
	pair := func(plan domain.ServingPlan) (string, string, bool) {
		if len(plan.Bindings) != 2 {
			return "", "", false
		}
		first, second := plan.Bindings[0].BindingID, plan.Bindings[1].BindingID
		switch {
		case kind(first) == "deployment" && kind(second) == "external":
			return first, second, true
		case kind(first) == "external" && kind(second) == "deployment":
			return second, first, true
		}
		return "", "", false
	}
	switch {
	case candidate.MigrationVerdictID != "" && candidate.RoutingPolicy == "weighted" && active.RoutingPolicy == "manual" && len(active.Bindings) == 1:
		ownedID, externalID, ok := pair(*candidate)
		if ok && active.Bindings[0].BindingID == externalID {
			return MigrationStepCanary, ownedID, externalID
		}
	case candidate.MigrationVerdictID != "" && candidate.RoutingPolicy == "primary-fallback" && active.MigrationVerdictID != "" && active.RoutingPolicy == "weighted":
		ownedID, externalID, ok := pair(*candidate)
		activeOwned, activeExternal, activeOK := pair(active)
		if ok && activeOK && ownedID == activeOwned && externalID == activeExternal {
			return MigrationStepPrimary, ownedID, externalID
		}
	case active.MigrationVerdictID != "" && candidate.MigrationVerdictID == "" && candidate.RoutingPolicy == "manual" && len(candidate.Bindings) == 1:
		ownedID, externalID, ok := pair(active)
		if ok && candidate.Bindings[0].BindingID == externalID {
			return MigrationStepRollback, ownedID, externalID
		}
	}
	return "", "", ""
}

func planBinding(resolved domain.ResolvedEndpoint, id string) domain.BackendBinding {
	for _, binding := range resolved.Bindings {
		if binding.ID == id {
			return binding
		}
	}
	return domain.BackendBinding{}
}

func canaryBasisPoints(plan domain.ServingPlan, owned string) int {
	total, share := 0, 0
	for _, binding := range plan.Bindings {
		total += binding.Weight
		if binding.BindingID == owned {
			share = binding.Weight
		}
	}
	if total == 0 {
		return 10000
	}
	return share * 10000 / total
}

// evaluateMigrationGuard decides a migration step. It returns ok=false when
// the endpoint's change is not a migration step.
func (s *Store) evaluateMigrationGuard(ctx context.Context, tenant string, resolved domain.ResolvedEndpoint, policy domain.EndpointReleaseGuardPolicy, window time.Duration) (domain.EndpointReleaseGuardEvaluation, bool, error) {
	step, ownedID, externalID := migrationTransition(resolved)
	if step == "" {
		return domain.EndpointReleaseGuardEvaluation{}, false, nil
	}
	owned, external := planBinding(resolved, ownedID), planBinding(resolved, externalID)
	externalMetrics, err := s.BindingMetrics(ctx, tenant, external, window)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, true, err
	}
	topology := map[string]any{"migration_step": step, "owned_binding": owned.Name, "closed_api_binding": external.Name}
	record := func(decision string, reasons any, metrics map[string]any) (domain.EndpointReleaseGuardEvaluation, bool, error) {
		evaluation, recordErr := s.recordEndpointGuardEvaluation(ctx, tenant, resolved, policy, decision, reasons, metrics)
		return evaluation, true, recordErr
	}
	if step == MigrationStepRollback {
		// Going back to the closed API that served before is always allowed.
		return record("PASS", []string{"migration_rollback_to_closed_api"}, map[string]any{"active": externalMetrics, "plan_topology": topology})
	}
	verdict, err := s.MigrationVerdict(ctx, tenant, resolved.CandidatePlan.MigrationVerdictID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.EndpointReleaseGuardEvaluation{}, true, err
	}
	verdictErr := err
	if verdictErr == nil {
		verdictErr = checkMigrationVerdict(verdict, resolved.Endpoint.ID)
	}
	if verdictErr == nil {
		var activeRevision string
		if err = s.QueryRowContext(ctx, `SELECT COALESCE(active_revision_id,'') FROM deployments WHERE id=? AND tenant_id=?`, owned.DeploymentID, tenant).Scan(&activeRevision); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.EndpointReleaseGuardEvaluation{}, true, err
		}
		if owned.DeploymentID != verdict.DeploymentID || activeRevision != verdict.RevisionID {
			verdictErr = errors.New("the owned deployment no longer serves the revision the verdict judged")
		}
	}
	topology["migration_verdict_id"] = resolved.CandidatePlan.MigrationVerdictID
	if verdictErr != nil {
		topology["migration_verdict"] = verdictErr.Error()
		return record("REJECT", []string{"migration_quality_verdict_invalid"}, map[string]any{"active": externalMetrics, "plan_topology": topology})
	}
	if step == MigrationStepCanary {
		if share := canaryBasisPoints(*resolved.CandidatePlan, ownedID); share > MaxMigrationCanaryBasisPoints {
			topology["canary_basis_points"] = share
			return record("REJECT", []string{"migration_canary_too_large"}, map[string]any{"active": externalMetrics, "plan_topology": topology})
		}
		// The owned deployment has served nothing on this endpoint yet. The
		// signed quality verdict admits a canary of at most 5%; its traffic
		// is what the next step is guarded on.
		return record("PASS", []string{"migration_canary_admitted_on_quality_verdict"}, map[string]any{"active": externalMetrics, "plan_topology": topology})
	}
	candidateMetrics, err := s.BindingMetrics(ctx, tenant, owned, window)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, true, err
	}
	compatible := true
	externalMetrics.Compatible, candidateMetrics.Compatible = &compatible, &compatible
	externalMetrics.EvidenceID, candidateMetrics.EvidenceID = external.ID, owned.ID
	externalMetrics.CompatibilityEvidence = "signed migration quality verdict " + verdict.ID
	candidateMetrics.CompatibilityEvidence = externalMetrics.CompatibilityEvidence
	result := releaseguard.Evaluate(releaseguard.Input{Policy: endpointGuardPolicy(policy), Active: externalMetrics, Candidate: candidateMetrics})
	decision := map[string]string{"ACCEPT": "PASS", "REJECT": "REJECT", "WAIT": "INCONCLUSIVE"}[result.Decision]
	return record(decision, result.Reasons, map[string]any{"active": externalMetrics, "candidate": candidateMetrics, "plan_topology": topology})
}

// MigrationStage is the outcome of one staged-switch step.
type MigrationStage struct {
	Step       string                                `json:"step"`
	Plan       domain.ServingPlan                    `json:"-"`
	Evaluation domain.EndpointReleaseGuardEvaluation `json:"-"`
	Activated  bool                                  `json:"activated"`
}

// StageMigration takes one step of a staged switch from a closed API to an
// owned deployment on an endpoint: it creates (or reuses) the step's plan,
// stages it as the candidate, evaluates Release Guard, and activates it on
// PASS. The caller refreshes routes after an activation.
func (s *Store) StageMigration(ctx context.Context, tenant, endpointName, step, verdictID string, canaryBasis int, window time.Duration) (MigrationStage, error) {
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return MigrationStage{}, err
	}
	stage := MigrationStage{Step: step}
	var plan domain.ServingPlan
	switch step {
	case MigrationStepCanary:
		if canaryBasis == 0 {
			canaryBasis = MaxMigrationCanaryBasisPoints
		}
		if canaryBasis < 1 || canaryBasis > MaxMigrationCanaryBasisPoints {
			return stage, fmt.Errorf("a canary sends 1 to %d basis points of traffic to the owned deployment", MaxMigrationCanaryBasisPoints)
		}
		verdict, verdictErr := s.MigrationVerdict(ctx, tenant, verdictID)
		if verdictErr != nil {
			return stage, verdictErr
		}
		owned, ownedErr := s.migrationOwnedBinding(ctx, tenant, resolved, verdict.DeploymentID)
		if ownedErr != nil {
			return stage, ownedErr
		}
		// Weighted routing hands out contiguous runs of picks per binding, so
		// the weights are reduced to their smallest equal ratio: 500:9500
		// would send the first 500 requests to the canary, 1:19 sends one in
		// every 20.
		ownedWeight, closedWeight := reducedWeights(canaryBasis, 10000-canaryBasis)
		plan = domain.ServingPlan{EndpointID: resolved.Endpoint.ID, RoutingPolicy: "weighted", MigrationVerdictID: verdict.ID, Bindings: []domain.ServingPlanBinding{{BindingID: owned.ID, Priority: 0, Weight: ownedWeight}, {BindingID: verdict.BindingID, Priority: 1, Weight: closedWeight}}}
	case MigrationStepPrimary:
		active := resolved.ActivePlan
		if active.MigrationVerdictID == "" || active.RoutingPolicy != "weighted" {
			return stage, fmt.Errorf("%w: the endpoint is not on a migration canary", ErrConflict)
		}
		if verdictID == "" {
			verdictID = active.MigrationVerdictID
		}
		verdict, verdictErr := s.MigrationVerdict(ctx, tenant, verdictID)
		if verdictErr != nil {
			return stage, verdictErr
		}
		owned, ownedErr := s.migrationOwnedBinding(ctx, tenant, resolved, verdict.DeploymentID)
		if ownedErr != nil {
			return stage, ownedErr
		}
		plan = domain.ServingPlan{EndpointID: resolved.Endpoint.ID, RoutingPolicy: "primary-fallback", MigrationVerdictID: verdict.ID, Bindings: []domain.ServingPlanBinding{{BindingID: owned.ID, Priority: 0, Weight: 100}, {BindingID: verdict.BindingID, Priority: 1, Weight: 100}}}
	case MigrationStepRollback:
		active := resolved.ActivePlan
		if active.MigrationVerdictID == "" {
			return stage, fmt.Errorf("%w: the endpoint is not on a migration plan", ErrConflict)
		}
		externalID := ""
		for _, planned := range active.Bindings {
			if planBinding(resolved, planned.BindingID).Kind == "external" {
				externalID = planned.BindingID
			}
		}
		plan = domain.ServingPlan{EndpointID: resolved.Endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: externalID, Priority: 0, Weight: 100}}}
	default:
		return stage, errors.New("step must be canary, primary, or rollback")
	}
	if plan, err = s.findOrCreatePlan(ctx, tenant, resolved, plan); err != nil {
		return stage, err
	}
	stage.Plan = plan
	if err = s.SetEndpointPlan(ctx, tenant, endpointName, plan.ID, "candidate"); err != nil {
		return stage, err
	}
	if stage.Evaluation, err = s.EvaluateEndpointReleaseGuard(ctx, tenant, endpointName, window); err != nil {
		return stage, err
	}
	if stage.Evaluation.Decision != "PASS" {
		return stage, nil
	}
	if err = s.SetEndpointPlan(ctx, tenant, endpointName, plan.ID, "active"); err != nil {
		return stage, err
	}
	stage.Activated = true
	return stage, nil
}

// migrationOwnedBinding returns the endpoint's traffic-managed binding to
// the deployment, creating it when the endpoint has none.
func (s *Store) migrationOwnedBinding(ctx context.Context, tenant string, resolved domain.ResolvedEndpoint, deploymentID string) (domain.BackendBinding, error) {
	for _, binding := range resolved.Bindings {
		if binding.Kind == "deployment" && binding.DeploymentID == deploymentID && binding.OwnershipMode != "observe-only" {
			return binding, nil
		}
	}
	suffix := deploymentID
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return s.CreateBackendBinding(ctx, tenant, domain.BackendBinding{EndpointID: resolved.Endpoint.ID, Name: "owned-" + suffix, Kind: "deployment", OwnershipMode: "traffic-managed", DeploymentID: deploymentID, ConfigJSON: "{}"})
}

// findOrCreatePlan returns the endpoint's existing plan with the same
// canonical spec, or creates it. Plans are immutable, so a step taken twice
// reuses its plan.
func (s *Store) findOrCreatePlan(ctx context.Context, tenant string, resolved domain.ResolvedEndpoint, plan domain.ServingPlan) (domain.ServingPlan, error) {
	_, body, err := normalizePlan(plan.RoutingPolicy, plan.Bindings, plan.MigrationVerdictID)
	if err != nil {
		return plan, err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	plans, err := s.plansForEndpoint(ctx, tenant, resolved.Endpoint.ID)
	if err != nil {
		return plan, err
	}
	for _, existing := range plans {
		if existing.SpecDigest == digest {
			return existing, nil
		}
	}
	return s.CreateServingPlan(ctx, tenant, plan)
}

// StartMigrationCanary is the canary step a migration campaign's activation
// takes: at most 5% of the endpoint's traffic to the verdict's deployment.
func (s *Store) StartMigrationCanary(ctx context.Context, tenant, endpoint, verdictID string) (bool, string, error) {
	stage, err := s.StageMigration(ctx, tenant, endpoint, MigrationStepCanary, verdictID, MaxMigrationCanaryBasisPoints, time.Hour)
	return stage.Activated, stage.Evaluation.Decision, err
}

// reducedWeights divides two weights by their greatest common divisor.
func reducedWeights(left, right int) (int, int) {
	a, b := left, right
	for b != 0 {
		a, b = b, a%b
	}
	if a <= 1 {
		return left, right
	}
	return left / a, right / a
}
