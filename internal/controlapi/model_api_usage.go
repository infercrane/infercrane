package controlapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (a API) hostedModelAPIUsage(w http.ResponseWriter, r *http.Request) {
	usageStore, ok := a.Store.(hostedModelAPIUsageStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "hosted Model API usage is not supported by this store")
		return
	}
	allowed := map[string]bool{"window_seconds": true, "bucket_seconds": true, "model": true}
	for key, values := range r.URL.Query() {
		if !allowed[key] || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "hosted Model API usage accepts one window_seconds, bucket_seconds, and model value")
			return
		}
	}
	windowSeconds := 24 * 60 * 60
	if raw := r.URL.Query().Get("window_seconds"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "window_seconds must be an integer")
			return
		}
		windowSeconds = value
	}
	bucketSeconds := defaultMonitoringBucket(windowSeconds)
	if raw := r.URL.Query().Get("bucket_seconds"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "bucket_seconds must be an integer")
			return
		}
		bucketSeconds = value
	}
	if windowSeconds < 60 || windowSeconds > 30*24*60*60 || bucketSeconds < 60 || bucketSeconds > 24*60*60 || bucketSeconds > windowSeconds || (windowSeconds+bucketSeconds-1)/bucketSeconds+1 > 500 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "hosted Model API usage requires a 1 minute..30 day window, a 1 minute..24 hour bucket, and at most 500 buckets")
		return
	}
	model := r.URL.Query().Get("model")
	if model != strings.TrimSpace(model) || len(model) > 256 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "model must be trimmed and at most 256 characters")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	snapshot, err := usageStore.HostedModelAPIUsage(r.Context(), actor.TenantID, model, time.Duration(windowSeconds)*time.Second, time.Duration(bucketSeconds)*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_api_usage_unavailable", "hosted Model API usage evidence could not be read")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
