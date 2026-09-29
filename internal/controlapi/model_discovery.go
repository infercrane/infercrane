package controlapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/modeloptcatalog"
)

// huggingFaceCatalogModels returns a deliberately small, reviewed discovery
// snapshot. It is not a live popularity feed and does not turn a publisher
// checkpoint into InferCrane performance or quality evidence.
func (a API) huggingFaceCatalogModels(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_query", "this reviewed snapshot does not accept filters")
		return
	}

	entries := map[string]map[string]any{}
	for _, model := range curatedrecipe.All() {
		entries[strings.ToLower(model.Model)] = map[string]any{
			"repository":   model.Model,
			"revision":     model.Revision,
			"author":       model.Publisher,
			"current":      true,
			"access":       accessLabel(model.Gated),
			"pipeline_tag": firstTask(model.Tasks),
			"license":      model.License,
			"reviewed":     true,
			"source_kind":  "reviewed",
		}
	}

	for _, seed := range modeloptcatalog.MustDefault().Seeds {
		key := strings.ToLower(seed.BaseRepository)
		entry, found := entries[key]
		if !found {
			entry = map[string]any{
				"repository":  seed.BaseRepository,
				"current":     seed.ScreeningBaseRevision != "",
				"access":      "public",
				"license":     seed.LicenseSPDX,
				"reviewed":    false,
				"source_kind": "publisher-base",
			}
			entries[key] = entry
		}
		if revision := seed.ScreeningBaseRevision; revision != "" {
			entry["revision"] = revision
		}
		entry["optimization"] = map[string]any{
			"catalog_id":                seed.ID,
			"publisher":                 seed.Publisher,
			"tool":                      seed.Tool,
			"tool_version":              seed.ToolVersion,
			"algorithm":                 seed.Algorithm,
			"output_repository":         seed.OutputRepository,
			"output_revision":           seed.OutputRevision,
			"screening_base_revision":   seed.ScreeningBaseRevision,
			"screening_revision_source": seed.ScreeningRevisionSource,
			"lineage_state":             seed.LineageState,
			"accelerator_architectures": seed.AcceleratorArchitectures,
			"runtimes":                  seed.Runtimes,
			"source_url":                seed.SourceURL,
			"evidence_state":            "unmeasured",
			"qualification_boundary":    "publisher artifact available; exact workload, quality, cost, and release evidence are still required",
		}
	}

	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		models = append(models, entry)
	}
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i]["repository"].(string)) < strings.ToLower(models[j]["repository"].(string))
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": "infercrane.model-discovery/v1",
		"state":          "reviewed_snapshot",
		"limitations": []string{
			"This is a reviewed discovery snapshot, not live Hugging Face popularity data.",
			"Publisher-optimized checkpoints remain unmeasured until an exact InferCrane campaign qualifies them.",
		},
		"models": models,
	})
}

func accessLabel(gated bool) string {
	if gated {
		return "gated"
	}
	return "public"
}

func firstTask(tasks []string) string {
	if len(tasks) == 0 {
		return "text-generation"
	}
	return tasks[0]
}
