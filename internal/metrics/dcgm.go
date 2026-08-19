package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const (
	maxDCGMPayloadBytes = 8 << 20
	maxDCGMSeries       = 4096
)

// DCGMOptions defines an explicit trust boundary for one dcgm-exporter
// snapshot. Selectors prevent a shared exporter from leaking another workload's
// series into a deployment aggregate.
type DCGMOptions struct {
	Selector        map[string]string
	ReplicaID       string
	Source          string
	UtilizationUnit string
	ObservedAt      time.Time
	TTL             time.Duration
}

type dcgmAggregate struct {
	name    string
	unit    string
	value   float64
	samples int
	mode    string
}

// ParseDCGM converts the bounded subset of NVIDIA DCGM metrics used by the
// InferCrane evidence model. It intentionally does not expose arbitrary
// Prometheus labels or metrics, which would create unbounded cardinality and a
// misleading compatibility promise.
func ParseDCGM(text string, options DCGMOptions) ([]domain.OperationalMeasurement, error) {
	if len(text) > maxDCGMPayloadBytes {
		return nil, errors.New("DCGM payload exceeds 8 MiB")
	}
	if options.Source == "" {
		options.Source = "dcgm_exporter"
	}
	if options.UtilizationUnit == "" {
		options.UtilizationUnit = "percent"
	}
	if options.UtilizationUnit != "percent" && options.UtilizationUnit != "ratio" {
		return nil, errors.New("DCGM utilization unit must be percent or ratio")
	}
	if options.ObservedAt.IsZero() {
		options.ObservedAt = time.Now().UTC()
	}
	if options.TTL <= 0 || options.TTL > 24*time.Hour {
		return nil, errors.New("DCGM evidence TTL must be greater than zero and at most 24 hours")
	}

	aggregates := map[string]*dcgmAggregate{}
	series := 0
	devUtilObserved := false
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), maxDCGMPayloadBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, relevant, err := parseDCGMLine(line)
		if err != nil {
			if relevant {
				return nil, err
			}
			continue
		}
		if !relevant || !labelsMatch(labels, options.Selector) {
			continue
		}
		series++
		if series > maxDCGMSeries {
			return nil, errors.New("DCGM payload contains more than 4096 selected series")
		}
		switch name {
		case "DCGM_FI_DEV_GPU_UTIL":
			if options.UtilizationUnit == "ratio" {
				value *= 100
			}
			if value < 0 || value > 100 {
				return nil, fmt.Errorf("DCGM GPU utilization %.4g is outside 0..100 percent", value)
			}
			if !devUtilObserved {
				delete(aggregates, "gpu_utilization")
				devUtilObserved = true
			}
			aggregateDCGM(aggregates, "gpu_utilization", "percent", value, "max")
		case "DCGM_FI_PROF_GR_ENGINE_ACTIVE":
			if devUtilObserved {
				continue
			}
			if value < 0 || value > 1 {
				return nil, fmt.Errorf("DCGM graphics-engine activity %.4g is outside 0..1 ratio", value)
			}
			aggregateDCGM(aggregates, "gpu_utilization", "percent", value*100, "max")
		case "DCGM_FI_DEV_FB_USED":
			if value < 0 {
				return nil, errors.New("DCGM framebuffer usage cannot be negative")
			}
			aggregateDCGM(aggregates, "gpu_memory", "bytes", value*1024*1024, "sum")
		case "DCGM_FI_DEV_GPU_TEMP":
			aggregateDCGM(aggregates, "gpu_temperature", "celsius", value, "max")
		case "DCGM_FI_DEV_POWER_USAGE":
			if value < 0 {
				return nil, errors.New("DCGM power usage cannot be negative")
			}
			aggregateDCGM(aggregates, "gpu_power", "watts", value, "sum")
		case "DCGM_FI_DEV_XID_ERRORS":
			if value < 0 {
				return nil, errors.New("DCGM XID error count cannot be negative")
			}
			aggregateDCGM(aggregates, "gpu_xid_errors", "count", value, "sum")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan DCGM payload: %w", err)
	}
	if len(aggregates) == 0 {
		return nil, errors.New("DCGM payload contained no selected supported measurements")
	}

	order := []string{"gpu_utilization", "gpu_memory", "gpu_temperature", "gpu_power", "gpu_xid_errors"}
	result := make([]domain.OperationalMeasurement, 0, len(aggregates))
	for _, name := range order {
		aggregate, ok := aggregates[name]
		if !ok {
			continue
		}
		result = append(result, domain.OperationalMeasurement{
			ReplicaID: options.ReplicaID, Name: aggregate.name, Value: aggregate.value,
			Unit: aggregate.unit, EvidenceClass: "measured", Source: options.Source,
			SampleCount: aggregate.samples, ObservedAt: options.ObservedAt.UTC(),
			ValidUntil: options.ObservedAt.UTC().Add(options.TTL),
		})
	}
	return result, nil
}

func aggregateDCGM(values map[string]*dcgmAggregate, name, unit string, value float64, mode string) {
	current, ok := values[name]
	if !ok {
		values[name] = &dcgmAggregate{name: name, unit: unit, value: value, samples: 1, mode: mode}
		return
	}
	current.samples++
	if current.mode == "sum" {
		current.value += value
	} else if value > current.value {
		current.value = value
	}
}

func parseDCGMLine(line string) (string, map[string]string, float64, bool, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, 0, strings.HasPrefix(line, "DCGM_FI_"), errors.New("malformed DCGM metric line")
	}
	identity := fields[0]
	name := identity
	labels := map[string]string{}
	if start := strings.IndexByte(identity, '{'); start >= 0 {
		if !strings.HasSuffix(identity, "}") {
			return "", nil, 0, strings.HasPrefix(identity, "DCGM_FI_"), errors.New("malformed DCGM label set")
		}
		name = identity[:start]
		var err error
		labels, err = parsePrometheusLabels(identity[start+1 : len(identity)-1])
		if err != nil {
			return name, nil, 0, supportedDCGMMetric(name), err
		}
	}
	relevant := supportedDCGMMetric(name)
	if !relevant {
		return name, labels, 0, false, nil
	}
	value, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return name, labels, 0, true, fmt.Errorf("DCGM metric %s has a non-finite numeric value", name)
	}
	return name, labels, value, true, nil
}

func parsePrometheusLabels(raw string) (map[string]string, error) {
	labels := map[string]string{}
	for len(strings.TrimSpace(raw)) > 0 {
		raw = strings.TrimSpace(raw)
		equals := strings.IndexByte(raw, '=')
		if equals <= 0 {
			return nil, errors.New("malformed Prometheus label")
		}
		key := strings.TrimSpace(raw[:equals])
		raw = strings.TrimSpace(raw[equals+1:])
		if !strings.HasPrefix(raw, `"`) {
			return nil, errors.New("Prometheus label value must be quoted")
		}
		end := 1
		escaped := false
		for ; end < len(raw); end++ {
			if raw[end] == '"' && !escaped {
				break
			}
			if raw[end] == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		if end >= len(raw) {
			return nil, errors.New("unterminated Prometheus label value")
		}
		value, err := strconv.Unquote(raw[:end+1])
		if err != nil {
			return nil, errors.New("invalid Prometheus label escape")
		}
		labels[key] = value
		raw = strings.TrimSpace(raw[end+1:])
		if raw == "" {
			break
		}
		if raw[0] != ',' {
			return nil, errors.New("malformed Prometheus label separator")
		}
		raw = raw[1:]
	}
	return labels, nil
}

func labelsMatch(labels, selector map[string]string) bool {
	for key, expected := range selector {
		if labels[key] != expected {
			return false
		}
	}
	return true
}

func supportedDCGMMetric(name string) bool {
	switch name {
	case "DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_PROF_GR_ENGINE_ACTIVE", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_XID_ERRORS":
		return true
	default:
		return false
	}
}
