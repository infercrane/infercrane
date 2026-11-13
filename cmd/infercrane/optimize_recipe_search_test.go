package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/spf13/cobra"
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
	_, err := optimizationProposal(context.Background(), request, recipeSearchCatalog(t), "aiconfigurator", "python3", false)
	if err == nil {
		t.Fatal("recipe search was combined with the estimator source")
	}
	if !strings.Contains(err.Error(), "--search recipes") || internalMethodWording.MatchString(err.Error()) {
		t.Fatalf("error must name the flags in customer wording: %v", err)
	}
}

var internalMethodWording = regexp.MustCompile(`(?i)recipe search|search control|\barms?\b`)

// Help and usage text is customer-facing. Flag names stay stable for scripts;
// descriptions say what the customer gets, not the internal method name.
func TestCLIHelpOmitsInternalMethodNames(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(command *cobra.Command) {
		text := command.Short + "\n" + command.Long + "\n" + command.UsageString()
		if match := internalMethodWording.FindString(text); match != "" {
			t.Errorf("%s help shows %q", command.CommandPath(), match)
		}
		for _, child := range command.Commands() {
			walk(child)
		}
	}
	walk(newRootCommand(context.Background()))

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	runErr := optimizeCommand(context.Background(), []string{"propose", "acme/model", "-h"})
	os.Stderr = original
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	usage, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runErr, flag.ErrHelp) || !strings.Contains(string(usage), "-model-config") {
		t.Fatalf("optimize usage err=%v output=%q", runErr, usage)
	}
	if match := internalMethodWording.FindString(string(usage)); match != "" {
		t.Fatalf("optimize usage shows %q:\n%s", match, usage)
	}
}
