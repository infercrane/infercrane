package controlapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/resultprior"
	"github.com/infercrane/infercrane/internal/resultstore"
)

// maxResultImportBytes bounds one import: MaxPutRecords records of a few KiB.
const maxResultImportBytes = 8 << 20

// maxRankCandidates bounds the candidates one ranking compares.
const maxRankCandidates = 32

func (a API) resultStore(w http.ResponseWriter) (resultstore.Store, bool) {
	store, ok := a.Store.(resultstore.Store)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "measured results are not configured on this control plane")
	}
	return store, ok
}

// listResultRecords reads the caller's own records; another tenant's records
// are never returned, shared or not.
func (a API) listResultRecords(w http.ResponseWriter, r *http.Request) {
	store, ok := a.resultStore(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := resultstore.Filter{Model: query.Get("model"), Accelerator: query.Get("accelerator"), Engine: query.Get("engine"), Kind: query.Get("kind"),
		Metric: query.Get("metric"), Technique: query.Get("technique"), Cell: query.Get("cell")}
	for name, target := range map[string]*int{"min_load": &filter.MinLoad, "max_load": &filter.MaxLoad, "limit": &filter.Limit} {
		if text := query.Get(name); text != "" {
			value, err := strconv.Atoi(text)
			if err != nil || value < 0 || value > resultstore.MaxListLimit*1000 {
				writeError(w, http.StatusBadRequest, "invalid_request", name+" must be a whole number")
				return
			}
			*target = value
		}
	}
	for _, value := range []string{filter.Model, filter.Accelerator, filter.Engine, filter.Kind, filter.Metric, filter.Technique, filter.Cell} {
		if len(value) > 128 {
			writeError(w, http.StatusBadRequest, "invalid_request", "a filter value is longer than 128 characters")
			return
		}
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	records, err := store.ListResultRecords(r.Context(), actor.TenantID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "measured results could not be read")
		return
	}
	if records == nil {
		records = []resultstore.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": records})
}

func (a API) resultRecord(w http.ResponseWriter, r *http.Request) {
	store, ok := a.resultStore(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	record, err := store.GetResultRecord(r.Context(), actor.TenantID, r.PathValue("id"))
	switch {
	case errors.Is(err, resultstore.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "measured result not found")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "the measured result could not be read")
	default:
		writeJSON(w, http.StatusOK, record)
	}
}

// resultImport is a bundle of sealed records and the caller's sharing
// choice for them.
type resultImport struct {
	SchemaVersion string               `json:"schema_version"`
	Sharing       string               `json:"sharing"`
	Records       []resultstore.Record `json:"records"`
}

// importResultRecords adds sealed records to the caller's tenant. Their
// sharing is the request's, never the records' own.
func (a API) importResultRecords(w http.ResponseWriter, r *http.Request) {
	store, ok := a.resultStore(w)
	if !ok {
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResultImportBytes))
	decoder.DisallowUnknownFields()
	var request resultImport
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	if request.SchemaVersion != resultstore.BundleSchemaV1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "schema_version must be "+resultstore.BundleSchemaV1)
		return
	}
	if request.Sharing == "" {
		request.Sharing = resultstore.SharingPrivate
	}
	if request.Sharing != resultstore.SharingPrivate && request.Sharing != resultstore.SharingNumbers {
		writeError(w, http.StatusBadRequest, "invalid_request", "sharing must be private or numbers")
		return
	}
	if len(request.Records) == 0 || len(request.Records) > resultstore.MaxPutRecords {
		writeError(w, http.StatusBadRequest, "invalid_request", "send 1 to "+strconv.Itoa(resultstore.MaxPutRecords)+" measured results")
		return
	}
	for index := range request.Records {
		request.Records[index].Sharing = request.Sharing
		if err := request.Records[index].Validate(); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_result_record", "measured result "+strconv.Itoa(index+1)+": "+err.Error())
			return
		}
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	added, err := store.PutResultRecords(r.Context(), actor.TenantID, request.Records)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "measured results could not be stored")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "received": len(request.Records), "sharing": request.Sharing})
}

// rankResultSetups ranks candidates for a cell from the caller's records and
// other tenants' shared numbers.
func (a API) rankResultSetups(w http.ResponseWriter, r *http.Request) {
	store, ok := a.resultStore(w)
	if !ok {
		return
	}
	var query resultprior.Query
	if !decodeMutationBody(w, r, &query) {
		return
	}
	if strings.TrimSpace(query.Metric) == "" || strings.TrimSpace(query.Kind) == "" || strings.TrimSpace(query.Accelerator) == "" || query.Load < 1 ||
		len(query.Candidates) == 0 || len(query.Candidates) > maxRankCandidates {
		writeError(w, http.StatusBadRequest, "invalid_request", "a ranking needs a metric, a traffic kind, a GPU, a load of at least 1, and 1 to 32 setups")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	ranking, err := (resultprior.StorePrior{Store: store, UseShared: true}).Rank(r.Context(), actor.TenantID, query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "measured results could not be read")
		return
	}
	writeJSON(w, http.StatusOK, ranking)
}
