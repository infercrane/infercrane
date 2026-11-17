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

// AMDDeviceMetricsOptions defines the trust boundary for one AMD Device
// Metrics Exporter snapshot, as DCGMOptions does for NVIDIA.
type AMDDeviceMetricsOptions struct {
	Selector   map[string]string
	ReplicaID  string
	Source     string
	ObservedAt time.Time
	TTL        time.Duration
	// MetricPrefix is the exporter's configured MetricsFieldPrefix, such as
	// "amd_". It is empty by default.
	MetricPrefix string
}

// amdFields maps the AMD Device Metrics Exporter (v1.5.3) gauges whose name,
// unit, and meaning were read from its source to InferCrane measurements.
// Fields whose unit or equivalence to a DCGM field is not confirmed are not
// read, so they stay unknown:
//   - gpu_used_vram is documented as MB without saying MiB or 10^6 bytes;
//   - gpu_umc_activity (memory controller) is not shown to equal DCGM's DRAM
//     active, and GPU_PROF_* counters need profiler mode and are not shown to
//     equal SM, tensor, or DRAM active, so the GPU bottleneck rules never
//     read AMD values.
var amdFields = map[string]dcgmField{
	// Graphics engine activity, 0-100. Like DCGM's GPU utilization it is
	// shown and never diagnosed from.
	"gpu_gfx_activity": {MeasurementGPUUtilization, "percent", "max", 1, false},
	// Socket power on MI300-series; gpu_power_usage elsewhere. The first name
	// seen is counted so one GPU is never summed twice.
	"gpu_package_power": {MeasurementGPUPower, "watts", "sum", 1, false},
	"gpu_power_usage":   {MeasurementGPUPower, "watts", "sum", 1, false},
	// The hotspot (junction) sensor, MI300-series.
	"gpu_junction_temperature": {MeasurementGPUTemperature, "celsius", "max", 1, false},
}

// ParseAMDDeviceMetrics converts the bounded subset of AMD Device Metrics
// Exporter gauges InferCrane reads. Like ParseDCGM it never exposes other
// metrics or labels.
func ParseAMDDeviceMetrics(text string, options AMDDeviceMetricsOptions) ([]domain.OperationalMeasurement, error) {
	if len(text) > maxDCGMPayloadBytes {
		return nil, errors.New("AMD device metrics payload exceeds 8 MiB")
	}
	if options.Source == "" {
		options.Source = "amd_device_metrics_exporter"
	}
	if options.ObservedAt.IsZero() {
		options.ObservedAt = time.Now().UTC()
	}
	if options.TTL <= 0 || options.TTL > 24*time.Hour {
		return nil, errors.New("AMD device metrics evidence TTL must be greater than zero and at most 24 hours")
	}
	prefix := strings.TrimSpace(options.MetricPrefix)
	aggregates := map[string]*dcgmAggregate{}
	devices := map[string]struct{}{}
	identified, series := true, 0
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), maxDCGMPayloadBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawLabels, rawValue, ok := splitSample(line)
		if !ok {
			for field := range amdFields {
				if strings.HasPrefix(line, prefix+field) {
					return nil, errors.New("malformed AMD device metrics line")
				}
			}
			continue
		}
		field, relevant := amdFields[strings.TrimPrefix(name, prefix)]
		if !relevant || !strings.HasPrefix(name, prefix) {
			continue
		}
		labels, err := parsePrometheusLabels(rawLabels)
		if err != nil {
			return nil, err
		}
		if !labelsMatch(labels, options.Selector) {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("AMD metric %s has a non-finite numeric value", name)
		}
		series++
		if series > maxDCGMSeries {
			return nil, errors.New("AMD device metrics payload contains more than 4096 selected series")
		}
		if device, ok := amdGPUIdentity(labels); ok {
			devices[device] = struct{}{}
		} else {
			identified = false
		}
		if field.measurement == MeasurementGPUUtilization && (value < 0 || value > 100) {
			return nil, fmt.Errorf("AMD %s %.4g is outside 0..100", name, value)
		}
		if value < 0 && field.measurement != MeasurementGPUTemperature {
			return nil, fmt.Errorf("AMD %s cannot be negative", name)
		}
		aggregateDCGM(aggregates, name, field.measurement, field.unit, value*field.scale, field.mode)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan AMD device metrics payload: %w", err)
	}
	if len(aggregates) == 0 {
		return nil, errors.New("AMD device metrics payload contained no selected supported measurements")
	}
	if identified && len(devices) > 0 {
		aggregates[MeasurementGPUDevices] = &dcgmAggregate{name: MeasurementGPUDevices, unit: "count", value: float64(len(devices)), samples: len(devices), mode: "max"}
	}
	result := make([]domain.OperationalMeasurement, 0, len(aggregates))
	for _, spec := range HardwareMeasurementCatalog {
		aggregate, ok := aggregates[spec.Name]
		if !ok {
			continue
		}
		result = append(result, domain.OperationalMeasurement{
			ReplicaID: options.ReplicaID, Name: aggregate.name, Value: math.Round(aggregate.value*1e6) / 1e6,
			Unit: aggregate.unit, EvidenceClass: "measured", Source: options.Source,
			SampleCount: aggregate.samples, ObservedAt: options.ObservedAt.UTC(),
			ValidUntil: options.ObservedAt.UTC().Add(options.TTL),
		})
	}
	return result, nil
}

// amdGPUIdentity names the physical GPU behind a series. The exporter always
// sets serial_number, gpu_id, and hostname.
func amdGPUIdentity(labels map[string]string) (string, bool) {
	if serial := labels["serial_number"]; serial != "" {
		return serial, true
	}
	if gpu := labels["gpu_id"]; gpu != "" {
		return labels["hostname"] + "\x00" + gpu, true
	}
	return "", false
}
