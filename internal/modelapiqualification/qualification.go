// Package modelapiqualification builds deterministic, time-bounded
// qualification evidence for one provider-bound Model API target.
package modelapiqualification

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
)

const (
	SchemaVersion     = "infercrane.model-api.qualification/v1"
	MaximumValidity   = 24 * time.Hour
	digestPrefix      = "sha256:"
	nanosecondsPerMS  = int64(time.Millisecond)
	nanosecondsPerSec = int64(time.Second)
)

// Target is the provider-bound supplier tuple observed by a qualification run.
// TupleKey is the opaque key already carried by an immutable supplier offer;
// the other fields make the key's meaning auditable instead of relying on
// convention. A routed provider tuple is not proof of its private runtime or
// model revision, so evidence is always limited to MaximumValidity.
type Target struct {
	TupleKey        string   `json:"tuple_key"`
	Supplier        string   `json:"supplier"`
	Adapter         string   `json:"adapter"`
	SupplierModelID string   `json:"supplier_model_id"`
	Operation       string   `json:"operation"`
	Protocol        string   `json:"protocol"`
	Region          string   `json:"region"`
	Capabilities    []string `json:"capabilities"`
}

// Sample is one successful request with complete timing and usage. Throughput
// is measured over generation time (first token to completion), not TTFT.
type Sample struct {
	RequestID    string    `json:"request_id"`
	StartedAt    time.Time `json:"started_at"`
	FirstTokenAt time.Time `json:"first_token_at"`
	CompletedAt  time.Time `json:"completed_at"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// Evidence is a content-addressed measurement. Digest commits to the exact
// tuple, validity window, and every raw timing/usage sample, not only to the
// aggregate percentiles.
type Evidence struct {
	SchemaVersion           string    `json:"schema_version"`
	Target                  Target    `json:"target"`
	ObservedAt              time.Time `json:"observed_at"`
	ValidUntil              time.Time `json:"valid_until"`
	SampleCount             int       `json:"sample_count"`
	TTFTP95MS               float64   `json:"ttft_p95_ms"`
	OutputTokensPerSecondP5 float64   `json:"output_tokens_per_second_p5"`
	Digest                  string    `json:"digest"`
}

type canonicalSample struct {
	RequestID    string    `json:"request_id"`
	StartedAt    time.Time `json:"started_at"`
	FirstTokenAt time.Time `json:"first_token_at"`
	CompletedAt  time.Time `json:"completed_at"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

type digestEnvelope struct {
	SchemaVersion string            `json:"schema_version"`
	Target        Target            `json:"target"`
	ObservedAt    time.Time         `json:"observed_at"`
	ValidUntil    time.Time         `json:"valid_until"`
	Samples       []canonicalSample `json:"samples"`
}

// Measure validates a set of complete successful samples and derives
// nearest-rank p95 TTFT and p5 output-token throughput. Sample order does not
// affect either the statistics or the digest.
func Measure(target Target, samples []Sample, validFor time.Duration) (Evidence, error) {
	target, err := canonicalTarget(target)
	if err != nil {
		return Evidence{}, err
	}
	if validFor <= 0 || validFor > MaximumValidity {
		return Evidence{}, fmt.Errorf("qualification validity must be positive and at most %s", MaximumValidity)
	}
	if len(samples) == 0 {
		return Evidence{}, errors.New("qualification requires at least one complete sample")
	}

	canonical := make([]canonicalSample, 0, len(samples))
	ttfts := make([]time.Duration, 0, len(samples))
	throughput := make([]Sample, 0, len(samples))
	seen := make(map[string]struct{}, len(samples))
	var observedAt time.Time
	for _, sample := range samples {
		normalized, validationErr := canonicalizeSample(sample)
		if validationErr != nil {
			return Evidence{}, validationErr
		}
		if _, exists := seen[normalized.RequestID]; exists {
			return Evidence{}, fmt.Errorf("qualification sample request id %q is duplicated", normalized.RequestID)
		}
		seen[normalized.RequestID] = struct{}{}
		if normalized.CompletedAt.After(observedAt) {
			observedAt = normalized.CompletedAt
		}
		ttfts = append(ttfts, normalized.FirstTokenAt.Sub(normalized.StartedAt))
		throughput = append(throughput, normalized)
		canonical = append(canonical, canonicalSample{
			RequestID: normalized.RequestID, StartedAt: normalized.StartedAt,
			FirstTokenAt: normalized.FirstTokenAt, CompletedAt: normalized.CompletedAt,
			InputTokens: normalized.InputTokens, OutputTokens: normalized.OutputTokens,
		})
	}

	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })
	sort.Slice(throughput, func(i, j int) bool {
		comparison := compareThroughput(throughput[i], throughput[j])
		if comparison == 0 {
			return throughput[i].RequestID < throughput[j].RequestID
		}
		return comparison < 0
	})
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].RequestID < canonical[j].RequestID })

	validUntil := observedAt.Add(validFor)
	envelope := digestEnvelope{SchemaVersion: SchemaVersion, Target: target, ObservedAt: observedAt, ValidUntil: validUntil, Samples: canonical}
	digest, err := digestJSON(envelope)
	if err != nil {
		return Evidence{}, err
	}
	ttft := ttfts[nearestRankIndex(len(ttfts), 95)]
	throughputSample := throughput[nearestRankIndex(len(throughput), 5)]
	generation := throughputSample.CompletedAt.Sub(throughputSample.FirstTokenAt)

	return Evidence{
		SchemaVersion: SchemaVersion, Target: target, ObservedAt: observedAt, ValidUntil: validUntil,
		SampleCount: len(samples), TTFTP95MS: float64(ttft) / float64(nanosecondsPerMS),
		OutputTokensPerSecondP5: float64(throughputSample.OutputTokens) * float64(nanosecondsPerSec) / float64(generation),
		Digest:                  digest,
	}, nil
}

// ValidAt reports whether evidence is inside its half-open validity window.
func (e Evidence) ValidAt(at time.Time) bool {
	at = canonicalTime(at)
	return !at.Before(e.ObservedAt) && at.Before(e.ValidUntil)
}

func canonicalTarget(target Target) (Target, error) {
	required := map[string]string{
		"tuple key": target.TupleKey, "supplier": target.Supplier, "adapter": target.Adapter,
		"supplier model id": target.SupplierModelID, "operation": target.Operation,
		"protocol": target.Protocol, "region": target.Region,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return Target{}, fmt.Errorf("qualification target %s must be non-empty and canonical", name)
		}
	}
	if len(target.Capabilities) == 0 {
		return Target{}, errors.New("qualification target requires at least one capability")
	}
	target.Capabilities = append([]string(nil), target.Capabilities...)
	sort.Strings(target.Capabilities)
	for index, capability := range target.Capabilities {
		if strings.TrimSpace(capability) == "" || capability != strings.TrimSpace(capability) {
			return Target{}, errors.New("qualification target capabilities must be non-empty and canonical")
		}
		if index > 0 && capability == target.Capabilities[index-1] {
			return Target{}, fmt.Errorf("qualification target capability %q is duplicated", capability)
		}
	}
	return target, nil
}

func canonicalizeSample(sample Sample) (Sample, error) {
	if strings.TrimSpace(sample.RequestID) == "" || sample.RequestID != strings.TrimSpace(sample.RequestID) {
		return Sample{}, errors.New("qualification sample request id must be non-empty and canonical")
	}
	sample.StartedAt = canonicalTime(sample.StartedAt)
	sample.FirstTokenAt = canonicalTime(sample.FirstTokenAt)
	sample.CompletedAt = canonicalTime(sample.CompletedAt)
	if sample.StartedAt.IsZero() || sample.FirstTokenAt.IsZero() || sample.CompletedAt.IsZero() {
		return Sample{}, fmt.Errorf("qualification sample %q requires complete timestamps", sample.RequestID)
	}
	if sample.FirstTokenAt.Before(sample.StartedAt) || !sample.CompletedAt.After(sample.FirstTokenAt) {
		return Sample{}, fmt.Errorf("qualification sample %q has invalid timing order", sample.RequestID)
	}
	if sample.InputTokens < 0 || sample.OutputTokens <= 0 {
		return Sample{}, fmt.Errorf("qualification sample %q requires non-negative input and positive output usage", sample.RequestID)
	}
	return sample, nil
}

func canonicalTime(value time.Time) time.Time { return value.Round(0).UTC() }

func nearestRankIndex(length, percentile int) int {
	// ceil(percentile*length/100)-1, expressed without floating point.
	return (percentile*length+99)/100 - 1
}

func compareThroughput(left, right Sample) int {
	// Compare outputTokens/generationSeconds by cross multiplication. big.Int
	// avoids overflow for valid int64 usage and time.Duration values.
	leftValue := new(big.Int).Mul(big.NewInt(left.OutputTokens), big.NewInt(int64(right.CompletedAt.Sub(right.FirstTokenAt))))
	rightValue := new(big.Int).Mul(big.NewInt(right.OutputTokens), big.NewInt(int64(left.CompletedAt.Sub(left.FirstTokenAt))))
	return leftValue.Cmp(rightValue)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("qualification evidence could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return digestPrefix + hex.EncodeToString(digest[:]), nil
}
