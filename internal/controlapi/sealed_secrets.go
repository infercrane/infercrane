package controlapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secrets"
)

var secretReferenceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// createSealedSecret seals a workspace's own value. The value is never stored
// in plaintext, returned, logged, or audited.
func (a API) createSealedSecret(w http.ResponseWriter, r *http.Request, actor domain.Principal, name, reference, value string) {
	if reference != "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "a sealed secret takes a value, not a reference")
		return
	}
	if !a.SealedSecrets.Available() {
		writeError(w, http.StatusNotImplemented, "sealed_secrets_unavailable", "this control plane has no credential encryption key, so it cannot keep a sealed secret")
		return
	}
	if err := secrets.ValidateSealedValue(value); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	sealed, keyReference, err := a.SealedSecrets.Seal(actor.TenantID, name, value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "the secret could not be sealed")
		return
	}
	item, err := a.Store.CreateSealedSecretReference(r.Context(), actor.TenantID, name, sealed, keyReference)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "a secret with this name already exists; delete it first to replace its value")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "the sealed secret could not be saved")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "secret.create", ResourceType: "secret_reference", ResourceName: item.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"secret": item})
}

// operatorComputeGranted reports whether the workspace may deploy on the
// operator's own providers: the bootstrap operator or a granted workspace.
func (a API) operatorComputeGranted(actor domain.Principal) bool {
	if actor.ID == "bootstrap" {
		return true
	}
	for _, tenant := range a.OperatorComputeTenants {
		if tenant == "*" || (actor.TenantID != "" && tenant == actor.TenantID) {
			return true
		}
	}
	return false
}

// workspaceSecretReference finds one of the workspace's own secret references.
func (a API) workspaceSecretReference(ctx context.Context, tenant, id string) (domain.SecretReference, *modelCheckError) {
	references, err := a.Store.SecretReferencesForTenant(ctx, tenant)
	if err != nil {
		return domain.SecretReference{}, &modelCheckError{Status: http.StatusInternalServerError, Code: "internal", Message: "model credential reference could not be validated"}
	}
	for _, reference := range references {
		if reference.ID == id {
			return reference, nil
		}
	}
	return domain.SecretReference{}, &modelCheckError{Status: http.StatusUnprocessableEntity, Code: "secret_reference_not_found", Message: "model credential reference was not found in the active tenant", Client: true}
}
