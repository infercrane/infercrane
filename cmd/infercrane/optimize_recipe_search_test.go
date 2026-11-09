package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func recipeSearchCatalog(t *testing.T) optimizer.CatalogSource {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	return optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot())
}

func TestApplyRecipeSearchReadsConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"text_config":{"mtp_num_hidden_layers":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := optimizer.Request{ModelIdentity: "acme/model", ModelRevision: strings.Repeat("a", 40)}
	if err := applyRecipeSearch(context.Background(), &request, recipeSearchCatalog(t), "recipes", path, false); err != nil {
		t.Fatal(err)
	}
	if request.Search != optimizer.SearchRecipes || len(request.ModelFeatures) != 1 || !strings.HasPrefix(request.ModelConfigDigest, "sha256:") {
		t.Fatalf("request=%+v", request)
	}
}

func TestApplyRecipeSearchUsesNetworkOnlyWhenAllowed(t *testing.T) {
	revision := strings.Repeat("b", 40)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/acme/model/raw/"+revision+"/config.json" {
			t.Errorf("unexpected metadata path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"num_nextn_predict_layers":1}`))
	}))
	defer server.Close()
	previous := modelConfigBaseURL
	modelConfigBaseURL = server.URL
	defer func() { modelConfigBaseURL = previous }()

	request := optimizer.Request{ModelIdentity: "acme/model", ModelRevision: revision}
	if err := applyRecipeSearch(context.Background(), &request, recipeSearchCatalog(t), "recipes", "", false); err != nil {
		t.Fatal(err)
	}
	if requests != 0 || len(request.ModelFeatures) != 0 {
		t.Fatalf("network used without opt-in: requests=%d request=%+v", requests, request)
	}
	if err := applyRecipeSearch(context.Background(), &request, recipeSearchCatalog(t), "recipes", "", true); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(request.ModelFeatures) != 1 {
		t.Fatalf("metadata was not applied: requests=%d request=%+v", requests, request)
	}
}

func TestFetchPublicModelConfigRejectsUnsafeIdentity(t *testing.T) {
	for _, test := range []struct{ model, revision string }{
		{"../secret", strings.Repeat("a", 40)},
		{"acme/model/extra", strings.Repeat("a", 40)},
		{"acme/model", "main"},
	} {
		if _, err := fetchPublicModelConfig(context.Background(), test.model, test.revision); err == nil {
			t.Fatalf("unsafe identity accepted: %+v", test)
		}
	}
}

func TestRecipeSearchRequiresSearchForConfigAndRejectsEstimator(t *testing.T) {
	request := optimizer.Request{ModelIdentity: "acme/model"}
	if err := applyRecipeSearch(context.Background(), &request, recipeSearchCatalog(t), "", "config.json", false); err == nil {
		t.Fatal("--model-config without search was accepted")
	}
	request.Search = optimizer.SearchRecipes
	if _, err := optimizationProposal(context.Background(), request, recipeSearchCatalog(t), "aiconfigurator", "python3", false); err == nil {
		t.Fatal("recipe search was combined with the estimator source")
	}
}
