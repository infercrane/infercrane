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
	// NVLinkPeakBytesPerSecond and PCIePeakBytesPerSecond are the operator's
	// declared per-GPU, per-direction link peaks. DCGM reports throughput but
	// not link capacity, so saturation stays unknown unless a peak is declared.
	NVLinkPeakBytesPerSecond float64
	PCIePeakBytesPerSecond   float64
}

// Hardware measurement names. GPU profiling (DCP) fields are disabled in many
// exporter configurations; their absence must stay unknown, never zero.
const (
	MeasurementGPUUtilization      = "gpu_utilization"
	MeasurementGPUMemory           = "gpu_memory"
	MeasurementGPUTemperature      = "gpu_temperature"
	MeasurementGPUPower            = "gpu_power"
	MeasurementGPUXIDErrors        = "gpu_xid_errors"
	MeasurementGPUDevices          = "gpu_devices"
	MeasurementGPUSMActive         = "gpu_sm_active"
	MeasurementGPUSMOccupancy      = "gpu_sm_occupancy"
	MeasurementGPUTensorActive     = "gpu_tensor_active"
	MeasurementGPUDRAMActive       = "gpu_dram_active"
	MeasurementGPUPCIeThroughput   = "gpu_pcie_throughput"
	MeasurementGPUNVLinkThroughput = "gpu_nvlink_throughput"
	MeasurementGPUPCIeSaturation   = "gpu_pcie_saturation"
	MeasurementGPUNVLinkSaturation = "gpu_nvlink_saturation"
)

// HardwareMeasurementCatalog is the complete, ordered set of hardware
// measurements and how monitoring combines each across replicas.
var HardwareMeasurementCatalog = []MeasurementSpec{
	{MeasurementGPUUtilization, "percent", AggregateMax},
	{MeasurementGPUMemory, "bytes", AggregateSum},
	{MeasurementGPUTemperature, "celsius", AggregateMax},
	{MeasurementGPUPower, "watts", AggregateSum},
	{MeasurementGPUXIDErrors, "count", AggregateSum},
	{MeasurementGPUDevices, "count", AggregateMax},
	{MeasurementGPUSMActive, "percent", AggregateWeighted},
	{MeasurementGPUSMOccupancy, "percent", AggregateWeighted},
	{MeasurementGPUTensorActive, "percent", AggregateWeighted},
	{MeasurementGPUDRAMActive, "percent", AggregateWeighted},
	{MeasurementGPUPCIeThroughput, "bytes_per_second", AggregateMax},
	{MeasurementGPUNVLinkThroughput, "bytes_per_second", AggregateMax},
	{MeasurementGPUPCIeSaturation, "percent", AggregateMax},
	{MeasurementGPUNVLinkSaturation, "percent", AggregateMax},
}

type dcgmAggregate struct {
	name, unit, mode, metric string
	value                    float64
	samples                  int
}

// dcgmField maps one exporter metric to a measurement. dcgm-exporter 4.9
// renamed most fields (for example DCGM_FI_DEV_GPU_UTIL became
// DCGM_FI_DEV_GPU_UTIL_RATIO and DCGM_FI_PROF_DRAM_ACTIVE became
// DCGM_FI_PROF_DRAM_UTIL_RATIO); both spellings are accepted and converted to
// one canonical unit.
type dcgmField struct {
	measurement, unit, mode string
	// scale converts the exporter value to the canonical unit.
	scale float64
	// ratio marks a 0..1 activity ratio that is validated before scaling.
	ratio bool
}

var dcgmFields = map[string]dcgmField{
	"DCGM_FI_DEV_FB_USED":             {MeasurementGPUMemory, "bytes", "sum", 1024 * 1024, false},
	"DCGM_FI_DEV_FB_USED_BYTES":       {MeasurementGPUMemory, "bytes", "sum", 1, false},
	"DCGM_FI_DEV_GPU_TEMP":            {MeasurementGPUTemperature, "celsius", "max", 1, false},
	"DCGM_FI_DEV_GPU_TEMP_CELSIUS":    {MeasurementGPUTemperature, "celsius", "max", 1, false},
	"DCGM_FI_DEV_POWER_USAGE":         {MeasurementGPUPower, "watts", "sum", 1, false},
	"DCGM_FI_DEV_BOARD_POWER_WATTS":   {MeasurementGPUPower, "watts", "sum", 1, false},
	"DCGM_FI_DEV_XID_ERRORS":          {MeasurementGPUXIDErrors, "count", "sum", 1, false},
	"DCGM_FI_PROF_SM_ACTIVE":          {MeasurementGPUSMActive, "percent", "mean", 100, true},
	"DCGM_FI_PROF_SM_UTIL_RATIO":      {MeasurementGPUSMActive, "percent", "mean", 100, true},
	"DCGM_FI_PROF_SM_OCCUPANCY":       {MeasurementGPUSMOccupancy, "percent", "mean", 100, true},
	"DCGM_FI_PROF_SM_OCCUPANCY_RATIO": {MeasurementGPUSMOccupancy, "percent", "mean", 100, true},
	"DCGM_FI_PROF_PIPE_TENSOR_ACTIVE": {MeasurementGPUTensorActive, "percent", "mean", 100, true},
	"DCGM_FI_PROF_TENSOR_UTIL_RATIO":  {MeasurementGPUTensorActive, "percent", "mean", 100, true},
	"DCGM_FI_PROF_DRAM_ACTIVE":        {MeasurementGPUDRAMActive, "percent", "mean", 100, true},
	"DCGM_FI_PROF_DRAM_UTIL_RATIO":    {MeasurementGPUDRAMActive, "percent", "mean", 100, true},
	"DCGM_FI_PROF_PCIE_TX_BYTES":      {MeasurementGPUPCIeThroughput, "bytes_per_second", "max", 1, false},
	"DCGM_FI_PROF_PCIE_RX_BYTES":      {MeasurementGPUPCIeThroughput, "bytes_per_second", "max", 1, false},
	"DCGM_FI_PROF_NVLINK_TX_BYTES":    {MeasurementGPUNVLinkThroughput, "bytes_per_second", "max", 1, false},
	"DCGM_FI_PROF_NVLINK_RX_BYTES":    {MeasurementGPUNVLinkThroughput, "bytes_per_second", "max", 1, false},
}

// Utilization fields are handled separately: the device counter wins over the
// graphics-engine fallback, and the legacy name honors the declared unit.
var dcgmUtilizationFields = map[string]bool{
	"DCGM_FI_DEV_GPU_UTIL": true, "DCGM_FI_DEV_GPU_UTIL_RATIO": true,
	"DCGM_FI_PROF_GR_ENGINE_ACTIVE": false, "DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO": false,
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
	for _, peak := range []float64{options.NVLinkPeakBytesPerSecond, options.PCIePeakBytesPerSecond} {
		if peak < 0 || math.IsNaN(peak) || math.IsInf(peak, 0) {
			return nil, errors.New("declared link peaks must be finite and not negative")
		}
	}

	aggregates := map[string]*dcgmAggregate{}
	devices := map[string]struct{}{}
	identified := true
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
		if device, ok := gpuIdentity(labels); ok {
			devices[device] = struct{}{}
		} else {
			identified = false
		}
		if device, isUtilization := dcgmUtilizationFields[name]; isUtilization {
			if !device && devUtilObserved {
				continue
			}
			if name == "DCGM_FI_DEV_GPU_UTIL" && options.UtilizationUnit == "percent" {
				value /= 100
			}
			if value < 0 || value > 1 {
				return nil, fmt.Errorf("DCGM %s %.4g is outside its utilization range", name, value)
			}
			if device && !devUtilObserved {
				delete(aggregates, MeasurementGPUUtilization)
				devUtilObserved = true
			}
			aggregateDCGM(aggregates, name, MeasurementGPUUtilization, "percent", value*100, "max")
			continue
		}
		field := dcgmFields[name]
		if field.ratio && (value < 0 || value > 1) {
			return nil, fmt.Errorf("DCGM %s %.4g is outside the 0..1 ratio range", name, value)
		}
		if value < 0 && field.measurement != MeasurementGPUTemperature {
			return nil, fmt.Errorf("DCGM %s cannot be negative", name)
		}
		aggregateDCGM(aggregates, name, field.measurement, field.unit, value*field.scale, field.mode)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan DCGM payload: %w", err)
	}
	if len(aggregates) == 0 {
		return nil, errors.New("DCGM payload contained no selected supported measurements")
	}
	if identified && len(devices) > 0 {
		aggregates[MeasurementGPUDevices] = &dcgmAggregate{name: MeasurementGPUDevices, unit: "count", value: float64(len(devices)), samples: len(devices), mode: "max"}
	}
	for _, link := range []struct {
		throughput, saturation string
		peak                   float64
	}{
		{MeasurementGPUPCIeThroughput, MeasurementGPUPCIeSaturation, options.PCIePeakBytesPerSecond},
		{MeasurementGPUNVLinkThroughput, MeasurementGPUNVLinkSaturation, options.NVLinkPeakBytesPerSecond},
	} {
		throughput, ok := aggregates[link.throughput]
		if !ok || link.peak <= 0 {
			continue
		}
		// PCIe counters include protocol headers, so a busy link can read
		// slightly above its payload peak; saturation is capped at 100%.
		aggregates[link.saturation] = &dcgmAggregate{name: link.saturation, unit: "percent", value: math.Min(100, throughput.value/link.peak*100), samples: throughput.samples, mode: "max"}
	}

	result := make([]domain.OperationalMeasurement, 0, len(aggregates))
	for _, spec := range HardwareMeasurementCatalog {
		aggregate, ok := aggregates[spec.Name]
		if !ok {
			continue
		}
		value := aggregate.value
		if aggregate.mode == "mean" {
			value /= float64(aggregate.samples)
		}
		result = append(result, domain.OperationalMeasurement{
			ReplicaID: options.ReplicaID, Name: aggregate.name, Value: math.Round(value*1e6) / 1e6,
			Unit: aggregate.unit, EvidenceClass: "measured", Source: options.Source,
			SampleCount: aggregate.samples, ObservedAt: options.ObservedAt.UTC(),
			ValidUntil: options.ObservedAt.UTC().Add(options.TTL),
		})
	}
	return result, nil
}

// aggregateDCGM folds one series into its measurement. When an exporter emits
// the same field under both its legacy and its current name, only the first
// spelling seen is counted so the GPU is not double counted.
func aggregateDCGM(values map[string]*dcgmAggregate, metric, name, unit string, value float64, mode string) {
	current, ok := values[name]
	if !ok {
		values[name] = &dcgmAggregate{name: name, unit: unit, value: value, samples: 1, mode: mode, metric: metric}
		return
	}
	if current.metric != metric && !sameDirectionPair(current.metric, metric) {
		return
	}
	current.samples++
	switch current.mode {
	case "sum", "mean":
		current.value += value
	default:
		if value > current.value {
			current.value = value
		}
	}
}

// sameDirectionPair lets the TX and RX series of one link feed the same
// busiest-direction throughput.
func sameDirectionPair(left, right string) bool {
	strip := func(value string) string {
		return strings.NewReplacer("_TX_", "_", "_RX_", "_").Replace(value)
	}
	return strip(left) == strip(right)
}

// gpuIdentity names the physical GPU behind a series so the device count is
// exact. Series without a GPU label leave the count unknown.
func gpuIdentity(labels map[string]string) (string, bool) {
	if uuid := labels["UUID"]; uuid != "" {
		return uuid, true
	}
	if gpu, ok := labels["gpu"]; ok && gpu != "" {
		return labels["Hostname"] + "\x00" + gpu, true
	}
	return "", false
}

func parseDCGMLine(line string) (string, map[string]string, float64, bool, error) {
	// Label values such as modelName="NVIDIA H100 80GB HBM3" contain spaces,
	// so the sample is split with the quote-aware text-format splitter.
	name, rawLabels, rawValue, ok := splitSample(line)
	if !ok {
		return "", nil, 0, strings.HasPrefix(line, "DCGM_FI_"), errors.New("malformed DCGM metric line")
	}
	relevant := supportedDCGMMetric(name)
	labels, err := parsePrometheusLabels(rawLabels)
	if err != nil {
		return name, nil, 0, relevant, err
	}
	if !relevant {
		return name, labels, 0, false, nil
	}
	value, err := strconv.ParseFloat(rawValue, 64)
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
	if _, ok := dcgmUtilizationFields[name]; ok {
		return true
	}
	_, ok := dcgmFields[name]
	return ok
}
