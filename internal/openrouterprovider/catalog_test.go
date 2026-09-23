package openrouterprovider

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validCatalog() Catalog {
	return Catalog{Data: []Model{{
		SchemaVersion: "2.4", ID: "qwen/qwen3.8-27b", Name: "Qwen 3.8 27B",
		HuggingFaceID: "Qwen/Qwen3.8-27B-FP8", Created: 1, Quantization: "fp8",
		InputModalities:  []InputModality{{Type: "text", Pricing: []Price{{Type: "prompt", Unit: "token", CostUSD: "0.00000011"}}}},
		OutputModalities: []OutputModality{{Type: "text", Streaming: true, SupportedParameters: map[string]any{"tools": map[string]any{"type": "boolean"}}, Pricing: []Price{{Type: "completion", Unit: "token", CostUSD: "0.00000250"}}}},
		Capacity:         []Capacity{{Type: "request", Unit: "request", Per: "minute", Value: 24}},
		OpenRouter:       OpenRouterIdentity{Slug: "qwen/qwen3.8-27b"},
	}}}
}

func TestCatalogValidationAndHandler(t *testing.T) {
	catalog := validCatalog()
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	catalog.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/openrouter/v1/models", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"schema_version":"2.4"`) {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestCatalogRejectsFloatOrNegativePrices(t *testing.T) {
	catalog := validCatalog()
	catalog.Data[0].OutputModalities[0].Pricing[0].CostUSD = "not-a-decimal"
	if err := catalog.Validate(); err == nil {
		t.Fatal("expected invalid price to fail")
	}
	catalog = validCatalog()
	catalog.Data[0].OutputModalities[0].Pricing[0].CostUSD = "-0.1"
	if err := catalog.Validate(); err == nil {
		t.Fatal("expected negative price to fail")
	}
}

func TestCatalogValidatesUserDiscount(t *testing.T) {
	catalog := validCatalog()
	catalog.Data[0].DiscountToUser = 0.19
	if err := catalog.Validate(); err != nil {
		t.Fatalf("expected launch discount to pass: %v", err)
	}

	catalog.Data[0].DiscountToUser = -0.1
	if err := catalog.Validate(); err != nil {
		t.Fatalf("expected documented markup value to pass: %v", err)
	}

	catalog.Data[0].DiscountToUser = 1
	if err := catalog.Validate(); err == nil {
		t.Fatal("expected a discount of one to fail")
	}
}

func TestLoadIsStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"data":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field to fail")
	}
}

func TestStagedQwenCatalogMatchesLaunchBoundary(t *testing.T) {
	catalog, err := Load(filepath.Join("..", "..", "deploy", "openrouter", "qwen38-provider-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	model := catalog.Data[0]
	if model.IsReady || model.ID != "qwen/qwen3.8-27b" || len(model.Capacity) != 1 || model.Capacity[0].Value != 30 {
		t.Fatalf("unexpected staged model: %+v", model)
	}
	if model.InputModalities[0].SupportedInputs["max_context_length"].(map[string]any)["value"] != float64(32768) {
		t.Fatalf("unexpected context boundary: %+v", model.InputModalities[0].SupportedInputs)
	}
	if model.InputModalities[0].Pricing[0].CostUSD != "0.00000010" || model.OutputModalities[0].Pricing[0].CostUSD != "0.00000220" {
		t.Fatalf("unexpected staged pricing: input=%s output=%s", model.InputModalities[0].Pricing[0].CostUSD, model.OutputModalities[0].Pricing[0].CostUSD)
	}
	if model.DiscountToUser != 0.19 {
		t.Fatalf("unexpected launch discount: %v", model.DiscountToUser)
	}
}

func TestReadyCatalogRequiresLocation(t *testing.T) {
	catalog := validCatalog()
	catalog.Data[0].IsReady = true
	if err := catalog.Validate(); err == nil {
		t.Fatal("expected ready model without deployment location to fail")
	}
	catalog.Data[0].Datacenters = []Datacenter{{CountryCode: "US", Region: "us-west"}}
	catalog.Data[0].DeploymentRegion = "us-west"
	if err := catalog.Validate(); err != nil {
		t.Fatalf("expected located ready model to pass: %v", err)
	}
}
