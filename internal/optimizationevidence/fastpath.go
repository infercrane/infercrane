package optimizationevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// FastPathImport is deliberately not a qualification decision. It preserves
// what the producer claimed while assigning the imported document the highest
// trust state InferCrane can establish without its own revision binding and
// signed quality-evidence attachment.
type FastPathImport struct {
	SchemaVersion            string        `json:"schema_version"`
	ManifestDigest           string        `json:"manifest_digest"`
	CandidateID              string        `json:"candidate_id"`
	ModelIdentity            string        `json:"model_identity"`
	TokenizerIdentity        string        `json:"tokenizer_identity"`
	RuntimeIdentity          string        `json:"runtime_identity"`
	RuntimeImage             string        `json:"runtime_image"`
	GPU                      string        `json:"gpu"`
	GPUCount                 int           `json:"gpu_count"`
	ClaimedStatus            string        `json:"claimed_status"`
	ClaimedEvidenceState     string        `json:"claimed_evidence_state"`
	ImportedEvidenceState    EvidenceState `json:"imported_evidence_state"`
	QualificationEligible    bool          `json:"qualification_eligible"`
	RequiresRevisionBinding  bool          `json:"requires_revision_binding"`
	RequiresSignedQuality    bool          `json:"requires_signed_quality_evidence"`
	OptimizationStillRunning bool          `json:"optimization_still_running"`
	Campaign                 Campaign      `json:"campaign"`
}

// ReproductionBinding is supplied only after the control plane has resolved
// the imported recipe to one of its own immutable revisions and verified a
// signed quality-evidence record for that revision.
type ReproductionBinding struct {
	ModelIdentity     string `json:"model_identity"`
	RevisionID        string `json:"revision_id"`
	QualityEvidenceID string `json:"quality_evidence_id"`
	QualityPassed     bool   `json:"quality_passed"`
}

func (i FastPathImport) BindReproduced(binding ReproductionBinding) (Campaign, error) {
	if binding.ModelIdentity != i.ModelIdentity || strings.TrimSpace(binding.RevisionID) == "" || strings.TrimSpace(binding.QualityEvidenceID) == "" || !binding.QualityPassed {
		return Campaign{}, errors.New("FastPath reproduction binding requires the exact model, immutable InferCrane revision, and passing signed quality evidence")
	}
	if len(i.Campaign.Candidates) != 1 || i.Campaign.Candidates[0].EvidenceState != StateExternalUnverified {
		return Campaign{}, errors.New("FastPath import is not at the external-unverified binding boundary")
	}
	campaign := i.Campaign
	campaign.Candidates = append([]Candidate(nil), i.Campaign.Candidates...)
	campaign.Candidates[0].EvidenceState = StateReproduced
	if err := campaign.Validate(); err != nil {
		return Campaign{}, err
	}
	return campaign, nil
}

type fastPathManifest struct {
	SchemaVersion                 int    `json:"schema_version"`
	ID                            string `json:"id"`
	Status                        string `json:"status"`
	EvidenceState                 string `json:"evidence_state"`
	FinalRelease                  bool   `json:"final_release"`
	OptimizationCampaignContinues bool   `json:"optimization_campaign_continues"`
	Model                         struct {
		Repository          string                    `json:"repository"`
		Revision            string                    `json:"revision"`
		TokenizerRepository string                    `json:"tokenizer_repository"`
		TokenizerRevision   string                    `json:"tokenizer_revision"`
		Artifacts           map[string]map[string]any `json:"artifacts"`
	} `json:"model"`
	Runtime struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BaseImage string `json:"base_image"`
		CUDA      string `json:"cuda_version"`
	} `json:"runtime"`
	Launch struct {
		DeploymentArguments []string `json:"deployment_argument_vector"`
		Environment         struct {
			SecretValuesIncluded bool `json:"secret_values_included"`
		} `json:"environment"`
	} `json:"launch"`
	Hardware struct {
		GPUModel    string `json:"gpu_model"`
		GPUCount    int    `json:"gpu_count"`
		MinimumVRAM int    `json:"minimum_gpu_vram_gib"`
	} `json:"hardware"`
	Qualification struct {
		Provider        string `json:"provider"`
		Passed          bool   `json:"passed"`
		EvidenceLevel   string `json:"evidence_level"`
		IndependentRuns int    `json:"independent_runs"`
		Requests        int    `json:"requests"`
		Successful      int    `json:"successful_requests"`
		Workload        struct {
			ContractID                       string  `json:"contract_id"`
			Digest                           string  `json:"digest"`
			ConcurrencyLanes                 []int   `json:"concurrency_lanes"`
			MinimumMeasurementSecondsPerLane float64 `json:"minimum_measurement_seconds_per_lane"`
			MinimumRequestsPerLanePerRun     int     `json:"minimum_requests_per_lane_per_run"`
		} `json:"workload"`
		SLOPolicy struct {
			Scope                      string   `json:"scope"`
			MaxTTFTMS                  float64  `json:"ttft_ms_max"`
			MaxITLMS                   float64  `json:"tpot_itl_ms_max"`
			MinSuccessfulFraction      float64  `json:"successful_fraction_min"`
			MaxErrorRate               float64  `json:"error_rate_max"`
			MaxPromptTokenMismatchRate float64  `json:"prompt_token_mismatch_rate_max"`
			RequiredQualityGates       []string `json:"required_quality_gates"`
		} `json:"slo_policy"`
		Metrics struct {
			Campaign struct {
				ErrorRate               float64 `json:"error_rate"`
				PromptTokenMismatchRate float64 `json:"prompt_token_mismatch_rate"`
			} `json:"campaign"`
			Lanes map[string]struct {
				Requests           int     `json:"requests"`
				SuccessfulRequests int     `json:"successful_requests"`
				SLOAttainment      float64 `json:"slo_attainment_fraction"`
				TTFT               struct {
					P95 float64 `json:"p95"`
				} `json:"ttft_ms_pooled"`
				ITL struct {
					P95 float64 `json:"p95"`
				} `json:"tpot_itl_ms_pooled"`
				AggregateOutputTokensSecond    float64 `json:"aggregate_output_tokens_per_second_mean"`
				RequestThroughput              float64 `json:"request_throughput_requests_per_second_mean"`
				SLOGoodput                     float64 `json:"slo_goodput_requests_per_second_mean"`
				SLOQualifiedOutputTokensSecond float64 `json:"slo_qualified_output_tokens_per_second_mean"`
				GPUSecondsPerRequest           float64 `json:"gpu_seconds_per_request_mean"`
				CostPerMillionOutputTokens     float64 `json:"measured_cogs_usd_per_successful_1m_output_tokens_mean"`
			} `json:"lanes"`
		} `json:"metrics"`
		Quality struct {
			Passed bool              `json:"passed"`
			Gates  map[string]string `json:"gates"`
		} `json:"quality"`
	} `json:"qualification"`
}

func DecodeFastPathManifest(body []byte) (FastPathImport, error) {
	if len(body) == 0 || len(body) > MaxDocumentBytes {
		return FastPathImport{}, fmt.Errorf("FastPath manifest must be between 1 byte and %d bytes", MaxDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var manifest fastPathManifest
	if err := decoder.Decode(&manifest); err != nil {
		return FastPathImport{}, fmt.Errorf("decode FastPath manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return FastPathImport{}, errors.New("FastPath manifest contains trailing JSON")
		}
		return FastPathImport{}, fmt.Errorf("decode trailing FastPath manifest: %w", err)
	}
	if err := validateFastPathManifest(manifest); err != nil {
		return FastPathImport{}, err
	}
	digest := sha256.Sum256(body)
	recipeBytes, err := json.Marshal(struct {
		ModelRepository string   `json:"model_repository"`
		ModelRevision   string   `json:"model_revision"`
		Runtime         string   `json:"runtime"`
		RuntimeVersion  string   `json:"runtime_version"`
		RuntimeCommit   string   `json:"runtime_commit"`
		RuntimeImage    string   `json:"runtime_image"`
		CUDA            string   `json:"cuda_version"`
		GPU             string   `json:"gpu"`
		GPUCount        int      `json:"gpu_count"`
		Arguments       []string `json:"arguments"`
	}{manifest.Model.Repository, manifest.Model.Revision, manifest.Runtime.Name, manifest.Runtime.Version, manifest.Runtime.Commit, manifest.Runtime.BaseImage, manifest.Runtime.CUDA, manifest.Hardware.GPUModel, manifest.Hardware.GPUCount, manifest.Launch.DeploymentArguments})
	if err != nil {
		return FastPathImport{}, fmt.Errorf("hash FastPath serving recipe: %w", err)
	}
	recipeDigest := sha256.Sum256(recipeBytes)
	candidateID := strings.TrimSpace(manifest.ID)
	if candidateID == "" {
		candidateID = hex.EncodeToString(digest[:])
	}
	qualityGates := make([]QualityGate, 0, len(manifest.Qualification.Quality.Gates))
	for name, state := range manifest.Qualification.Quality.Gates {
		qualityGates = append(qualityGates, QualityGate{Name: name, Passed: strings.HasPrefix(state, "passed")})
	}
	sortQualityGates(qualityGates)
	lanes := make([]LaneEvidence, 0, len(manifest.Qualification.Metrics.Lanes))
	for key, value := range manifest.Qualification.Metrics.Lanes {
		concurrency, _ := strconv.Atoi(key)
		lanes = append(lanes, LaneEvidence{
			Concurrency: concurrency, Requests: value.Requests, SuccessfulRequests: value.SuccessfulRequests,
			IndependentRuns: manifest.Qualification.IndependentRuns, MeasurementSeconds: manifest.Qualification.Workload.MinimumMeasurementSecondsPerLane,
			SLOAttainment: value.SLOAttainment, TTFTP95MS: value.TTFT.P95, ITLP95MS: value.ITL.P95,
			AggregateOutputTokensSecond: value.AggregateOutputTokensSecond, RequestThroughput: value.RequestThroughput,
			SLOGoodput: value.SLOGoodput, SLOQualifiedOutputTokensSecond: value.SLOQualifiedOutputTokensSecond,
			GPUSecondsPerSuccessfulRequest: value.GPUSecondsPerRequest, CostPerMillionSuccessfulOutputTokens: value.CostPerMillionOutputTokens,
		})
	}
	sortLanes(lanes)
	campaign := Campaign{
		SchemaVersion: Schema, ModelIdentity: manifest.Model.Repository + "@" + manifest.Model.Revision,
		HardwareIdentity: strings.TrimSpace(manifest.Qualification.Provider) + ":" + manifest.Hardware.GPUModel + ":" + strconv.Itoa(manifest.Hardware.GPUCount),
		WorkloadID:       manifest.Qualification.Workload.ContractID, WorkloadDigest: manifest.Qualification.Workload.Digest,
		RequiredLanes: append([]int(nil), manifest.Qualification.Workload.ConcurrencyLanes...),
		SLO:           SLOPolicy{Scope: manifest.Qualification.SLOPolicy.Scope, MaxTTFTMS: manifest.Qualification.SLOPolicy.MaxTTFTMS, MaxITLMS: manifest.Qualification.SLOPolicy.MaxITLMS, MinSuccessfulFraction: manifest.Qualification.SLOPolicy.MinSuccessfulFraction, MaxErrorRate: manifest.Qualification.SLOPolicy.MaxErrorRate, MaxPromptTokenMismatchRate: manifest.Qualification.SLOPolicy.MaxPromptTokenMismatchRate, RequiredQualityGates: append([]string(nil), manifest.Qualification.SLOPolicy.RequiredQualityGates...), MinimumRequestsPerLane: manifest.Qualification.Workload.MinimumRequestsPerLanePerRun * manifest.Qualification.IndependentRuns, MinimumIndependentRuns: manifest.Qualification.IndependentRuns, MinimumMeasurementSeconds: manifest.Qualification.Workload.MinimumMeasurementSecondsPerLane},
		Selection:     SelectionPolicy{ID: "fastpath-import-qualification", Version: 1, Objective: ObjectiveQualifiedOutputThroughput, LaneAggregation: "arithmetic_mean", EvidenceLevel: LevelQualification, QualificationBeforeScoring: true, CostTieBreaker: true},
		Candidates:    []Candidate{{ID: candidateID, RuntimeID: manifest.Runtime.Name + "-" + manifest.Runtime.Version, Origin: "fastpath_import", EvidenceLevel: LevelQualification, EvidenceState: StateExternalUnverified, RecipeDigest: "sha256:" + hex.EncodeToString(recipeDigest[:]), WorkloadDigest: manifest.Qualification.Workload.Digest, QualityPassed: manifest.Qualification.Quality.Passed, QualityGates: qualityGates, ErrorRate: manifest.Qualification.Metrics.Campaign.ErrorRate, PromptTokenMismatchRate: manifest.Qualification.Metrics.Campaign.PromptTokenMismatchRate, Lanes: lanes}},
	}
	if err := campaign.Validate(); err != nil {
		return FastPathImport{}, fmt.Errorf("normalize FastPath manifest: %w", err)
	}
	return FastPathImport{
		SchemaVersion: "infercrane.dev/fastpath-import/v1", ManifestDigest: "sha256:" + hex.EncodeToString(digest[:]), CandidateID: candidateID,
		ModelIdentity: campaign.ModelIdentity, TokenizerIdentity: manifest.Model.TokenizerRepository + "@" + manifest.Model.TokenizerRevision,
		RuntimeIdentity: manifest.Runtime.Name + "@" + manifest.Runtime.Version + "+" + manifest.Runtime.Commit, RuntimeImage: manifest.Runtime.BaseImage,
		GPU: manifest.Hardware.GPUModel, GPUCount: manifest.Hardware.GPUCount, ClaimedStatus: manifest.Status, ClaimedEvidenceState: manifest.EvidenceState,
		ImportedEvidenceState: StateExternalUnverified, QualificationEligible: false, RequiresRevisionBinding: true, RequiresSignedQuality: true,
		OptimizationStillRunning: manifest.OptimizationCampaignContinues, Campaign: campaign,
	}, nil
}

func validateFastPathManifest(m fastPathManifest) error {
	if m.SchemaVersion != 1 || strings.TrimSpace(m.Model.Repository) == "" || strings.TrimSpace(m.Model.Revision) == "" || strings.TrimSpace(m.Model.TokenizerRepository) == "" || strings.TrimSpace(m.Model.TokenizerRevision) == "" {
		return errors.New("FastPath manifest requires schema v1 and exact model/tokenizer identities")
	}
	if !immutableRevision(m.Model.Revision) || !immutableRevision(m.Model.TokenizerRevision) {
		return errors.New("FastPath model and tokenizer revisions must be immutable hexadecimal revisions")
	}
	for _, artifact := range []string{"tokenizer_json", "chat_template_jinja"} {
		value, ok := m.Model.Artifacts[artifact]["sha256"].(string)
		if !ok || !isSHA256(value) {
			return fmt.Errorf("FastPath manifest requires a SHA-256 hash for %s", artifact)
		}
	}
	if m.Status != "qualified_candidate" && m.Status != "provisional_serverless_recipe" {
		return errors.New("FastPath import accepts only a qualified candidate or provisional serverless recipe")
	}
	if m.EvidenceState != "qualified" && m.EvidenceState != "optimized_qualified" {
		return errors.New("FastPath candidate must claim qualified evidence")
	}
	if m.FinalRelease {
		return errors.New("a provisional FastPath import cannot claim final release")
	}
	if strings.TrimSpace(m.Runtime.Name) == "" || strings.TrimSpace(m.Runtime.Version) == "" || !immutableRevision(m.Runtime.Commit) || !immutableImage(m.Runtime.BaseImage) || strings.TrimSpace(m.Runtime.CUDA) == "" {
		return errors.New("FastPath manifest requires exact runtime, commit, CUDA version, and digest-pinned image")
	}
	if m.Hardware.GPUCount < 1 || m.Hardware.MinimumVRAM < 1 || strings.TrimSpace(m.Hardware.GPUModel) == "" || strings.TrimSpace(m.Qualification.Provider) == "" {
		return errors.New("FastPath manifest requires bounded exact hardware")
	}
	if len(m.Launch.DeploymentArguments) == 0 || !argumentPair(m.Launch.DeploymentArguments, "--revision", m.Model.Revision) || m.Launch.Environment.SecretValuesIncluded {
		return errors.New("FastPath launch arguments must pin the model revision and exclude secret values")
	}
	if !m.Qualification.Passed || m.Qualification.EvidenceLevel != string(LevelQualification) || m.Qualification.IndependentRuns < 1 || m.Qualification.Requests < 1 || m.Qualification.Successful < 0 || m.Qualification.Successful > m.Qualification.Requests || !m.Qualification.Quality.Passed {
		return errors.New("FastPath manifest lacks complete passing qualification metadata")
	}
	if !fraction(m.Qualification.Metrics.Campaign.ErrorRate) || !fraction(m.Qualification.Metrics.Campaign.PromptTokenMismatchRate) {
		return errors.New("FastPath manifest has invalid measured campaign rates")
	}
	if !isSHA256(m.Qualification.Workload.Digest) || len(m.Qualification.Workload.ConcurrencyLanes) == 0 || len(m.Qualification.Metrics.Lanes) != len(m.Qualification.Workload.ConcurrencyLanes) {
		return errors.New("FastPath qualification requires a workload digest and every declared lane")
	}
	totalRequests, totalSuccessful := 0, 0
	for _, lane := range m.Qualification.Workload.ConcurrencyLanes {
		measurement, present := m.Qualification.Metrics.Lanes[strconv.Itoa(lane)]
		if !present {
			return fmt.Errorf("FastPath qualification is missing concurrency lane %d", lane)
		}
		totalRequests += measurement.Requests
		totalSuccessful += measurement.SuccessfulRequests
	}
	if totalRequests != m.Qualification.Requests || totalSuccessful != m.Qualification.Successful {
		return errors.New("FastPath campaign request totals do not match its lane evidence")
	}
	return nil
}

func immutableRevision(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func immutableImage(value string) bool {
	parts := strings.Split(value, "@")
	return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && isSHA256(parts[1])
}

func argumentPair(arguments []string, name, wanted string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name && arguments[index+1] == wanted {
			return true
		}
	}
	return false
}

func sortLanes(lanes []LaneEvidence) {
	for i := 1; i < len(lanes); i++ {
		for j := i; j > 0 && lanes[j].Concurrency < lanes[j-1].Concurrency; j-- {
			lanes[j], lanes[j-1] = lanes[j-1], lanes[j]
		}
	}
}

func sortQualityGates(gates []QualityGate) {
	for i := 1; i < len(gates); i++ {
		for j := i; j > 0 && gates[j].Name < gates[j-1].Name; j-- {
			gates[j], gates[j-1] = gates[j-1], gates[j]
		}
	}
}
