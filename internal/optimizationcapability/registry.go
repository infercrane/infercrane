// Package optimizationcapability owns the versioned, fail-closed boundary
// between provider-neutral optimization intent and runtime-specific launch
// arguments. A runtime supporting a flag does not make a tuple qualified.
package optimizationcapability

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const SchemaV1 = "infercrane.optimization-capability/v1"

type State string

const (
	Registered     State = "registered"
	LocalQualified State = "local-qualified"
	RealQualified  State = "real-qualified"
	Deferred       State = "deferred"
)

type Mechanism string

const (
	ContinuousBatching Mechanism = "continuous-batching"
	PrefixCaching      Mechanism = "prefix-caching"
	ChunkedPrefill     Mechanism = "chunked-prefill"
	AttentionBackend   Mechanism = "attention-backend"
	WeightPrecision    Mechanism = "weight-precision"
	KVCachePrecision   Mechanism = "kv-cache-precision"
	SpeculativeDecode  Mechanism = "speculative-decoding"
	KVReuse            Mechanism = "kv-reuse"
	Disaggregated      Mechanism = "prefill-decode-disaggregation"
)

// Descriptor is one exact compatibility fact. Empty model or accelerator
// lists are forbidden: broad wildcard support would turn upstream capability
// into an unearned InferCrane qualification claim.
type Descriptor struct {
	ID                 string              `json:"id"`
	SchemaVersion      string              `json:"schema_version"`
	Mechanism          Mechanism           `json:"mechanism"`
	Runtime            string              `json:"runtime"`
	RuntimeVersion     string              `json:"runtime_version"`
	Models             []string            `json:"models"`
	ArtifactPrecisions []string            `json:"artifact_precisions"`
	Accelerators       []string            `json:"accelerators"`
	Parameters         map[string][]string `json:"parameters,omitempty"`
	Compiler           string              `json:"compiler"`
	State              State               `json:"state"`
	Evidence           string              `json:"evidence"`
	Upstream           string              `json:"upstream"`
	License            string              `json:"license"`
	Limitations        []string            `json:"limitations,omitempty"`
}

type Request struct {
	Mechanism         Mechanism         `json:"mechanism"`
	Runtime           string            `json:"runtime"`
	RuntimeVersion    string            `json:"runtime_version"`
	Model             string            `json:"model"`
	ArtifactPrecision string            `json:"artifact_precision"`
	Accelerator       string            `json:"accelerator"`
	Parameters        map[string]string `json:"parameters,omitempty"`
}

type Compiled struct {
	DescriptorID string   `json:"descriptor_id"`
	State        State    `json:"state"`
	Arguments    []string `json:"arguments,omitempty"`
	Evidence     string   `json:"evidence"`
	Limitations  []string `json:"limitations,omitempty"`
}

type Registry struct{ descriptors []Descriptor }

func New(descriptors ...Descriptor) (Registry, error) {
	seen := map[string]struct{}{}
	for index := range descriptors {
		descriptors[index] = normalize(descriptors[index])
		if err := validateDescriptor(descriptors[index]); err != nil {
			return Registry{}, fmt.Errorf("descriptor %d: %w", index, err)
		}
		if _, duplicate := seen[descriptors[index].ID]; duplicate {
			return Registry{}, fmt.Errorf("duplicate capability descriptor %q", descriptors[index].ID)
		}
		seen[descriptors[index].ID] = struct{}{}
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].ID < descriptors[j].ID })
	return Registry{descriptors: append([]Descriptor(nil), descriptors...)}, nil
}

func (r Registry) Compile(request Request) (Compiled, error) {
	request.Runtime = strings.ToLower(strings.TrimSpace(request.Runtime))
	request.RuntimeVersion = strings.TrimSpace(request.RuntimeVersion)
	request.Model = strings.TrimSpace(request.Model)
	request.ArtifactPrecision = strings.ToLower(strings.TrimSpace(request.ArtifactPrecision))
	if request.ArtifactPrecision == "" {
		request.ArtifactPrecision = "bf16"
	}
	request.Accelerator = normalizeAccelerator(request.Accelerator)
	if request.Mechanism == "" || request.Runtime == "" || request.RuntimeVersion == "" || request.Model == "" || request.Accelerator == "" {
		return Compiled{}, errors.New("mechanism, exact runtime/version, model, and accelerator are required")
	}
	var matches []Descriptor
	for _, descriptor := range r.descriptors {
		if descriptor.Mechanism == request.Mechanism && descriptor.Runtime == request.Runtime && descriptor.RuntimeVersion == request.RuntimeVersion && contains(descriptor.Models, request.Model) && contains(descriptor.ArtifactPrecisions, request.ArtifactPrecision) && containsFold(descriptor.Accelerators, request.Accelerator) {
			matches = append(matches, descriptor)
		}
	}
	if len(matches) == 0 {
		return Compiled{}, fmt.Errorf("no capability descriptor matches %s %s %s %s %s", request.Mechanism, request.Runtime, request.RuntimeVersion, request.Model, request.Accelerator)
	}
	if len(matches) != 1 {
		return Compiled{}, errors.New("ambiguous capability descriptors; registry must select exactly one compiler")
	}
	descriptor := matches[0]
	if descriptor.State != LocalQualified && descriptor.State != RealQualified {
		return Compiled{}, fmt.Errorf("capability %s is %s and cannot produce executable configuration", descriptor.ID, descriptor.State)
	}
	arguments, err := compile(descriptor, request.Parameters)
	if err != nil {
		return Compiled{}, fmt.Errorf("compile %s: %w", descriptor.ID, err)
	}
	return Compiled{DescriptorID: descriptor.ID, State: descriptor.State, Arguments: arguments, Evidence: descriptor.Evidence, Limitations: append([]string(nil), descriptor.Limitations...)}, nil
}

func compile(descriptor Descriptor, parameters map[string]string) ([]string, error) {
	for name := range parameters {
		if _, ok := descriptor.Parameters[name]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	switch descriptor.Compiler {
	case "runtime-owned":
		if len(parameters) != 0 {
			return nil, errors.New("runtime-owned capability accepts no parameters")
		}
		return nil, nil
	case "vllm-prefix-cache":
		if len(parameters) != 0 {
			return nil, errors.New("prefix caching accepts no parameters")
		}
		return []string{"--enable-prefix-caching"}, nil
	case "vllm-batch-token-budget":
		value := strings.TrimSpace(parameters["max_num_batched_tokens"])
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 256 || parsed > 1_048_576 {
			return nil, errors.New("max_num_batched_tokens must be an integer between 256 and 1048576")
		}
		return []string{"--max-num-batched-tokens", strconv.Itoa(parsed)}, nil
	default:
		return nil, fmt.Errorf("unknown compiler %q", descriptor.Compiler)
	}
}

func normalize(descriptor Descriptor) Descriptor {
	descriptor.ID = strings.TrimSpace(descriptor.ID)
	if descriptor.SchemaVersion == "" {
		descriptor.SchemaVersion = SchemaV1
	}
	descriptor.Runtime = strings.ToLower(strings.TrimSpace(descriptor.Runtime))
	descriptor.RuntimeVersion = strings.TrimSpace(descriptor.RuntimeVersion)
	descriptor.Compiler = strings.TrimSpace(descriptor.Compiler)
	descriptor.Evidence = strings.TrimSpace(descriptor.Evidence)
	descriptor.Upstream = strings.TrimSpace(descriptor.Upstream)
	descriptor.License = strings.TrimSpace(descriptor.License)
	descriptor.Models = normalizedSet(descriptor.Models, false)
	descriptor.ArtifactPrecisions = normalizedSet(descriptor.ArtifactPrecisions, true)
	descriptor.Accelerators = normalizedAccelerators(descriptor.Accelerators)
	return descriptor
}

func normalizedAccelerators(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = normalizeAccelerator(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// Provider resource labels are not hardware identities. Normalize only
// explicit NVIDIA family aliases; a generic kubernetes resource such as
// nvidia.com/gpu remains unknown and cannot satisfy an exact tuple.
func normalizeAccelerator(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	for _, prefix := range []string{"NVIDIA-", "NVIDIA_"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	return value
}

func validateDescriptor(descriptor Descriptor) error {
	if descriptor.ID == "" || descriptor.SchemaVersion != SchemaV1 || descriptor.Mechanism == "" || descriptor.Runtime == "" || descriptor.RuntimeVersion == "" || descriptor.Compiler == "" || descriptor.Evidence == "" || descriptor.Upstream == "" || descriptor.License == "" {
		return errors.New("complete versioned capability identity, compiler, evidence, upstream, and license are required")
	}
	if len(descriptor.Models) == 0 || len(descriptor.ArtifactPrecisions) == 0 || len(descriptor.Accelerators) == 0 {
		return errors.New("explicit model, artifact precision, and accelerator sets are required")
	}
	if descriptor.State != Registered && descriptor.State != LocalQualified && descriptor.State != RealQualified && descriptor.State != Deferred {
		return errors.New("invalid qualification state")
	}
	for name, values := range descriptor.Parameters {
		if strings.TrimSpace(name) == "" || len(values) == 0 {
			return errors.New("capability parameters require a name and allowed value contract")
		}
	}
	return nil
}

func normalizedSet(values []string, lower bool) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value == "" {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}
