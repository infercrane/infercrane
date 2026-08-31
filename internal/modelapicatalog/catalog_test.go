package modelapicatalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultCatalogIsCuratedValidatedAndPaged(t *testing.T) {
	catalog := Default()
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) < 12 || len(catalog.Models) > 24 {
		t.Fatalf("expected a deliberate 12-24 model discovery shelf, got %d", len(catalog.Models))
	}
	page := catalog.List(Filter{Task: "coding", Limit: 2})
	if len(page.Models) != 2 || page.Total < 3 || page.NextOffset == nil {
		t.Fatalf("unexpected coding page: %+v", page)
	}
	for _, model := range page.Models {
		if model.Access == "ready" || model.Qualification == "measured" || len(model.Offers) != 0 {
			t.Fatalf("built-in discovery identity fabricated service evidence: %+v", model)
		}
	}
}

func TestPricingCurrentAtFailsClosed(t *testing.T) {
	pricing := &Pricing{ValidUntil: "2030-01-01T00:00:00Z"}
	if !PricingCurrentAt(pricing, time.Date(2029, 12, 31, 23, 59, 59, 0, time.UTC)) {
		t.Fatal("expected current rate card")
	}
	if PricingCurrentAt(pricing, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) || PricingCurrentAt(&Pricing{ValidUntil: "unknown"}, time.Now()) || PricingCurrentAt(nil, time.Now()) {
		t.Fatal("expired, invalid, and absent rate cards must fail closed")
	}
}

func TestPreferredOfferAtIsOrderIndependentAndFailClosed(t *testing.T) {
	at := time.Date(2029, 12, 31, 23, 0, 0, 0, time.UTC)
	current := &Pricing{ValidUntil: "2030-01-01T00:00:00Z"}
	expired := &Pricing{ValidUntil: "2029-01-01T00:00:00Z"}
	offers := []Offer{
		{ID: "z-expired", Access: "ready", Availability: "available", Pricing: expired},
		{ID: "b-ready", Access: "ready", Availability: "available", Pricing: current},
		{ID: "a-request", Access: "request-access", Availability: "unknown"},
	}
	selected, ok := PreferredOfferAt(offers, at)
	if !ok || selected.ID != "b-ready" {
		t.Fatalf("expected executable offer, got %+v ok=%v", selected, ok)
	}
	selected, ok = PreferredOfferAt([]Offer{offers[2], offers[0]}, at)
	if !ok || selected.ID != "a-request" {
		t.Fatalf("expected deterministic discovery fallback, got %+v ok=%v", selected, ok)
	}
	if _, ok = PreferredOfferAt(nil, at); ok {
		t.Fatal("empty offers must not produce a service")
	}
}

func TestCatalogSearchAndCopyIsolation(t *testing.T) {
	catalog := Default()
	page := catalog.List(Filter{Query: "BGE", Capability: "embeddings", Limit: 100})
	if page.Total < 1 {
		t.Fatalf("expected embedding search results, got %+v", page)
	}
	page.Models[0].Tasks[0] = "mutated"
	again := catalog.List(Filter{Query: "BGE", Capability: "embeddings", Limit: 100})
	if again.Models[0].Tasks[0] == "mutated" {
		t.Fatal("catalog list leaked mutable backing storage")
	}
}

func TestLoadRejectsUnknownFieldsAndIncompletePricing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":"model-api-catalog/v1","models":[],"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field rejection")
	}
	invalid := `{"schema_version":"model-api-catalog/v1","models":[{"id":"one","display_name":"One","publisher":"Test","publisher_slug":"test","family":"One","description":"Test model","tasks":["chat"],"capabilities":["streaming"],"input_modalities":["text"],"output_modalities":["text"],"access":"ready","qualification":"supplier-reported","qualification_note":"Supplier contract only.","offers":[{"id":"one-managed","supplier":"internal","supplier_slug":"internal","supplier_model_id":"secret","adapter":"openai-compatible-external","protocol":"openai","access":"ready","availability":"available","pricing":{"currency":"USD","input_microusd_per_million":1}}]}]}`
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected incomplete pricing provenance rejection")
	}
}
