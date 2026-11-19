package controlapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/modelimport"
)

// modelImportStore keeps imported object store manifests.
type modelImportStore interface {
	CreateModelImport(context.Context, domain.ModelImport) (domain.ModelImport, error)
	ModelImportForTenant(context.Context, string, string, string) (domain.ModelImport, error)
	ModelImportsForTenant(context.Context, string, int) ([]domain.ModelImport, error)
}

// createModelImport registers weights or a LoRA adapter in the workspace's own
// bucket by its file manifest. It never reads the bucket: the checks cover the
// manifest and any configuration bytes sent with it, and the worker checks the
// bytes it downloads. A manifest whose files cannot serve is refused with its
// checks so the caller can show what failed.
func (a API) createModelImport(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(modelImportStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "model imports are not configured on this control plane")
		return
	}
	var body struct {
		Kind                string             `json:"kind"`
		URI                 string             `json:"uri"`
		Files               []domain.ModelFile `json:"files"`
		ConfigBase64        string             `json:"config_base64"`
		AdapterConfigBase64 string             `json:"adapter_config_base64"`
		BaseModel           string             `json:"base_model"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	request := modelimport.Request{Kind: strings.TrimSpace(body.Kind), URI: body.URI, Files: body.Files, BaseModel: strings.TrimSpace(body.BaseModel)}
	for _, field := range []struct {
		raw    string
		target *[]byte
		name   string
	}{{body.ConfigBase64, &request.Config, "config_base64"}, {body.AdapterConfigBase64, &request.AdapterConfig, "adapter_config_base64"}} {
		if field.raw == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(field.raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", field.name+" must be standard base64")
			return
		}
		*field.target = decoded
	}
	if request.Kind == modelimport.KindWeights && len(request.AdapterConfig) > 0 || request.Kind == modelimport.KindAdapter && len(request.Config) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "send config_base64 with weights and adapter_config_base64 with an adapter")
		return
	}
	item, report, err := modelimport.Build(request)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item.TenantID = actor.TenantID
	if len(item.Checks.Failed) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  map[string]any{"code": "model_import_failed", "category": "validation", "message": "These files cannot be served: " + strings.Join(item.Checks.Failed, "; "), "retryable": false},
			"import": item, "check": report, "performance_claims": false,
		})
		return
	}
	saved, err := store.CreateModelImport(r.Context(), item)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "the model import could not be saved")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "model.import", ResourceType: "model_import", ResourceName: saved.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"import": saved, "check": report, "performance_claims": false})
}

func (a API) modelImports(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(modelImportStore)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.ModelImportsForTenant(r.Context(), actor.TenantID, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "model imports could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

// checkHuggingFaceAdapter pins a LoRA adapter repository to a commit and reads
// its adapter_config.json: rank, declared base model, and whether adapter
// weights are listed. It is a read and never deploys or stores anything.
func (a API) checkHuggingFaceAdapter(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	allowed := map[string]bool{"repository": true, "revision": true, "model_secret_reference_id": true, "base_model": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "supported parameters are repository, revision, model_secret_reference_id, and base_model, once each")
			return
		}
	}
	repository, revision := strings.TrimSpace(query.Get("repository")), strings.TrimSpace(query.Get("revision"))
	if !modelcheck.ValidRepository(repository) || revision != "" && !modelcheck.ValidRevision(revision) {
		writeError(w, http.StatusBadRequest, "invalid_query", "repository must be an exact owner/name Hugging Face repository, and revision a branch, tag, or commit")
		return
	}
	if a.ModelCheck == nil {
		writeError(w, http.StatusNotImplemented, codeMetadataAccessDisabled, "Hugging Face metadata access is turned off on this control plane, so adapters cannot be checked.")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	read := modelcheck.Request{Repository: repository, Revision: revision}
	if referenceID := strings.TrimSpace(query.Get("model_secret_reference_id")); referenceID != "" {
		token, failure := a.huggingFaceToken(r.Context(), actor.TenantID, referenceID)
		if failure != nil {
			writeError(w, failure.Status, failure.Code, failure.Message)
			return
		}
		read.Token, read.Scope = token, actor.TenantID+"/"+referenceID
	}
	adapter, err := a.ModelCheck.ResolveAdapter(r.Context(), read)
	if err != nil {
		var checkErr *modelcheck.Error
		status, code, message := http.StatusBadGateway, modelcheck.CodeUnavailable, "Hugging Face could not be read"
		if errors.As(err, &checkErr) {
			code, message = checkErr.Code, checkErr.Message
			if mapped := map[string]int{modelcheck.CodeInvalidModel: http.StatusBadRequest, modelcheck.CodeModelNotFound: http.StatusNotFound, modelcheck.CodeRevisionNotFound: http.StatusNotFound, modelcheck.CodeTokenRejected: http.StatusUnprocessableEntity}[checkErr.Code]; mapped != 0 {
				status = mapped
			}
		}
		writeError(w, status, code, message)
		return
	}
	check := domain.ModelImportCheck{}
	result := map[string]any{"repository": adapter.Model.Repository, "requested_revision": adapter.Model.RequestedRevision, "revision": adapter.Model.Commit, "private": adapter.Model.Private, "gated": adapter.Model.Gated != "", "token_used": adapter.Model.TokenUsed, "performance_claims": false}
	switch adapter.ConfigState {
	case modelcheck.ConfigRead:
		config, parseErr := modelimport.ParseAdapterConfig(adapter.Config)
		if parseErr != nil {
			check.Failed = append(check.Failed, parseErr.Error())
			break
		}
		check = modelimport.BaseModelCheck(config.BaseModel, strings.TrimSpace(query.Get("base_model")))
		check.AdapterRank, check.AdapterBase = config.Rank, config.BaseModel
		check.Verified = append([]string{"Pinned to commit " + adapter.Model.Commit, "adapter_config.json read: LoRA rank " + strconv.Itoa(config.Rank)}, check.Verified...)
	case modelcheck.ConfigTokenRequired:
		check.Unknown = append(check.Unknown, "adapter_config.json needs a Hugging Face token")
	default:
		check.Failed = append(check.Failed, "No adapter_config.json at the pinned commit")
	}
	if adapter.Weights {
		check.Verified = append(check.Verified, "Adapter weights are safetensors files")
	} else {
		check.Failed = append(check.Failed, "No adapter_model.safetensors at the pinned commit")
	}
	check.Unknown = append(check.Unknown, "Whether the runtime loads this adapter on its base model: known once a replica starts")
	result["checks"] = check
	writeJSON(w, http.StatusOK, result)
}

// validateModelImports checks that object store weights and adapters were
// imported into this workspace before a deployment or proposal names them.
func (a API) validateModelImports(ctx context.Context, tenant, model, revision string, adapters []domain.ModelAdapter) *modelCheckError {
	type pinned struct{ uri, digest, kind string }
	var wanted []pinned
	if modelimport.IsObjectModel(model) {
		wanted = append(wanted, pinned{model, revision, modelimport.KindWeights})
	}
	for _, adapter := range adapters {
		if adapter.Source() != "huggingface" {
			wanted = append(wanted, pinned{adapter.Repository, adapter.Revision, modelimport.KindAdapter})
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	store, ok := a.Store.(modelImportStore)
	if !ok {
		return &modelCheckError{Status: http.StatusNotImplemented, Code: "capability_unavailable", Message: "object store imports are not configured on this control plane"}
	}
	for _, item := range wanted {
		found, err := store.ModelImportForTenant(ctx, tenant, item.uri, item.digest)
		if errors.Is(err, domain.ErrNotFound) {
			return &modelCheckError{Status: http.StatusUnprocessableEntity, Code: "model_import_not_found", Message: item.uri + " with manifest " + item.digest + " was not imported into this workspace; import it first", Client: true}
		}
		if err != nil {
			return &modelCheckError{Status: http.StatusInternalServerError, Code: "internal", Message: "model imports could not be read"}
		}
		if found.Kind != item.kind {
			return &modelCheckError{Status: http.StatusUnprocessableEntity, Code: "model_import_kind_mismatch", Message: item.uri + " was imported as " + found.Kind + ", not " + item.kind, Client: true}
		}
	}
	return nil
}
