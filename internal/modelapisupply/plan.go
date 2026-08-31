// Package modelapisupply compiles private managed-inference offers into an
// immutable, explainable supply plan. Supplier identity is intentionally kept
// inside the control plane; customer APIs expose only the InferCrane service
// contract selected from this plan.
package modelapisupply

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/managedbilling"
)

const (
	SchemaVersion                     = "managed-model-supply-plan/v1"
	MinimumSafeGrossMarginBPS         = 1500
	defaultMaximumFallbacks           = 2
	StatusReady                       = "ready"
	StatusInsufficient                = "insufficient"
	ReasonModelMismatch               = "model_mismatch"
	ReasonProtocolMismatch            = "protocol_mismatch"
	ReasonCapabilityMissing           = "capability_missing"
	ReasonRegionUnavailable           = "region_unavailable"
	ReasonAccessNotReady              = "access_not_ready"
	ReasonCapacityUnavailable         = "capacity_unavailable"
	ReasonHealthNotReady              = "health_not_ready"
	ReasonObservationStale            = "observation_stale"
	ReasonRateExpired                 = "rate_expired"
	ReasonCostBasisAbsent             = "cost_basis_absent"
	ReasonMarginBelowFloor            = "margin_below_floor"
	ReasonCapacityEvidenceAbsent      = "capacity_evidence_absent"
	ReasonCapacityEvidenceStale       = "capacity_evidence_stale"
	ReasonCapacityEvidenceMismatched  = "capacity_evidence_mismatched"
	ReasonCapacitySamplesInsufficient = "capacity_samples_insufficient"
	ReasonTTFTSLOMiss                 = "ttft_slo_miss"
	ReasonThroughputSLOMiss           = "throughput_slo_miss"
)

// Request is the private planning contract. A zero input/output token shape is
// valid, but cost-based ranking is then unavailable and the plan says so.
type Request struct {
	ModelID                string
	Protocol               string
	Capabilities           []string
	Region                 string
	InputTokens            int
	OutputTokens           int
	MinimumGrossMarginBPS  int
	MaximumObservationAge  time.Duration
	MaximumEvidenceAge     time.Duration
	MinimumEvidenceSamples int
	MaximumTTFTP95MS       *float64
	MinimumOutputTokensPS  *float64
	MaximumFallbacks       int
	At                     time.Time
}

// CapacityEvidence is comparable only when TupleKey exactly matches the
// candidate tuple. Percentiles are optional unless the request asks for the
// corresponding SLO.
type CapacityEvidence struct {
	TupleKey       string
	ObservedAt     time.Time
	SampleCount    int
	TTFTP95MS      *float64
	OutputTokensP5 *float64
}

// Candidate is a private supplier offer. It deliberately contains no secret
// or credential fields so the resulting plan is safe to persist internally.
type Candidate struct {
	ID                          string
	Supplier                    string
	ModelID                     string
	SupplierModelID             string
	TupleKey                    string
	Protocol                    string
	Capabilities                []string
	Regions                     []string
	Access                      string
	Availability                string
	Health                      string
	ObservedAt                  time.Time
	RateValidUntil              time.Time
	RetailInputMicrousdPerMTok  *int64
	RetailOutputMicrousdPerMTok *int64
	CostInputMicrousdPerMTok    *int64
	CostOutputMicrousdPerMTok   *int64
	CostBasisProvenance         string
	Evidence                    *CapacityEvidence
}

type Rejection struct {
	CandidateID string   `json:"candidate_id"`
	Reasons     []string `json:"reasons"`
}

type Selection struct {
	CandidateID                   string `json:"candidate_id"`
	Supplier                      string `json:"supplier"`
	SupplierModelID               string `json:"supplier_model_id"`
	TupleKey                      string `json:"tuple_key"`
	EstimatedRetailMicrousd       *int64 `json:"estimated_retail_microusd,omitempty"`
	EstimatedSupplierCostMicrousd *int64 `json:"estimated_supplier_cost_microusd,omitempty"`
	GrossMarginBPS                int    `json:"gross_margin_bps"`
	EvidenceSampleCount           int    `json:"evidence_sample_count,omitempty"`
}

type Plan struct {
	SchemaVersion string      `json:"schema_version"`
	Digest        string      `json:"digest"`
	ModelID       string      `json:"model_id"`
	Protocol      string      `json:"protocol"`
	Status        string      `json:"status"`
	RankingBasis  string      `json:"ranking_basis"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Primary       *Selection  `json:"primary,omitempty"`
	Fallbacks     []Selection `json:"fallbacks"`
	Rejections    []Rejection `json:"rejections"`
}

// Compile produces a deterministic decision for the same request, candidates,
// and timestamp. Business-policy failures are represented as an insufficient
// plan; malformed planner input is returned as an error.
func Compile(request Request, candidates []Candidate) (Plan, error) {
	if err := validateRequest(request); err != nil {
		return Plan{}, err
	}
	request.At = request.At.UTC()
	ordered := append([]Candidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for index, candidate := range ordered {
		if strings.TrimSpace(candidate.ID) == "" || strings.TrimSpace(candidate.Supplier) == "" || strings.TrimSpace(candidate.SupplierModelID) == "" || strings.TrimSpace(candidate.TupleKey) == "" {
			return Plan{}, fmt.Errorf("candidate %d requires id, supplier, supplier model id, and tuple key", index)
		}
		if index > 0 && candidate.ID == ordered[index-1].ID {
			return Plan{}, fmt.Errorf("candidate id %q is duplicated", candidate.ID)
		}
	}

	eligible := make([]Selection, 0, len(ordered))
	rejections := make([]Rejection, 0, len(ordered))
	for _, candidate := range ordered {
		selection, reasons := evaluate(request, candidate)
		if len(reasons) > 0 {
			rejections = append(rejections, Rejection{CandidateID: candidate.ID, Reasons: reasons})
			continue
		}
		eligible = append(eligible, selection)
	}

	rankingBasis := "stable_candidate_identity"
	if request.InputTokens > 0 || request.OutputTokens > 0 {
		rankingBasis = "estimated_customer_cost_for_declared_workload"
		sort.SliceStable(eligible, func(i, j int) bool {
			left, right := *eligible[i].EstimatedRetailMicrousd, *eligible[j].EstimatedRetailMicrousd
			if left != right {
				return left < right
			}
			return eligible[i].CandidateID < eligible[j].CandidateID
		})
	}

	plan := Plan{
		SchemaVersion: SchemaVersion,
		ModelID:       request.ModelID,
		Protocol:      request.Protocol,
		Status:        StatusInsufficient,
		RankingBasis:  rankingBasis,
		GeneratedAt:   request.At,
		Fallbacks:     []Selection{},
		Rejections:    rejections,
	}
	if len(eligible) > 0 {
		plan.Status = StatusReady
		plan.Primary = selectionPointer(eligible[0])
		plan.Fallbacks = chooseFallbacks(eligible[1:], eligible[0].Supplier, maximumFallbacks(request.MaximumFallbacks))
	}
	digest, err := digestPlan(plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Digest = digest
	return plan, nil
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.ModelID) == "" || strings.TrimSpace(request.Protocol) == "" {
		return errors.New("model id and protocol are required")
	}
	if request.At.IsZero() {
		return errors.New("planning timestamp is required")
	}
	if request.InputTokens < 0 || request.OutputTokens < 0 {
		return errors.New("workload token counts cannot be negative")
	}
	if request.MinimumGrossMarginBPS < MinimumSafeGrossMarginBPS || request.MinimumGrossMarginBPS >= 10_000 {
		return fmt.Errorf("minimum gross margin must be between %d and 9999 basis points", MinimumSafeGrossMarginBPS)
	}
	if request.MaximumObservationAge <= 0 {
		return errors.New("maximum observation age must be positive")
	}
	if request.MaximumFallbacks < 0 {
		return errors.New("maximum fallbacks cannot be negative")
	}
	if request.MaximumTTFTP95MS != nil && *request.MaximumTTFTP95MS <= 0 {
		return errors.New("maximum TTFT p95 must be positive")
	}
	if request.MinimumOutputTokensPS != nil && *request.MinimumOutputTokensPS <= 0 {
		return errors.New("minimum output throughput must be positive")
	}
	if request.MaximumTTFTP95MS != nil || request.MinimumOutputTokensPS != nil {
		if request.MaximumEvidenceAge <= 0 || request.MinimumEvidenceSamples <= 0 {
			return errors.New("SLO planning requires a positive evidence age and minimum sample count")
		}
	}
	return nil
}

func evaluate(request Request, candidate Candidate) (Selection, []string) {
	reasons := make([]string, 0, 4)
	if candidate.ModelID != request.ModelID {
		reasons = append(reasons, ReasonModelMismatch)
	}
	if candidate.Protocol != request.Protocol {
		reasons = append(reasons, ReasonProtocolMismatch)
	}
	if !containsAll(candidate.Capabilities, request.Capabilities) {
		reasons = append(reasons, ReasonCapabilityMissing)
	}
	if request.Region != "" && !contains(candidate.Regions, request.Region) {
		reasons = append(reasons, ReasonRegionUnavailable)
	}
	if candidate.Access != "ready" {
		reasons = append(reasons, ReasonAccessNotReady)
	}
	if candidate.Availability != "available" {
		reasons = append(reasons, ReasonCapacityUnavailable)
	}
	if candidate.Health != "healthy" {
		reasons = append(reasons, ReasonHealthNotReady)
	}
	if candidate.ObservedAt.IsZero() || request.At.Sub(candidate.ObservedAt.UTC()) < 0 || request.At.Sub(candidate.ObservedAt.UTC()) > request.MaximumObservationAge {
		reasons = append(reasons, ReasonObservationStale)
	}
	if candidate.RateValidUntil.IsZero() || !candidate.RateValidUntil.UTC().After(request.At) {
		reasons = append(reasons, ReasonRateExpired)
	}

	pricesPresent := candidate.RetailInputMicrousdPerMTok != nil && candidate.RetailOutputMicrousdPerMTok != nil && candidate.CostInputMicrousdPerMTok != nil && candidate.CostOutputMicrousdPerMTok != nil && strings.TrimSpace(candidate.CostBasisProvenance) != ""
	marginBPS := 0
	if !pricesPresent {
		reasons = append(reasons, ReasonCostBasisAbsent)
	} else if !profitable(*candidate.RetailInputMicrousdPerMTok, *candidate.CostInputMicrousdPerMTok, request.MinimumGrossMarginBPS) || !profitable(*candidate.RetailOutputMicrousdPerMTok, *candidate.CostOutputMicrousdPerMTok, request.MinimumGrossMarginBPS) {
		reasons = append(reasons, ReasonMarginBelowFloor)
	} else {
		inputMargin := grossMarginBPS(*candidate.RetailInputMicrousdPerMTok, *candidate.CostInputMicrousdPerMTok)
		outputMargin := grossMarginBPS(*candidate.RetailOutputMicrousdPerMTok, *candidate.CostOutputMicrousdPerMTok)
		marginBPS = min(inputMargin, outputMargin)
	}

	if request.MaximumTTFTP95MS != nil || request.MinimumOutputTokensPS != nil {
		evidence := candidate.Evidence
		switch {
		case evidence == nil:
			reasons = append(reasons, ReasonCapacityEvidenceAbsent)
		case evidence.TupleKey != candidate.TupleKey:
			reasons = append(reasons, ReasonCapacityEvidenceMismatched)
		case evidence.ObservedAt.IsZero() || request.At.Sub(evidence.ObservedAt.UTC()) < 0 || request.At.Sub(evidence.ObservedAt.UTC()) > request.MaximumEvidenceAge:
			reasons = append(reasons, ReasonCapacityEvidenceStale)
		case evidence.SampleCount < request.MinimumEvidenceSamples:
			reasons = append(reasons, ReasonCapacitySamplesInsufficient)
		default:
			if request.MaximumTTFTP95MS != nil && (evidence.TTFTP95MS == nil || *evidence.TTFTP95MS > *request.MaximumTTFTP95MS) {
				reasons = append(reasons, ReasonTTFTSLOMiss)
			}
			if request.MinimumOutputTokensPS != nil && (evidence.OutputTokensP5 == nil || *evidence.OutputTokensP5 < *request.MinimumOutputTokensPS) {
				reasons = append(reasons, ReasonThroughputSLOMiss)
			}
		}
	}

	selection := Selection{CandidateID: candidate.ID, Supplier: candidate.Supplier, SupplierModelID: candidate.SupplierModelID, TupleKey: candidate.TupleKey, GrossMarginBPS: marginBPS}
	if candidate.Evidence != nil {
		selection.EvidenceSampleCount = candidate.Evidence.SampleCount
	}
	if pricesPresent && (request.InputTokens > 0 || request.OutputTokens > 0) {
		retail, retailErr := managedbilling.TokenCostMicrousd(request.InputTokens, request.OutputTokens, *candidate.RetailInputMicrousdPerMTok, *candidate.RetailOutputMicrousdPerMTok)
		cost, costErr := managedbilling.TokenCostMicrousd(request.InputTokens, request.OutputTokens, *candidate.CostInputMicrousdPerMTok, *candidate.CostOutputMicrousdPerMTok)
		if retailErr != nil || costErr != nil {
			reasons = append(reasons, ReasonCostBasisAbsent)
		} else {
			selection.EstimatedRetailMicrousd = &retail
			selection.EstimatedSupplierCostMicrousd = &cost
		}
	}
	return selection, uniqueSorted(reasons)
}

func chooseFallbacks(candidates []Selection, primarySupplier string, maximum int) []Selection {
	if maximum == 0 || len(candidates) == 0 {
		return []Selection{}
	}
	ordered := append([]Selection(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		leftDifferent, rightDifferent := ordered[i].Supplier != primarySupplier, ordered[j].Supplier != primarySupplier
		if leftDifferent != rightDifferent {
			return leftDifferent
		}
		return false
	})
	if len(ordered) > maximum {
		ordered = ordered[:maximum]
	}
	return ordered
}

func maximumFallbacks(value int) int {
	if value == 0 {
		return defaultMaximumFallbacks
	}
	return value
}

func profitable(retail, cost int64, marginBPS int) bool {
	if retail <= 0 || cost < 0 {
		return false
	}
	minimum, err := managedbilling.MinimumRetailPriceMicrousd(cost, marginBPS)
	return err == nil && retail >= minimum
}

func grossMarginBPS(retail, cost int64) int {
	if retail <= 0 || cost < 0 || cost > retail {
		return 0
	}
	numerator := new(big.Int).Mul(big.NewInt(retail-cost), big.NewInt(10_000))
	numerator.Quo(numerator, big.NewInt(retail))
	return int(numerator.Int64())
}

func digestPlan(plan Plan) (string, error) {
	copy := plan
	copy.Digest = ""
	body, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("encode supply plan: %w", err)
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func selectionPointer(value Selection) *Selection { return &value }

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsAll(values, required []string) bool {
	for _, item := range required {
		if !contains(values, item) {
			return false
		}
	}
	return true
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
