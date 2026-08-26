package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestProtocol6Schema(t *testing.T) {
	server := providerserver.NewProtocol6(New("test"))()
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Provider == nil || response.ResourceSchemas["infercrane_deployment"] == nil || response.ResourceSchemas["infercrane_slo_policy"] == nil {
		t.Fatalf("incomplete provider schema: %#v", response)
	}
	for _, diagnostic := range response.Diagnostics {
		if diagnostic.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("schema diagnostic: %s", diagnostic.Summary)
		}
	}
}

func TestAccDeploymentCRUDImportAndInterruptedAdoption(t *testing.T) {
	fixture := newControlFixture(t)
	defer fixture.Close()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){"infercrane": providerserver.NewProtocol6WithError(New("test"))},
		Steps: []resource.TestStep{
			{Config: terraformConfig(fixture.URL, "Qwen/Qwen3-8B", 1), Check: resource.ComposeTestCheckFunc(resource.TestCheckResourceAttr("infercrane_deployment.qwen", "observed_state", "healthy"), resource.TestCheckResourceAttrSet("infercrane_deployment.qwen", "active_revision_id"))},
			{ResourceName: "infercrane_deployment.qwen", ImportState: true, ImportStateId: "qwen-prod/support-production", ImportStateVerify: true, ImportStateVerifyIgnore: []string{"operation_id", "operation_timeout_seconds"}},
			{Config: terraformConfig(fixture.URL, "Qwen/Qwen3-14B", 2), Check: resource.ComposeTestCheckFunc(resource.TestCheckResourceAttr("infercrane_deployment.qwen", "model", "Qwen/Qwen3-14B"), resource.TestCheckResourceAttr("infercrane_deployment.qwen", "max_replicas", "2"))},
		},
	})
}

func TestAccInterruptedCreateAdoptsLogicalDeployment(t *testing.T) {
	fixture := newControlFixture(t)
	fixture.interruptFirst = true
	defer fixture.Close()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){"infercrane": providerserver.NewProtocol6WithError(New("test"))},
		Steps: []resource.TestStep{
			{Config: terraformConfigWithTimeout(fixture.URL, "Qwen/Qwen3-8B", 1, 1), ExpectError: regexp.MustCompile(`durable operation continues`)},
			{Config: terraformConfigWithTimeout(fixture.URL, "Qwen/Qwen3-8B", 1, 5), Check: func(*terraform.State) error {
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				if fixture.logicalCreates != 1 {
					return fmt.Errorf("logical deployment created %d times", fixture.logicalCreates)
				}
				return nil
			}},
		},
	})
}

func TestAccSLOPolicyCRUDAndImport(t *testing.T) {
	fixture := newControlFixture(t)
	defer fixture.Close()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){"infercrane": providerserver.NewProtocol6WithError(New("test"))},
		Steps: []resource.TestStep{
			{Config: terraformSLOConfig(fixture.URL, 250), Check: resource.TestCheckResourceAttr("infercrane_slo_policy.qwen", "max_ttft_p95_ms", "250")},
			{ResourceName: "infercrane_slo_policy.qwen", ImportState: true, ImportStateId: "qwen-prod", ImportStateVerify: true},
			{Config: terraformSLOConfig(fixture.URL, 175), Check: resource.TestCheckResourceAttr("infercrane_slo_policy.qwen", "max_ttft_p95_ms", "175")},
		},
	})
}

func terraformConfig(endpoint, model string, max int) string {
	return terraformConfigWithTimeout(endpoint, model, max, 5)
}

func terraformConfigWithTimeout(endpoint, model string, max, timeout int) string {
	return fmt.Sprintf(`
provider "infercrane" {
  endpoint = %q
  api_key = "test-only"
}
resource "infercrane_deployment" "qwen" {
  name = "qwen-prod"
  endpoint_name = "support-production"
  model = %q
  cloud = "fixture"
  compute_mode = "elastic"
  gpu = "L40S"
  min_replicas = 1
  max_replicas = %d
  operation_timeout_seconds = %d
}
`, endpoint, model, max, timeout)
}

func terraformSLOConfig(endpoint string, maxTTFT int) string {
	return fmt.Sprintf(`
provider "infercrane" {
  endpoint = %q
  api_key = "test-only"
}
resource "infercrane_slo_policy" "qwen" {
  deployment = "qwen-prod"
  max_ttft_p95_ms = %d
}
`, endpoint, maxTTFT)
}

type controlFixture struct {
	*httptest.Server
	mu                sync.Mutex
	exists            bool
	model             string
	max               int
	active, candidate string
	candidateModel    string
	candidateMax      int
	next              int
	interruptFirst    bool
	operationGets     int
	logicalCreates    int
	maxTTFT           *float64
}

func newControlFixture(t *testing.T) *controlFixture {
	fixture := &controlFixture{model: "Qwen/Qwen3-8B", max: 1, next: 1}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fixture.serve(t, w, r) }))
	return fixture
}
func (f *controlFixture) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer test-only" {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "missing"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == "PUT" && path == "/deployments/qwen-prod/slo-policy":
		var body struct {
			MaxTTFT float64 `json:"max_ttft_p95_ms"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid SLO policy body")
		}
		f.maxTTFT = &body.MaxTTFT
		_ = json.NewEncoder(w).Encode(map[string]any{"policy": map[string]any{"deployment_id": "dep-1", "max_ttft_p95_ms": body.MaxTTFT}})
	case r.Method == "GET" && path == "/deployments/qwen-prod/slo-policy":
		if f.maxTTFT == nil {
			f.notFound(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"policy": map[string]any{"deployment_id": "dep-1", "max_ttft_p95_ms": *f.maxTTFT}})
	case r.Method == "DELETE" && path == "/deployments/qwen-prod/slo-policy":
		f.maxTTFT = nil
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "POST" && path == "/deployments":
		var body struct {
			Model string `json:"model"`
			Max   int    `json:"max_replicas"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid create body")
		}
		if !f.exists {
			f.logicalCreates++
		}
		f.exists, f.model, f.max, f.active = true, body.Model, body.Max, "rev-1"
		f.operation(w, "create")
	case r.Method == "GET" && path == "/deployments/qwen-prod":
		if !f.exists {
			f.notFound(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deployment": map[string]any{"id": "dep-1", "name": "qwen-prod", "endpoint_names": []string{"support-production"}, "model": f.model, "runtime": "vllm", "observed_state": "healthy", "min_replicas": 1, "max_replicas": f.max, "active_revision_id": f.active, "candidate_revision_id": f.candidate}, "lifecycle_status": map[string]any{"serving_state": "serving"}, "revisions": []any{map[string]any{"id": f.active, "spec": map[string]any{"cloud": "fixture", "compute_mode": "elastic", "gpu": "L40S"}}}})
	case r.Method == "POST" && path == "/deployments/qwen-prod/rollouts":
		var body struct {
			Spec struct {
				Model string `json:"model"`
				Max   int    `json:"max_replicas"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.candidate, f.candidateModel, f.candidateMax = "rev-2", body.Spec.Model, body.Spec.Max
		f.operation(w, "candidate")
	case r.Method == "POST" && strings.HasSuffix(path, "/promote"):
		f.active, f.candidate, f.model, f.max = "rev-2", "", f.candidateModel, f.candidateMax
		f.operation(w, "promote")
	case r.Method == "POST" && (strings.HasSuffix(path, "/provision") || strings.HasSuffix(path, "/guard/evaluate")):
		f.operation(w, "transition")
	case r.Method == "GET" && strings.HasPrefix(path, "/operations/"):
		f.operationGets++
		status, progress := "succeeded", 100
		if f.interruptFirst && f.operationGets == 1 {
			status, progress = "waiting", 55
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.TrimPrefix(path, "/operations/"), "kind": "fixture", "status": status, "progress": progress})
	case r.Method == "DELETE" && path == "/deployments/qwen-prod":
		f.exists = false
		f.operation(w, "delete")
	default:
		t.Errorf("unexpected API request %s %s", r.Method, path)
		f.notFound(w)
	}
}
func (f *controlFixture) operation(w http.ResponseWriter, kind string) {
	f.next++
	_ = json.NewEncoder(w).Encode(map[string]any{"operation": map[string]any{"id": fmt.Sprintf("op-%d", f.next), "kind": kind, "status": "succeeded", "progress": 100}})
}
func (f *controlFixture) notFound(w http.ResponseWriter) {
	w.WriteHeader(404)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "not_found", "message": "missing"}})
}
