package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func tenantOperator(tenantID string) *fakeStore {
	return &fakeStore{created: true, principal: domain.Principal{
		ID: "operator-" + tenantID, TenantID: tenantID, Name: "operator", Role: "operator",
		Scopes: []string{"read", "deploy"},
	}}
}

func listComputeProviders(t *testing.T, handler http.Handler, token string) map[string]ComputeProvider {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/compute/providers", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("compute providers status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Data []ComputeProvider `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	providers := make(map[string]ComputeProvider, len(body.Data))
	for _, provider := range body.Data {
		providers[provider.ID] = provider
	}
	return providers
}

var operatorBYOCInventory = []ComputeProvider{
	{ID: "kubernetes", Label: "Kubernetes", Mode: "BYOC", State: "ready", BillingModes: []string{"provider_account"}},
	{ID: "gcp", Label: "Google Cloud", Mode: "BYOC", State: "connection-required", Reason: "GCP project and workload identity are not configured"},
	{ID: "runpod", Label: "RunPod", Mode: "control-plane", State: "ready"},
}

func TestOperatorComputeGrantMarksOnlyGrantedWorkspaceAsOwner(t *testing.T) {
	for _, test := range []struct {
		name   string
		grants []string
		tenant string
		owned  bool
	}{
		{name: "no grant keeps hosted control plane closed", tenant: "tenant-1"},
		{name: "named workspace", grants: []string{"tenant-1"}, tenant: "tenant-1", owned: true},
		{name: "other workspace", grants: []string{"tenant-1"}, tenant: "tenant-2"},
		{name: "single-customer wildcard", grants: []string{"*"}, tenant: "tenant-2", owned: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := tenantOperator(test.tenant)
			handler := (API{Store: store, Authenticator: store, ComputeProviders: operatorBYOCInventory, OperatorComputeTenants: test.grants}).Handler()
			providers := listComputeProviders(t, handler, "tenant-session")
			if providers["kubernetes"].WorkspaceOwned != test.owned {
				t.Fatalf("kubernetes workspace_owned=%v, want %v", providers["kubernetes"].WorkspaceOwned, test.owned)
			}
			if providers["gcp"].WorkspaceOwned {
				t.Fatal("a provider that is not ready was reported as workspace-owned")
			}
			if providers["runpod"].WorkspaceOwned {
				t.Fatal("the operator's control-plane supply account was reported as workspace-owned")
			}
		})
	}
}

func TestOperatorComputeGrantNeverCoversManagedCapacity(t *testing.T) {
	store := tenantOperator("tenant-1")
	handler := (API{
		Store: store, Authenticator: store, OperatorComputeTenants: []string{"*"},
		ComputeProviders:   []ComputeProvider{{ID: "runpod", Label: "RunPod", Mode: "BYOC", State: "ready"}},
		ManagedDeployments: managedbilling.DeploymentPolicy{Enabled: true, Provider: "runpod"},
	}).Handler()
	if listComputeProviders(t, handler, "tenant-session")["runpod"].WorkspaceOwned {
		t.Fatal("InferCrane-managed prepaid capacity was reported as workspace-owned")
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","min_replicas":1,"max_replicas":1}`))
	request.Header.Set("Authorization", "Bearer tenant-session")
	request.Header.Set("Idempotency-Key", "managed-through-grant")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"compute_connection_required"`) {
		t.Fatalf("operator grant bypassed managed billing: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBootstrapOperatorOwnsItsReadyBYOCInventory(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ComputeProviders: operatorBYOCInventory}).Handler()
	providers := listComputeProviders(t, handler, "secret")
	if !providers["kubernetes"].WorkspaceOwned || providers["gcp"].WorkspaceOwned || providers["runpod"].WorkspaceOwned {
		t.Fatalf("bootstrap ownership = %+v", providers)
	}
}

func TestGrantedWorkspaceDeploysOnOperatorBYOCWithoutConnection(t *testing.T) {
	for _, test := range []struct {
		name   string
		grants []string
		status int
	}{
		{name: "granted", grants: []string{"tenant-1"}, status: http.StatusAccepted},
		{name: "not granted", grants: []string{"tenant-2"}, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := tenantOperator("tenant-1")
			handler := (API{
				Store: store, Authenticator: store, OperatorComputeTenants: test.grants,
				ComputeProviders: []ComputeProvider{{ID: "aws", Label: "AWS", Mode: "BYOC", State: "ready", BillingModes: []string{"provider_account"}}},
			}).Handler()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(`{"name":"qwen","model":"Qwen/Qwen3-8B","cloud":"aws","region":"eu-central-1","gpu":"L40S","min_replicas":1,"max_replicas":1}`))
			request.Header.Set("Authorization", "Bearer tenant-session")
			request.Header.Set("Idempotency-Key", "operator-grant-"+test.name)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("deployment status=%d want %d body=%s", response.Code, test.status, response.Body.String())
			}
			if test.status != http.StatusAccepted {
				if !strings.Contains(response.Body.String(), `"code":"compute_connection_required"`) {
					t.Fatalf("ungranted workspace error=%s", response.Body.String())
				}
				return
			}
			if store.deployment.TenantID != "tenant-1" || store.deployment.ComputeConnectionID != "" || !strings.Contains(store.operation.RequestJSON, `"billing_mode":"provider_account"`) {
				t.Fatalf("granted deployment was not bound to the caller with provider-account billing: deployment=%#v operation=%s", store.deployment, store.operation.RequestJSON)
			}
		})
	}
}

func TestOptimizationApprovalAcceptsOperatorGrantedBYOCCompute(t *testing.T) {
	for _, test := range []struct {
		name   string
		grants []string
		status int
	}{
		{name: "granted", grants: []string{"*"}, status: http.StatusAccepted},
		{name: "not granted", status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := tenantOperator("tenant-1")
			var draft optimizer.DeploymentDraft
			draft.Provider.Cloud = "kubernetes"
			draftJSON, err := json.Marshal(draft)
			if err != nil {
				t.Fatal(err)
			}
			store := &fakeOptimizationCampaignStore{
				fakeStore: base,
				campaign: domain.OptimizationCampaign{
					ID: "campaign-1", TenantID: "tenant-1", State: optimizationcampaign.CampaignAwaitingApproval,
					Candidates: []domain.OptimizationCandidateRun{{ID: "candidate-1", DeploymentSpecJSON: string(draftJSON)}},
				},
			}
			handler := (API{
				Store: store, Authenticator: base, OptimizationCosts: fakeOptimizationCosts{},
				ComputeProviders: operatorBYOCInventory, OperatorComputeTenants: test.grants,
			}).Handler()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns/campaign-1/approve", strings.NewReader(`{"max_cost_usd":20,"expires_in_seconds":3600}`))
			request.Header.Set("Authorization", "Bearer tenant-session")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("approval status=%d want %d body=%s", response.Code, test.status, response.Body.String())
			}
			if test.status == http.StatusConflict && store.campaign.State != optimizationcampaign.CampaignAwaitingApproval {
				t.Fatalf("rejected optimization changed campaign state: %s", store.campaign.State)
			}
		})
	}
}
