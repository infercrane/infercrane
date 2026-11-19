package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/secrets"
)

type importStore struct {
	*fakeStore
	mu      sync.Mutex
	imports map[string]domain.ModelImport
}

func (s *importStore) CreateModelImport(_ context.Context, item domain.ModelImport) (domain.ModelImport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item.ID = "import-1"
	s.imports[item.TenantID+"|"+item.URI+"|"+item.ManifestDigest] = item
	return item, nil
}

func (s *importStore) ModelImportForTenant(_ context.Context, tenant, uri, digest string) (domain.ModelImport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.imports[tenant+"|"+uri+"|"+digest]
	if !ok {
		return domain.ModelImport{}, domain.ErrNotFound
	}
	return item, nil
}

func (s *importStore) ModelImportsForTenant(_ context.Context, tenant string, _ int) ([]domain.ModelImport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []domain.ModelImport{}
	for _, item := range s.imports {
		if item.TenantID == tenant {
			out = append(out, item)
		}
	}
	return out, nil
}

func manifestFile(path string, content []byte) map[string]any {
	sum := sha256.Sum256(content)
	return map[string]any{"path": path, "size": len(content), "sha256": hex.EncodeToString(sum[:])}
}

func importRequest(t *testing.T, api API, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/model-imports", strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	return response
}

func TestModelImportRecordsManifestAndRefusesFilesThatCannotServe(t *testing.T) {
	store := &importStore{fakeStore: &fakeStore{}, imports: map[string]domain.ModelImport{}}
	api := API{Store: store, APIKey: "secret"}
	config := []byte(`{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`)
	files := []any{manifestFile("config.json", config), manifestFile("tokenizer.json", []byte("{}")), manifestFile("model.safetensors", []byte("w"))}
	response := importRequest(t, api, map[string]any{"kind": "weights", "uri": "s3://acme-models/run-42", "files": files, "config_base64": base64.StdEncoding.EncodeToString(config)})
	if response.Code != http.StatusCreated {
		t.Fatalf("import refused: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		Import domain.ModelImport `json:"import"`
		Check  *modelcheck.Report `json:"check"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.Import.ManifestDigest, "sha256:") || created.Check == nil || created.Check.Architecture.State != "supported" {
		t.Fatalf("created=%s err=%v", response.Body.String(), err)
	}
	if _, err := store.ModelImportForTenant(context.Background(), "global", "s3://acme-models/run-42", created.Import.ManifestDigest); err != nil {
		t.Fatal("the import was not stored for the caller's workspace")
	}

	failed := importRequest(t, api, map[string]any{"kind": "weights", "uri": "s3://acme-models/run-43", "files": []any{manifestFile("model.safetensors", []byte("w"))}})
	if failed.Code != http.StatusUnprocessableEntity || !strings.Contains(failed.Body.String(), "model_import_failed") || !strings.Contains(failed.Body.String(), "No config.json") {
		t.Fatalf("files that cannot serve were accepted: %d %s", failed.Code, failed.Body.String())
	}
	if len(store.imports) != 1 {
		t.Fatal("a failed import was stored")
	}
	if response := importRequest(t, api, map[string]any{"kind": "weights", "uri": "https://example.com/model", "files": files}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a non-bucket location was accepted: %d", response.Code)
	}
	if response := importRequest(t, API{Store: &fakeStore{}, APIKey: "secret"}, map[string]any{"kind": "weights", "uri": "s3://a/b", "files": files}); response.Code != http.StatusNotImplemented {
		t.Fatalf("a store without imports must say so: %d", response.Code)
	}
}

func TestDeploymentRefusesObjectStoreModelsThatWereNotImported(t *testing.T) {
	store := &importStore{fakeStore: &fakeStore{}, imports: map[string]domain.ModelImport{}}
	body := `{"name":"trained","model":"s3://acme-models/run-42","model_revision":"sha256:` + strings.Repeat("3", 64) + `","cloud":"kubernetes","gpu":"L4"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "deploy-1")
	response := httptest.NewRecorder()
	(API{Store: store, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "model_import_not_found") {
		t.Fatalf("deployment of an unknown import: %d %s", response.Code, response.Body.String())
	}
}

// The adapter check pins the commit and reads adapter_config.json with the
// workspace's sealed token, sent only to the Hub.
func TestAdapterCheckPinsCommitAndReadsRankWithSealedToken(t *testing.T) {
	const commit = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	var authorizations []string
	var mu sync.Mutex
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer hf_sealed_token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/models/acme/support-lora/revision/main":
			_, _ = w.Write([]byte(`{"id":"acme/support-lora","sha":"` + commit + `","private":true,"siblings":[{"rfilename":"adapter_config.json"},{"rfilename":"adapter_model.safetensors"}]}`))
		case "/acme/support-lora/raw/" + commit + "/adapter_config.json":
			_, _ = w.Write([]byte(`{"peft_type":"LORA","r":16,"base_model_name_or_path":"Qwen/Qwen3-8B"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	resolver, err := modelcheck.New(modelcheck.ResolverOptions{Endpoint: hub.URL})
	if err != nil {
		t.Fatal(err)
	}
	sealer := sealedSecretsForTest(t)
	sealed, keyReference, err := sealer.Seal("global", "hf", "hf_sealed_token")
	if err != nil {
		t.Fatal(err)
	}
	store := &sealedListStore{fakeStore: &fakeStore{}, reference: domain.SecretReference{ID: "sealed-1", TenantID: "global", Name: "hf", Resolver: "sealed", Sealed: sealed, KeyReference: keyReference}}
	api := API{Store: store, APIKey: "secret", ModelCheck: resolver, Secrets: secrets.Resolvers{Sealed: sealer}, SealedSecrets: sealer}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/catalog/hugging-face/adapter-check?repository=acme/support-lora&model_secret_reference_id=sealed-1&base_model=Qwen/Qwen3-8B", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "hf_sealed_token") {
		t.Fatalf("adapter check: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Revision string                  `json:"revision"`
		Private  bool                    `json:"private"`
		Checks   domain.ModelImportCheck `json:"checks"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Revision != commit || !result.Private || result.Checks.AdapterRank != 16 || len(result.Checks.Failed) != 0 || !strings.Contains(strings.Join(result.Checks.Verified, " "), "Trained on Qwen/Qwen3-8B") {
		t.Fatalf("result=%s err=%v", response.Body.String(), err)
	}
	for _, header := range authorizations {
		if header != "Bearer hf_sealed_token" {
			t.Fatalf("a Hub request went without the token or with another one: %q", header)
		}
	}
}

type sealedListStore struct {
	*fakeStore
	reference domain.SecretReference
}

func (s *sealedListStore) SecretReferencesForTenant(context.Context, string) ([]domain.SecretReference, error) {
	return []domain.SecretReference{s.reference}, nil
}
