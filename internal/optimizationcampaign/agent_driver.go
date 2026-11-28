package optimizationcampaign

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/support"
)

// An agent-measured run improves an endpoint an InferCrane agent serves in
// the customer's own cluster. The control plane may never dial the agent's
// servers, so every window is measured by the agent: it starts the setup,
// brackets each of its runs with runs of the customer's live server, asks the
// answer check on both, deletes the setup, and reports numbers only. The
// AgentDriver writes those numbers into the same MeasurementCheckpoint the
// control plane's own measurements use and judges them with the same code:
// finish (control check, paired comparison, machine allowance), the
// confirmation trigger and confirmedDecision, measureAgain, the quality rules
// (qualitycheck.Scored), the ranking, and Release Guard.

// Stages of an agent-measured run's windows.
const (
	AgentWindowFirst        = "first"
	AgentWindowConfirm      = "confirm"
	AgentWindowConfirmAgain = "confirm-again"
	AgentWindowsDone        = "done"

	// ComputeModeAgentCluster marks evidence measured in the customer's
	// cluster by an InferCrane agent.
	ComputeModeAgentCluster = "agent-cluster"

	// FailureAgentSetupUnsupported stops a setup an agent cannot start yet
	// (environment variables, kernel files, adapters, a private model's
	// credential, serverless compute).
	FailureAgentSetupUnsupported = "agent_setup_unsupported"
	// FailureAgentSetupNotPlainServer stops a setup that is not a plain
	// server, such as a Dynamo serving graph. An agent starts plain servers
	// only, so it would measure, and label, another setup.
	FailureAgentSetupNotPlainServer = "agent_setup_not_plain_server"
	// FailureAgentReplayUnsupported stops a run bound to a replayed trace;
	// agents measure the synthetic profile for now.
	FailureAgentReplayUnsupported = "agent_replay_unsupported"
	// FailureAgentWindowFailed stops a setup whose window failed past its
	// retries.
	FailureAgentWindowFailed = "agent_window_failed"
	// FailureAgentAnswerCheck ends a setup whose answers the agent could
	// not compare: unknown, never qualified.
	FailureAgentAnswerCheck = "agent_answer_check_failed"

	retryAgentWindowPending = "optimization_agent_window_pending"
	// maxAgentWindowRetries is how often one window is measured again after
	// its agent reported a failure that more time can fix (a lost run, a
	// restart, an expired lease).
	maxAgentWindowRetries = 2
	// maxAgentWindowTime bounds one window, start to cleanup.
	maxAgentWindowTime = 6 * time.Hour
	agentWindowActor   = "optimization-campaign"
)

// Agent failure codes that more time cannot fix: the setup stops at once.
var permanentAgentFailures = map[string]bool{
	"image_not_allowed": true, "accelerator_not_allowed": true, "invalid_command": true, "unsupported_command": true,
	"eval_set_unknown": true, "invalid_candidate": true, "invalid_result": true,
}

var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9 ._:/+-]`)

// errNotPlainServer marks a setup an agent cannot reproduce. An agent starts
// plain servers only: one engine container from an image, with its arguments,
// on its GPUs (agentprotocol.CandidateSetup holds nothing more).
var errNotPlainServer = errors.New("an agent starts plain servers only")

// AgentWindowsState is an agent-measured run's progress in its checkpoint.
type AgentWindowsState struct {
	Stage string `json:"stage"`
	// Retry counts failed tries at the current stage's window.
	Retry int `json:"retry,omitempty"`
	// Pending is the command measuring the current window, while it runs.
	Pending string `json:"pending,omitempty"`
	// Windows are the agent commands whose numbers the checkpoint holds, and
	// Failed the ones that did not finish.
	Windows []AgentWindowRecord `json:"windows,omitempty"`
	Failed  []AgentWindowRecord `json:"failed,omitempty"`
	// Answers is the answer check the verdict reads, scores and digests
	// only: the first window's, or after a confirmation the fresh window's
	// (the first window's pair is then kept in Confirmation.FirstAnswers).
	// QualityFailure says why the agent could not compare the answers.
	Answers        *agentprotocol.WindowAnswers `json:"answers,omitempty"`
	QualityFailure string                       `json:"quality_failure,omitempty"`
}

// AgentWindowRecord names one window's agent command and, for a finished
// window, the nodes its servers ran on as the agent named them (a keyed
// digest, "several", or "unknown").
type AgentWindowRecord struct {
	Stage         string `json:"stage"`
	CommandID     string `json:"command_id"`
	Retry         int    `json:"retry,omitempty"`
	FailureCode   string `json:"failure_code,omitempty"`
	CandidateNode string `json:"candidate_node,omitempty"`
	ControlNode   string `json:"control_node,omitempty"`
}

// Recoveries of the confirmation an agent measured. A setup started again
// counts as on a fresh machine (RecoveryMachineRetired) only when both its
// nodes are known and differ; a new pod on the same node, or on a node the
// agent could not name, is not a fresh machine, so the gain's fresh-machine
// evidence stays unknown.
const (
	RecoveryAgentSameNode    = "new_server_same_node"
	RecoveryAgentNodeUnknown = "new_server_node_unknown"
)

// AgentWindow is a measured window as the relay reports it.
type AgentWindow struct {
	ID, State, FailureCode, FailureDetail string
	StartedAt, FinishedAt                 *time.Time
	Result                                *agentprotocol.WindowResult
}

// AgentWindowSpec is the window an agent is asked for.
type AgentWindowSpec = agentprotocol.WindowSpec

// AgentWindows queues and reads measured windows. QueueWindow is idempotent
// per key: the same key returns the window queued first, in whatever state.
type AgentWindows interface {
	QueueWindow(ctx context.Context, tenant, agentID, key string, spec agentprotocol.WindowSpec) (AgentWindow, error)
	CancelWindow(ctx context.Context, tenant, agentID, id string) error
}

// AgentDriverStore is the persistence an agent-measured run needs.
type AgentDriverStore interface {
	Repository
	MeasurementCheckpointSink
	RecordBenchmark(context.Context, domain.BenchmarkResult) (domain.BenchmarkResult, error)
	BenchmarksByIDs(context.Context, string, []string) (map[string]domain.BenchmarkResult, error)
	RecordAgentQualityEvidence(context.Context, string, string, domain.QualityEvidence) (domain.QualityEvidence, bool, error)
	QualityEvidenceByIDs(context.Context, string, []string) (map[string]domain.QualityEvidence, error)
	RecordAgentReleaseGuardEvaluation(context.Context, domain.AgentReleaseGuardEvaluation) (domain.AgentReleaseGuardEvaluation, error)
}

// AgentDriver runs the steps of a run on an agent's endpoint. It owns no
// campaign state and cannot promote.
type AgentDriver struct {
	Store   AgentDriverStore
	Windows AgentWindows
	Ranker  CandidateRanker
	// Signer signs InferCrane-run quality evidence, as for the built-in
	// check.
	Signer ed25519.PrivateKey
	// Costs prices a cost objective; nil leaves cost unknown, so a cost
	// objective is never confirmed.
	Costs CostAuthority
	// Providers are the control plane's provider profiles; AgentSetup reads
	// a setup's adapter from them.
	Providers []integration.ProviderProfile
	Now       func() time.Time
	Entropy   io.Reader
}

// AgentRun reports whether a campaign is measured by an agent.
func AgentRun(campaign domain.OptimizationCampaign) bool {
	return campaign.TargetEndpoint != "" && campaign.AgentID != ""
}

// AgentServerID is the identity of the customer's live server behind an
// agent's endpoint: the run's control.
func AgentServerID(agentID, endpoint string) string {
	sum := sha256.Sum256([]byte("agentserver\x00" + agentID + "\x00" + endpoint))
	return "agentserver-" + hex.EncodeToString(sum[:])[:24]
}

// AgentSetupID is the identity of a setup an agent starts: the digest of its
// exact spec, so the same setup started again is the same setup.
func AgentSetupID(setup agentprotocol.CandidateSetup) string {
	encoded, _ := json.Marshal(setup)
	sum := sha256.Sum256(append([]byte("agentsetup\x00"), encoded...))
	return "agentsetup-" + hex.EncodeToString(sum[:])[:24]
}

// AgentSetup turns a candidate's deployment draft into the setup an agent
// starts, or says which part an agent cannot start yet. providers are the
// control plane's provider profiles; with the draft's serving graph they
// decide whether the setup is a plain server (agentPlainServer).
func AgentSetup(draft optimizer.DeploymentDraft, providers []integration.ProviderProfile) (agentprotocol.CandidateSetup, error) {
	if err := agentPlainServer(draft, providers); err != nil {
		return agentprotocol.CandidateSetup{}, err
	}
	unsupported := func(part string) error {
		return fmt.Errorf("an agent cannot start a setup with %s yet", part)
	}
	switch {
	case draft.Compute.Mode == "serverless":
		return agentprotocol.CandidateSetup{}, unsupported("serverless compute")
	case len(draft.Runtime.Env) > 0:
		return agentprotocol.CandidateSetup{}, unsupported("environment variables")
	case draft.Runtime.KernelConfigs != nil:
		return agentprotocol.CandidateSetup{}, unsupported("tuned kernel files")
	case len(draft.Model.Adapters) > 0:
		return agentprotocol.CandidateSetup{}, unsupported("LoRA adapters")
	case draft.ModelSecretReferenceID != "":
		return agentprotocol.CandidateSetup{}, unsupported("a private model's credential")
	}
	workload := support.ServedWorkloadFor(draft.Runtime.Engine, draft.Runtime.Version, draft.Resources.GPU, draft.Runtime.Workload)
	if workload.Empty() || workload.Image == "" {
		return agentprotocol.CandidateSetup{}, unsupported("no pinned image for " + draft.Runtime.Engine + " " + draft.Runtime.Version)
	}
	workload.Command = agentCommand(workload.Command)
	gpus := max(draft.Resources.GPUCount, 1)
	setup := agentprotocol.CandidateSetup{Model: draft.Model.ID, ModelRevision: draft.Model.Revision, Workload: workload, RuntimeArgs: append([]string(nil), draft.Runtime.Args...), Accelerator: draft.Resources.GPU, GPUCount: gpus}
	if err := setup.Validate(); err != nil {
		return agentprotocol.CandidateSetup{}, err
	}
	return setup, nil
}

// agentPlainServer refuses a setup that is not a plain server, so an agent
// never measures one setup under another's name. The data decide, never an
// adapter's name: a serving graph in the draft, or an adapter whose provider
// profile serves through software beside its cloud. An adapter without a
// profile is refused too, because nothing says what it starts.
func agentPlainServer(draft optimizer.DeploymentDraft, providers []integration.ProviderProfile) error {
	adapter := draft.Provider.Adapter
	var profile *integration.ProviderProfile
	for index := range providers {
		if providers[index].Adapter == adapter {
			profile = &providers[index]
		}
	}
	switch {
	case !draft.Serving.Empty() && profile != nil && !profile.PlainServer():
		return fmt.Errorf("%w; this setup needs a serving graph on %s", errNotPlainServer, profile.Prerequisite.Name)
	case !draft.Serving.Empty():
		return fmt.Errorf("%w; this setup needs a serving graph", errNotPlainServer)
	case adapter != "" && profile == nil:
		return fmt.Errorf("%w; this setup is on provider adapter %s, which this control plane has no profile for, so what it starts is unknown", errNotPlainServer, adapter)
	case profile != nil && !profile.PlainServer():
		return fmt.Errorf("%w; this setup runs through %s (provider adapter %s)", errNotPlainServer, profile.Prerequisite.Name, adapter)
	}
	return nil
}

// agentSetupRefused is the candidate failure for a setup AgentSetup refused:
// its code says whether the setup is not a plain server at all.
func agentSetupRefused(err error) error {
	if errors.Is(err, errNotPlainServer) {
		return operations.Permanent(FailureAgentSetupNotPlainServer, err)
	}
	return operations.Permanent(FailureAgentSetupUnsupported, err)
}

// agentCommand starts SGLang with its own entry point: an agent refuses an
// interpreter as a command, and `sglang serve` takes launch_server's flags.
func agentCommand(command []string) []string {
	if len(command) >= 3 && (command[0] == "python3" || command[0] == "python") && command[1] == "-m" && command[2] == "sglang.launch_server" {
		return append([]string{"sglang", "serve"}, command[3:]...)
	}
	return append([]string(nil), command...)
}

func (d AgentDriver) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d AgentDriver) inputs(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (domain.OptimizationCampaign, optimizer.DeploymentDraft, error) {
	if d.Store == nil || d.Windows == nil || candidate.ID == "" || candidate.ID != candidateID || candidate.TenantID == "" || candidate.CampaignID == "" {
		return domain.OptimizationCampaign{}, optimizer.DeploymentDraft{}, operations.Permanent("optimization_driver_invalid", errors.New("agent driver requires store, windows, and exact candidate identity"))
	}
	campaign, err := d.Store.OptimizationCampaign(ctx, candidate.TenantID, candidate.CampaignID)
	if err != nil {
		return campaign, optimizer.DeploymentDraft{}, err
	}
	if campaign.Intent != IntentEvolveEndpoint || !AgentRun(campaign) {
		return campaign, optimizer.DeploymentDraft{}, operations.Permanent("optimization_intent_invalid", errors.New("an agent measures only a run that evolves its endpoint"))
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return campaign, draft, operations.Permanent("optimization_candidate_spec_invalid", errors.New("candidate deployment spec is invalid"))
	}
	return campaign, draft, nil
}

func isControl(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) bool {
	control, found := CampaignControl(campaign)
	return found && control.ID == candidate.ID
}

// Provision starts nothing: each window starts the setup and deletes it. It
// fixes the setup's identity and refuses, before any window, a setup the
// agent cannot start. The control is the customer's live server.
func (d AgentDriver) Provision(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun, _ Budget) (ProvisionResult, error) {
	campaign, draft, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return ProvisionResult{}, err
	}
	if replayBound(campaign) {
		return ProvisionResult{}, operations.Permanent(FailureAgentReplayUnsupported, errors.New("an agent measures the synthetic profile; a run bound to a replayed trace is not supported on an agent's endpoint yet"))
	}
	if isControl(campaign, candidate) {
		return ProvisionResult{DeploymentName: campaign.TargetEndpoint, RevisionID: AgentServerID(campaign.AgentID, campaign.TargetEndpoint)}, nil
	}
	setup, err := AgentSetup(draft, d.Providers)
	if err != nil {
		return ProvisionResult{}, agentSetupRefused(err)
	}
	return ProvisionResult{DeploymentName: campaign.TargetEndpoint, RevisionID: AgentSetupID(setup)}, nil
}

func replayBound(campaign domain.OptimizationCampaign) bool {
	request := campaignRequest(campaign)
	return request.WorkloadSource == optimizer.WorkloadSourceCustomerObserved && request.WorkloadFingerprint != ""
}

// Measure queues the window the run needs next and waits for it; when the
// agent reports it, the runs join the checkpoint and are judged.
func (d AgentDriver) Measure(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun, budget Budget) (MeasurementResult, error) {
	campaign, draft, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return MeasurementResult{}, err
	}
	if !budget.ExpiresAt.After(d.now()) {
		return MeasurementResult{}, ErrApprovalExpired
	}
	profileName, err := campaignProfile(campaign, candidate.ProposalCandidateID)
	if err != nil {
		return MeasurementResult{}, operations.Permanent("optimization_profile_invalid", err)
	}
	profile, err := performanceprofile.Get(profileName)
	if err != nil {
		return MeasurementResult{}, operations.Permanent("optimization_profile_invalid", err)
	}
	benchmarkID := agentBenchmarkID(candidate.ID)
	if rows, lookupErr := d.Store.BenchmarksByIDs(ctx, candidate.TenantID, []string{benchmarkID}); lookupErr != nil {
		return MeasurementResult{}, operations.Retryable("optimization_benchmark_lookup_failed", lookupErr)
	} else if row, ok := rows[benchmarkID]; ok {
		return measuredResult(row), nil
	}
	state, err := d.checkpoint(campaign, candidate)
	if err != nil {
		return MeasurementResult{}, err
	}
	bracketed := !isControl(campaign, candidate)
	session := measurementSession{candidate: armTarget{candidateID: candidate.ID, revisionID: candidate.RevisionID}, state: &state, owner: candidate, now: d.now, deadline: budget.ExpiresAt}
	for state.Agent.Stage != AgentWindowsDone {
		spec, specErr := d.windowSpec(campaign, candidate, draft, profile, &state, budget)
		if specErr != nil {
			return MeasurementResult{}, specErr
		}
		key := childKey(candidate.ID, fmt.Sprintf("window-%s-%d", state.Agent.Stage, state.Agent.Retry))
		window, queueErr := d.Windows.QueueWindow(ctx, candidate.TenantID, campaign.AgentID, key, spec)
		if queueErr != nil {
			if errors.Is(queueErr, domain.ErrConflict) || errors.Is(queueErr, domain.ErrNotFound) {
				return MeasurementResult{}, operations.Retryable("optimization_agent_unavailable", queueErr)
			}
			return MeasurementResult{}, operations.Retryable("optimization_agent_window_queue_failed", queueErr)
		}
		switch window.State {
		case agentprotocol.CommandQueued, agentprotocol.CommandRunning:
			if state.Agent.Pending != window.ID {
				state.Agent.Pending = window.ID
				d.save(candidate, state)
			}
			return MeasurementResult{}, operations.Retryable(retryAgentWindowPending, fmt.Errorf("the agent is measuring window %s (%s)", window.ID, state.Agent.Stage))
		case agentprotocol.CommandSucceeded:
			if window.Result == nil {
				return MeasurementResult{}, operations.Permanent(FailureAgentWindowFailed, errors.New("the agent reported a window without its numbers"))
			}
			if turnsErr := window.Result.Turns(measurementRunsPerArm, bracketed); turnsErr != nil {
				return MeasurementResult{}, operations.Permanent(FailureAgentWindowFailed, fmt.Errorf("the agent's window is not a bracketed window: %w", turnsErr))
			}
			d.accept(&state, window, bracketed, spec.Fresh)
			d.advance(ctx, &session, campaign, candidate, draft)
			d.save(candidate, state)
		default:
			code := window.FailureCode
			if code == "" {
				code = window.State
			}
			state.Agent.Failed = append(state.Agent.Failed, AgentWindowRecord{Stage: state.Agent.Stage, CommandID: window.ID, Retry: state.Agent.Retry, FailureCode: code})
			state.Agent.Pending = ""
			if permanentAgentFailures[code] || state.Agent.Retry >= maxAgentWindowRetries {
				d.save(candidate, state)
				return MeasurementResult{}, operations.Permanent(FailureAgentWindowFailed, fmt.Errorf("the agent's window failed: %s %s", code, window.FailureDetail))
			}
			state.Agent.Retry++
			d.save(candidate, state)
		}
	}
	measured := session.finish()
	row, err := d.recordBenchmark(ctx, campaign, candidate, draft, profile, benchmarkID, measured, state)
	if err != nil {
		return MeasurementResult{}, err
	}
	return measuredResult(row), nil
}

// checkpoint returns the run's measurement so far, or a fresh one.
func (d AgentDriver) checkpoint(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (MeasurementCheckpoint, error) {
	if stored, ok := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON); ok && stored.Agent != nil {
		return stored, nil
	}
	base, err := invocationBase(d.Entropy)
	if err != nil {
		return MeasurementCheckpoint{}, err
	}
	control := ControlEvidence{Role: ControlRoleCandidate, CandidateID: candidate.ID, RevisionID: candidate.RevisionID}
	if owner, found := CampaignControl(campaign); found && owner.ID != candidate.ID {
		// The customer's live server is the control of every window, so it
		// is always serving: it is bracketed in every window.
		control = ControlEvidence{Role: ControlRoleBracketed, CandidateID: owner.ID, RevisionID: AgentServerID(campaign.AgentID, campaign.TargetEndpoint)}
	}
	identity := "agent:" + campaign.AgentID + ":" + campaign.TargetEndpoint + ":" + candidate.RevisionID
	return MeasurementCheckpoint{Version: measurementCheckpointVersion, Method: MeasurementMethod, Identity: identity, InvocationBase: base, Control: control,
		Comparisons: campaignComparisons(campaign), Agent: &AgentWindowsState{Stage: AgentWindowFirst}}, nil
}

// windowSpec is the window the run's current stage needs.
func (d AgentDriver) windowSpec(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft, profile performanceprofile.Profile, state *MeasurementCheckpoint, budget Budget) (agentprotocol.WindowSpec, error) {
	request := campaignRequest(campaign)
	concurrency := profile.Concurrency
	at, err := measuredConcurrency(request)
	if err != nil {
		return agentprotocol.WindowSpec{}, operations.Permanent("optimization_profile_invalid", err)
	}
	if at > 0 {
		concurrency = at
	}
	control, _ := CampaignControl(campaign)
	reference := AgentServerID(campaign.AgentID, campaign.TargetEndpoint)
	spec := agentprotocol.WindowSpec{
		Endpoint: campaign.TargetEndpoint, Label: windowLabel(campaign, candidate, state.Agent.Stage),
		Rounds: measurementRunsPerArm, Warmup: min(warmupRequests(concurrency), agentprotocol.MaxWindowWarmup),
		Load:     agentprotocol.CandidateLoad{Requests: profile.Requests, Concurrency: concurrency, InputTokens: profile.InputTokens, OutputTokens: profile.OutputTokens, Streaming: profile.Streaming, Tokenizer: draft.Model.ID},
		SeedBase: state.InvocationBase + state.NextInvocation, TTFTLimitMS: campaignTTFTSLO(campaign), TPOTLimitMS: campaignTPOTSLO(campaign),
		Fresh: state.Agent.Stage != AgentWindowFirst,
		// The answer check runs in every window: a setup confirmed on a fresh
		// server is judged on its fresh server's answers, pooled with its
		// first server's, as a check the control plane runs after a
		// confirmation is.
	}
	if !spec.Quality.Skip {
		spec.Quality.EvalSet, spec.Quality.ReferenceSubject, spec.Quality.CandidateSubject = "default", reference, candidate.RevisionID
		if digest := QualityEvalSetDigest(campaign); digest != "" {
			spec.Quality.EvalSet = digest
		}
	}
	if !isControl(campaign, candidate) {
		setup, setupErr := AgentSetup(draft, d.Providers)
		if setupErr != nil {
			return agentprotocol.WindowSpec{}, agentSetupRefused(setupErr)
		}
		if AgentSetupID(setup) != candidate.RevisionID {
			return agentprotocol.WindowSpec{}, operations.Permanent("optimization_candidate_identity_mismatch", errors.New("the setup no longer matches the identity it was measured under"))
		}
		spec.Candidate = &setup
		spec.Quality.Divergence = !spec.Quality.Skip && agentDivergence(campaign, control, draft)
	}
	timeout := maxAgentWindowTime
	if remaining := budget.ExpiresAt.Sub(d.now()); remaining < timeout {
		timeout = remaining
	}
	spec.TimeoutSecs = max(int(timeout/time.Second), 60)
	if err = spec.Validate(); err != nil {
		return agentprotocol.WindowSpec{}, operations.Permanent("optimization_agent_window_invalid", err)
	}
	return spec, nil
}

// liveSetupDeclared reports whether the run's proposal declares what the
// endpoint's own server runs. Without it the control's draft is the
// planner's assumption about a server nobody measured or described, so no
// rule may lean on comparing a setup with it: the run fails closed.
func liveSetupDeclared(campaign domain.OptimizationCampaign) bool {
	setup := campaignRequest(campaign).StartingSetup
	return setup != nil && setup.Endpoint != "" && setup.Endpoint == campaign.TargetEndpoint
}

// liveSetupBasis says what a run's comparisons with the endpoint's own server
// rest on: a setup its operator declared, which InferCrane did not read, or
// none, so the run fails closed.
func liveSetupBasis(campaign domain.OptimizationCampaign) string {
	if liveSetupDeclared(campaign) {
		return LiveSetupDeclared
	}
	return LiveSetupUndeclared
}

// Bases of the comparison with an agent endpoint's own server.
const (
	LiveSetupDeclared   = "declared by the endpoint's operator, not read by InferCrane: the answer rule and any divergence rest on that declaration"
	LiveSetupUndeclared = "not declared: every setup must show its answers held within 1 point, and no divergence is read"
)

// agentDivergence is when a setup's divergence from the live server is read
// and kept: a declared live setup, a change of precision, and the same
// weights, so both servers read text with one tokenizer. The window asks
// for it and Validate keeps it under this one rule.
func agentDivergence(campaign domain.OptimizationCampaign, control domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) bool {
	if !liveSetupDeclared(campaign) || !changesPrecision(campaign, draft) {
		return false
	}
	controlDraft, err := candidateDraft(control)
	return err == nil && controlDraft.Model.ID == draft.Model.ID && controlDraft.Model.Revision == draft.Model.Revision
}

func windowLabel(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, stage string) string {
	label := labelUnsafe.ReplaceAllString(fmt.Sprintf("run %s setup %s window %s", shortID(campaign.ID), candidate.ProposalCandidateID, stage), "-")
	if len(label) > 128 {
		label = label[:128]
	}
	return label
}

// accept adds a finished window's runs to the checkpoint as one segment, in
// the order they ran. A window of the control alone is the control's own
// measurement, so its runs are the measured setup's.
func (d AgentDriver) accept(state *MeasurementCheckpoint, window AgentWindow, bracketed, fresh bool) {
	started, ended := d.now(), d.now()
	if window.StartedAt != nil {
		started = *window.StartedAt
	}
	if window.FinishedAt != nil {
		ended = *window.FinishedAt
	}
	segment := MeasurementSegment{StartedAt: started, EndedAt: &ended, Ended: SegmentCompleted, FirstInvocation: state.InvocationBase + state.NextInvocation, Confirmation: fresh}
	if state.Confirmation != nil {
		segment.Attempt = state.Confirmation.Attempt
	}
	result := window.Result
	for _, run := range result.Runs {
		measured := windowBenchmark(result, run)
		if bracketed && run.Arm == agentprotocol.ArmControl {
			state.ControlRuns = append(state.ControlRuns, measured)
			segment.ControlRuns++
			state.Order = append(state.Order, ArmControl)
			continue
		}
		state.CandidateRuns = append(state.CandidateRuns, measured)
		segment.CandidateRuns++
		state.Order = append(state.Order, ArmCandidate)
	}
	state.Segments = append(state.Segments, segment)
	// Warm-ups took seeds too; the gap keeps the next window's seeds fresh.
	state.NextInvocation += int64(len(result.Runs)+2) + resumeInvocationGap
	record := AgentWindowRecord{Stage: state.Agent.Stage, CommandID: window.ID, Retry: state.Agent.Retry, CandidateNode: "unknown", ControlNode: "unknown"}
	for _, run := range result.Runs {
		node := run.Node
		if node == "" {
			node = "unknown"
		}
		if run.Arm == agentprotocol.ArmControl {
			record.ControlNode = node
		}
		if run.Arm == agentprotocol.ArmCandidate || !bracketed {
			record.CandidateNode = node
		}
	}
	state.Agent.Windows = append(state.Agent.Windows, record)
	state.Agent.Pending, state.Agent.Retry = "", 0
	// The latest window's answers are the ones the verdict reads; the first
	// window's pair was kept for pooling before a fresh window ran
	// (keepFirstAnswers).
	state.Agent.Answers, state.Agent.QualityFailure = result.Quality, result.QualityFailure
}

// advance decides what follows a finished window, with the control plane's
// own rules: a first-window gain on the objective is measured again on a
// fresh server, and a fresh window that came out invalid is measured once
// more while the approval leaves room for it.
func (d AgentDriver) advance(ctx context.Context, session *measurementSession, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) {
	state := session.state
	switch state.Agent.Stage {
	case AgentWindowFirst:
		measured := session.finish()
		if isControl(campaign, candidate) || measured.evidence.Control.Role != ControlRoleBracketed || !objectiveGain(ctx, d.Costs, d.now, campaign, candidate, draft)(measured.evidence) {
			state.Agent.Stage = AgentWindowsDone
			return
		}
		// The customer's server serves production and is kept; the setup is
		// started again on a new server, which the agent's next window does.
		now := d.now()
		state.Confirmation = &ConfirmationState{Phase: ConfirmMeasure, KeepControl: true}
		keepFirstAnswers(state, campaign, candidate, draft)
		// Until the new server's node is known it is not a fresh machine.
		state.Interruptions = append(state.Interruptions, MeasurementInterruption{Sequence: len(state.Interruptions) + 1, At: now, Arm: ArmCandidate, Kind: RunKindConfirmation,
			OwnerCandidateID: candidate.ID, Reason: "started again on a new server so the gain can be measured again", Recovery: RecoveryAgentNodeUnknown, ResolvedAt: &now})
		state.Order = append(state.Order, "retire:"+ArmCandidate)
		state.Agent.Stage = AgentWindowConfirm
	case AgentWindowConfirm:
		markFreshMachine(state)
		if session.measureAgain(session.finish()) {
			state.Agent.Stage = AgentWindowConfirmAgain
			return
		}
		state.Agent.Stage = AgentWindowsDone
	default:
		markFreshMachine(state)
		state.Agent.Stage = AgentWindowsDone
	}
}

// keepFirstAnswers keeps the first window's answers in the checkpoint as a
// server pair before the setup is started again on a fresh server, so the
// verdict pools them with the fresh window's, exactly as a check the control
// plane runs pools a setup's first servers (AskFirstPair): only on a task
// set, for a setup held to the rule for setups that change answers. First
// answers the agent could not compare are recorded as skipped; the verdict
// then rests on the fresh window alone.
func keepFirstAnswers(state *MeasurementCheckpoint, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) {
	reference := AgentServerID(campaign.AgentID, campaign.TargetEndpoint)
	if state.Confirmation == nil || !agentRequiresHeld(campaign, candidate, draft, reference) {
		return
	}
	answers := state.Agent.Answers
	if answers == nil {
		reason := state.Agent.QualityFailure
		if reason == "" {
			reason = "no answers"
		}
		state.Confirmation.FirstAnswersSkipped = "the first server's answers could not be compared: " + reason
		return
	}
	if answers.Grading != qualitycheck.GradingAnswerLine {
		return
	}
	state.Confirmation.FirstAnswers = &qualitycheck.ServerPair{EvalSet: answers.EvalSetDigest, ReferenceID: reference, CandidateID: candidate.RevisionID,
		Reference: append([]float64(nil), answers.ReferenceScores...), Candidate: append([]float64(nil), answers.CandidateScores...)}
}

// agentRequiresHeld reports whether a setup must show its answers held
// within a point. Without a declared live setup, whether a setup changes
// answers is a guess about the customer's server, so every setup must.
func agentRequiresHeld(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft, reference string) bool {
	return reference != candidate.RevisionID && (!liveSetupDeclared(campaign) || changesAnswers(campaign, candidate, draft))
}

// markFreshMachine records whether the setup's latest window ran on another
// node than its first window: only then is the confirmation a fresh machine.
func markFreshMachine(state *MeasurementCheckpoint) {
	var first, last string
	for _, window := range state.Agent.Windows {
		if window.Stage == AgentWindowFirst && first == "" {
			first = window.CandidateNode
		}
		if window.Stage != AgentWindowFirst {
			last = window.CandidateNode
		}
	}
	recovery := RecoveryAgentNodeUnknown
	switch {
	case !strings.HasPrefix(first, "node-") || !strings.HasPrefix(last, "node-"):
	case first == last:
		recovery = RecoveryAgentSameNode
	default:
		recovery = RecoveryMachineRetired
	}
	for index := len(state.Interruptions) - 1; index >= 0; index-- {
		if interruption := &state.Interruptions[index]; interruption.Kind == RunKindConfirmation && interruption.Arm == ArmCandidate {
			interruption.Recovery = recovery
			return
		}
	}
}

func (d AgentDriver) save(candidate domain.OptimizationCandidateRun, state MeasurementCheckpoint) {
	session := measurementSession{state: &state, sink: d.Store, owner: candidate}
	session.save()
}

func windowBenchmark(result *agentprotocol.WindowResult, run agentprotocol.WindowRun) benchmark.Result {
	return benchmark.Result{Tool: result.Tool, ToolVersion: result.ToolVersion, Requests: run.Requests, Succeeded: run.Succeeded, Failed: run.Failed, ProfileRuns: 1,
		DurationSeconds: run.DurationSeconds, RequestThroughput: run.RequestThroughput, OutputTokenThroughput: run.OutputTokenThroughput, Goodput: run.Goodput,
		UsefulOutputThroughput: run.UsefulOutputThroughput, TTFTP50MS: run.TTFTP50MS, TTFTP95MS: run.TTFTP95MS, TPOTP50MS: run.TPOTP50MS, TPOTP95MS: run.TPOTP95MS,
		LatencyP50MS: run.LatencyP50MS, LatencyP95MS: run.LatencyP95MS, OutputTokensPerRequest: run.OutputTokensPerRequest}
}

func agentBenchmarkID(candidateID string) string {
	sum := sha256.Sum256([]byte("agent-benchmark\x00" + candidateID))
	return "agentbench-" + hex.EncodeToString(sum[:])[:24]
}

// recordBenchmark stores the measured row under the setup's identity, with
// the same workload document the control plane's measurements carry, so the
// ranking reads it exactly as it reads its own.
func (d AgentDriver) recordBenchmark(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft, profile performanceprofile.Profile, id string, measured measurement, state MeasurementCheckpoint) (domain.BenchmarkResult, error) {
	if len(measured.candidate) == 0 {
		return domain.BenchmarkResult{}, operations.Permanent(FailureAgentWindowFailed, errors.New("the agent's windows hold no runs of the setup"))
	}
	pooled, confidence := aggregate(measured.candidate)
	spec, _ := d.windowSpec(campaign, candidate, draft, profile, &MeasurementCheckpoint{Agent: &AgentWindowsState{Stage: AgentWindowFirst}}, Budget{ExpiresAt: d.now().Add(maxAgentWindowTime)})
	commands := make([]string, 0, len(state.Agent.Windows))
	for _, window := range state.Agent.Windows {
		commands = append(commands, window.CommandID)
	}
	fresh := "not measured again"
	if measured.evidence.Confirmation != nil {
		fresh = "a fresh machine: the setup ran on another node"
		if !measured.evidence.Confirmation.FreshCandidate {
			fresh = "same node, or a node the agent could not name; a fresh-machine confirmation needs a second GPU node"
		}
	}
	workload := map[string]any{
		"endpoint_type": "chat", "streaming": spec.Load.Streaming, "request_count": spec.Load.Requests, "request_count_per_run": spec.Load.Requests,
		"profile_runs": measurementRunsPerArm, "warmup_requests": spec.Warmup, "concurrency": spec.Load.Concurrency,
		"random_seed": measurementSeed, "seed_policy": "unique_per_invocation", "measurement_method": MeasurementMethod,
		"profile": profile.Name, "profile_version": performanceprofile.Version, "ttft_slo_ms": spec.TTFTLimitMS, "tpot_slo_ms": spec.TPOTLimitMS,
		"server_token_count": true, "output_token_basis": benchmark.OutputTokenBasis, "revision_selector": candidate.RevisionID, "direct_revision_validation": true,
		"confidence_95": confidence, "measured": measured.evidence,
		"workload_source": "synthetic_profile", "input_tokens": spec.Load.InputTokens, "output_tokens": spec.Load.OutputTokens,
		"measured_by": "agent", "agent_id": campaign.AgentID, "agent_windows": state.Agent.Windows, "fresh_machine": fresh, "live_setup": liveSetupBasis(campaign),
		"gpu": "unknown: the agent cannot read which GPU a pod holds without exec or node access",
	}
	workloadJSON, err := json.Marshal(workload)
	if err != nil {
		return domain.BenchmarkResult{}, fmt.Errorf("encode agent workload evidence: %w", err)
	}
	runtimeConfig, _ := json.Marshal(support.BenchmarkRuntimeConfig(draft.Runtime.Args, draft.Runtime.Env))
	cost, _ := json.Marshal(map[string]any{"available": false, "reason": "the setup ran on the customer's own GPUs, measured by an InferCrane agent; no hourly rate is declared"})
	gpus := max(draft.Resources.GPUCount, 1)
	row := domain.BenchmarkResult{ID: id, TenantID: candidate.TenantID, AgentID: campaign.AgentID, DeploymentName: campaign.TargetEndpoint, RevisionID: candidate.RevisionID,
		ModelIdentity: draft.Model.ID + "@" + draft.Model.Revision, Runtime: draft.Runtime.Engine, RuntimeVersion: draft.Runtime.Version, RuntimeConfigJSON: string(runtimeConfig),
		Provider: "kubernetes", Region: draft.Provider.Region, GPU: draft.Resources.GPU, GPUCount: &gpus, ComputeMode: ComputeModeAgentCluster,
		Tool: pooled.Tool, ToolVersion: pooled.ToolVersion, WorkloadJSON: string(workloadJSON),
		ReproductionCommand: "measured in the customer's cluster by InferCrane agent " + campaign.AgentID + " (measure_window " + strings.Join(commands, ", ") + ")",
		RequestCount:        pooled.Requests, Succeeded: pooled.Succeeded, Failed: pooled.Failed, DurationSeconds: pooled.DurationSeconds,
		RequestThroughput: pooled.RequestThroughput, OutputTokenThroughput: pooled.OutputTokenThroughput, TTFTP50MS: pooled.TTFTP50MS, TTFTP95MS: pooled.TTFTP95MS,
		TPOTP50MS: pooled.TPOTP50MS, TPOTP95MS: pooled.TPOTP95MS, LatencyP50MS: pooled.LatencyP50MS, LatencyP95MS: pooled.LatencyP95MS, Goodput: pooled.Goodput,
		CostMetadataJSON: string(cost)}
	recorded, err := d.Store.RecordBenchmark(ctx, row)
	if err != nil {
		return domain.BenchmarkResult{}, operations.Retryable("optimization_benchmark_record_failed", err)
	}
	return recorded, nil
}

// Validate judges the setup's answers by the same rules as a check the
// control plane runs itself, signs the result, and stores it under the
// setup's identity: the first window's answers, or after a confirmation the
// fresh window's pooled with the first window's pair. Answers the agent
// could not compare leave the setup unknown.
func (d AgentDriver) Validate(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (ValidationResult, error) {
	campaign, draft, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return ValidationResult{}, err
	}
	if QualityWaitsForCustomer(campaign) {
		return ValidationResult{}, operations.Permanent("optimization_quality_mode_unsupported", errors.New("customer-signed evidence for a setup an agent measured is not supported yet; use the built-in check with an eval set kept beside the agent"))
	}
	if len(d.Signer) != ed25519.PrivateKeySize {
		return ValidationResult{}, operations.Retryable(retryQualityCheckFailed, errors.New("the control plane has no key to sign quality evidence"))
	}
	state, ok := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON)
	if !ok || state.Agent == nil {
		return ValidationResult{}, operations.Permanent("optimization_measurement_missing", errors.New("the setup has no agent measurement to read answers from"))
	}
	answers := state.Agent.Answers
	if answers == nil {
		code := FailureAgentAnswerCheck
		if state.Agent.QualityFailure == "quality_reference_unanswered" {
			code = FailureQualityReferenceUnanswered
		}
		return ValidationResult{Inconclusive: true, FailureCode: code}, nil
	}
	reference := AgentServerID(campaign.AgentID, campaign.TargetEndpoint)
	set := qualitycheck.RemoteSet(answers.EvalSetName, answers.EvalSetVersion, answers.EvalSetDigest, answers.Grading, answers.PairingDigest)
	var options []qualitycheck.CompareOption
	if agentRequiresHeld(campaign, candidate, draft, reference) {
		options = append(options, qualitycheck.RequireAnswersHeld())
	}
	// A setup confirmed on a fresh server answered on its first server too:
	// the verdict pools both pairs.
	if state.Confirmation != nil && state.Confirmation.FirstAnswers != nil {
		options = append(options, qualitycheck.PoolWith(*state.Confirmation.FirstAnswers))
	}
	comparison, err := qualitycheck.Scored(set, reference, candidate.RevisionID, answers.ReferenceScores, answers.CandidateScores, answers.ToolCalls, answers.ItemsAtOnce, answers.ResultDigest, answers.EvaluatedAt, options...)
	if err != nil {
		return ValidationResult{Inconclusive: true, FailureCode: FailureAgentAnswerCheck}, nil
	}
	if control, found := CampaignControl(campaign); found && reference != candidate.RevisionID && agentDivergence(campaign, control, draft) {
		comparison.Divergence = answers.Divergence
	}
	if reference != candidate.RevisionID {
		if _, err = d.recordEvidence(ctx, campaign, comparison.Payload(campaign.TargetEndpoint, true), reference); err != nil {
			return ValidationResult{}, err
		}
	}
	row, err := d.recordEvidence(ctx, campaign, comparison.Payload(campaign.TargetEndpoint, false), reference)
	if err != nil {
		return ValidationResult{}, err
	}
	return evidenceResult(row), nil
}

func (d AgentDriver) recordEvidence(ctx context.Context, campaign domain.OptimizationCampaign, payload qualityevidence.Payload, reference string) (domain.QualityEvidence, error) {
	row, err := signedEvidence(d.Signer, payload, reference)
	if err != nil && payload.Distribution != nil && payload.Distribution.Divergence != nil {
		// A divergence is evidence and decides nothing: one the evidence
		// rules refuse is left out, unknown, rather than failing the setup.
		distribution := *payload.Distribution
		distribution.Divergence = nil
		payload.Distribution = &distribution
		row, err = signedEvidence(d.Signer, payload, reference)
	}
	if err != nil {
		return domain.QualityEvidence{}, operations.Permanent("optimization_quality_evidence_invalid", err)
	}
	stored, _, err := d.Store.RecordAgentQualityEvidence(ctx, campaign.TenantID, campaign.AgentID, row)
	if err != nil {
		return domain.QualityEvidence{}, operations.Retryable("optimization_quality_record_failed", err)
	}
	return stored, nil
}

// Rank ranks the run's measured setups with the control plane's own ranker.
func (d AgentDriver) Rank(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (RankingResult, error) {
	if _, _, err := d.inputs(ctx, candidateID, candidate); err != nil {
		return RankingResult{}, err
	}
	if d.Ranker == nil {
		return RankingResult{}, operations.Permanent("optimization_ranker_unavailable", errors.New("measured campaign ranker is not configured"))
	}
	return d.Ranker.Rank(ctx, candidate)
}

// AgentGuardPolicy is the Release Guard policy a setup an agent measured is
// held to before it may serve: the defaults of a deployment's policy. Its
// answers were judged when it was validated, by the 1-point rule where it
// changes answers, so quality is not gated twice here.
func AgentGuardPolicy() domain.ReleaseGuardPolicy {
	return domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 20, MaxTTFTRegressionPercent: 15, MaxLatencyRegressionPercent: 15, MaxErrorRateIncrease: 0.01,
		MaxOutputThroughputDropPercent: 20, RequireSyntheticEvidence: true, AutoRollbackEnabled: true, AutoRollbackWindowSeconds: 300, QualityComparisonMode: "threshold"}
}

// Guard applies Release Guard to the setup's first window: its runs against
// the customer's server's runs bracketing them, under AgentGuardPolicy.
func (d AgentDriver) Guard(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (GuardResult, error) {
	campaign, _, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return GuardResult{}, err
	}
	rows, err := d.Store.BenchmarksByIDs(ctx, candidate.TenantID, []string{candidate.BenchmarkID})
	if err != nil {
		return GuardResult{}, operations.Retryable("optimization_benchmark_lookup_failed", err)
	}
	row, ok := rows[candidate.BenchmarkID]
	if !ok {
		return GuardResult{}, operations.Permanent("optimization_benchmark_missing", errors.New("the setup's measured evidence is missing"))
	}
	var workload struct {
		Measured MeasuredEvidence `json:"measured"`
	}
	if json.Unmarshal([]byte(row.WorkloadJSON), &workload) != nil || len(workload.Measured.Runs) == 0 {
		return GuardResult{}, operations.Permanent("optimization_benchmark_invalid", errors.New("the setup's measured evidence has no runs"))
	}
	active, measuredSetup := guardMetrics(workload.Measured.Runs, ArmControl, row.ID), guardMetrics(workload.Measured.Runs, ArmCandidate, row.ID)
	result := releaseguard.Evaluate(releaseguard.Input{Policy: AgentGuardPolicy(), Active: active, Candidate: measuredSetup})
	decision := strings.ToUpper(result.Decision)
	code := ""
	switch decision {
	case "ACCEPT":
		decision = "PASS"
	case "REJECT":
	default:
		decision = "INCONCLUSIVE"
	}
	if len(result.Reasons) > 0 && decision != "PASS" {
		code = "release_guard_" + strings.ToLower(result.Reasons[0].Code)
	}
	// The decision is stored as Release Guard evidence the candidate names,
	// as a guard the control plane runs on a deployment is.
	codes := make([]string, 0, len(result.Reasons))
	for _, reason := range result.Reasons {
		codes = append(codes, reason.Code)
	}
	reasons, _ := json.Marshal(codes)
	metrics, _ := json.Marshal(map[string]domain.RevisionMetrics{"active": active, "candidate": measuredSetup})
	policy, _ := json.Marshal(AgentGuardPolicy())
	recorded, err := d.Store.RecordAgentReleaseGuardEvaluation(ctx, domain.AgentReleaseGuardEvaluation{TenantID: candidate.TenantID, AgentID: campaign.AgentID, CampaignID: campaign.ID, CandidateID: candidate.ID,
		ActiveSetupID: AgentServerID(campaign.AgentID, campaign.TargetEndpoint), CandidateSetupID: candidate.RevisionID, BenchmarkID: row.ID, Decision: decision, ReasonCodesJSON: string(reasons), MetricsJSON: string(metrics), PolicyJSON: string(policy)})
	if err != nil {
		return GuardResult{}, operations.Retryable("optimization_release_guard_record_failed", err)
	}
	return GuardResult{EvaluationID: recorded.ID, Decision: decision, FailureCode: code}, nil
}

// guardMetrics pools one arm's runs of a window into Release Guard's input.
func guardMetrics(runs []RunSummary, arm, evidenceID string) domain.RevisionMetrics {
	metrics := domain.RevisionMetrics{EvidenceSource: "agent_window", EvidenceID: evidenceID, ReadyReplicas: 1, SyntheticValidation: true}
	var ttft, latency, throughput []float64
	failed := 0
	for _, run := range runs {
		if run.Arm != arm {
			continue
		}
		metrics.Requests += run.Requests
		failed += run.Failed
		for target, value := range map[*[]float64]*float64{&ttft: run.TTFTP95MS, &latency: run.LatencyP95MS, &throughput: run.OutputTokenThroughput} {
			if value != nil {
				*target = append(*target, *value)
			}
		}
	}
	if metrics.Requests > 0 {
		metrics.ErrorRate = float64(failed) / float64(metrics.Requests)
	}
	metrics.P95TTFTMS, metrics.P95LatencyMS, metrics.OutputTokensPerSecond = meanOf(ttft), meanOf(latency), meanOf(throughput)
	return metrics
}

func meanOf(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	mean := total / float64(len(values))
	return &mean
}

// Cleanup cancels a window still running; the agent deletes its setup on
// its next report. Nothing else of a run outlives its windows.
func (d AgentDriver) Cleanup(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) error {
	campaign, _, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return err
	}
	state, ok := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON)
	if !ok || state.Agent == nil || state.Agent.Pending == "" {
		return nil
	}
	if err = d.Windows.CancelWindow(ctx, candidate.TenantID, campaign.AgentID, state.Agent.Pending); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return operations.Retryable("optimization_agent_window_cancel_failed", err)
	}
	return nil
}

// SiteDriver runs each step where the run's target is measured: by the
// agent for a run on an agent's endpoint, by the control plane's own
// lifecycle otherwise. It is composition by where the target runs, never a
// branch on a provider or runtime name.
type SiteDriver struct {
	Repository Repository
	Control    Driver
	// Agent is nil when this control plane does not measure through agents;
	// a run on an agent's endpoint then stops with a clear code.
	Agent Driver
}

func (d SiteDriver) pick(ctx context.Context, candidate domain.OptimizationCandidateRun) (Driver, error) {
	if d.Repository == nil || d.Control == nil {
		return nil, operations.Permanent("optimization_driver_invalid", errors.New("site driver requires a repository and the control plane's driver"))
	}
	campaign, err := d.Repository.OptimizationCampaign(ctx, candidate.TenantID, candidate.CampaignID)
	if err != nil {
		return nil, err
	}
	if !AgentRun(campaign) {
		return d.Control, nil
	}
	if d.Agent == nil {
		return nil, operations.Permanent("optimization_agent_runs_unavailable", errors.New("this control plane does not measure runs through agents"))
	}
	return d.Agent, nil
}

func (d SiteDriver) Provision(ctx context.Context, id string, candidate domain.OptimizationCandidateRun, budget Budget) (ProvisionResult, error) {
	driver, err := d.pick(ctx, candidate)
	if err != nil {
		return ProvisionResult{}, err
	}
	return driver.Provision(ctx, id, candidate, budget)
}

func (d SiteDriver) Measure(ctx context.Context, id string, candidate domain.OptimizationCandidateRun, budget Budget) (MeasurementResult, error) {
	driver, err := d.pick(ctx, candidate)
	if err != nil {
		return MeasurementResult{}, err
	}
	return driver.Measure(ctx, id, candidate, budget)
}

func (d SiteDriver) Validate(ctx context.Context, id string, candidate domain.OptimizationCandidateRun) (ValidationResult, error) {
	driver, err := d.pick(ctx, candidate)
	if err != nil {
		return ValidationResult{}, err
	}
	return driver.Validate(ctx, id, candidate)
}

func (d SiteDriver) Rank(ctx context.Context, id string, candidate domain.OptimizationCandidateRun) (RankingResult, error) {
	driver, err := d.pick(ctx, candidate)
	if err != nil {
		return RankingResult{}, err
	}
	return driver.Rank(ctx, id, candidate)
}

func (d SiteDriver) Guard(ctx context.Context, id string, candidate domain.OptimizationCandidateRun) (GuardResult, error) {
	driver, err := d.pick(ctx, candidate)
	if err != nil {
		return GuardResult{}, err
	}
	return driver.Guard(ctx, id, candidate)
}

func (d SiteDriver) Cleanup(ctx context.Context, id string, candidate domain.OptimizationCandidateRun) error {
	driver, err := d.pick(ctx, candidate)
	if err != nil {
		return err
	}
	return driver.Cleanup(ctx, id, candidate)
}
