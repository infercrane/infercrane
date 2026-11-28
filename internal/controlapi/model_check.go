package controlapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/intentplan"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// codeMetadataAccessDisabled reports that the operator has not enabled
// Hugging Face metadata access on this control plane.
const codeMetadataAccessDisabled = "metadata_access_disabled"

// modelCheckRequest is one automatic check, read on behalf of a tenant.
type modelCheckRequest struct {
	Tenant                 string
	Repository             string
	Revision               string
	ModelSecretReferenceID string
	ContextTokens          int
	Concurrency            int
}

// modelCheckError carries an HTTP status for the check route and a reason
// the planner can show instead of failing the whole plan.
type modelCheckError struct {
	Status  int
	Code    string
	Message string
	// Client marks a request the caller must fix, such as an unknown
	// credential reference. The planner returns those as errors too.
	Client bool
}

// runModelCheck reads one model from Hugging Face and evaluates it against
// the pinned runtime registries and the GPUs this control plane can price.
func (a API) runModelCheck(ctx context.Context, request modelCheckRequest) (*modelcheck.Report, *modelCheckError) {
	model, registries, failure := a.readModel(ctx, request)
	if failure != nil {
		return nil, failure
	}
	now := time.Now().UTC()
	var gpus []modelcheck.GPU
	if a.GPUPriceCatalog != nil {
		gpus = modelcheck.PricedGPUs(a.GPUPriceCatalog.Snapshot(), now)
	}
	report := modelcheck.Evaluate(model, modelcheck.EvaluateOptions{ContextTokens: request.ContextTokens, Concurrency: request.Concurrency, GPUs: gpus, Registries: registries, Now: now})
	return &report, nil
}

// readModel reads one model from Hugging Face, with the pinned runtime
// registries a check evaluates it against. A token, when named, is resolved
// here and never leaves this function.
func (a API) readModel(ctx context.Context, request modelCheckRequest) (modelcheck.Model, modelcheck.RegistrySnapshot, *modelCheckError) {
	if a.ModelCheck == nil {
		return modelcheck.Model{}, modelcheck.RegistrySnapshot{}, &modelCheckError{Status: http.StatusNotImplemented, Code: codeMetadataAccessDisabled, Message: "Hugging Face metadata access is turned off on this control plane, so models outside the reviewed catalog cannot be checked."}
	}
	registries, err := modelcheck.Registries()
	if err != nil {
		return modelcheck.Model{}, modelcheck.RegistrySnapshot{}, &modelCheckError{Status: http.StatusInternalServerError, Code: "internal", Message: "runtime model registries could not be read"}
	}
	read := modelcheck.Request{Repository: request.Repository, Revision: request.Revision}
	if request.ModelSecretReferenceID != "" {
		token, failure := a.huggingFaceToken(ctx, request.Tenant, request.ModelSecretReferenceID)
		if failure != nil {
			return modelcheck.Model{}, modelcheck.RegistrySnapshot{}, failure
		}
		read.Token, read.Scope = token, request.Tenant+"/"+request.ModelSecretReferenceID
	}
	model, err := a.ModelCheck.Resolve(ctx, read)
	if err != nil {
		var checkErr *modelcheck.Error
		if !errors.As(err, &checkErr) {
			return modelcheck.Model{}, modelcheck.RegistrySnapshot{}, &modelCheckError{Status: http.StatusBadGateway, Code: modelcheck.CodeUnavailable, Message: "Hugging Face could not be read"}
		}
		status := map[string]int{modelcheck.CodeInvalidModel: http.StatusBadRequest, modelcheck.CodeModelNotFound: http.StatusNotFound, modelcheck.CodeRevisionNotFound: http.StatusNotFound, modelcheck.CodeTokenRejected: http.StatusUnprocessableEntity}[checkErr.Code]
		if status == 0 {
			status = http.StatusBadGateway
			if checkErr.Timeout {
				status = http.StatusGatewayTimeout
			}
		}
		return modelcheck.Model{}, modelcheck.RegistrySnapshot{}, &modelCheckError{Status: status, Code: checkErr.Code, Message: checkErr.Message, Client: checkErr.Code == modelcheck.CodeInvalidModel}
	}
	return model, registries, nil
}

// huggingFaceToken resolves the tenant's saved Hugging Face token reference.
func (a API) huggingFaceToken(ctx context.Context, tenant, referenceID string) (string, *modelCheckError) {
	if a.Secrets == nil || a.Store == nil {
		return "", &modelCheckError{Status: http.StatusNotImplemented, Code: "capability_unavailable", Message: "saved model credentials are not configured on this control plane", Client: true}
	}
	references, err := a.Store.SecretReferencesForTenant(ctx, tenant)
	if err != nil {
		return "", &modelCheckError{Status: http.StatusInternalServerError, Code: "internal", Message: "model credential reference could not be loaded", Client: true}
	}
	for _, reference := range references {
		if reference.ID != referenceID {
			continue
		}
		token, resolveErr := a.Secrets.Resolve(ctx, reference)
		if resolveErr != nil || strings.TrimSpace(token) == "" {
			return "", &modelCheckError{Status: http.StatusUnprocessableEntity, Code: "secret_reference_unavailable", Message: "the saved Hugging Face token could not be read on this control plane", Client: true}
		}
		return token, nil
	}
	return "", &modelCheckError{Status: http.StatusUnprocessableEntity, Code: "secret_reference_not_found", Message: "model credential reference was not found in the active tenant", Client: true}
}

// checkHuggingFaceModel answers the automatic check for one repository. It
// is a read: it never deploys, reserves, or stores anything.
func (a API) checkHuggingFaceModel(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	allowed := map[string]bool{"repository": true, "revision": true, "model_secret_reference_id": true, "context_tokens": true, "concurrency": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "supported parameters are repository, revision, model_secret_reference_id, context_tokens, and concurrency, once each")
			return
		}
	}
	repository := strings.TrimSpace(query.Get("repository"))
	revision := strings.TrimSpace(query.Get("revision"))
	if !modelcheck.ValidRepository(repository) || revision != "" && !modelcheck.ValidRevision(revision) {
		writeError(w, http.StatusBadRequest, "invalid_query", "repository must be an exact owner/name Hugging Face repository, and revision a branch, tag, or commit")
		return
	}
	contextTokens, concurrency := 0, 0
	for name, target := range map[string]*int{"context_tokens": &contextTokens, "concurrency": &concurrency} {
		raw := query.Get(name)
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		limit := modelcheck.MaxConcurrency
		floor := 1
		if name == "context_tokens" {
			floor, limit = modelcheck.MinContextTokens, modelcheck.MaxContextTokens
		}
		if err != nil || parsed < floor || parsed > limit {
			writeError(w, http.StatusBadRequest, "invalid_query", name+" must be an integer from "+strconv.Itoa(floor)+" to "+strconv.Itoa(limit))
			return
		}
		*target = parsed
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	report, failure := a.runModelCheck(r.Context(), modelCheckRequest{Tenant: actor.TenantID, Repository: repository, Revision: revision, ModelSecretReferenceID: strings.TrimSpace(query.Get("model_secret_reference_id")), ContextTokens: contextTokens, Concurrency: concurrency})
	if failure != nil {
		writeError(w, failure.Status, failure.Code, failure.Message)
		return
	}
	response := map[string]any{"check": report, "reviewed_model": nil, "performance_claims": false}
	if name := reviewedCatalogName(report.Repository); name != "" {
		response["reviewed_model"] = name
	}
	writeJSON(w, http.StatusOK, response)
}

// reviewedCatalogName returns the reviewed catalog entry for a repository,
// so a caller can prefer the reviewed setup when one exists.
func reviewedCatalogName(repository string) string {
	for _, entry := range curatedrecipe.All() {
		if strings.EqualFold(entry.Model, repository) {
			return entry.Name
		}
	}
	return ""
}

// unreviewedForPlan checks a requested model that is not in the reviewed
// catalog. A check that cannot run becomes a reason in the plan; only a
// request the caller must fix becomes an error.
func (a API) unreviewedForPlan(ctx context.Context, tenant string, request intentplan.Request) (*intentplan.UnreviewedModel, *modelCheckError) {
	repository := intentplan.UnreviewedRepository(request)
	recipes := curatedrecipe.All()
	if repository == "" || intentplan.ResolvesReviewed(recipes, request) {
		return nil, nil
	}
	report, failure := a.runModelCheck(ctx, modelCheckRequest{Tenant: tenant, Repository: repository, Revision: intentplan.UnreviewedRevision(request), ModelSecretReferenceID: request.ModelSecretReferenceID, ContextTokens: request.ContextTokens, Concurrency: request.Concurrency})
	if failure != nil {
		if failure.Client {
			return nil, failure
		}
		return &intentplan.UnreviewedModel{Repository: repository, Code: failure.Code, Reason: failure.Message}, nil
	}
	return &intentplan.UnreviewedModel{Repository: repository, Report: report}, nil
}

// checkUncatalogedProposal checks a proposal's model when it is outside the
// reviewed catalog. It pins a missing revision to the checked commit, bounds
// the baseline's context to the length the GPU was sized for, and refuses a
// model the check found unsupported. When the check cannot run, a request
// that already names a commit proceeds as before.
func (a API) checkUncatalogedProposal(ctx context.Context, tenant string, request *optimizer.Request, secretReferenceID string) *modelCheckError {
	if a.ModelCheck == nil && secretReferenceID == "" {
		return nil
	}
	identity, revision := strings.TrimSpace(request.ModelIdentity), strings.ToLower(strings.TrimSpace(request.ModelRevision))
	if repository, embedded, found := strings.Cut(identity, "@"); found {
		identity = repository
		if revision == "" {
			revision = strings.ToLower(strings.TrimSpace(embedded))
		}
	}
	if !modelcheck.ValidRepository(identity) || reviewedCatalogName(identity) != "" || intentplan.ResolvesReviewed(nil, intentplan.Request{Model: identity}) {
		return nil
	}
	report, failure := a.runModelCheck(ctx, modelCheckRequest{Tenant: tenant, Repository: identity, Revision: revision, ModelSecretReferenceID: secretReferenceID})
	if failure != nil {
		if failure.Client || revision == "" {
			if failure.Status == http.StatusBadRequest || failure.Status == http.StatusNotFound || failure.Status == http.StatusNotImplemented {
				failure.Status = http.StatusUnprocessableEntity
			}
			return failure
		}
		return nil
	}
	if report.Verdict == modelcheck.Unsupported {
		return &modelCheckError{Status: http.StatusUnprocessableEntity, Code: "model_unsupported", Message: report.Reason}
	}
	if request.ModelRevision == "" && !strings.Contains(request.ModelIdentity, "@") {
		request.ModelRevision = report.Revision
	}
	if request.MaxModelLen == 0 && report.Task == modelcheck.TaskChat {
		request.MaxModelLen = report.Memory.ContextTokens
	}
	addModelMemory(request.Diagnosis, *report)
	return nil
}

// addModelMemory gives a diagnosis the model's memory as the check read it:
// the bytes its served weights take and the KV cache bytes one token takes.
// The planner predicts from them how much cache room FP8 weights would free
// and how much of each step's reads the KV cache is. A fact the check could
// not read is left out, and the planner then predicts without it.
func addModelMemory(diagnosis *optimizer.Diagnosis, report modelcheck.Report) {
	weights, perToken := report.Memory.WeightsBytes, report.Config.KVCacheBytesPerToken
	if diagnosis == nil || weights == nil || perToken == nil || *weights <= 0 || *perToken <= 0 {
		return
	}
	if diagnosis.Signals == nil {
		diagnosis.Signals = map[string]float64{}
	}
	diagnosis.Signals[optimizer.SignalModelWeightsBytes] = float64(*weights)
	diagnosis.Signals[optimizer.SignalKVCacheBytesPerToken] = float64(*perToken)
}
