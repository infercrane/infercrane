package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
)

type gpuProfileStore interface {
	RecordGPUProfile(context.Context, string, string, gpuprofile.Recording) (gpuprofile.Receipt, error)
	EndpointGPUProfile(context.Context, string, string) (gpuprofile.Profile, error)
}

// recordGPUProfile attaches one bounded, content-free GPU profile capture to a
// deployment's active revision. It is a control-plane write and never runs on
// the inference path.
func (a API) recordGPUProfile(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(gpuProfileStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "GPU profile storage is not configured")
		return
	}
	var recording gpuprofile.Recording
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, gpuprofile.MaxRecordingBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recording); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "gpu_profile_too_large", "a GPU profile capture must not exceed 8 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	recording = gpuprofile.Normalize(recording)
	if err := gpuprofile.Validate(recording, time.Now().UTC()); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_gpu_profile", err.Error())
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	receipt, err := store.RecordGPUProfile(r.Context(), actor.TenantID, r.PathValue("name"), recording)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "a retry of this GPU profile capture changed its samples")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "GPU profile could not be recorded")
	default:
		writeJSON(w, http.StatusCreated, receipt)
	}
}

// endpointGPUProfile returns the newest capture of the active revision behind
// an endpoint. A missing capture is a typed 404, never an empty profile.
func (a API) endpointGPUProfile(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(gpuProfileStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "GPU profile storage is not configured")
		return
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "gpu-profile accepts no query parameters")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	profile, err := store.EndpointGPUProfile(r.Context(), actor.TenantID, r.PathValue("name"))
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "endpoint was not found")
	case errors.Is(err, gpuprofile.ErrNotRecorded):
		writeError(w, http.StatusNotFound, "gpu_profile_not_found", "no GPU profile is recorded for the active revision of this endpoint")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "GPU profile could not be read")
	default:
		writeJSON(w, http.StatusOK, profile)
	}
}
