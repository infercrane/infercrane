package controlapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
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

	checkpoints := checkpointcatalog.MustDefault()
	candidates := checkpoints.Candidates()
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.RevisionState != b.RevisionState {
			return a.RevisionState == checkpointcatalog.RevisionCurrent
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		return a.Repository < b.Repository
	})
	for _, candidate := range candidates {
		key := strings.ToLower(candidate.Base.Repository)
		entry, found := entries[key]
		if !found {
			entry = map[string]any{
				"repository":  candidate.Base.Repository,
				"current":     candidate.Base.ScreeningRevision != "",
				"access":      "public",
				"license":     candidate.License.SPDX,
				"reviewed":    false,
				"source_kind": "publisher-base",
			}
			entries[key] = entry
		}
		if _, primary := entry["optimization"]; primary {
			continue
		}
		if revision := candidate.Base.ScreeningRevision; revision != "" {
			entry["revision"] = revision
		}
		entry["optimization"] = map[string]any{
			"catalog_id":                candidate.ID,
			"publisher":                 checkpoints.PublisherName(candidate.Publisher),
			"tool":                      candidate.Quantization.Tool,
			"tool_version":              candidate.Quantization.ToolVersion,
			"algorithm":                 candidate.Quantization.Scheme,
			"output_repository":         candidate.Repository,
			"output_revision":           candidate.Revision,
			"screening_base_revision":   candidate.Base.ScreeningRevision,
			"screening_revision_source": candidate.Base.ScreeningRevisionSource,
			"lineage_state":             candidate.Base.Lineage,
			"hardware_vendor":           candidate.Hardware.Vendor,
			"accelerator_architectures": candidate.Hardware.Architectures,
			"runtimes":                  candidate.Runtimes,
			"source_url":                candidate.SourceURL,
			"evidence_state":            "unmeasured",
			"qualification_boundary":    "publisher artifact available; exact workload, quality, cost, and release evidence are still required",
		}
	}
	for _, build := range checkpoints.Entries {
		entry, found := entries[strings.ToLower(build.Base.Repository)]
		if !found {
			continue
		}
		builds, _ := entry["publisher_builds"].([]map[string]any)
		entry["publisher_builds"] = append(builds, publisherBuild(build, checkpoints.PublisherName(build.Publisher)))
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
			"Publisher builds for hardware InferCrane cannot run yet are listed as unsupported_hardware and never become candidates.",
		},
		"models": models,
	})
}

// publisherBuild summarizes one catalog entry, including the gates that keep
// it from becoming a proposal candidate.
func publisherBuild(entry checkpointcatalog.Entry, publisher string) map[string]any {
	build := map[string]any{
		"catalog_id":                entry.ID,
		"publisher":                 publisher,
		"kind":                      entry.Kind,
		"repository":                entry.Repository,
		"revision":                  entry.Revision,
		"revision_state":            entry.RevisionState,
		"hardware_vendor":           entry.Hardware.Vendor,
		"accelerator_architectures": entry.Hardware.Architectures,
		"product_support":           entry.Hardware.ProductSupport,
		"runtimes":                  entry.Runtimes,
		"review_state":              entry.Review.State,
		"eligibility":               entry.Eligibility.State,
		"blockers":                  append([]string{}, entry.Eligibility.Blockers...),
		"license":                   entry.License.Tag,
		"license_state":             entry.License.State,
		"lineage_state":             entry.Base.Lineage,
		"publisher_prior_tables":    len(entry.PublisherPriors),
		"source_url":                entry.SourceURL,
		"evidence_state":            "unmeasured",
	}
	if entry.Quantization != nil {
		build["tool"], build["format"] = entry.Quantization.Tool, entry.Quantization.Scheme
	}
	if entry.Speculator != nil {
		build["format"], build["speculator_targets"] = entry.Speculator.Method, entry.Speculator.Targets
	}
	return build
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
