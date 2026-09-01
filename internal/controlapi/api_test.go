package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/asyncinference"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/contextpassport"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/hfcatalog"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/intentplan"
	"github.com/infercrane/infercrane/internal/modelapicatalog"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizedartifact"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/trainingartifact"
)

type fakeStore struct {
	operation       domain.Operation
	cancelled       bool
	err             error
	created         bool
	principal       domain.Principal
	targets         []domain.Target
	resolved        domain.ResolvedDeployment
	revisions       []domain.DeploymentRevision
	artifact        domain.ModelArtifact
	benchmarks      []domain.BenchmarkResult
	replicas        []domain.Replica
	activeOperation domain.Operation
	sloPolicy       domain.SLOPolicy
	recommendations []domain.InferenceRecommendation
	capacity        domain.CapacityEvidence
	consoleIdentity domain.ConsoleIdentity
	operations      []domain.Operation
	principals      []domain.Principal
	consoleMembers  []domain.ConsoleIdentity
	sandboxRefs     []domain.SandboxReference
	trainingRows    []domain.TrainingArtifactHandoff
}

type fakeOptimizationCosts struct{}

type fakeLaunchProber struct {
	evidence provision.LaunchProbeEvidence
	err      error
}

func (p fakeLaunchProber) ProbeLaunch(context.Context, provision.LaunchProbeRequest) (provision.LaunchProbeEvidence, error) {
	return p.evidence, p.err
}

func (fakeOptimizationCosts) Quote(_ context.Context, _ optimizer.DeploymentDraft, requiredUntil time.Time) (optimizationcampaign.CostQuote, error) {
	return optimizationcampaign.CostQuote{HourlyUSD: 2, Source: "test-price-list", ObservedAt: requiredUntil.Add(-time.Hour), ValidUntil: requiredUntil.Add(time.Hour)}, nil
}

type fakeMembershipStore struct {
	*fakeStore
	instances []domain.ControlPlaneInstance
}
type fakeEndpointStore struct {
	*fakeStore
	resolvedEndpoint domain.ResolvedEndpoint
	binding          domain.BackendBinding
}

type fakeProviderEndpointStore struct {
	*fakeEndpointStore
	connections []domain.ProviderConnection
}

type fakeManagedBillingStore struct {
	*fakeStore
	wallet             domain.ManagedWallet
	ledger             []domain.ManagedWalletLedgerEntry
	reservations       []domain.ManagedUsageReservation
	reservationTenant  string
	reservationState   string
	settledReservation domain.ManagedUsageReservation
	releasedTenant     string
	releasedID         string
	releaseReason      string
	payment            domain.ManagedPaymentEvent
	paymentResult      domain.ManagedPaymentResult
}

func (f *fakeManagedBillingStore) ManagedWallet(context.Context, string) (domain.ManagedWallet, error) {
	return f.wallet, f.err
}

func (f *fakeManagedBillingStore) ManagedWalletLedger(context.Context, string, int) ([]domain.ManagedWalletLedgerEntry, error) {
	return f.ledger, f.err
}

func (f *fakeManagedBillingStore) ManagedUsageReservations(_ context.Context, tenant, state string, _ int) ([]domain.ManagedUsageReservation, error) {
	f.reservationTenant, f.reservationState = tenant, state
	return f.reservations, f.err
}

func (f *fakeManagedBillingStore) CreditManagedWallet(context.Context, string, string, string, int64) (domain.ManagedWallet, error) {
	return f.wallet, f.err
}

func (f *fakeManagedBillingStore) SettleManagedUsage(_ context.Context, tenant, id string, settlement domain.ManagedUsageSettlement) (domain.ManagedUsageReservation, error) {
	f.settledReservation = domain.ManagedUsageReservation{ID: id, TenantID: tenant, State: "settled", InputTokens: settlement.InputTokens, OutputTokens: settlement.OutputTokens}
	return f.settledReservation, f.err
}

func (f *fakeManagedBillingStore) ReleaseManagedUsage(_ context.Context, tenant, id, reason string) error {
	f.releasedTenant, f.releasedID, f.releaseReason = tenant, id, reason
	return f.err
}

func (f *fakeManagedBillingStore) ProcessManagedPaymentEvent(_ context.Context, payment domain.ManagedPaymentEvent) (domain.ManagedPaymentResult, error) {
	f.payment = payment
	return f.paymentResult, f.err
}

type fakeManagedCheckoutProvider struct {
	tenant  string
	amount  int64
	session domain.ManagedCheckoutSession
	payment domain.ManagedPaymentEvent
	err     error
}

func (f *fakeManagedCheckoutProvider) CreateCheckoutSession(_ context.Context, tenant string, amount int64) (domain.ManagedCheckoutSession, error) {
	f.tenant, f.amount = tenant, amount
	return f.session, f.err
}

func (f *fakeManagedCheckoutProvider) ParseWebhook(_ []byte, signature string) (domain.ManagedPaymentEvent, error) {
	if signature == "" {
		return domain.ManagedPaymentEvent{}, errors.New("signature is required")
	}
	return f.payment, f.err
}

func (f *fakeProviderEndpointStore) CreateProviderConnection(_ context.Context, tenant string, item domain.ProviderConnection) (domain.ProviderConnection, error) {
	item.ID, item.TenantID = "connection", tenant
	item.TargetName, item.SecretReferenceName = "provider-openrouter", "openrouter"
	f.connections = append(f.connections, item)
	return item, f.err
}
func (f *fakeProviderEndpointStore) ProviderConnectionForTenant(_ context.Context, _, name string) (domain.ProviderConnection, error) {
	for _, item := range f.connections {
		if item.Name == name {
			return item, f.err
		}
	}
	return domain.ProviderConnection{}, domain.ErrNotFound
}
func (f *fakeProviderEndpointStore) ProviderConnectionsForTenant(context.Context, string) ([]domain.ProviderConnection, error) {
	return f.connections, f.err
}
func (f *fakeProviderEndpointStore) DeleteProviderConnectionForTenant(_ context.Context, _, name string) error {
	for index, item := range f.connections {
		if item.Name == name {
			f.connections = append(f.connections[:index], f.connections[index+1:]...)
			return f.err
		}
	}
	return domain.ErrNotFound
}

func (f *fakeEndpointStore) CreateEnvironment(context.Context, string, domain.Environment) (domain.Environment, error) {
	return domain.Environment{}, f.err
}
func (f *fakeEndpointStore) EnvironmentsForTenant(context.Context, string) ([]domain.Environment, error) {
	return nil, f.err
}
func (f *fakeEndpointStore) EnvironmentForTenant(context.Context, string, string) (domain.Environment, error) {
	return domain.Environment{}, f.err
}
func (f *fakeEndpointStore) CreateLogicalModel(context.Context, string, domain.LogicalModel) (domain.LogicalModel, error) {
	return domain.LogicalModel{}, f.err
}
func (f *fakeEndpointStore) LogicalModelsForTenant(context.Context, string) ([]domain.LogicalModel, error) {
	return nil, f.err
}
func (f *fakeEndpointStore) LogicalModelForTenant(context.Context, string, string) (domain.LogicalModel, error) {
	return domain.LogicalModel{}, f.err
}
func (f *fakeEndpointStore) CreateEndpoint(context.Context, string, domain.Endpoint) (domain.Endpoint, error) {
	return domain.Endpoint{}, f.err
}
func (f *fakeEndpointStore) EndpointsForTenant(context.Context, string) ([]domain.Endpoint, error) {
	return nil, f.err
}
func (f *fakeEndpointStore) ResolveEndpointForTenant(context.Context, string, string) (domain.ResolvedEndpoint, error) {
	return f.resolvedEndpoint, f.err
}
func (f *fakeEndpointStore) CreateBackendBinding(_ context.Context, tenant string, binding domain.BackendBinding) (domain.BackendBinding, error) {
	binding.ID, binding.TenantID = "binding", tenant
	f.binding = binding
	return binding, f.err
}
func (f *fakeEndpointStore) CreateServingPlan(context.Context, string, domain.ServingPlan) (domain.ServingPlan, error) {
	return domain.ServingPlan{}, f.err
}
func (f *fakeEndpointStore) SetEndpointPlan(context.Context, string, string, string, string) error {
	return f.err
}
func (f *fakeEndpointStore) DeleteEndpointForTenant(context.Context, string, string) error {
	return f.err
}
func (f *fakeEndpointStore) EndpointReleaseGuardPolicy(context.Context, string, string) (domain.EndpointReleaseGuardPolicy, error) {
	return domain.EndpointReleaseGuardPolicy{}, f.err
}
func (f *fakeEndpointStore) SetEndpointReleaseGuardPolicy(context.Context, string, string, domain.EndpointReleaseGuardPolicy) (domain.EndpointReleaseGuardPolicy, error) {
	return domain.EndpointReleaseGuardPolicy{}, f.err
}
func (f *fakeEndpointStore) EvaluateEndpointReleaseGuard(context.Context, string, string, time.Duration) (domain.EndpointReleaseGuardEvaluation, error) {
	return domain.EndpointReleaseGuardEvaluation{}, f.err
}
func (f *fakeEndpointStore) EndpointReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.EndpointReleaseGuardEvaluation, error) {
	return nil, f.err
}
func (f *fakeEndpointStore) EndpointReleaseGuardAccepted(context.Context, string, string, string) (bool, error) {
	return false, f.err
}

type fakeContextBurstStore struct {
	*fakeStore
	passport domain.ContextPassport
}

type fakeAlertStore struct {
	*fakeStore
	policies []domain.AlertPolicy
}

func (f *fakeAlertStore) CreateAlertPolicy(_ context.Context, tenant, _ string, item domain.AlertPolicy) (domain.AlertPolicy, error) {
	item.ID, item.TenantID, item.EndpointID = "alert-1", tenant, "endpoint-1"
	item.CreatedAt, item.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	f.policies = append(f.policies, item)
	return item, nil
}
func (f *fakeAlertStore) AlertPoliciesForEndpoint(context.Context, string, string) ([]domain.AlertPolicy, error) {
	return f.policies, nil
}

type fakeOptimizationStore struct {
	*fakeStore
	report domain.FinOpsReport
}

type fakeOptimizationCampaignStore struct {
	*fakeStore
	campaign domain.OptimizationCampaign
}

type fakeOptimizedArtifactStore struct {
	*fakeStore
	artifact domain.OptimizedArtifact
}

func (f *fakeOptimizedArtifactStore) CreateOptimizedArtifact(_ context.Context, tenant, key string, plan optimizedartifact.Plan) (domain.OptimizedArtifact, bool, error) {
	digest, err := optimizedartifact.InputDigest(plan)
	if err != nil {
		return domain.OptimizedArtifact{}, false, err
	}
	if f.artifact.ID != "" {
		return f.artifact, false, f.err
	}
	f.artifact = domain.OptimizedArtifact{ID: "optimized-1", TenantID: tenant, IdempotencyKey: key, InputDigest: digest, BaseModelArtifactID: plan.BaseModelArtifactID, Kind: plan.Kind, Format: plan.Format, Tool: plan.Tool, ToolVersion: plan.ToolVersion, Algorithm: plan.Algorithm, BuilderImageDigest: plan.BuilderImageDigest, CalibrationDigest: plan.CalibrationDigest, LicenseSPDX: plan.LicenseSPDX, ConfigurationJSON: string(plan.Configuration), HardwareConstraintsJSON: string(plan.HardwareConstraints), RequiresQualityReview: plan.RequiresQualityReview, State: optimizedartifact.StatePlanned, EvidenceState: "unmeasured", BuildEvidenceJSON: `{}`}
	return f.artifact, true, f.err
}
func (f *fakeOptimizedArtifactStore) OptimizedArtifact(context.Context, string, string) (domain.OptimizedArtifact, bool, error) {
	if f.artifact.ID == "" {
		return domain.OptimizedArtifact{}, false, domain.ErrNotFound
	}
	return f.artifact, true, f.err
}
func (f *fakeOptimizedArtifactStore) OptimizedArtifacts(context.Context, string, int) ([]domain.OptimizedArtifact, error) {
	if f.artifact.ID == "" {
		return nil, f.err
	}
	return []domain.OptimizedArtifact{f.artifact}, f.err
}
func (f *fakeOptimizedArtifactStore) BeginOptimizedArtifactBuild(context.Context, string, string) (domain.OptimizedArtifact, error) {
	f.artifact.State = optimizedartifact.StateBuilding
	return f.artifact, f.err
}
func (f *fakeOptimizedArtifactStore) AttestOptimizedArtifact(_ context.Context, _, _ string, state string, attestation optimizedartifact.Attestation) (domain.OptimizedArtifact, error) {
	f.artifact.State, f.artifact.OutputRepository, f.artifact.OutputImmutableRevision, f.artifact.OutputDigest, f.artifact.FailureCode = state, attestation.OutputRepository, attestation.OutputImmutableRevision, attestation.OutputDigest, attestation.FailureCode
	f.artifact.BuildEvidenceJSON = string(attestation.BuildEvidence)
	if state == optimizedartifact.StateReady {
		f.artifact.EvidenceState = "measured"
	} else {
		f.artifact.EvidenceState = "rejected"
	}
	return f.artifact, f.err
}
func (f *fakeOptimizedArtifactStore) QualifyOptimizedArtifact(_ context.Context, _, _, _, qualityID string) (domain.OptimizedArtifact, error) {
	f.artifact.EvidenceState, f.artifact.QualityEvidenceID = "qualified", qualityID
	return f.artifact, f.err
}

func (f *fakeOptimizationCampaignStore) CreateOptimizationCampaign(_ context.Context, campaign domain.OptimizationCampaign, candidates []domain.OptimizationCandidateRun) (domain.OptimizationCampaign, bool, error) {
	if f.campaign.ID != "" {
		return f.campaign, false, f.err
	}
	campaign.ID, campaign.State, campaign.MaxCandidates = "campaign-1", optimizationcampaign.CampaignAwaitingApproval, len(candidates)
	campaign.CreatedAt, campaign.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	for index := range candidates {
		candidates[index].ID, candidates[index].CampaignID, candidates[index].TenantID, candidates[index].State = "candidate-"+string(rune('1'+index)), campaign.ID, campaign.TenantID, optimizationcampaign.CandidateProposed
		candidates[index].CreatedAt, candidates[index].UpdatedAt = campaign.CreatedAt, campaign.UpdatedAt
	}
	campaign.Candidates = candidates
	f.campaign = campaign
	return campaign, true, f.err
}
func (f *fakeOptimizationCampaignStore) OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error) {
	if f.campaign.ID == "" {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.campaign, f.err
}
func (f *fakeOptimizationCampaignStore) OptimizationCampaigns(context.Context, string, int) ([]domain.OptimizationCampaign, error) {
	if f.campaign.ID == "" {
		return []domain.OptimizationCampaign{}, f.err
	}
	return []domain.OptimizationCampaign{f.campaign}, f.err
}
func (f *fakeOptimizationCampaignStore) ApproveOptimizationCampaign(_ context.Context, _, _ string, actor string, cost float64, expires time.Time) (domain.OptimizationCampaign, error) {
	f.campaign.State, f.campaign.ApprovedBy, f.campaign.ApprovedMaxCostUSD, f.campaign.ApprovalExpiresAt = optimizationcampaign.CampaignApproved, actor, &cost, &expires
	now := time.Now().UTC()
	f.campaign.ApprovedAt = &now
	return f.campaign, f.err
}
func (f *fakeOptimizationCampaignStore) CancelOptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error) {
	f.campaign.State, f.campaign.CancelRequested = optimizationcampaign.CampaignCancelled, true
	return f.campaign, f.err
}

type fakeIntelligenceStore struct {
	*fakeStore
	trace             domain.ReplayTrace
	artifact          domain.ModelArtifact
	artifactErr       error
	prefetch          domain.ArtifactPrefetch
	updateErr         error
	cacheObservations []domain.ArtifactCacheObservation
}

type fakeQualityEvidenceStore struct {
	*fakeStore
	evidence []domain.QualityEvidence
}

type fakeMonitoringStore struct {
	*fakeStore
	snapshot domain.EndpointMonitoringSnapshot
}

type fakeOperationalMeasurementStore struct {
	*fakeStore
	rows []domain.OperationalMeasurement
}

type fakeCostEvidenceStore struct {
	*fakeStore
	rows []domain.CostEvidence
}

type fakeCostOptimizationStore struct {
	*fakeOptimizationStore
	costs []domain.CostEvidence
}

func (f *fakeCostOptimizationStore) RecordCostEvidence(_ context.Context, _, _ string, rows []domain.CostEvidence) ([]domain.CostEvidence, error) {
	f.costs = append(f.costs, rows...)
	return rows, f.err
}

func (f *fakeCostOptimizationStore) CostEvidenceForDeployment(context.Context, string, string, time.Time, time.Time, int) ([]domain.CostEvidence, error) {
	return f.costs, f.err
}

func (f *fakeCostEvidenceStore) RecordCostEvidence(_ context.Context, tenant, deployment string, rows []domain.CostEvidence) ([]domain.CostEvidence, error) {
	for index := range rows {
		rows[index].ID = "cost"
		rows[index].TenantID = tenant
		rows[index].Deployment = deployment
	}
	f.rows = append(f.rows, rows...)
	return rows, f.err
}

func (f *fakeCostEvidenceStore) CostEvidenceForDeployment(context.Context, string, string, time.Time, time.Time, int) ([]domain.CostEvidence, error) {
	return f.rows, f.err
}

func (f *fakeOperationalMeasurementStore) RecordOperationalMeasurements(_ context.Context, tenant, deployment string, rows []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error) {
	for index := range rows {
		rows[index].ID = "measurement"
		rows[index].TenantID = tenant
		rows[index].Deployment = deployment
	}
	f.rows = append(f.rows, rows...)
	return rows, f.err
}

func (f *fakeMonitoringStore) EndpointMonitoring(context.Context, string, string, time.Duration, time.Duration) (domain.EndpointMonitoringSnapshot, error) {
	return f.snapshot, f.err
}

func TestEndpointMonitoringIsAuthenticatedBoundedAndContentFree(t *testing.T) {
	now := time.Now().UTC()
	value := .02
	store := &fakeMonitoringStore{fakeStore: &fakeStore{}, snapshot: domain.EndpointMonitoringSnapshot{Endpoint: "coder-production", LogicalModel: "coder", Environment: "production", WindowStart: now.Add(-time.Hour), WindowEnd: now, BucketSeconds: 60, Summary: domain.MonitoringSummary{Requests: 20, ErrorRate: &value}, Series: []domain.MonitoringBucket{}, Breakdowns: []domain.MonitoringBreakdown{}, Events: []domain.MonitoringEvent{}, Evidence: domain.MonitoringEvidence{Source: "infercrane_gateway_request_records", SampleCount: 20, Fresh: true, ContentRecorded: false, Available: []string{}, Unavailable: []string{}}}}
	pool := admission.New()
	pool.Replace([]admission.Policy{{Key: "global\x00coder-production", MaxConcurrency: 4, MaxQueueDepth: 8, QueueTimeout: time.Second, MaxRequestBytes: 4096, MaxOutputTokens: 4096, AllowedPriorities: map[string]struct{}{"normal": {}}, Enabled: true}})
	handler := (API{Store: store, APIKey: "secret", AdmissionState: pool, GatewayInstanceID: "gateway-a"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/endpoints/coder-production/monitoring?window_seconds=3600&bucket_seconds=60", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"endpoint":"coder-production"`) || !strings.Contains(response.Body.String(), `"content_recorded":false`) || !strings.Contains(response.Body.String(), `"capacity_state":"accepting"`) || !strings.Contains(response.Body.String(), `"scope":"gateway_instance"`) || !strings.Contains(response.Body.String(), `"instance_id":"gateway-a"`) || strings.Contains(response.Body.String(), "prompt") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/endpoints/coder-production/monitoring?window_seconds=2592000&bucket_seconds=60", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unbounded points status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/endpoints/coder-production/monitoring?unexpected=1", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown query status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOperationalMeasurementIngestionIsAuthenticatedStrictAndContentFree(t *testing.T) {
	store := &fakeOperationalMeasurementStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	body := `{"source":"dcgm_exporter","evidence_class":"measured","replica_id":"replica-1","observed_at":"2026-08-19T20:00:00Z","valid_until":"2026-08-19T20:02:00Z","measurements":[{"name":"gpu_utilization","value":72,"unit":"percent","sample_count":2}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/measurements", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.rows) != 1 || store.rows[0].Name != "gpu_utilization" || strings.Contains(response.Body.String(), "prompt") || !strings.Contains(response.Body.String(), `"content_recorded":false`) {
		t.Fatalf("status=%d body=%s rows=%+v", response.Code, response.Body.String(), store.rows)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/measurements", strings.NewReader(strings.Replace(body, `"measurements"`, `"unknown":true,"measurements"`, 1)))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/measurements", strings.NewReader(body))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCostEvidenceIngestionIsAuthenticatedStrictMeasuredAndCurrencyExplicit(t *testing.T) {
	store := &fakeCostEvidenceStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	body := `{"source":"opencost/allocation","currency":"USD","evidence_class":"measured","observed_at":"2026-08-19T20:00:00Z","valid_until":"2026-08-19T21:00:00Z","allocations":[{"scope":"deployment_hourly_rate/inference","resource":"inference","billing_unit":"hour","amount":1.25,"window_start":"2026-08-19T19:00:00Z","window_end":"2026-08-19T20:00:00Z"}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/cost-evidence", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.rows) != 1 || store.rows[0].Currency != "USD" || store.rows[0].Amount != 1.25 || strings.Contains(response.Body.String(), "prompt") || !strings.Contains(response.Body.String(), `"currency_converted":false`) {
		t.Fatalf("status=%d body=%s rows=%+v", response.Code, response.Body.String(), store.rows)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/cost-evidence", strings.NewReader(strings.Replace(body, `"allocations"`, `"unknown":true,"allocations"`, 1)))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/cost-evidence", strings.NewReader(strings.Replace(body, `"evidence_class":"measured"`, `"evidence_class":"provider_reported"`, 1)))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || len(store.rows) != 1 || !strings.Contains(response.Body.String(), "provider-reported rates come only from provider adapters") {
		t.Fatalf("caller minted provider authority: status=%d body=%s rows=%+v", response.Code, response.Body.String(), store.rows)
	}
}

func TestModelCatalogIsAuthenticatedSearchableAndTruthful(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/catalog/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/models?query=embeddings", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"bge-m3-embeddings"`) || !strings.Contains(response.Body.String(), `"performance_claims":false`) || strings.Contains(response.Body.String(), `"name":"qwen3-8b"`) {
		t.Fatalf("catalog status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/catalog/models/qwen3-8b", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":"b968826d9c46dd6066d109eabc6255188de91218"`) || !strings.Contains(response.Body.String(), `"evidence_class":"configuration-verified"`) || !strings.Contains(response.Body.String(), `"qualification_scope"`) {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/catalog/models?limit=10", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unexpected query status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPICatalogPaginatesAndNeverExposesSupplierRouting(t *testing.T) {
	price := int64(125000)
	catalog := modelapicatalog.Catalog{SchemaVersion: modelapicatalog.SchemaVersion, Models: []modelapicatalog.Model{{
		ID: "private-model", DisplayName: "Private Model", Publisher: "Publisher", PublisherSlug: "publisher", Family: "Private", Description: "Managed model.",
		Tasks: []string{"chat"}, Capabilities: []string{"streaming"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		Access: "ready", Qualification: "supplier-reported", QualificationNote: "Managed contract observed.",
		Offers: []modelapicatalog.Offer{{ID: "secret-offer", Supplier: "Secret Supplier", SupplierSlug: "secret-supplier", SupplierModelID: "supplier/private", Adapter: "openai-compatible-external", Protocol: "openai", Access: "connect-provider", Availability: "available", Regions: []string{"supplier-private-region"}, SourceURL: "https://supplier.example/private", Pricing: &modelapicatalog.Pricing{Currency: "USD", InputMicrousdPerMillion: &price, Provenance: "Secret Supplier contract", ObservedAt: "2026-08-30T00:00:00Z", ValidUntil: "2099-01-01T00:00:00Z"}}},
	}}}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ModelAPICatalog: catalog}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog?task=chat&limit=1", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"display_name":"Private Model"`) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
	if !strings.Contains(body, `"access":"request-access"`) || strings.Contains(body, `"access":"connect-provider"`) {
		t.Fatalf("customer response exposed a supplier connection action: %s", body)
	}
	if !strings.Contains(body, `"id":"infercrane-standard"`) || !strings.Contains(body, `"provenance":"InferCrane rate card"`) {
		t.Fatalf("customer response did not present the InferCrane service contract: %s", body)
	}
	if !strings.Contains(body, `"qualification":"reported"`) {
		t.Fatalf("customer response did not normalize internal qualification: %s", body)
	}
	for _, secret := range []string{"Secret Supplier", "secret-offer", "secret-supplier", "supplier/private", "openai-compatible-external", "supplier.example", "supplier-private-region", "Secret Supplier contract"} {
		if strings.Contains(body, secret) {
			t.Fatalf("customer response exposed supplier detail %q: %s", secret, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "supplier") {
		t.Fatalf("customer response exposed procurement vocabulary: %s", body)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog?limit=101", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHuggingFaceCatalogReturnsOnlyNormalizedCachedMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"Qwen/Qwen3-8B","author":"Qwen","sha":"b968826d9c46dd6066d109eabc6255188de91218","private":false,"gated":false,"pipeline_tag":"text-generation","cardData":{"license":"apache-2.0"},"siblings":[{"rfilename":"do-not-expose.bin"}]}`))
	}))
	defer server.Close()
	cache, err := hfcatalog.New([]string{"Qwen/Qwen3-8B"})
	if err != nil {
		t.Fatal(err)
	}
	if err = cache.Refresh(context.Background(), hfcatalog.Client{BaseURL: server.URL, HTTPClient: server.Client()}); err != nil {
		t.Fatal(err)
	}
	handler := (API{APIKey: "secret", HFCatalog: cache}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/hugging-face/models?query=qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{`"schema_version":"infercrane.hugging-face-catalog/v1"`, `"repository":"Qwen/Qwen3-8B"`, `"provider":"huggingface_hub_api"`, `"current":true`, `not an InferCrane-reviewed deployment recipe`} {
		if response.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("Hugging Face catalog status=%d missing=%q body=%s", response.Code, expected, body)
		}
	}
	if strings.Contains(body, "do-not-expose.bin") || strings.Contains(body, `"siblings"`) {
		t.Fatalf("raw upstream payload crossed the normalized catalog boundary: %s", body)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/catalog/hugging-face/models?limit=10", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported filter status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPICatalogDoesNotPublishExpiredRateCard(t *testing.T) {
	price := int64(125000)
	catalog := modelapicatalog.Catalog{SchemaVersion: modelapicatalog.SchemaVersion, Models: []modelapicatalog.Model{{
		ID: "expired-model", DisplayName: "Expired Model", Publisher: "Publisher", PublisherSlug: "publisher", Family: "Expired", Description: "Managed model.",
		Tasks: []string{"chat"}, Capabilities: []string{"streaming"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		Access: "ready", Qualification: "supplier-reported", QualificationNote: "Managed contract observed.",
		Offers: []modelapicatalog.Offer{{ID: "expired-offer", Supplier: "internal", SupplierSlug: "internal", SupplierModelID: "private", Adapter: "openai-compatible-external", Protocol: "openai", Access: "ready", Availability: "available", Pricing: &modelapicatalog.Pricing{Currency: "USD", InputMicrousdPerMillion: &price, Provenance: "private", ObservedAt: "1999-01-01T00:00:00Z", ValidUntil: "2000-01-01T00:00:00Z"}}},
	}}}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ModelAPICatalog: catalog}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog/expired-model", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"access":"request-access"`) || !strings.Contains(body, `"availability":"unknown"`) || strings.Contains(body, `"pricing"`) {
		t.Fatalf("expired rate card was not hidden: status=%d body=%s", response.Code, body)
	}
}

func TestComputeCatalogSeparatesPriceFromExecutionReadiness(t *testing.T) {
	now := time.Now().UTC()
	handler := (API{
		Store:  &fakeStore{},
		APIKey: "secret",
		ComputeProviders: []ComputeProvider{
			{ID: "runpod", Label: "RunPod", Mode: "control-plane", State: "connection-required", Reason: "RUNPOD_API_KEY is not configured"},
		},
		GPUPrices: []GPUPriceObservation{
			{Provider: "runpod", Region: "eu", GPU: "L40S", GPUCount: 1, Replicas: 1, Currency: "USD", HourlyUSD: 0.42, CostScope: string(pricing.CostScopeInstanceTotal), PriceAuthority: string(pricing.PriceAuthorityProviderAPI), Source: "test catalog", ObservedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)},
		},
	}).Handler()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/gpu-prices", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"hourly_usd":0.42`) || !strings.Contains(response.Body.String(), `"cost_scope":"instance_total"`) || !strings.Contains(response.Body.String(), `"availability":"unknown"`) || !strings.Contains(response.Body.String(), `"current":true`) {
		t.Fatalf("price catalog status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"hourly_usd":0.42`) {
		t.Fatalf("public price catalog status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/compute/providers", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"connection-required"`) {
		t.Fatalf("compute readiness status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPublicGPUPricesFilterAndPaginateWithoutDumpingCatalog(t *testing.T) {
	now := time.Now().UTC()
	handler := (API{
		Store:  &fakeStore{},
		APIKey: "secret",
		GPUPrices: []GPUPriceObservation{
			{Provider: "runpod", Region: "eu", GPU: "L40S", GPUCount: 1, Replicas: 1, Currency: "USD", HourlyUSD: 0.42, Source: "test", ObservedAt: now, ValidUntil: now.Add(time.Hour)},
			{Provider: "lambda", Region: "us", GPU: "H100", GPUCount: 1, Replicas: 1, Currency: "USD", HourlyUSD: 2.49, Source: "test", ObservedAt: now, ValidUntil: now.Add(time.Hour)},
			{Provider: "runpod", Region: "us", GPU: "H100", GPUCount: 1, Replicas: 1, Currency: "USD", HourlyUSD: 1.99, Source: "test", ObservedAt: now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Hour)},
		},
	}).Handler()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices?q=h100&current=true&limit=1&offset=0&sort=hourly_usd", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"provider":"lambda"`) || strings.Contains(body, `"provider":"runpod"`) {
		t.Fatalf("filtered catalog status=%d body=%s", response.Code, body)
	}
	if !strings.Contains(body, `"total":1`) || !strings.Contains(body, `"providers":1`) || !strings.Contains(body, `"has_more":false`) {
		t.Fatalf("filtered catalog metadata missing: %s", body)
	}
	if response.Header().Get("Cache-Control") == "" || response.Header().Get("Last-Modified") == "" {
		t.Fatalf("public cache evidence missing: %#v", response.Header())
	}
	if strings.Contains(response.Header().Get("Cache-Control"), "stale-while-revalidate") {
		t.Fatalf("current price evidence may not be served stale: %q", response.Header().Get("Cache-Control"))
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices?limit=101", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_query"`) {
		t.Fatalf("invalid public limit status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPublicGPUPricesFilterAcceptsReviewedRunPodAlias(t *testing.T) {
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("runpod", map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 0.74, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "runpod provider API", ObservedAt: now, StaleAfter: time.Minute,
		},
	})
	handler := (API{GPUPriceCatalog: catalog}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices?provider=runpod&gpu=L40S&current=true", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"gpu":"NVIDIA L40S"`) {
		t.Fatalf("canonical RunPod alias did not filter exact provider SKU: code=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "price_guaranteed_until") {
		t.Fatalf("unlocked marketplace quote claimed a guarantee: %s", response.Body.String())
	}
}

func TestCloudDeploymentFailsClosedWithoutReadyCompute(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "runpod", Label: "RunPod", State: "connection-required"}}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","min_replicas":1,"max_replicas":1}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "test-compute-readiness")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"compute_connection_required"`) {
		t.Fatalf("deployment status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestIntentPlanningReturnsEditableTruthBoundedConfiguration(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: .74, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now, StaleAfter: time.Hour},
	})
	handler := (API{APIKey: "secret", Integrations: registry.Snapshot(), ComputeProviders: []ComputeProvider{{ID: "runpod", Label: "RunPod", State: "ready"}, {ID: "aws", Label: "AWS", State: "connection-required", Reason: "AWS workload identity is not configured"}}, GPUPriceCatalog: catalog}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/planning/intents", strings.NewReader(`{"intent":"Deploy Qwen/Qwen3-8B for low latency on RunPod"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{`"schema_version":"infercrane.intent-plan/v1"`, `"status":"ready"`, `"repository":"Qwen/Qwen3-8B"`, `"profile":"vllm-interactive"`, `"deployment_draft":{"name":"qwen3-8b"`, `"cloud":"runpod"`, `"provider_adapter":"runpod-pods"`, `"hourly_usd_per_replica":0.74`, `"performance":"unmeasured"`, `"capacity_reserved":false`, `"performance_claims":false`, `"editable":true`} {
		if response.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("intent plan status=%d missing=%q body=%s", response.Code, expected, body)
		}
	}
	if strings.Contains(body, `"capacity":"available"`) || strings.Contains(body, `"provider_mutation":true`) {
		t.Fatalf("intent plan fabricated capacity or mutation: %s", body)
	}
	var envelope intentplan.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.DeploymentDraft == nil {
		t.Fatalf("ready response omitted deployment draft: %s", body)
	}
	if err := envelope.DeploymentDraft.Validate(); err != nil {
		t.Fatalf("response draft failed CloudRequest validation: %v body=%s", err, body)
	}

	needsInput := httptest.NewRequest(http.MethodPost, "/api/v1/planning/intents", strings.NewReader(`{"intent":"deploy a model"}`))
	needsInput.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, needsInput)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"needs_input"`) || strings.Contains(response.Body.String(), `"deployment_draft"`) {
		t.Fatalf("needs-input plan exposed a deployment draft: status=%d body=%s", response.Code, response.Body.String())
	}

	unauthenticated := httptest.NewRequest(http.MethodPost, "/api/v1/planning/intents", strings.NewReader(`{"intent":"Deploy Qwen/Qwen3-8B"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, unauthenticated)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated intent plan status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestIntentPlanningRejectsUnboundedOrAmbiguousInputWithoutMutation(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	handler := (API{APIKey: "secret", Integrations: registry.Snapshot()}).Handler()
	tests := []struct {
		body string
		code int
	}{
		{`{"intent":"deploy a model","unknown":true}`, http.StatusBadRequest},
		{`{"intent":"deploy a model"}{}`, http.StatusBadRequest},
		{`{"intent":"deploy a model","objective":"magical"}`, http.StatusUnprocessableEntity},
		{`{"intent":"` + strings.Repeat("x", 17<<10) + `"}`, http.StatusBadRequest},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/planning/intents", strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.code {
			t.Fatalf("body bytes=%d status=%d want=%d response=%s", len(test.body), response.Code, test.code, response.Body.String())
		}
	}
}

func TestCapacityProbeKeepsCatalogQuoteSeparateFromLaunchEvidence(t *testing.T) {
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "EU-RO-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: 0.42, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "provider catalog", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
	})
	probeEvidence := provision.LaunchProbeEvidence{Provider: "runpod", Region: "EU-RO-1", GPU: "L40S", GPUCount: 1, ConnectionState: "configured", AvailabilityState: "constrained", QuotaState: "unknown", Deployability: "unknown", Source: "runpod.stock", ObservedAt: now, ExpiresAt: now.Add(30 * time.Second), Message: "low stock"}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "runpod", Label: "RunPod", State: "ready"}}, GPUPriceCatalog: catalog, LaunchProbers: map[string]provision.LaunchProber{"runpod": fakeLaunchProber{evidence: probeEvidence}}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/capacity/probes", strings.NewReader(`{"provider":"runpod","region":"EU-RO-1","gpu":"L40S","gpu_count":1}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{`"state":"current"`, `"hourly_usd":0.42`, `"availability_state":"constrained"`, `"quota_state":"unknown"`, `"deployability":"unknown"`, `only an accepted provider launch reserves capacity`} {
		if response.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("capacity probe status=%d missing=%q body=%s", response.Code, expected, body)
		}
	}
}

func TestCapacityProbeMatchesReviewedRunPodAliasToExactProviderSKU(t *testing.T) {
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA H100 80GB HBM3", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 2.69, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "runpod secure price", ObservedAt: now, StaleAfter: time.Minute,
		},
	})
	api := API{GPUPriceCatalog: catalog}
	quote := api.catalogLaunchQuote(provision.LaunchProbeRequest{Provider: "runpod", Region: "EU-RO-1", GPU: "H100", GPUCount: 1}, now)
	if quote.State != "current" || quote.HourlyUSD == nil || *quote.HourlyUSD != 2.69 || quote.GPU != "NVIDIA H100 80GB HBM3" {
		t.Fatalf("exact RunPod SKU was not selected: %#v", quote)
	}
}

func TestCapacityProbeDoesNotRankAcceleratorOnlyComponentAsInstanceTotal(t *testing.T) {
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{
		{Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 0.67, CostScope: pricing.CostScopeAcceleratorOnly, Source: "gcp billing component", ObservedAt: now, StaleAfter: time.Hour,
		},
	})
	quote := (API{GPUPriceCatalog: catalog}).catalogLaunchQuote(provision.LaunchProbeRequest{Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", GPUCount: 1}, now)
	if quote.State != "unavailable" || quote.HourlyUSD != nil {
		t.Fatalf("partial GCP component became a launch total: %#v", quote)
	}

	handler := (API{GPUPriceCatalog: catalog}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices?provider=gcp&current=true", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cost_scope":"accelerator_only"`) {
		t.Fatalf("partial price scope was not disclosed: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCapacityProbeDoesNotRankRepositorySnapshot(t *testing.T) {
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 0.72, CostScope: pricing.CostScopeInstanceTotal,
			Source: "https://raw.githubusercontent.com/example/catalog/main/prices.csv", ObservedAt: now, StaleAfter: time.Hour,
		},
	})
	quote := (API{GPUPriceCatalog: catalog}).catalogLaunchQuote(provision.LaunchProbeRequest{Provider: "runpod", GPU: "NVIDIA L40S", GPUCount: 1}, now)
	if quote.State != "unavailable" || quote.HourlyUSD != nil {
		t.Fatalf("repository snapshot became launch price authority: %#v", quote)
	}
}

func TestGPUPriceCatalogCanReturnOnlyDeploymentComparableTotals(t *testing.T) {
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(nil)
	catalog.ReplaceProvider("runpod", map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "L40S", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 0.74, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI,
			Source: "https://api.runpod.io/graphql", ObservedAt: now, StaleAfter: time.Hour,
		},
	})
	catalog.ReplaceProvider("gcp", map[pricing.Request]pricing.Estimate{
		{Cloud: "gcp", Region: "us-central1", GPU: "L4", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 0.67, CostScope: pricing.CostScopeAcceleratorOnly, Authority: pricing.PriceAuthorityProviderAPI,
			Source: "https://cloudbilling.googleapis.com/v1/services/compute/skus", ObservedAt: now, StaleAfter: time.Hour,
		},
	})
	catalog.ReplaceProvider("snapshot", map[pricing.Request]pricing.Estimate{
		{Cloud: "snapshot", Region: "global", GPU: "L40S", GPUCount: 1, Replicas: 1}: {
			Currency: "USD", Hourly: 0.50, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI,
			Source: "https://raw.githubusercontent.com/example/catalog/main/prices.csv", ObservedAt: now, StaleAfter: time.Hour,
		},
	})

	handler := (API{GPUPriceCatalog: catalog}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices?current=true&deployment_comparable=true&sort=hourly_usd", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"provider":"runpod"`) || !strings.Contains(body, `"deployment_comparable":true`) || strings.Contains(body, `"provider":"gcp"`) || strings.Contains(body, `"provider":"snapshot"`) || !strings.Contains(body, `"total":1`) {
		t.Fatalf("deployment-comparable catalog status=%d body=%s", response.Code, body)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/public/catalog/gpu-prices?deployment_comparable=maybe", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_query"`) {
		t.Fatalf("invalid comparable filter status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCapacityProbeFailsClosedWhenConnectionOrProviderProbeIsUnavailable(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "lambda", Label: "Lambda", State: "connection-required", Reason: "credential missing"}}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/capacity/probes", strings.NewReader(`{"provider":"lambda","gpu":"H100"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"state":"unavailable"`) || !strings.Contains(body, `"connection_state":"connection-required"`) || !strings.Contains(body, `"deployability":"unknown"`) || !strings.Contains(body, `credential missing`) {
		t.Fatalf("capacity probe status=%d body=%s", response.Code, body)
	}

	now := time.Now().UTC()
	handler = (API{Store: &fakeStore{}, APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "runpod", State: "ready"}}, LaunchProbers: map[string]provision.LaunchProber{"runpod": fakeLaunchProber{evidence: provision.LaunchProbeEvidence{Provider: "runpod", GPU: "H100", GPUCount: 1, Source: "runpod.stock", ObservedAt: now, ExpiresAt: now.Add(time.Second)}, err: errors.New("private transport failure")}}}).Handler()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/capacity/probes", strings.NewReader(`{"provider":"runpod","gpu":"H100"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body = response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"availability_state":"unknown"`) || strings.Contains(body, "private transport failure") {
		t.Fatalf("failed provider probe leaked or claimed evidence: status=%d body=%s", response.Code, body)
	}
}

func TestCustomerWalletBindingResponseRedactsSupplierAndCostBasis(t *testing.T) {
	response := bindingResponse(domain.BackendBinding{
		ID: "binding", TenantID: "tenant", EndpointID: "endpoint", Name: "runpod-primary", Kind: "external", OwnershipMode: "traffic-managed", TargetID: "private-target",
		ConfigJSON: `{"adapter":"runpod-serverless-api","secret_reference_id":"private-secret","enabled":true,"privacy_acknowledged":true,"request_limit":100,"cost_limit_microusd":1000000,"max_request_cost_microusd":10000,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"private supplier quote","rate_card_valid_until":"2099-01-01T00:00:00Z"}`,
	})
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(body)
	for _, secret := range []string{"runpod", "private-secret", "private-target", "cost_basis", "gross_margin", "supplier quote", "privacy_acknowledged"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("binding response exposed private field %q: %s", secret, encoded)
		}
	}
	for _, visible := range []string{`"service":"infercrane-standard"`, `"billing_mode":"customer_wallet"`, `"input_microusd_per_mtok":100000`, `"rate_card_valid_until":"2099-01-01T00:00:00Z"`} {
		if !strings.Contains(encoded, visible) {
			t.Fatalf("binding response omitted customer contract %q: %s", visible, encoded)
		}
	}
}

func TestOptimizationCampaignRequiresImmutableProposalAndExplicitBoundedApproval(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot()).Propose(context.Background(), optimizer.Request{ModelIdentity: "llama-3.1-8b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", MaxCandidates: 2})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"proposal": proposal})
	store := &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: registry.Snapshot()}).Handler()

	request := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/proposals", strings.NewReader(`{"model_identity":"llama-3.1-8b-instruct","provider":"aws","region":"eu-central-1","gpu":"L40S","objective":"interactive","max_candidates":2}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"provider_mutation":false`) || !strings.Contains(response.Body.String(), `"performance_claims":false`) || !strings.Contains(response.Body.String(), `"input_digest"`) || !strings.Contains(response.Body.String(), `"candidates":[`) {
		t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
	}

	create := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns", strings.NewReader(string(body)))
	create.Header.Set("Authorization", "Bearer secret")
	create.Header.Set("Idempotency-Key", "campaign-create-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, create)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"provider_mutation":false`) || store.campaign.State != optimizationcampaign.CampaignAwaitingApproval || store.campaign.Intent != optimizationcampaign.IntentNewEndpoint {
		t.Fatalf("create status=%d body=%s campaign=%+v", response.Code, response.Body.String(), store.campaign)
	}

	invalidIntentBody, _ := json.Marshal(map[string]any{"proposal": proposal, "intent": "evolve_endpoint"})
	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns", strings.NewReader(string(invalidIntentBody)))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "campaign-create-missing-target")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "target_deployment") {
		t.Fatalf("missing evolution target status=%d body=%s", response.Code, response.Body.String())
	}

	tampered := proposal
	tampered.Input.GPU = "H100"
	tamperedBody, _ := json.Marshal(map[string]any{"proposal": tampered})
	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns", strings.NewReader(string(tamperedBody)))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "campaign-create-2")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tampered proposal status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns/campaign-1/approve", strings.NewReader(`{"max_cost_usd":20,"expires_in_seconds":3600}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.campaign.State != optimizationcampaign.CampaignApproved || !strings.Contains(response.Body.String(), `"execution":"queued"`) || store.operation.Kind != optimizationcampaign.ExecuteKind {
		t.Fatalf("approval status=%d body=%s campaign=%+v", response.Code, response.Body.String(), store.campaign)
	}

	store.campaign.State = optimizationcampaign.CampaignQualified
	store.campaign.Candidates[0].State = optimizationcampaign.CandidateQualified
	store.campaign.Candidates[0].DeploymentName = "llama-production-candidate"
	store.campaign.Candidates[0].RevisionID = "revision-qualified"
	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns/campaign-1/activate", strings.NewReader(`{"candidate_id":"candidate-1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != optimizationcampaign.ActivateKind || !strings.Contains(response.Body.String(), `"automatic_promotion":false`) {
		t.Fatalf("activation status=%d body=%s operation=%+v", response.Code, response.Body.String(), store.operation)
	}
	var activation optimizationcampaign.ActivateRequest
	if json.Unmarshal([]byte(store.operation.RequestJSON), &activation) != nil || activation.TenantID != store.campaign.TenantID || activation.CampaignID != "campaign-1" || activation.CandidateID != "candidate-1" || activation.Actor == "" {
		t.Fatalf("activation operation lost identity: %+v", store.operation)
	}

	// A client retry after the durable activation completed remains successful
	// even when the original bounded approval has subsequently expired.
	store.campaign.Candidates[0].State = optimizationcampaign.CandidatePromoted
	expired := time.Now().UTC().Add(-time.Minute)
	store.campaign.ApprovalExpiresAt = &expired
	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns/campaign-1/activate", strings.NewReader(`{"candidate_id":"candidate-1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"activation":"already_completed"`) {
		t.Fatalf("completed activation retry status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/optimization/campaigns/campaign-1", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"modeled evidence cannot qualify`) || !strings.Contains(response.Body.String(), `"target_endpoint":`) {
		t.Fatalf("inspect status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOptimizedArtifactLifecycleKeepsExternalBuilderAndQualityBoundariesExplicit(t *testing.T) {
	store := &fakeOptimizedArtifactStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	plan := `{"base_model_artifact_id":"base-1","kind":"quantized_checkpoint","format":"safetensors","tool":"llm-compressor","tool_version":"0.9.0","algorithm":"w8a8-fp8","builder_image_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","calibration_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","license_spdx":"Apache-2.0","configuration":{"scheme":"FP8"},"hardware_constraints":{"minimum_compute_capability":"8.9"},"requires_quality_review":true}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/optimized-artifacts", strings.NewReader(plan))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "fp8-build-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"builder_execution":"external_not_started"`) || store.artifact.State != optimizedartifact.StatePlanned {
		t.Fatalf("plan status=%d body=%s artifact=%+v", response.Code, response.Body.String(), store.artifact)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimized-artifacts/optimized-1/build", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.artifact.State != optimizedartifact.StateBuilding {
		t.Fatalf("build status=%d body=%s", response.Code, response.Body.String())
	}

	attestation := `{"state":"ready","attestation":{"output_repository":"acme/model-fp8","output_immutable_revision":"commit-1","output_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","build_evidence":{"builder_job":"job-1"}}}`
	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimized-artifacts/optimized-1/attest", strings.NewReader(attestation))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.artifact.EvidenceState != "measured" || strings.Contains(response.Body.String(), `"evidence_state":"qualified"`) {
		t.Fatalf("attest status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/optimized-artifacts/optimized-1/qualify", strings.NewReader(`{"candidate_run_id":"candidate-1","quality_evidence_id":"quality-1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.artifact.EvidenceState != "qualified" || !strings.Contains(response.Body.String(), `"quality_evidence_id":"quality-1"`) {
		t.Fatalf("qualify status=%d body=%s", response.Code, response.Body.String())
	}
}

func (f *fakeQualityEvidenceStore) RecordQualityEvidence(_ context.Context, tenant, _ string, item domain.QualityEvidence) (domain.QualityEvidence, bool, error) {
	item.ID, item.TenantID, item.DeploymentID = "quality-1", tenant, "deployment-1"
	item.CreatedAt = time.Now().UTC()
	f.evidence = append(f.evidence, item)
	return item, true, nil
}

func (f *fakeQualityEvidenceStore) QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error) {
	return f.evidence, nil
}

func (f *fakeIntelligenceStore) CaptureReplayTrace(context.Context, string, string, time.Duration, int) (domain.ReplayTrace, error) {
	return f.trace, nil
}
func (f *fakeIntelligenceStore) ReplayTrace(context.Context, string, string) (domain.ReplayTrace, error) {
	return f.trace, nil
}
func (f *fakeIntelligenceStore) RecordArtifactCacheObservation(_ context.Context, tenant string, row domain.ArtifactCacheObservation) (domain.ArtifactCacheObservation, error) {
	row.ID, row.TenantID = "cache-observation-1", tenant
	f.cacheObservations = append(f.cacheObservations, row)
	return row, nil
}
func (f *fakeIntelligenceStore) RequestArtifactPrefetch(_ context.Context, tenant string, row domain.ArtifactPrefetch) (domain.ArtifactPrefetch, bool, error) {
	if f.prefetch.ID != "" {
		return f.prefetch, false, nil
	}
	row.ID, row.TenantID, row.Status = "prefetch-1", tenant, "requested"
	f.prefetch = row
	return row, true, nil
}
func (f *fakeIntelligenceStore) CapacityIntelligence(context.Context, string, time.Duration) ([]domain.CapacitySummary, error) {
	return []domain.CapacitySummary{}, nil
}
func (f *fakeIntelligenceStore) ModelArtifactForTenantByID(context.Context, string, string) (domain.ModelArtifact, error) {
	return f.artifact, f.artifactErr
}
func (f *fakeIntelligenceStore) UpdateArtifactPrefetch(_ context.Context, _, _ string, status, providerOperationID, errorCode string) (domain.ArtifactPrefetch, error) {
	if f.updateErr != nil {
		return f.prefetch, f.updateErr
	}
	f.prefetch.Status, f.prefetch.ProviderOperationID, f.prefetch.ErrorCode = status, providerOperationID, errorCode
	return f.prefetch, nil
}

type fakeArtifactCacheAdapter struct {
	calls        int
	resourceKeys map[string]string
}

func (f *fakeArtifactCacheAdapter) Observe(context.Context, artifactcache.Request) (artifactcache.Observation, error) {
	return artifactcache.Observation{}, nil
}
func (f *fakeArtifactCacheAdapter) Prefetch(_ context.Context, request artifactcache.Request) (artifactcache.Operation, error) {
	f.calls++
	if err := request.Validate(); err != nil {
		return artifactcache.Operation{}, err
	}
	if f.resourceKeys == nil {
		f.resourceKeys = map[string]string{}
	}
	operationID := f.resourceKeys[request.IdempotencyKey]
	if operationID == "" {
		operationID = "provider-prefetch-1"
		f.resourceKeys[request.IdempotencyKey] = operationID
	}
	return artifactcache.Operation{ProviderOperationID: operationID, Status: "running"}, nil
}

func TestArtifactPrefetchExecutesConfiguredProviderWithoutDuplicateResource(t *testing.T) {
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "org/model@commit"}}
	adapter := &fakeArtifactCacheAdapter{}
	handler := (API{Store: store, APIKey: "secret", ArtifactCacheAdapters: map[string]artifactcache.Adapter{"fixture": adapter}}).Handler()
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"fixture","region":"zone-a","location":"cache://models","idempotency_key":"warm-release-1"}`))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("attempt=%d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
		if attempt == 0 && (!strings.Contains(response.Body.String(), `"execution":"provider_accepted"`) || !strings.Contains(response.Body.String(), `"provider_operation_id":"provider-prefetch-1"`)) {
			t.Fatalf("configured execution was not disclosed: %s", response.Body.String())
		}
		if attempt == 1 && !strings.Contains(response.Body.String(), `"execution":"already_requested"`) {
			t.Fatalf("replay state was not disclosed: %s", response.Body.String())
		}
	}
	if adapter.calls != 1 || len(adapter.resourceKeys) != 1 {
		t.Fatalf("provider calls=%d resources=%d, want one accepted resource", adapter.calls, len(adapter.resourceKeys))
	}
}

type completedArtifactCacheAdapter struct{}

func (completedArtifactCacheAdapter) Prefetch(context.Context, artifactcache.Request) (artifactcache.Operation, error) {
	return artifactcache.Operation{ProviderOperationID: "snap-verified", Status: "succeeded"}, nil
}
func (completedArtifactCacheAdapter) Observe(_ context.Context, _ artifactcache.Request) (artifactcache.Observation, error) {
	now := time.Now().UTC()
	return artifactcache.Observation{State: "present", Source: "aws-ebs-snapshot", EvidenceJSON: `{"encrypted":true}`, ObservedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, nil
}

func TestCompletedArtifactPrefetchPersistsFreshProviderObservation(t *testing.T) {
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "org/model@commit"}}
	handler := (API{Store: store, APIKey: "secret", ArtifactCacheAdapters: map[string]artifactcache.Adapter{"aws": completedArtifactCacheAdapter{}}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"aws","region":"eu-central-1","location":"aws-ebs://snap-verified","idempotency_key":"warm-release-1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"execution":"provider_completed"`) || len(store.cacheObservations) != 1 || store.cacheObservations[0].State != "present" {
		t.Fatalf("status=%d body=%s observations=%#v", response.Code, response.Body.String(), store.cacheObservations)
	}
}

type recoveringCompletedArtifactCacheAdapter struct{ observes int }

func (*recoveringCompletedArtifactCacheAdapter) Prefetch(context.Context, artifactcache.Request) (artifactcache.Operation, error) {
	return artifactcache.Operation{ProviderOperationID: "snap-verified", Status: "succeeded"}, nil
}
func (a *recoveringCompletedArtifactCacheAdapter) Observe(_ context.Context, _ artifactcache.Request) (artifactcache.Observation, error) {
	a.observes++
	if a.observes == 1 {
		return artifactcache.Observation{}, errors.New("observation temporarily unavailable")
	}
	now := time.Now().UTC()
	return artifactcache.Observation{State: "present", Source: "aws-ebs-snapshot", EvidenceJSON: `{}`, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
}

func TestCompletedArtifactPrefetchRecoversPendingObservation(t *testing.T) {
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "org/model@commit"}}
	adapter := &recoveringCompletedArtifactCacheAdapter{}
	handler := (API{Store: store, APIKey: "secret", ArtifactCacheAdapters: map[string]artifactcache.Adapter{"aws": adapter}}).Handler()
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"aws","region":"eu-central-1","location":"aws-ebs://snap-verified","idempotency_key":"warm-release-1"}`))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("attempt=%d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
		if attempt == 0 && (!strings.Contains(response.Body.String(), `"execution":"provider_completed_observation_pending"`) || store.prefetch.ErrorCode != "observation_pending") {
			t.Fatalf("pending observation was not durable: body=%s row=%#v", response.Body.String(), store.prefetch)
		}
	}
	if adapter.observes != 2 || len(store.cacheObservations) != 1 || store.prefetch.ErrorCode != "" {
		t.Fatalf("observes=%d observations=%d prefetch=%#v", adapter.observes, len(store.cacheObservations), store.prefetch)
	}
}

type uncertainArtifactCacheAdapter struct{ keys []string }

func (f *uncertainArtifactCacheAdapter) Observe(context.Context, artifactcache.Request) (artifactcache.Observation, error) {
	return artifactcache.Observation{}, nil
}

type rejectingArtifactCacheAdapter struct{ calls int }

func (*rejectingArtifactCacheAdapter) Observe(context.Context, artifactcache.Request) (artifactcache.Observation, error) {
	return artifactcache.Observation{}, nil
}
func (f *rejectingArtifactCacheAdapter) Prefetch(context.Context, artifactcache.Request) (artifactcache.Operation, error) {
	f.calls++
	return artifactcache.Operation{}, artifactcache.Definitive("cache_not_configured", errors.New("no immutable cache mapping"))
}

func TestArtifactPrefetchDoesNotRetryDefiniteProviderRejection(t *testing.T) {
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "org/model@commit"}}
	adapter := &rejectingArtifactCacheAdapter{}
	handler := (API{Store: store, APIKey: "secret", ArtifactCacheAdapters: map[string]artifactcache.Adapter{"fixture": adapter}}).Handler()
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"fixture","location":"cache://missing","idempotency_key":"rejected-prefetch"}`))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("attempt=%d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
		if attempt == 0 && (!strings.Contains(response.Body.String(), `"execution":"provider_rejected"`) || !strings.Contains(response.Body.String(), `"error_code":"cache_not_configured"`)) {
			t.Fatalf("definite rejection was not explained: %s", response.Body.String())
		}
		if attempt == 1 && !strings.Contains(response.Body.String(), `"execution":"already_requested"`) {
			t.Fatalf("definite rejection was retried: %s", response.Body.String())
		}
	}
	if adapter.calls != 1 || store.prefetch.Status != "failed" {
		t.Fatalf("definite rejection calls=%d state=%#v", adapter.calls, store.prefetch)
	}
}
func (f *uncertainArtifactCacheAdapter) Prefetch(_ context.Context, request artifactcache.Request) (artifactcache.Operation, error) {
	f.keys = append(f.keys, request.IdempotencyKey)
	if len(f.keys) == 1 {
		return artifactcache.Operation{}, errors.New("response lost after provider acceptance")
	}
	return artifactcache.Operation{ProviderOperationID: "adopted-provider-operation", Status: "running"}, nil
}

func TestArtifactPrefetchRetriesUnknownProviderResultWithStableIdentity(t *testing.T) {
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "org/model@commit"}}
	adapter := &uncertainArtifactCacheAdapter{}
	handler := (API{Store: store, APIKey: "secret", ArtifactCacheAdapters: map[string]artifactcache.Adapter{"fixture": adapter}}).Handler()
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"fixture","location":"cache://models","idempotency_key":"stable-prefetch"}`))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("attempt=%d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
		if attempt == 0 && !strings.Contains(response.Body.String(), `"execution":"provider_result_unknown"`) {
			t.Fatalf("unknown result not preserved: %s", response.Body.String())
		}
		if attempt == 1 && !strings.Contains(response.Body.String(), `"provider_operation_id":"adopted-provider-operation"`) {
			t.Fatalf("provider operation not adopted: %s", response.Body.String())
		}
	}
	if len(adapter.keys) != 2 || adapter.keys[0] != adapter.keys[1] {
		t.Fatalf("provider retry identity changed: %#v", adapter.keys)
	}
}

func TestArtifactPrefetchStorageFailuresRemainReplayableAndNeverDuplicateProviderWork(t *testing.T) {
	adapter := &fakeArtifactCacheAdapter{}
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, artifactErr: errors.New("database temporarily unavailable")}
	handler := (API{Store: store, APIKey: "secret", ArtifactCacheAdapters: map[string]artifactcache.Adapter{"fixture": adapter}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"fixture","location":"cache://models","idempotency_key":"storage-failure"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"prefetch_artifact_read_failed"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if adapter.calls != 0 || store.prefetch.Status != "requested" {
		t.Fatalf("provider calls=%d durable status=%q, want no side effect and replayable request", adapter.calls, store.prefetch.Status)
	}

	store.artifactErr = nil
	store.artifact = domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "org/model@commit"}
	store.updateErr = errors.New("checkpoint unavailable")
	request = httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/artifact-1/prefetches", strings.NewReader(`{"provider":"fixture","location":"cache://models","idempotency_key":"storage-failure"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"prefetch_checkpoint_failed"`) {
		t.Fatalf("checkpoint status=%d body=%s", response.Code, response.Body.String())
	}
	if adapter.calls != 1 || store.prefetch.Status != "requested" {
		t.Fatalf("provider calls=%d durable status=%q, want one idempotent call and replayable request", adapter.calls, store.prefetch.Status)
	}
}

func TestQualityEvidenceAPIRequiresValidRevisionBoundSignedAggregate(t *testing.T) {
	base := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: "prod", ActiveRevisionID: "rev-1"}}}
	store := &fakeQualityEvidenceStore{fakeStore: base}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := qualityevidence.Payload{Schema: qualityevidence.Schema, Deployment: "prod", RevisionID: "rev-2", Suite: "support-answers", SuiteVersion: "git:abc123", Evaluator: "offline", EvaluatorVersion: "1.0.0", Score: .91, Passed: true, SampleCount: 200, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), EvaluatedAt: time.Now().UTC()}
	envelope, err := passport.Sign(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(envelope)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/quality-evidence", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.evidence) != 1 || store.evidence[0].RevisionID != "rev-2" || !strings.Contains(response.Body.String(), `"content_recorded":false`) {
		t.Fatalf("status=%d evidence=%+v body=%s", response.Code, store.evidence, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/deployments/prod/quality-evidence", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"suite":"support-answers"`) || strings.Contains(response.Body.String(), "prompt") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	payload.Deployment = "other"
	envelope, err = passport.Sign(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(envelope)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/quality-evidence", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || len(store.evidence) != 1 {
		t.Fatalf("mismatched deployment status=%d evidence=%+v body=%s", response.Code, store.evidence, response.Body.String())
	}
}

func (f *fakeOptimizationStore) RecordFinOpsReport(_ context.Context, row domain.FinOpsReport) (domain.FinOpsReport, error) {
	row.ID = "finops-1"
	row.CreatedAt = time.Now().UTC()
	f.report = row
	return row, nil
}
func (f *fakeOptimizationStore) FinOpsReports(context.Context, string, string, int) ([]domain.FinOpsReport, error) {
	return []domain.FinOpsReport{f.report}, nil
}
func (f *fakeOptimizationStore) CreateAutopilotPlan(context.Context, domain.AutopilotPlan) (domain.AutopilotPlan, bool, error) {
	return domain.AutopilotPlan{}, false, domain.ErrNotFound
}
func (f *fakeOptimizationStore) AutopilotPlan(context.Context, string, string, string, string) (domain.AutopilotPlan, error) {
	return domain.AutopilotPlan{}, domain.ErrNotFound
}
func (f *fakeOptimizationStore) ApproveAutopilotPlan(context.Context, string, string, string) (domain.AutopilotPlan, error) {
	return domain.AutopilotPlan{}, domain.ErrNotFound
}

func (f *fakeContextBurstStore) CreateContextPassport(_ context.Context, tenant, name string, row domain.ContextPassport) (domain.ContextPassport, error) {
	row.ID = "session-1"
	row.TenantID = tenant
	row.DeploymentID = "deployment-1"
	row.DeploymentName = name
	row.Status = "active"
	f.passport = row
	return row, nil
}
func (f *fakeContextBurstStore) ContextPassport(context.Context, string, string) (domain.ContextPassport, error) {
	if f.passport.ID == "" {
		return domain.ContextPassport{}, domain.ErrNotFound
	}
	return f.passport, nil
}
func (f *fakeContextBurstStore) SetBurstGuardPolicy(context.Context, string, string, domain.BurstGuardPolicy) (domain.BurstGuardPolicy, error) {
	return domain.BurstGuardPolicy{}, nil
}
func (f *fakeContextBurstStore) RecordBurstGuardDecision(context.Context, domain.BurstGuardDecision) (domain.BurstGuardDecision, error) {
	return domain.BurstGuardDecision{}, nil
}

type fakeRecipeLabStore struct {
	*fakeStore
	recipes []domain.ModelRecipe
	lab     domain.LabEvaluation
}

func (f *fakeRecipeLabStore) CreateModelRecipe(_ context.Context, _ string, value domain.ModelRecipe) (domain.ModelRecipe, error) {
	value.ID = "recipe-1"
	value.CreatedAt = time.Now().UTC()
	f.recipes = append(f.recipes, value)
	return value, nil
}
func (f *fakeRecipeLabStore) ModelRecipe(_ context.Context, _, name, version string) (domain.ModelRecipe, error) {
	for _, row := range f.recipes {
		if row.Name == name && row.Version == version {
			return row, nil
		}
	}
	return domain.ModelRecipe{}, domain.ErrNotFound
}
func (f *fakeRecipeLabStore) ModelRecipes(context.Context, string, string, int) ([]domain.ModelRecipe, error) {
	return f.recipes, nil
}
func (f *fakeRecipeLabStore) BenchmarksForModel(_ context.Context, _ string, model string, _ int) ([]domain.BenchmarkResult, error) {
	var out []domain.BenchmarkResult
	for _, row := range f.benchmarks {
		if row.ModelIdentity == model {
			out = append(out, row)
		}
	}
	return out, nil
}
func (f *fakeRecipeLabStore) RecordLabEvaluation(_ context.Context, _ string, value domain.LabEvaluation) (domain.LabEvaluation, error) {
	value.ID = "lab-1"
	value.CreatedAt = time.Now().UTC()
	f.lab = value
	return value, nil
}

func TestRecipeCaptureAndLabUseImmutableMeasuredEvidence(t *testing.T) {
	identity := "org/model@" + strings.Repeat("a", 40)
	base := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: "prod", ActiveRevisionID: "rev-1"}}, revisions: []domain.DeploymentRevision{{ID: "rev-1", SpecJSON: `{"model":"org/model","runtime":"vllm","runtime_version":"0.10.2","routing_strategy":"round_robin","min_replicas":1,"max_replicas":1,"compute_mode":"elastic","cloud":"aws","gpu":"H100"}`}}, artifact: domain.ModelArtifact{ID: "artifact-1", Repository: "org/model", ImmutableRevision: strings.Repeat("a", 40), ModelIdentity: identity}, benchmarks: []domain.BenchmarkResult{{ID: "bench-1", DeploymentName: "prod", RevisionID: "rev-1", ModelIdentity: identity, Runtime: "vllm", RuntimeVersion: "0.10.2", Provider: "aws", GPU: "H100", ComputeMode: "elastic", Tool: "aiperf", ToolVersion: "0.9", WorkloadJSON: `{"requests":10}`, CostMetadataJSON: `{"available":false}`, RequestCount: 10, Succeeded: 10}}}
	store := &fakeRecipeLabStore{fakeStore: base}
	handler := (API{Store: store, APIKey: "secret", ProductVersion: "1.7.0"}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/recipes", strings.NewReader(`{"name":"balanced","version":"1.0.0"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.recipes) != 1 || store.recipes[0].Digest == "" || !strings.Contains(response.Body.String(), `"evidence_class":"measured"`) {
		t.Fatalf("status=%d recipes=%#v body=%s", response.Code, store.recipes, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/lab/evaluations", strings.NewReader(`{"model_identity":"`+identity+`","objective":"latency","max_ttft_p95_ms":250}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.lab.ID != "lab-1" || !strings.Contains(response.Body.String(), `"evidence_class":"measured"`) {
		t.Fatalf("status=%d lab=%#v body=%s", response.Code, store.lab, response.Body.String())
	}
}

func TestLabRejectsUnknownOptimizationObjectiveAndProfile(t *testing.T) {
	store := &fakeRecipeLabStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	for _, body := range []string{
		`{"model_identity":"model@commit","objective":"magic"}`,
		`{"model_identity":"model@commit","workload_profile":"not-a-profile"}`,
		`{"model_identity":"model@commit","max_ttft_p95_ms":1e999}`,
		`{"model_identity":"model@commit","max_tpot_p95_ms":-1}`,
		`{"model_identity":"model@commit","max_error_rate":1.1}`,
		`{"model_identity":"model@commit","min_goodput":-1}`,
		`{"model_identity":"model@commit","max_hourly_cost":-1}`,
		`{"model_identity":"model@commit","max_gpu_count":0}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/lab/evaluations", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || response.Code >= 500 {
			t.Fatalf("body=%s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
}

func (f *fakeMembershipStore) ControlPlaneInstances(context.Context, time.Duration) ([]domain.ControlPlaneInstance, error) {
	return f.instances, nil
}

func TestMutationEndpointsRejectTrailingJSONValues(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()
	tests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPut, "/api/v1/tenant/quota", `{"max_deployments":1,"max_replicas":1,"max_requests_per_minute":1} {}`},
		{http.MethodPost, "/api/v1/tenants", `{"id":"tenant-a","name":"Tenant A"} {}`},
		{http.MethodPost, "/api/v1/principals", `{"name":"operator","role":"operator"} {}`},
		{http.MethodPut, "/api/v1/deployments/prod/route", `{"strategy":"round-robin"} {}`},
		{http.MethodPost, "/api/v1/targets", `{"name":"worker","url":"http://worker.test:8000","provider":"existing","runtime":"vllm"} {}`},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
			t.Errorf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestAsyncInferenceRequiresConsentAndReturnsDurableJob(t *testing.T) {
	service := &fakeAsyncService{}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", AsyncInference: service}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder/async", strings.NewReader(`{"input":{"model":"coder","messages":[]},"idempotency_key":"key"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "content_consent_required") {
		t.Fatalf("consent status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder/async", strings.NewReader(`{"protocol":"chat","input":{"model":"coder","messages":[]},"idempotency_key":"key","store_encrypted_content":true}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || service.submitted.Endpoint != "coder" || service.submitted.IdempotencyKey != "key" || strings.Contains(response.Body.String(), "ciphertext") {
		t.Fatalf("submit status=%d submitted=%#v body=%s", response.Code, service.submitted, response.Body.String())
	}
}

func TestAsyncInferenceRejectsInvalidProtocolAndEndpointModelBeforeQueueing(t *testing.T) {
	for _, body := range []string{
		`{"protocol":"audio","input":{"model":"coder"},"idempotency_key":"key","store_encrypted_content":true}`,
		`{"protocol":"chat","input":{"messages":[]},"idempotency_key":"key","store_encrypted_content":true}`,
		`{"protocol":"chat","input":{"model":"different","messages":[]},"idempotency_key":"key","store_encrypted_content":true}`,
	} {
		service := &fakeAsyncService{}
		handler := (API{Store: &fakeStore{}, APIKey: "secret", AsyncInference: service}).Handler()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder/async", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("body=%s status=%d", body, response.Code)
		}
		if service.submitted.Endpoint != "" {
			t.Fatalf("invalid request was queued: %#v", service.submitted)
		}
	}
}

func TestAsyncInferenceTypedNilServiceReturnsCapabilityUnavailable(t *testing.T) {
	var service *fakeAsyncService
	handler := (API{Store: &fakeStore{}, APIKey: "secret", AsyncInference: service}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/qwen-prod/async", strings.NewReader(`{"store_encrypted_content":true,"idempotency_key":"request-1","input":{"model":"qwen-prod"}}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"capability_unavailable"`) {
		t.Fatalf("body = %s, want capability_unavailable", response.Body.String())
	}
}

func TestCreateContextPassportPublishesDataPathHint(t *testing.T) {
	directory := contextpassport.New()
	store := &fakeContextBurstStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret", ContextPassports: directory}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/context-passports", strings.NewReader(`{"deployment":"coder","ttl_seconds":3600,"preferred_binding_id":"binding"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	hint, ok := directory.Resolve("global", "session-1", "coder", time.Now())
	if !ok || hint.PreferredBindingID != "binding" {
		t.Fatalf("hint=%#v ok=%v", hint, ok)
	}
}

func TestFinOpsPersistsUnavailableWithoutInventingCurrency(t *testing.T) {
	base := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: "prod"}}}
	store := &fakeOptimizationStore{fakeStore: base}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/finops/reports", strings.NewReader(`{"window_seconds":3600}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.report.Status != "unavailable" || store.report.Currency != "" || store.report.KnownCost != nil || !strings.Contains(response.Body.String(), `"currency":""`) {
		t.Fatalf("status=%d report=%#v body=%s", response.Code, store.report, response.Body.String())
	}
}

func TestFinOpsUsesFreshImportedCostEvidenceBeforeBenchmarkPriceFallback(t *testing.T) {
	now := time.Now().UTC()
	base := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: "prod"}}}
	store := &fakeCostOptimizationStore{fakeOptimizationStore: &fakeOptimizationStore{fakeStore: base}, costs: []domain.CostEvidence{{ID: "cost-1", Scope: "deployment_hourly_rate/prod", Resource: "prod", Source: "opencost/allocation", Currency: "USD", BillingUnit: "hour", EvidenceClass: "measured", Amount: 1.25, WindowStart: now.Add(-time.Hour), WindowEnd: now, ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(time.Hour)}}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/prod/finops/reports", strings.NewReader(`{"window_seconds":3600}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.report.Status != "measured" || store.report.Currency != "USD" || store.report.KnownCost == nil || *store.report.KnownCost != 1.25 || !strings.Contains(store.report.EvidenceJSON, "opencost/allocation") {
		t.Fatalf("status=%d report=%#v body=%s", response.Code, store.report, response.Body.String())
	}
}

func TestAsyncInferenceResultAndCancellation(t *testing.T) {
	service := &fakeAsyncService{}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", AsyncInference: service}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/async/jobs/job-1", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"result":{"ok":true}`) {
		t.Fatalf("result status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodDelete, "/api/v1/async/jobs/job-1", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.cancelled != "job-1" {
		t.Fatalf("cancel status=%d id=%s", response.Code, service.cancelled)
	}
}

func TestReplaySerializesPersistedJSONAsObjects(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}, trace: domain.ReplayTrace{
		ID: "replay-1", DeploymentID: "deployment-1", DeploymentName: "qwen-prod",
		SchemaVersion: "infercrane.replay/v1", ShapeJSON: `[{"input_tokens":8}]`,
		SummaryJSON: `{"requests":1,"input_tokens_mean":8,"output_tokens_mean":4,"peak_concurrency":1}`,
		ShapeDigest: strings.Repeat("a", 64), RequestCount: 1,
		WindowStart: now.Add(-time.Hour), WindowEnd: now, CreatedAt: now,
	}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/deployments/qwen-prod/replays", `{"window_seconds":3600,"max_requests":10}`},
		{http.MethodGet, "/api/v1/replays/replay-1", ""},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.path, response.Code, response.Body.String())
		}
		var envelope struct {
			Replay struct {
				Shape   []map[string]any `json:"shape"`
				Summary map[string]any   `json:"summary"`
			} `json:"replay"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Replay.Shape) != 1 || envelope.Replay.Summary["requests"] != float64(1) {
			t.Fatalf("%s %s replay JSON was double encoded: body=%s err=%v", tc.method, tc.path, response.Body.String(), err)
		}
	}
}

func TestCapacityIntelligenceSerializesEmptyCollection(t *testing.T) {
	store := &fakeIntelligenceStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/capacity/intelligence", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"capacity":[]`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAlertPolicyResponsesUsePublicJSONContract(t *testing.T) {
	store := &fakeAlertStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	create := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/prod/alerts", strings.NewReader(`{"name":"ops","webhook_url":"https://example.test/hook","secret_reference_id":"secret-1","minimum_severity":"warning","max_attempts":2}`))
	create.Header.Set("Authorization", "Bearer secret")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"webhook_url"`) || strings.Contains(created.Body.String(), `"WebhookURL"`) {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	list := httptest.NewRequest(http.MethodGet, "/api/v1/endpoints/prod/alerts", nil)
	list.Header.Set("Authorization", "Bearer secret")
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, list)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"minimum_severity":"warning"`) || strings.Contains(listed.Body.String(), `"MinimumSeverity"`) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
}

func TestUntaggedDomainValuesAreNormalizedAtAPIBoundary(t *testing.T) {
	values := []map[string]any{
		logicalModelResponse(domain.LogicalModel{ID: "model-1", TenantID: "tenant-1", Name: "coder"}),
		adoptedWorkloadResponse(domain.AdoptedWorkload{ID: "adoption-1", OwnershipMode: "traffic-managed"}),
		artifactCacheObservationResponse(domain.ArtifactCacheObservation{ID: "cache-1", EvidenceJSON: `{}`}),
		artifactPrefetchResponse(domain.ArtifactPrefetch{ID: "prefetch-1", IdempotencyKey: "key-1"}),
	}
	for _, value := range values {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"TenantID", "OwnershipMode", "EvidenceJSON", "IdempotencyKey"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("internal field %q leaked in %s", forbidden, body)
			}
		}
	}
}

type fakeAsyncService struct {
	submitted asyncinference.SubmitRequest
	cancelled string
}

func (f *fakeAsyncService) Submit(_ context.Context, request asyncinference.SubmitRequest) (domain.AsyncInferenceJob, bool, error) {
	f.submitted = request
	return domain.AsyncInferenceJob{ID: "job-1", TenantID: request.Tenant, EndpointID: "endpoint-1", RequestID: "request-1", Protocol: request.Protocol, Status: "queued", ExecutionDeadline: request.ExecutionDeadline, ExpiresAt: request.ExpiresAt, CreatedAt: time.Now().UTC(), WebhookStatus: "not_configured"}, true, nil
}
func (f *fakeAsyncService) Result(context.Context, string, string) (domain.AsyncInferenceJob, []byte, error) {
	return domain.AsyncInferenceJob{ID: "job-1", Status: "succeeded", Protocol: "chat", WebhookStatus: "not_configured"}, []byte(`{"ok":true}`), nil
}
func (f *fakeAsyncService) Cancel(_ context.Context, _ string, id string) error {
	f.cancelled = id
	return nil
}

func (f *fakeStore) SetSLOPolicy(_ context.Context, _, _ string, policy domain.SLOPolicy) (domain.SLOPolicy, error) {
	f.sloPolicy = policy
	return policy, f.err
}
func (f *fakeStore) SLOPolicy(context.Context, string, string) (domain.SLOPolicy, error) {
	if f.sloPolicy.DeploymentID == "" && f.sloPolicy.MaxTTFTP95MS == nil && f.sloPolicy.MaxLatencyP95MS == nil && f.sloPolicy.MaxErrorRate == nil && f.sloPolicy.MinOutputTokensSecond == nil && f.sloPolicy.MaxHourlyCost == nil {
		return domain.SLOPolicy{}, domain.ErrNotFound
	}
	return f.sloPolicy, f.err
}
func (f *fakeStore) DeleteSLOPolicy(context.Context, string, string) error {
	f.sloPolicy = domain.SLOPolicy{}
	return f.err
}
func (f *fakeStore) RecordInferenceRecommendation(_ context.Context, row domain.InferenceRecommendation) (domain.InferenceRecommendation, error) {
	row.ID = "recommendation-1"
	row.InputDigest = strings.Repeat("a", 64)
	f.recommendations = append([]domain.InferenceRecommendation{row}, f.recommendations...)
	return row, f.err
}
func (f *fakeStore) InferenceRecommendations(context.Context, string, string, int) ([]domain.InferenceRecommendation, error) {
	return f.recommendations, f.err
}
func (f *fakeStore) LatestCapacityEvidence(context.Context, string, string, string, string, string, string, int) (domain.CapacityEvidence, error) {
	if f.capacity.ID == "" {
		return domain.CapacityEvidence{}, domain.ErrNotFound
	}
	return f.capacity, f.err
}

func (f *fakeStore) AuthenticatePrincipal(context.Context, string) (domain.Principal, error) {
	if f.principal.ID == "" {
		return domain.Principal{}, domain.ErrNotFound
	}
	return f.principal, nil
}

func (f *fakeStore) ActiveOperationForResource(context.Context, string, string, string) (domain.Operation, error) {
	if f.activeOperation.ID == "" {
		return domain.Operation{}, domain.ErrNotFound
	}
	return f.activeOperation, nil
}

func (f *fakeStore) Operation(context.Context, string) (domain.Operation, error) {
	return f.operation, f.err
}
func (f *fakeStore) OperationsForTenant(context.Context, string, time.Time, int) ([]domain.Operation, error) {
	return f.operations, f.err
}
func (f *fakeStore) RequestOperationCancel(context.Context, string) error {
	f.cancelled = true
	return f.err
}
func (f *fakeStore) EnqueueOperation(_ context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	operation.ID = "queued"
	operation.Status = "pending"
	f.operation = operation
	return operation, f.created, f.err
}
func (f *fakeStore) SubmitCloudDeployment(_ context.Context, deployment domain.Deployment, operation domain.Operation) (domain.Deployment, domain.Operation, bool, error) {
	deployment.ID = "deployment"
	operation.ID, operation.Status = "queued", "pending"
	operation.ResourceType, operation.ResourceName = "deployment", deployment.Name
	f.operation = operation
	return deployment, operation, f.created, f.err
}
func (f *fakeStore) SubmitDeploymentDelete(_ context.Context, _, name, _ string, operation domain.Operation) (domain.Operation, bool, error) {
	operation.ID, operation.Status, operation.ResourceName = "queued", "pending", name
	f.operation = operation
	return operation, f.created, f.err
}
func (f *fakeStore) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	if f.err != nil {
		return domain.ResolvedDeployment{}, f.err
	}
	if f.resolved.Deployment.ID != "" {
		return f.resolved, nil
	}
	return domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment", Name: "qwen"}, Targets: f.targets}, nil
}
func (f *fakeStore) EventsForTenant(context.Context, string, string) ([]domain.Event, error) {
	return nil, f.err
}
func (f *fakeStore) RequestStats(context.Context, string, time.Duration) (domain.RequestStats, error) {
	return domain.RequestStats{}, f.err
}
func (f *fakeStore) ColdStartStats(context.Context, string, time.Duration) (domain.ColdStartStats, error) {
	return domain.ColdStartStats{}, f.err
}
func (f *fakeStore) ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error) {
	return f.replicas, f.err
}
func (f *fakeStore) Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error) {
	return f.revisions, f.err
}
func (f *fakeStore) OperationEvents(context.Context, string, int) ([]domain.OperationEvent, error) {
	return nil, f.err
}
func (f *fakeStore) ScalingDecisionsForTenant(context.Context, string, string, int) ([]domain.ScalingDecision, error) {
	return nil, f.err
}
func (f *fakeStore) ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error) {
	if f.artifact.ID != "" {
		return f.artifact, nil
	}
	return domain.ModelArtifact{}, domain.ErrNotFound
}
func (f *fakeStore) ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error) {
	return nil, f.err
}
func (f *fakeStore) ReleaseGuardPolicy(context.Context, string, string) (domain.ReleaseGuardPolicy, error) {
	return domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 20}, f.err
}
func (f *fakeStore) SetReleaseGuardPolicy(_ context.Context, _, _ string, policy domain.ReleaseGuardPolicy) (domain.ReleaseGuardPolicy, error) {
	return policy, f.err
}
func (f *fakeStore) AddTargetForTenant(_ context.Context, _ string, target domain.Target) (domain.Target, error) {
	target.ID = "target"
	return target, f.err
}
func (f *fakeStore) TargetsForTenant(context.Context, string) ([]domain.Target, error) {
	return nil, f.err
}
func (f *fakeStore) TargetForTenantByName(_ context.Context, _, name string) (domain.Target, error) {
	for _, target := range f.targets {
		if target.Name == name {
			return target, nil
		}
	}
	return domain.Target{}, domain.ErrNotFound
}
func (f *fakeStore) DeploymentsForTenant(context.Context, string) ([]domain.Deployment, error) {
	return nil, f.err
}
func (f *fakeStore) OrphanedTargetsForTenant(context.Context, string) ([]domain.Orphan, error) {
	return nil, f.err
}
func (f *fakeStore) Audit(context.Context, domain.AuditEvent) error { return nil }
func (f *fakeStore) AuditEventsForTenant(context.Context, string, time.Time, int) ([]domain.AuditEvent, error) {
	return nil, f.err
}
func (f *fakeStore) SetTenantQuota(context.Context, string, int, int, int) error { return f.err }
func (f *fakeStore) CreatePrincipalScoped(_ context.Context, tenant, name string, role authz.Role, scopes []authz.Action) (domain.Principal, string, error) {
	names := make([]string, len(scopes))
	for i, scope := range scopes {
		names[i] = string(scope)
	}
	return domain.Principal{ID: "new", TenantID: tenant, Name: name, Role: string(role), Kind: "service_account", Scopes: names}, "ic_token", f.err
}
func (f *fakeStore) RotatePrincipalForTenant(context.Context, string, string) (string, error) {
	return "ic_rotated", f.err
}
func (f *fakeStore) RevokePrincipalForTenant(context.Context, string, string) error { return f.err }
func (f *fakeStore) CreateTenant(context.Context, string, string) error             { return f.err }
func (f *fakeStore) ProvisionConsoleIdentity(_ context.Context, tenant string, request domain.ConsoleIdentityProvisioning) (domain.ConsoleIdentity, error) {
	f.consoleIdentity = domain.ConsoleIdentity{UserID: "user-internal", TenantID: tenant, DisplayName: request.DisplayName, Role: request.Role, Scopes: request.Scopes, Access: request.Access}
	return f.consoleIdentity, f.err
}
func (f *fakeStore) ConsoleIdentitiesForTenant(context.Context, string) ([]domain.ConsoleIdentity, error) {
	return f.consoleMembers, f.err
}
func (f *fakeStore) PrincipalsForTenant(context.Context, string) ([]domain.Principal, error) {
	return f.principals, f.err
}
func (f *fakeStore) CreateSandboxReference(_ context.Context, tenant string, row domain.SandboxReference, ttl time.Duration) (domain.SandboxReference, string, error) {
	row.ID, row.TenantID, row.PrincipalID, row.Status = "sandbox-ref", tenant, "sandbox-principal", "referenced"
	row.CreatedAt, row.UpdatedAt, row.ExpiresAt = time.Now().UTC(), time.Now().UTC(), time.Now().UTC().Add(ttl)
	f.sandboxRefs = append([]domain.SandboxReference{row}, f.sandboxRefs...)
	return row, "ic_sandbox_once", f.err
}
func (f *fakeStore) SandboxReferences(context.Context, string) ([]domain.SandboxReference, error) {
	return f.sandboxRefs, f.err
}
func (f *fakeStore) RevokeSandboxReference(context.Context, string, string) error { return f.err }
func (f *fakeStore) RotateSandboxCredential(context.Context, string, string) (string, error) {
	return "ic_sandbox_rotated", f.err
}
func (f *fakeStore) AttachTrainingArtifactHandoff(_ context.Context, tenant, _ string, row domain.TrainingArtifactHandoff, artifact domain.ModelArtifact) (domain.TrainingArtifactHandoff, domain.ModelArtifact, error) {
	row.ID, row.TenantID, row.DeploymentID, row.ModelArtifactID = "handoff", tenant, "deployment", "artifact"
	row.CreatedAt = time.Now().UTC()
	artifact.ID, artifact.TenantID = "artifact", tenant
	f.trainingRows = append([]domain.TrainingArtifactHandoff{row}, f.trainingRows...)
	return row, artifact, f.err
}
func (f *fakeStore) TrainingArtifactHandoffs(context.Context, string, string) ([]domain.TrainingArtifactHandoff, error) {
	return f.trainingRows, f.err
}
func (f *fakeStore) CreateSecretReference(_ context.Context, tenant, name, resolver, reference string) (domain.SecretReference, error) {
	return domain.SecretReference{ID: "secret", TenantID: tenant, Name: name, Resolver: resolver, Reference: reference}, f.err
}
func (f *fakeStore) SecretReferencesForTenant(context.Context, string) ([]domain.SecretReference, error) {
	return []domain.SecretReference{{ID: "secret", Name: "openrouter", Resolver: "env", Reference: "OPENROUTER_API_KEY"}}, f.err
}
func (f *fakeStore) DeleteSecretReferenceForTenant(context.Context, string, string) error {
	return f.err
}
func (f *fakeStore) SetExternalTargetPolicyForTenant(_ context.Context, policy domain.ExternalTargetPolicy) (domain.ExternalTargetPolicy, error) {
	policy.ID = "policy"
	return policy, f.err
}
func (f *fakeStore) ExternalTargetPolicyForDeployment(context.Context, string, string) (domain.ExternalTargetPolicy, error) {
	return domain.ExternalTargetPolicy{ID: "policy", Enabled: true, PrivacyAcknowledged: true}, f.err
}
func (f *fakeStore) SetRouteForTenant(context.Context, string, string, string) error { return f.err }
func (f *fakeStore) RecordBenchmark(_ context.Context, result domain.BenchmarkResult) (domain.BenchmarkResult, error) {
	result.ID = "benchmark"
	f.benchmarks = append(f.benchmarks, result)
	return result, f.err
}
func (f *fakeStore) BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error) {
	return f.benchmarks, f.err
}

func TestManagedExternalEndpointBindingRequiresConsentReferenceAndHardBudget(t *testing.T) {
	base := &fakeStore{targets: []domain.Target{{ID: "target", Name: "managed-api", Provider: "openai-compatible-external", Runtime: "openai-compatible", URL: "https://provider.invalid/v1", UpstreamModel: "provider/coder"}}}
	store := &fakeEndpointStore{fakeStore: base, resolvedEndpoint: domain.ResolvedEndpoint{Endpoint: domain.Endpoint{ID: "endpoint", TenantID: "global", Name: "coder-production"}}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	valid := `{"name":"managed","kind":"external","ownership_mode":"traffic-managed","target":"managed-api","config":{"adapter":"openai-compatible-external","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":100,"cost_limit_microusd":1000000,"max_request_cost_microusd":10000}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder-production/bindings", strings.NewReader(valid))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.binding.ID == "" || !strings.Contains(store.binding.ConfigJSON, `"secret_reference_id":"secret"`) || strings.Contains(store.binding.ConfigJSON, "api_key") {
		t.Fatalf("response=%d %s binding=%#v", response.Code, response.Body.String(), store.binding)
	}

	for name, body := range map[string]string{
		"missing policy": `{"name":"unsafe","kind":"external","ownership_mode":"traffic-managed","target":"managed-api","config":{}}`,
		"raw credential": `{"name":"unsafe","kind":"external","ownership_mode":"traffic-managed","target":"managed-api","config":{"adapter":"openai-compatible-external","secret_reference_id":"secret","api_key":"leak","enabled":true,"privacy_acknowledged":true,"request_limit":1,"cost_limit_microusd":1,"max_request_cost_microusd":1}}`,
		"no consent":     `{"name":"unsafe","kind":"external","ownership_mode":"traffic-managed","target":"managed-api","config":{"adapter":"openai-compatible-external","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":false,"request_limit":1,"cost_limit_microusd":1,"max_request_cost_microusd":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder-production/bindings", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestTenantOperatorCannotCreateCustomerWalletBinding(t *testing.T) {
	base := &fakeStore{
		principal: domain.Principal{ID: "operator-1", TenantID: "global", Name: "operator", Role: "operator", Scopes: []string{"read", "deploy", "manage_external"}},
		targets:   []domain.Target{{ID: "target", Name: "managed-api", Provider: "openai-compatible-external", Runtime: "openai-compatible", URL: "https://provider.invalid/v1", UpstreamModel: "provider/coder"}},
	}
	store := &fakeEndpointStore{fakeStore: base, resolvedEndpoint: domain.ResolvedEndpoint{Endpoint: domain.Endpoint{ID: "endpoint", TenantID: "global", Name: "coder-production"}}}
	handler := (API{Store: store, Authenticator: base}).Handler()
	body := `{"name":"managed","kind":"external","ownership_mode":"traffic-managed","target":"managed-api","config":{"adapter":"openai-compatible-external","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":100,"cost_limit_microusd":1000000,"max_request_cost_microusd":10000,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"supplier quote fixture","rate_card_valid_until":"2099-01-01T00:00:00Z"}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder-production/bindings", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer hosted-session")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || store.binding.ID != "" || !strings.Contains(response.Body.String(), "managed_billing_operator_required") {
		t.Fatalf("response=%d %s binding=%#v", response.Code, response.Body.String(), store.binding)
	}
}

func TestBootstrapCanReconcileManagedUsageWithoutRecordingContent(t *testing.T) {
	base := &fakeStore{}
	store := &fakeManagedBillingStore{
		fakeStore: base,
		reservations: []domain.ManagedUsageReservation{{
			ID:       "usage-1",
			TenantID: "tenant-1",
			State:    "pending_reconciliation",
		}},
	}
	handler := (API{Store: store, APIKey: "secret"}).Handler()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/billing/reservations?tenant_id=tenant-1&state=pending_reconciliation", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.reservationTenant != "tenant-1" || store.reservationState != "pending_reconciliation" || !strings.Contains(response.Body.String(), `"content_recorded":false`) {
		t.Fatalf("reservation response=%d %s tenant=%q state=%q", response.Code, response.Body.String(), store.reservationTenant, store.reservationState)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/billing/reservations/usage-1/settlement", strings.NewReader(`{"tenant_id":"tenant-1","input_tokens":12,"output_tokens":34}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.settledReservation.ID != "usage-1" || store.settledReservation.InputTokens == nil || *store.settledReservation.InputTokens != 12 || !strings.Contains(response.Body.String(), `"content_recorded":false`) {
		t.Fatalf("settlement response=%d %s reservation=%#v", response.Code, response.Body.String(), store.settledReservation)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/billing/reservations/usage-2/release", strings.NewReader(`{"tenant_id":"tenant-1","reason":"supplier confirmed no charge"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.releasedTenant != "tenant-1" || store.releasedID != "usage-2" || store.releaseReason != "supplier confirmed no charge" {
		t.Fatalf("release response=%d %s tenant=%q id=%q reason=%q", response.Code, response.Body.String(), store.releasedTenant, store.releasedID, store.releaseReason)
	}
}

func TestManagedPrepaidCheckoutCreditsOnlyFromVerifiedWebhook(t *testing.T) {
	base := &fakeStore{}
	store := &fakeManagedBillingStore{
		fakeStore: base,
		wallet: domain.ManagedWallet{
			TenantID:          "global",
			Currency:          "USD",
			BalanceMicrousd:   25_000_000,
			AvailableMicrousd: 25_000_000,
		},
		paymentResult: domain.ManagedPaymentResult{
			Provider:      "stripe",
			EventID:       "evt_paid",
			Status:        "applied",
			CreditApplied: true,
		},
	}
	checkout := &fakeManagedCheckoutProvider{
		session: domain.ManagedCheckoutSession{
			Provider:       "stripe",
			ProviderID:     "cs_test_paid",
			URL:            "https://checkout.stripe.test/session",
			AmountMicrousd: 25_000_000,
			Currency:       "USD",
		},
		payment: domain.ManagedPaymentEvent{
			Provider:       "stripe",
			EventID:        "evt_paid",
			EventType:      "checkout.session.completed",
			PayloadDigest:  strings.Repeat("a", 64),
			TenantID:       "global",
			SessionID:      "cs_test_paid",
			Currency:       "USD",
			AmountMicrousd: 25_000_000,
			MetadataJSON:   `{}`,
			Apply:          true,
		},
	}
	handler := (API{Store: store, APIKey: "secret", BillingCheckout: checkout}).Handler()

	request := httptest.NewRequest(http.MethodPost, "/api/v1/billing/checkout-sessions", strings.NewReader(`{"amount_microusd":25000000}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated checkout response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/billing/checkout-sessions", strings.NewReader(`{"amount_microusd":25000000}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || checkout.tenant != "global" || checkout.amount != 25_000_000 || store.payment.EventID != "" || !strings.Contains(response.Body.String(), `"balance_changed":false`) || !strings.Contains(response.Body.String(), `"credit_authority":"verified_provider_webhook"`) {
		t.Fatalf("checkout response=%d %s tenant=%q amount=%d payment=%#v", response.Code, response.Body.String(), checkout.tenant, checkout.amount, store.payment)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/billing/checkout-sessions", strings.NewReader(`{"amount_microusd":26000000}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_amount") {
		t.Fatalf("invalid checkout response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/billing/webhooks/stripe", strings.NewReader(`{"id":"evt_paid"}`))
	request.Header.Set("Stripe-Signature", "verified-test-signature")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.payment.EventID != "evt_paid" || !strings.Contains(response.Body.String(), `"credit_applied":true`) {
		t.Fatalf("webhook response=%d %s payment=%#v", response.Code, response.Body.String(), store.payment)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/billing/webhooks/stripe", strings.NewReader(`{"id":"evt_paid"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_signature") {
		t.Fatalf("unsigned webhook response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/billing/wallet", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"funding_available":true`) || !strings.Contains(response.Body.String(), `25000000`) {
		t.Fatalf("wallet response=%d %s", response.Code, response.Body.String())
	}
}

func TestTenantOperatorCannotUseManagedBillingAdministration(t *testing.T) {
	base := &fakeStore{principal: domain.Principal{ID: "operator-1", TenantID: "tenant-1", Name: "operator", Role: "admin", Scopes: authz.DefaultScopeNames(authz.Admin)}}
	store := &fakeManagedBillingStore{fakeStore: base}
	handler := (API{Store: store, Authenticator: base}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/billing/reservations?tenant_id=tenant-1", nil)
	request.Header.Set("Authorization", "Bearer hosted-session")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "bootstrap administrator") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestProviderConnectionCompilesBindingWithoutBrowserCredentialMetadata(t *testing.T) {
	base := &fakeStore{targets: []domain.Target{{ID: "target", Name: "provider-openrouter", Provider: "openrouter", Runtime: "openai-compatible-api", URL: "https://openrouter.ai/api/v1", UpstreamModel: "provider/model"}}}
	endpointStore := &fakeEndpointStore{fakeStore: base, resolvedEndpoint: domain.ResolvedEndpoint{Endpoint: domain.Endpoint{ID: "endpoint", TenantID: "global", Name: "coder-production"}}}
	store := &fakeProviderEndpointStore{fakeEndpointStore: endpointStore, connections: []domain.ProviderConnection{{ID: "connection", TenantID: "global", Name: "openrouter-main", Adapter: "openrouter", TargetID: "target", TargetName: "provider-openrouter", SecretReferenceID: "secret", SecretReferenceName: "openrouter"}}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	body := `{"name":"fallback","kind":"external","ownership_mode":"traffic-managed","provider_connection":"openrouter-main","config":{"enabled":true,"privacy_acknowledged":true,"request_limit":100,"cost_limit_microusd":1000000,"max_request_cost_microusd":10000}}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder-production/bindings", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || store.binding.TargetID != "target" || !strings.Contains(store.binding.ConfigJSON, `"adapter":"openrouter"`) || !strings.Contains(store.binding.ConfigJSON, `"secret_reference_id":"secret"`) {
		t.Fatalf("response=%d %s binding=%#v", response.Code, response.Body.String(), store.binding)
	}
	conflict := strings.Replace(body, `"config":{`, `"config":{"adapter":"openai-compatible-external",`, 1)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/coder-production/bindings", strings.NewReader(conflict))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "conflicts") {
		t.Fatalf("conflicting connection metadata response=%d %s", response.Code, response.Body.String())
	}
}

type fakeBenchmarkRunner struct{ config benchmark.Config }

type fakeBenchmarkEvidenceStore struct {
	*fakeStore
	measurement domain.MeasurementEvidence
	costs       []domain.CostEvidence
}

func (f *fakeBenchmarkEvidenceStore) BenchmarkOperationalMeasurement(context.Context, string, string, string, string, time.Time, time.Time) (domain.MeasurementEvidence, error) {
	if f.measurement.Value == nil {
		return domain.MeasurementEvidence{}, domain.ErrNotFound
	}
	return f.measurement, nil
}
func (f *fakeBenchmarkEvidenceStore) RecordCostEvidence(_ context.Context, _, _ string, rows []domain.CostEvidence) ([]domain.CostEvidence, error) {
	return rows, f.err
}
func (f *fakeBenchmarkEvidenceStore) CostEvidenceForDeployment(context.Context, string, string, time.Time, time.Time, int) ([]domain.CostEvidence, error) {
	return f.costs, f.err
}

type fakePassportStore struct {
	*fakeStore
	payload   passport.Payload
	passports []domain.InferencePassport
}

func (f *fakePassportStore) InferencePassportPayload(context.Context, string, string, string) (passport.Payload, error) {
	return f.payload, f.err
}
func (f *fakePassportStore) RecordInferencePassport(_ context.Context, value domain.InferencePassport) (domain.InferencePassport, error) {
	value.ID = "passport-1"
	value.CreatedAt = time.Now().UTC()
	f.passports = append(f.passports, value)
	return value, f.err
}
func (f *fakePassportStore) InferencePassports(context.Context, string, string, int) ([]domain.InferencePassport, error) {
	return f.passports, f.err
}

func (f *fakeBenchmarkRunner) Run(_ context.Context, cfg benchmark.Config) (benchmark.Result, error) {
	f.config = cfg
	value := 12.5
	goodput := 4.25
	return benchmark.Result{Tool: "aiperf", ToolVersion: "0.9.0", Command: "aiperf profile --api-key ${INFERCRANE_API_KEY}", Requests: 10, Succeeded: 10, TTFTP95MS: &value, Goodput: &goodput}, nil
}
func TestOperationAPIAuthenticationAndResponse(t *testing.T) {
	store := &fakeStore{operation: domain.Operation{ID: "op", TenantID: "global", Status: "succeeded", ResultJSON: `{"endpoint_name":"coder-production"}`, MaxAttempts: 5}}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/operations/op", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/op", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"max_attempts":5`) || !strings.Contains(response.Body.String(), `"result":{"endpoint_name":"coder-production"}`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated control response may be cached: %q", response.Header().Get("Cache-Control"))
	}
}

func TestIntegrationsReturnsVersionedCapabilityEvidence(t *testing.T) {
	registry, err := integration.BaseCatalog()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret", Integrations: registry.Snapshot()}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, required := range []string{integration.ProviderContractV1, integration.RuntimeContractV1, "runpod-serverless", "real-runpod-serverless", "deferred"} {
		if !strings.Contains(body, required) {
			t.Fatalf("integration response missing %q: %s", required, body)
		}
	}
}

func TestRecommendationAPIUsesPersistedQualifiedEvidenceAndDisclosesCapacity(t *testing.T) {
	maxTTFT, measured := 250.0, 200.0
	observed := time.Now().UTC()
	store := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", TenantID: "global", Name: "qwen", ActiveRevisionID: "revision-1"}}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "Qwen/Qwen3-8B@commit"}, capacity: domain.CapacityEvidence{ID: "capacity-1", State: "available", Source: "provider.stock", ObservedAt: observed, ExpiresAt: observed.Add(time.Minute)}, sloPolicy: domain.SLOPolicy{DeploymentID: "deployment-1", MaxTTFTP95MS: &maxTTFT}, benchmarks: []domain.BenchmarkResult{{ID: "benchmark-1", DeploymentID: "deployment-1", ModelIdentity: "Qwen/Qwen3-8B@commit", Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Provider: "runpod", GPU: "L40S", ComputeMode: "elastic", WorkloadJSON: `{"requests":100,"concurrency":1}`, RequestCount: 100, Succeeded: 100, TTFTP95MS: &measured, CostMetadataJSON: `{"available":false}`}}}
	registry, err := integration.PortableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/recommendations", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", Integrations: registry.Snapshot()}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"status":"recommended"`) || !strings.Contains(response.Body.String(), `"capacity_state":"available"`) || !strings.Contains(response.Body.String(), `"capacity_source":"provider.stock"`) || !strings.Contains(response.Body.String(), `"input_snapshot"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if len(store.recommendations) != 1 || store.recommendations[0].InputSnapshotJSON == "" {
		t.Fatalf("recommendation not persisted: %#v", store.recommendations)
	}
}

func TestRecommendationAPIRejectsInputAndUnlikeEvidence(t *testing.T) {
	maxTTFT, measured := 250.0, 100.0
	store := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "revision-1"}}, artifact: domain.ModelArtifact{ID: "artifact-1", ModelIdentity: "model@active"}, sloPolicy: domain.SLOPolicy{DeploymentID: "deployment-1", MaxTTFTP95MS: &maxTTFT}, benchmarks: []domain.BenchmarkResult{{ID: "benchmark-1", ModelIdentity: "model@other", Runtime: "vllm", Provider: "runpod", ComputeMode: "elastic", WorkloadJSON: `{}`, RequestCount: 10, TTFTP95MS: &measured}}}
	registry, _ := integration.PortableCatalog()
	for _, body := range []string{`{"unexpected":true}`, `{}`, "{} {}"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/recommendations", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		(API{Store: store, APIKey: "secret", Integrations: registry.Snapshot()}).Handler().ServeHTTP(response, request)
		if body == `{}` {
			if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"status":"unknown"`) || !strings.Contains(response.Body.String(), `comparable_model_artifact`) || !strings.Contains(response.Body.String(), `runtime-version-mismatch`) {
				t.Fatalf("unlike evidence response=%d %s", response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid body %q accepted: %d %s", body, response.Code, response.Body.String())
		}
	}
}

func TestSLOPolicyAPIRejectsUnknownAndEmptyInput(t *testing.T) {
	store := &fakeStore{}
	for _, body := range []string{`{"typo":1}`, `{}`, `{"max_ttft_p95_ms":1}{}`} {
		request := httptest.NewRequest(http.MethodPut, "/api/v1/deployments/qwen/slo-policy", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatalf("body %s accepted: %d %s", body, response.Code, response.Body.String())
		}
	}
}

func TestDeploymentReadAPIReturnsDurableState(t *testing.T) {
	store := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment", Name: "qwen"}, EndpointNames: []string{"support-production"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deployment"`) || !strings.Contains(response.Body.String(), `"revisions"`) || !strings.Contains(response.Body.String(), `"endpoint_names":["support-production"]`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestWhoAmIReturnsAuthenticatedIdentity(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"bootstrap"`) || !strings.Contains(response.Body.String(), `"role":"admin"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestConsoleSessionUsesAuthenticatedTenantAndNeverBrowserTenantInput(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "human-1", TenantID: "tenant-a", Name: "Ada", Role: "viewer", Kind: "human", Scopes: []string{"read"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/console/session?tenant_id=tenant-b", nil)
	request.Header.Set("Authorization", "Bearer hosted-session")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tenant_id":"tenant-a"`) || strings.Contains(response.Body.String(), `tenant-b`) || !strings.Contains(response.Body.String(), `"web_console_access"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestConsoleAccessProvisioningIsAdminOnlyAndTenantScoped(t *testing.T) {
	body := `{"provider":"clerk","external_user_id":"user_external","external_organization_id":"org_external","display_name":"Ada","role":"operator","scopes":["read","deploy"],"access":true}`
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/console/access", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer bootstrap")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "bootstrap"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.consoleIdentity.TenantID != "global" || !store.consoleIdentity.Access {
		t.Fatalf("identity=%#v response=%d %s", store.consoleIdentity, response.Code, response.Body.String())
	}

	store.principal = domain.Principal{ID: "viewer", TenantID: "tenant-a", Name: "viewer", Role: "viewer", Scopes: []string{"read"}}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/console/access", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer viewer-token")
	response = httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("viewer response=%d %s", response.Code, response.Body.String())
	}
}

func TestConsoleAdministrativeListsAreTenantScopedAndCredentialSafe(t *testing.T) {
	store := &fakeStore{
		operations:     []domain.Operation{{ID: "op-1", TenantID: "global", Kind: "converge", Status: "running"}},
		principals:     []domain.Principal{{ID: "key-1", TenantID: "global", Name: "ci", Role: "operator", Kind: "service_account", Scopes: []string{"read", "deploy"}}},
		consoleMembers: []domain.ConsoleIdentity{{UserID: "user-1", TenantID: "global", DisplayName: "Ada", Role: "admin", Scopes: []string{"read"}, Access: true}},
	}
	for _, endpoint := range []string{"/api/v1/operations", "/api/v1/principals", "/api/v1/console/access"} {
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data"`) || strings.Contains(response.Body.String(), "credential_hash") {
			t.Fatalf("endpoint=%s response=%d %s", endpoint, response.Code, response.Body.String())
		}
	}
}

func TestDeploymentAndOperationEventsAreTenantScoped(t *testing.T) {
	store := &fakeStore{operation: domain.Operation{ID: "op", TenantID: "global"}}
	for _, endpoint := range []string{"/api/v1/deployments/qwen/events", "/api/v1/operations/op/events"} {
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data"`) {
			t.Fatalf("endpoint=%s response=%d %s", endpoint, response.Code, response.Body.String())
		}
	}
}

func TestScalingDecisionsAreReadThroughTenantAPI(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/qwen/scaling-decisions", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestLifecycleStatusSeparatesServingFromConvergence(t *testing.T) {
	resolved := domain.ResolvedDeployment{
		Deployment: domain.Deployment{ID: "dep", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2", MinReplicas: 1},
		Targets: []domain.Target{
			{ID: "ready", Health: "healthy"},
			{ID: "starting", Health: "starting"},
		},
	}
	replicas := []domain.Replica{
		{RevisionID: "rev-1", LifecycleState: "active", Health: "healthy"},
		{RevisionID: "rev-1", LifecycleState: "starting", Health: "starting"},
		{RevisionID: "rev-old", LifecycleState: "draining", Health: "healthy"},
	}
	operation := domain.Operation{ID: "op-scale", Kind: "deployment.scale", RequestJSON: `{"desired_replicas":2}`}
	status := deploymentLifecycleStatus(resolved, replicas, []domain.DeploymentRevision{{ID: "rev-2", Status: "candidate"}}, operation, true)
	if status.ServingState != "serving" || status.ConvergenceState != "converging" || status.ReadyReplicas != 1 || status.DesiredReplicas != 2 || status.ProvisioningReplicas != 1 || status.DrainingReplicas != 1 || status.CandidateState != "candidate" || status.BlockingOperationID != "op-scale" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestLifecycleStatusReportsReadyBeforeRoutePublication(t *testing.T) {
	resolved := domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", ActiveRevisionID: "rev-1", MinReplicas: 1}}
	status := deploymentLifecycleStatus(resolved, []domain.Replica{{RevisionID: "rev-1", LifecycleState: "active", Health: "healthy"}}, nil, domain.Operation{}, false)
	if status.ServingState != "ready" || status.ConvergenceState != "converged" || status.ReadyReplicas != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestLifecycleStatusCountsHealthyExistingTargetsAsReadyCapacity(t *testing.T) {
	resolved := domain.ResolvedDeployment{
		Deployment: domain.Deployment{ID: "dep", MinReplicas: 1},
		Targets: []domain.Target{
			{ID: "ready-a", Health: "healthy"},
			{ID: "ready-b", Health: "healthy"},
		},
	}
	status := deploymentLifecycleStatus(resolved, nil, nil, domain.Operation{}, false)
	if status.ServingState != "serving" || status.ReadyReplicas != 2 || status.DesiredReplicas != 2 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestLifecycleStatusDoesNotCountGovernedFallbackAsPrimaryCapacity(t *testing.T) {
	resolved := domain.ResolvedDeployment{
		Deployment: domain.Deployment{ID: "dep", MinReplicas: 1},
		Targets: []domain.Target{
			{ID: "primary", Provider: "existing", Health: "healthy"},
			{ID: "fallback", Provider: "openai-compatible-external", Health: "starting"},
		},
	}
	status := deploymentLifecycleStatus(resolved, nil, nil, domain.Operation{}, false)
	if status.ServingState != "serving" || status.ConvergenceState != "converged" || status.ReadyReplicas != 1 || status.DesiredReplicas != 1 || status.UnhealthyTargets != 0 {
		t.Fatalf("fallback changed primary lifecycle status: %+v", status)
	}
}

func TestBenchmarkRunsThroughControlPlaneAndPersistsIdentity(t *testing.T) {
	spec := `{"model":"Qwen/Qwen3-8B","model_revision":"commit","runtime":"vllm","runtime_version":"0.10","compute_mode":"elastic","gpu":"L40S","region":"EU"}`
	store := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev"}, Targets: []domain.Target{{Provider: "runpod"}}}, revisions: []domain.DeploymentRevision{{ID: "rev", SpecJSON: spec}}, artifact: domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@commit"}}
	runner := &fakeBenchmarkRunner{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2,"random_seed":42}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: runner, GatewayURL: "http://gateway", AIPerfBinary: "aiperf"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"model_identity":"Qwen/Qwen3-8B@commit"`) || !strings.Contains(response.Body.String(), `"gpu_count":1`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if runner.config.APIKey != "secret" || runner.config.RandomSeed != 42 || runner.config.Model != "qwen" || runner.config.Tokenizer != "Qwen/Qwen3-8B" || len(store.benchmarks) != 1 || store.benchmarks[0].GPU != "L40S" || store.benchmarks[0].GPUCount == nil || *store.benchmarks[0].GPUCount != 1 || !strings.Contains(store.benchmarks[0].CostMetadataJSON, `"available":false`) {
		t.Fatalf("config=%#v benchmarks=%#v", runner.config, store.benchmarks)
	}
}

func TestBenchmarkPersistsProfileSLOAndGoodputEvidence(t *testing.T) {
	spec := `{"model":"mistralai/Mistral-7B-Instruct-v0.3","model_revision":"commit","runtime":"vllm","compute_mode":"elastic","gpu":"L40S"}`
	store := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "mistral", ActiveRevisionID: "rev"}}, revisions: []domain.DeploymentRevision{{ID: "rev", SpecJSON: spec}}, artifact: domain.ModelArtifact{ID: "artifact", Repository: "mistralai/Mistral-7B-Instruct-v0.3", ModelIdentity: "mistralai/Mistral-7B-Instruct-v0.3@commit"}}
	runner := &fakeBenchmarkRunner{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/mistral/benchmarks", strings.NewReader(`{"requests":256,"concurrency":1,"input_tokens":512,"output_tokens":128,"streaming":true,"profile":"interactive","profile_version":"benchmark-profile-v1","ttft_slo_ms":250,"tpot_slo_ms":20}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: runner, GatewayURL: "http://gateway"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.benchmarks) != 1 || !strings.Contains(store.benchmarks[0].WorkloadJSON, `"profile":"interactive"`) || runner.config.TTFTSLOMS != 250 || runner.config.TPOTSLOMS != 20 || store.benchmarks[0].Goodput == nil || *store.benchmarks[0].Goodput != 4.25 {
		t.Fatalf("response=%d %s config=%#v benchmarks=%#v", response.Code, response.Body.String(), runner.config, store.benchmarks)
	}
}

func TestBenchmarkAttachesOnlyFreshRevisionBoundOperationalAndCostEvidence(t *testing.T) {
	now := time.Now().UTC()
	gpu := 73.5
	base := &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "llama", ActiveRevisionID: "rev"}}, revisions: []domain.DeploymentRevision{{ID: "rev", SpecJSON: `{"model":"meta-llama/Llama-3.1-8B-Instruct","model_revision":"commit","runtime":"vllm","compute_mode":"elastic","cloud":"aws","gpu":"L40S"}`}}, artifact: domain.ModelArtifact{ID: "artifact", Repository: "meta-llama/Llama-3.1-8B-Instruct", ModelIdentity: "meta-llama/Llama-3.1-8B-Instruct@commit"}}
	store := &fakeBenchmarkEvidenceStore{fakeStore: base, measurement: domain.MeasurementEvidence{Name: "gpu_utilization", Value: &gpu, Unit: "percent", Availability: "available", EvidenceClass: "measured", Source: "dcgm_exporter"}, costs: []domain.CostEvidence{{RevisionID: "rev", Source: "opencost/allocation", Scope: "deployment_hourly_rate/llama", Resource: "g6e.xlarge", Currency: "USD", BillingUnit: "hour", EvidenceClass: "measured", Amount: 1.86, WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(-time.Minute), ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/llama/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: &fakeBenchmarkRunner{}, GatewayURL: "http://gateway"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(base.benchmarks) != 1 || base.benchmarks[0].GPUUtilization == nil || *base.benchmarks[0].GPUUtilization != gpu || !strings.Contains(base.benchmarks[0].CostMetadataJSON, `"available":true`) || !strings.Contains(base.benchmarks[0].CostMetadataJSON, `"source":"opencost/allocation"`) || !strings.Contains(base.benchmarks[0].CostMetadataJSON, `"revision_id":"rev"`) {
		t.Fatalf("response=%d %s benchmarks=%#v", response.Code, response.Body.String(), base.benchmarks)
	}

	store.measurement.EvidenceClass = "provider_reported"
	store.costs[0].EvidenceClass = "provider_reported"
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/llama/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: &fakeBenchmarkRunner{}, GatewayURL: "http://gateway"}).Handler().ServeHTTP(response, request)
	latest := base.benchmarks[len(base.benchmarks)-1]
	if response.Code != http.StatusCreated || latest.GPUUtilization != nil || !strings.Contains(latest.CostMetadataJSON, `"available":false`) {
		t.Fatalf("unqualified evidence was attached: response=%d %s benchmark=%#v", response.Code, response.Body.String(), latest)
	}
}

func TestBenchmarkRejectsUnknownOrVersionSkewedProfile(t *testing.T) {
	for _, body := range []string{
		`{"requests":10,"concurrency":1,"profile":"fastest","profile_version":"benchmark-profile-v1"}`,
		`{"requests":10,"concurrency":1,"profile":"interactive","profile_version":"benchmark-profile-v0"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/m/benchmarks", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		(API{Store: &fakeStore{}, APIKey: "secret", BenchmarkRunner: &fakeBenchmarkRunner{}, GatewayURL: "http://gateway"}).Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body=%s response=%d %s", body, response.Code, response.Body.String())
		}
	}
}

func TestBenchmarkNormalizesQualifiedRuntimeCloudAndObservedRegion(t *testing.T) {
	spec := `{"model":"Qwen/Qwen3-8B","runtime":"vllm","compute_mode":"elastic","cloud":"runpod","gpu":"H100"}`
	store := &fakeStore{
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev"}, Targets: []domain.Target{{Provider: "skypilot"}}},
		revisions: []domain.DeploymentRevision{{ID: "rev", SpecJSON: spec}},
		replicas:  []domain.Replica{{RevisionID: "rev", ProviderDetails: `[{"cloud":"RunPod","region":"US-CA-1"}]`}},
		artifact:  domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@commit"},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", BenchmarkRunner: &fakeBenchmarkRunner{}, GatewayURL: "http://gateway", AIPerfBinary: "aiperf"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || len(store.benchmarks) != 1 {
		t.Fatalf("response=%d %s benchmarks=%#v", response.Code, response.Body.String(), store.benchmarks)
	}
	result := store.benchmarks[0]
	if result.RuntimeVersion != support.DefaultRuntimeVersion || result.Provider != "runpod" || result.Region != "US-CA-1" {
		t.Fatalf("runtime=%q provider=%q region=%q", result.RuntimeVersion, result.Provider, result.Region)
	}
}

func TestCandidateBenchmarkUsesExplicitHealthyRevisionEndpoint(t *testing.T) {
	spec := `{"model":"Qwen/Qwen3-8B","runtime":"vllm","compute_mode":"elastic","gpu":"L40S"}`
	store := &fakeStore{
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev-active", CandidateRevisionID: "rev-candidate"}},
		revisions: []domain.DeploymentRevision{{ID: "rev-active", SpecJSON: spec}, {ID: "rev-candidate", SpecJSON: spec}},
		replicas:  []domain.Replica{{RevisionID: "rev-candidate", Ordinal: 0, Provider: "skypilot", LifecycleState: "ready", Health: "healthy", Endpoint: "https://candidate.invalid"}},
		artifact:  domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@commit"},
	}
	runner := &fakeBenchmarkRunner{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/benchmarks", strings.NewReader(`{"requests":10,"concurrency":2,"random_seed":42,"revision":"candidate"}`))
	request.Header.Set("Authorization", "Bearer bootstrap")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "bootstrap", BenchmarkRunner: runner, Backends: map[string]BackendMetadata{"skypilot": {APIKey: "worker-secret", APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}}, GatewayURL: "http://gateway", AIPerfBinary: "aiperf"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || runner.config.Endpoint != "https://candidate.invalid" || runner.config.Model != "Qwen/Qwen3-8B" || runner.config.Tokenizer != "Qwen/Qwen3-8B" || runner.config.APIKey != "worker-secret" || runner.config.APIKeyEnv != "INFERCRANE_WORKER_API_KEY" || len(store.benchmarks) != 1 || store.benchmarks[0].RevisionID != "rev-candidate" || !strings.Contains(store.benchmarks[0].WorkloadJSON, `"direct_revision_validation":true`) {
		t.Fatalf("response=%d %s config=%#v benchmarks=%#v", response.Code, response.Body.String(), runner.config, store.benchmarks)
	}
}

func TestInferencePassportAPIProducesVerifiableSecretFreeEvidence(t *testing.T) {
	_, privateKey, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakePassportStore{fakeStore: &fakeStore{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "dep", Name: "qwen", ActiveRevisionID: "rev-1"}}}, payload: passport.Payload{Schema: "infercrane.inference-passport/v1", Deployment: "qwen", RevisionID: "rev-1", RevisionSpec: domain.DeploymentRevisionSpec{Model: "model", Runtime: "vllm"}, Benchmarks: []passport.Benchmark{}, Reproduce: passport.Reproduction{BenchmarkCommands: []string{}}, IssuedAt: time.Unix(1, 0).UTC()}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/passports", strings.NewReader(`{"revision_id":"rev-1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", PassportPrivateKey: privateKey}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"verified":true`) || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if len(store.passports) != 1 {
		t.Fatalf("passports=%#v", store.passports)
	}
	row := store.passports[0]
	if err = passport.Verify(passport.Envelope{PayloadJSON: row.PayloadJSON, Digest: row.PayloadDigest, Signature: row.Signature, PublicKey: row.PublicKey, Algorithm: row.Algorithm, KeyID: row.KeyID}); err != nil {
		t.Fatal(err)
	}
}

func TestInferencePassportAPIFailsClosedWithoutSigningKey(t *testing.T) {
	store := &fakePassportStore{fakeStore: &fakeStore{}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/qwen/passports", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "passport_signing_unavailable") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestRouteAndTenantMutationsUseAuthenticatedAPI(t *testing.T) {
	store := &fakeStore{}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPut, "/api/v1/deployments/qwen/route", `{"strategy":"round-robin"}`, http.StatusOK},
		{http.MethodPost, "/api/v1/tenants", `{"id":"tenant-a","name":"Tenant A"}`, http.StatusCreated},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s %s response=%d %s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestApplyQueuesIdempotentOperation(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/apply", strings.NewReader(`{"name":"prod","model":"model","targets":["gpu-a"]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "release-1")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/v1/operations/queued" || store.operation.Kind != "deployment.apply-existing" || !strings.Contains(store.operation.RequestJSON, `"endpoint_name":"prod"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestRolloutTransitionsQueueDurableOperations(t *testing.T) {
	store := &fakeStore{created: true}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	tests := []struct {
		path, body, kind string
	}{
		{"/api/v1/deployments/prod/rollouts", `{"spec":{"model":"Qwen/Qwen3-8B"}}`, "rollout.create-candidate"},
		{"/api/v1/deployments/prod/rollouts/guard/evaluate", ``, "release-guard.evaluate"},
		{"/api/v1/deployments/prod/rollouts/rev-2/promote", ``, "rollout.promote"},
		{"/api/v1/deployments/prod/rollouts/rev-2/provision", ``, "rollout.provision-candidate"},
		{"/api/v1/deployments/prod/rollouts/rev-2/reject", `{"reason":"readiness failed"}`, "rollout.reject"},
		{"/api/v1/deployments/prod/rollback", `{"revision_id":"rev-1","reason":"operator rollback"}`, "rollout.rollback"},
	}
	for i, test := range tests {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Idempotency-Key", "rollout-"+test.kind)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted || store.operation.Kind != test.kind || store.operation.ResourceName != "prod" {
			t.Fatalf("case %d response=%d %s operation=%#v", i, response.Code, response.Body.String(), store.operation)
		}
	}
}

func TestCloudDeployPersistsAndQueuesConverge(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","min_replicas":1,"max_replicas":4}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/api/v1/operations/queued" || store.operation.Kind != "deployment.converge" || store.operation.ResourceName != "qwen" {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
	if !strings.Contains(store.operation.RequestJSON, `"tenant_id":"global"`) || !strings.Contains(store.operation.RequestJSON, `"endpoint_name":"qwen"`) {
		t.Fatalf("request=%s", store.operation.RequestJSON)
	}
}

func TestPortableRuntimeDeployValidationAndPersistence(t *testing.T) {
	validWorkload := `{"image":"registry.example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","command":["serve","--model","${MODEL}"],"protocol":"openai","port":8000,"readiness_path":"/health","models_path":"/v1/models","metrics_path":"/metrics","cancellation":"http-disconnect","drain":"connection","shutdown_grace_seconds":30}`
	for _, test := range []struct {
		name, body string
		status     int
		contains   string
	}{
		{"sglang-default", `{"name":"sg","model":"org/model","runtime":"sglang","cloud":"aws","region":"eu-central-1","gpu":"L40S"}`, http.StatusAccepted, `"image":"lmsysorg/sglang:v0.5.12@sha256:`},
		{"kubernetes-vllm", `{"name":"kube","model":"org/model","runtime":"vllm","cloud":"kubernetes","gpu":"NVIDIA-L40S"}`, http.StatusAccepted, `"cloud":"kubernetes"`},
		{"custom", `{"name":"custom","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S","workload":` + validWorkload + `}`, http.StatusAccepted, `"runtime":"custom-oci"`},
		{"mutable", `{"name":"bad","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S","workload":` + strings.Replace(validWorkload, "@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ":latest", 1) + `}`, http.StatusUnprocessableEntity, `pinned by @sha256`},
		{"missing", `{"name":"bad","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S"}`, http.StatusUnprocessableEntity, `requires an explicit workload`},
		{"port-conflict", `{"name":"bad","model":"org/model","runtime":"custom-oci","cloud":"aws","region":"eu-central-1","gpu":"L40S","port":9000,"workload":` + validWorkload + `}`, http.StatusUnprocessableEntity, `port conflicts with workload.port`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{created: true}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer secret")
			request.Header.Set("Idempotency-Key", "portable-"+test.name)
			response := httptest.NewRecorder()
			(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
			combined := response.Body.String() + store.operation.RequestJSON
			if response.Code != test.status || !strings.Contains(combined, test.contains) {
				t.Fatalf("status=%d body=%s operation=%s", response.Code, response.Body.String(), store.operation.RequestJSON)
			}
		})
	}
}

func TestServerlessDeployQueuesProviderNativeConverge(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","compute_mode":"serverless","cloud":"runpod","gpu":"L40S","min_replicas":0,"max_replicas":4}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen-serverless")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.serverless.converge" || !strings.Contains(store.operation.RequestJSON, `"compute_mode":"serverless"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestCloudDeployPersistsExactProviderAdapterIntent(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen-gcp","model":"Qwen/Qwen3-8B","cloud":"gcp","provider_adapter":"gcp-compute","region":"europe-west4","gpu":"nvidia-l4","min_replicas":1,"max_replicas":1}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen-gcp")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(store.operation.RequestJSON, `"provider_adapter":"gcp-compute"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestCloudDeployInjectsOnlyConfiguredDeclarativeSkyPilotAdapter(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen-lambda","model":"Qwen/Qwen3-8B","cloud":"lambda","gpu":"H100","min_replicas":1,"max_replicas":1}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen-lambda")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "lambda", State: "ready"}}, DefaultProviderAdapters: map[string]string{"lambda": "skypilot-lambda"}}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(store.operation.RequestJSON, `"provider_adapter":"skypilot-lambda"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}

	store = &fakeStore{created: true}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen-lambda","model":"Qwen/Qwen3-8B","cloud":"lambda","gpu":"H100"}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-qwen-lambda-unconfigured")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "lambda", State: "connection-required"}}}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || store.operation.RequestJSON != "" {
		t.Fatalf("unconfigured declarative cloud did not fail closed: response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestDynamoDeployPersistsTopologyAndRejectsUnqualifiedModes(t *testing.T) {
	valid := `{"name":"llama-dynamo","model":"meta-llama/Llama-3.1-8B-Instruct","runtime":"vllm","cloud":"kubernetes","provider_adapter":"kubernetes-dynamo","gpu":"nvidia.com/gpu","min_replicas":1,"max_replicas":1,"serving":{"schema_version":"infercrane.serving/v1","backend":"dynamo","profile":"baseline","mode":"aggregated","routing":"direct","worker":{"replicas":1,"tensor_parallelism":1},"autoscaling":{"owner":"disabled"},"cache":{"backend":"none"}}}`
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(valid))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-llama-dynamo")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.converge" || !strings.Contains(store.operation.RequestJSON, `"backend":"dynamo"`) || !strings.Contains(store.operation.RequestJSON, `"provider_adapter":"kubernetes-dynamo"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}

	for _, test := range []struct {
		name, body, contains string
	}{
		{"missing-topology", strings.Replace(valid, `,"serving":{"schema_version":"infercrane.serving/v1","backend":"dynamo","profile":"baseline","mode":"aggregated","routing":"direct","worker":{"replicas":1,"tensor_parallelism":1},"autoscaling":{"owner":"disabled"},"cache":{"backend":"none"}}`, "", 1), "explicit Dynamo serving topology"},
		{"outer-autoscaling", strings.Replace(valid, `"max_replicas":1`, `"max_replicas":2`, 1), "outer replica bounds must both equal 1"},
		{"dynamo-planner", strings.Replace(valid, `"owner":"disabled"`, `"owner":"dynamo-planner","min":1,"max":2`, 1), "registered but not executable"},
		{"lmcache", strings.Replace(valid, `"backend":"none"`, `"backend":"lmcache"`, 1), "registered but not executable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalidStore := &fakeStore{created: true}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer secret")
			request.Header.Set("Idempotency-Key", "reject-"+test.name)
			response := httptest.NewRecorder()
			(API{Store: invalidStore, APIKey: "secret"}).Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), test.contains) || invalidStore.operation.ID != "" {
				t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), invalidStore.operation)
			}
		})
	}
}

func TestDeleteQueuesDurableCleanup(t *testing.T) {
	store := &fakeStore{created: true}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "delete-qwen")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.delete" || !strings.Contains(store.operation.RequestJSON, `"deployment_id":"deployment"`) {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestServerlessDeleteQueuesEndpointCleanup(t *testing.T) {
	store := &fakeStore{created: true, targets: []domain.Target{{Provider: "runpod-serverless"}}}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "delete-qwen-serverless")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", Backends: map[string]BackendMetadata{"runpod-serverless": {Serverless: true}}}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.operation.Kind != "deployment.serverless.delete" {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), store.operation)
	}
}

func TestViewerCannotApply(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "viewer", TenantID: "tenant-a", Name: "reader", Role: "viewer"}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/apply", strings.NewReader(`{"name":"prod","model":"model","targets":["gpu-a"]}`))
	request.Header.Set("Authorization", "Bearer ic_viewer")
	request.Header.Set("Idempotency-Key", "release-1")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestServiceAccountScopeRestrictsRolePermission(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "read-bot", Role: "operator", Scopes: []string{"read"}}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/apply", strings.NewReader(`{"name":"prod","model":"model","targets":["gpu-a"]}`))
	request.Header.Set("Authorization", "Bearer ic_operator")
	request.Header.Set("Idempotency-Key", "release-1")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestServiceAccountCannotRequestScopeAboveRole(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/principals", strings.NewReader(`{"name":"bad-bot","role":"viewer","scopes":["deploy"]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "exceeds role") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestSecretAPIAcceptsReferencesButNeverRawValues(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(`{"name":"openrouter","resolver":"env","reference":"OPENROUTER_API_KEY"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "OPENROUTER_API_KEY") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(`{"name":"unsafe","resolver":"env","reference":"OPENROUTER_API_KEY","value":"must-not-persist"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "must-not-persist") {
		t.Fatalf("raw value was accepted or reflected: %d %s", response.Code, response.Body.String())
	}
}

func TestSecretAPIEmptyListIsAJSONCollection(t *testing.T) {
	store := &fakeEmptySecretStore{fakeStore: &fakeStore{}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data":[]`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

type fakeEmptySecretStore struct{ *fakeStore }

func (*fakeEmptySecretStore) SecretReferencesForTenant(context.Context, string) ([]domain.SecretReference, error) {
	return nil, nil
}

func TestOperatorCannotManageSecretReferences(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: "operator", Scopes: []string{"read", "deploy"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/secrets", nil)
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestExternalTargetRegistrationRequiresExternalScopeAndSafeURL(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "deploy-bot", Role: "operator", Scopes: []string{"deploy"}}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/targets", strings.NewReader(`{"name":"external","provider":"openrouter","url":"https://openrouter.ai/api","runtime":"openai","upstream_model":"provider/model"}`))
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/targets", strings.NewReader(`{"name":"external","provider":"openrouter","url":"https://openrouter.ai/api?key=unsafe","runtime":"openai","upstream_model":"provider/model"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "query parameters") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestExternalPolicyRequiresPrivacyAndHardBudgets(t *testing.T) {
	store := &fakeStore{targets: []domain.Target{{ID: "external", Name: "external", Provider: "openrouter"}}}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/deployments/qwen/external-policy", strings.NewReader(`{"target":"external","adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":false,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "privacy_acknowledgement_required") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/deployments/qwen/external-policy", strings.NewReader(`{"target":"external","adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"policy"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/deployments/qwen/external-policy", strings.NewReader(`{"target":"external","adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100,"overflow_mode":"health_and_queue","queue_threshold":4,"breach_intervals":3,"recovery_intervals":2,"cooldown_seconds":60,"signal_max_age_seconds":30}`))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"overflow_mode":"health_and_queue"`) || !strings.Contains(response.Body.String(), `"queue_threshold":4`) {
		t.Fatalf("queue policy response=%d %s", response.Code, response.Body.String())
	}
}

func TestTargetRegistrationRejectsEmbeddedCredentials(t *testing.T) {
	store := &fakeStore{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/targets", strings.NewReader(`{"name":"gpu-a","url":"https://user:secret@worker.internal/v1"}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "without credentials") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestCancelHidesMissingOperation(t *testing.T) {
	store := &fakeStore{err: domain.ErrNotFound}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/operations/missing/cancel", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not_found") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestDoctorDiagnosticsRunInsideAuthenticatedControlPlane(t *testing.T) {
	called := false
	handler := (API{Store: &fakeStore{}, APIKey: "secret", Diagnostics: func(_ context.Context, cloud, serverless, aws, gcp, kubernetes bool, gpu string) doctor.Report {
		called = cloud && serverless && aws && gcp && kubernetes && gpu == "L4"
		return doctor.Report{Ready: true, Checks: []doctor.Check{{Name: "PostgreSQL", Status: doctor.Pass, Message: "connected"}}}
	}}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/doctor?cloud=true&serverless=true&aws=true&gcp=true&kubernetes=true&gpu=L4", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !called || !strings.Contains(response.Body.String(), `"ready":true`) {
		t.Fatalf("response=%d %s called=%t", response.Code, response.Body.String(), called)
	}
}

func TestControlPlaneMembershipIsAuthenticatedAndInspectable(t *testing.T) {
	store := &fakeMembershipStore{fakeStore: &fakeStore{}, instances: []domain.ControlPlaneInstance{{ID: "node-a", BinaryVersion: "1.6.0", ProtocolMin: 1, ProtocolMax: 2, HeartbeatAt: time.Now().UTC()}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/system/instances", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"node-a"`) || !strings.Contains(response.Body.String(), `"protocol_max":2`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestSandboxCompositionIssuesOneTimeScopedCredentialWithoutExternalMutation(t *testing.T) {
	store := &fakeStore{}
	refreshes := 0
	refresh := func(context.Context) error { refreshes++; return nil }
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/references", strings.NewReader(`{"provider":"e2b","external_id":"sandbox-42","endpoint":"coder-production","ttl_seconds":1800,"metadata":{"team":"agents"}}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", CredentialRefresh: refresh}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"credential":"ic_sandbox_once"`) || !strings.Contains(response.Body.String(), `"credential_cache_synchronized":true`) || !strings.Contains(response.Body.String(), `"external_resource_mutated":false`) || strings.Contains(response.Body.String(), "sandbox-principal") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/references/sandbox-ref/credential/rotate", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", CredentialRefresh: refresh}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"credential_cache_synchronized":true`) {
		t.Fatalf("rotate response=%d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/references/sandbox-ref", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret", CredentialRefresh: refresh}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"credential_revoked":true`) || !strings.Contains(response.Body.String(), `"external_resource_mutated":false`) {
		t.Fatalf("revoke response=%d %s", response.Code, response.Body.String())
	}
	if refreshes != 3 {
		t.Fatalf("credential cache refreshes=%d, want 3", refreshes)
	}
}

func TestInferenceTokenCannotUseControlAPI(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "sandbox", TenantID: "tenant", Kind: "inference_token", Role: "viewer", Scopes: []string{"read"}, EndpointNames: []string{"coder-production"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil)
	request.Header.Set("Authorization", "Bearer sandbox-token")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "endpoint-scoped inference credentials") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestSignedTrainingHandoffAttachesContentFreeRevisionEvidence(t *testing.T) {
	store := &fakeStore{}
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := trainingartifact.Payload{Schema: trainingartifact.Schema, Deployment: "coder", RevisionID: "rev-2", Provider: "mlflow", ExternalRunID: "run-42", Repository: "mlflow://models/coder/42", ImmutableRevision: "42", ArtifactDigest: "sha256:" + strings.Repeat("a", 64), DatasetFingerprint: "sha256:" + strings.Repeat("b", 64), ProducedAt: time.Now().UTC()}
	envelope, err := passport.Sign(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(envelope)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/training-artifacts", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"training_executed_by_infercrane":false`) || !strings.Contains(response.Body.String(), `"external_run_id":"run-42"`) || strings.Contains(response.Body.String(), `"signature"`) || strings.Contains(response.Body.String(), `"public_key"`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}

	tampered := envelope
	tampered.PayloadJSON = strings.Replace(tampered.PayloadJSON, "run-42", "run-43", 1)
	body, _ = json.Marshal(tampered)
	request = httptest.NewRequest(http.MethodPost, "/api/v1/deployments/coder/training-artifacts", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_training_handoff") {
		t.Fatalf("tampered response=%d %s", response.Code, response.Body.String())
	}
}

func TestErrorEnvelopeCarriesActionableTaxonomy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/deployments/missing", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	(API{Store: &fakeStore{err: domain.ErrNotFound}, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	var envelope struct {
		Error struct {
			Code, Category, Remediation string
			Retryable                   bool
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusNotFound || envelope.Error.Code != "not_found" || envelope.Error.Category != "not_found" || envelope.Error.Retryable || envelope.Error.Remediation == "" {
		t.Fatalf("status=%d envelope=%#v", response.Code, envelope)
	}
}
