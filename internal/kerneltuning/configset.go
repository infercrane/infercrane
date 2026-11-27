package kerneltuning

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
)

// TileConfig is one step size's tile settings for vLLM's Triton fused-MoE
// kernel, exactly the keys its tuned files hold. SplitK is present only when
// the entry is the engine's own default, which carries SPLIT_K 1.
type TileConfig struct {
	BlockSizeM int  `json:"BLOCK_SIZE_M"`
	BlockSizeN int  `json:"BLOCK_SIZE_N"`
	BlockSizeK int  `json:"BLOCK_SIZE_K"`
	GroupSizeM int  `json:"GROUP_SIZE_M"`
	SplitK     *int `json:"SPLIT_K,omitempty"`
	NumWarps   int  `json:"num_warps"`
	NumStages  int  `json:"num_stages"`
}

// Values a tile setting may take: the Triton kernel's own constraints
// (powers of two for block sizes and warps) bounded to what the engine's
// tuner and shipped files use.
var (
	blockSizes = []int{16, 32, 64, 128, 256}
	groupSizes = []int{1, 2, 4, 8, 16, 32, 64}
	warpCounts = []int{1, 2, 4, 8, 16}
)

const (
	minStages = 1
	maxStages = 8
)

var tileKeys = []string{"BLOCK_SIZE_M", "BLOCK_SIZE_N", "BLOCK_SIZE_K", "GROUP_SIZE_M", "SPLIT_K", "num_warps", "num_stages"}

func (config TileConfig) validate(shape FusedMoEShape) error {
	switch {
	case !slices.Contains(blockSizes, config.BlockSizeM) || !slices.Contains(blockSizes, config.BlockSizeN) || !slices.Contains(blockSizes, config.BlockSizeK):
		return fmt.Errorf("block sizes must each be one of %v", blockSizes)
	case !slices.Contains(groupSizes, config.GroupSizeM):
		return fmt.Errorf("GROUP_SIZE_M must be one of %v", groupSizes)
	case !slices.Contains(warpCounts, config.NumWarps):
		return fmt.Errorf("num_warps must be one of %v", warpCounts)
	case config.NumStages < minStages || config.NumStages > maxStages:
		return fmt.Errorf("num_stages must be from %d to %d", minStages, maxStages)
	case config.SplitK != nil && *config.SplitK != 1:
		return errors.New("SPLIT_K must be 1: the fused-MoE kernel does not split K")
	}
	// A tile must line up with the quantization block's scale grid: the
	// tile and the block divide one another (benchmark_moe.py's filter).
	if shape.BlockN > 0 && shape.BlockK > 0 {
		aligned := func(tile, block int) bool { return tile%block == 0 || block%tile == 0 }
		if !aligned(config.BlockSizeN, shape.BlockN) || !aligned(config.BlockSizeK, shape.BlockK) {
			return fmt.Errorf("tile %dx%d does not line up with the %dx%d quantization block", config.BlockSizeN, config.BlockSizeK, shape.BlockN, shape.BlockK)
		}
	}
	return nil
}

// CanonicalFile is a file's only accepted byte form: keys in ascending
// numeric order, each entry's settings in the engine's order, no spaces. The
// tuner writes the same bytes, so the digest names what the engine reads.
func CanonicalFile(entries map[int]TileConfig) ([]byte, error) {
	if len(entries) == 0 || len(entries) > MaxKeys {
		return nil, fmt.Errorf("a file holds 1 to %d step sizes", MaxKeys)
	}
	keys := make([]int, 0, len(entries))
	for key := range entries {
		if key < 1 || key > MaxKey {
			return nil, fmt.Errorf("step size %d is outside 1 to %d", key, MaxKey)
		}
		keys = append(keys, key)
	}
	sort.Ints(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			out.WriteByte(',')
		}
		config := entries[key]
		fmt.Fprintf(&out, `"%d":{"BLOCK_SIZE_M":%d,"BLOCK_SIZE_N":%d,"BLOCK_SIZE_K":%d,"GROUP_SIZE_M":%d,`, key, config.BlockSizeM, config.BlockSizeN, config.BlockSizeK, config.GroupSizeM)
		if config.SplitK != nil {
			fmt.Fprintf(&out, `"SPLIT_K":%d,`, *config.SplitK)
		}
		fmt.Fprintf(&out, `"num_warps":%d,"num_stages":%d}`, config.NumWarps, config.NumStages)
	}
	out.WriteByte('}')
	if out.Len() > MaxFileBytes {
		return nil, fmt.Errorf("a file is at most %d bytes", MaxFileBytes)
	}
	return out.Bytes(), nil
}

// ParseFile reads a file's entries, refusing anything but integer step sizes
// mapped to exactly the tile keys, each an integer.
func ParseFile(content []byte, shape FusedMoEShape) (map[int]TileConfig, error) {
	if len(content) == 0 || len(content) > MaxFileBytes {
		return nil, fmt.Errorf("a file is 1 to %d bytes", MaxFileBytes)
	}
	var raw map[string]map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&raw); err != nil {
		return nil, errors.New("a file must be one JSON object of step sizes to tile settings")
	}
	if decoder.More() {
		return nil, errors.New("a file must hold one JSON object")
	}
	if len(raw) == 0 || len(raw) > MaxKeys {
		return nil, fmt.Errorf("a file holds 1 to %d step sizes", MaxKeys)
	}
	entries := make(map[int]TileConfig, len(raw))
	for name, fields := range raw {
		key, err := strconv.Atoi(name)
		if err != nil || strconv.Itoa(key) != name || key < 1 || key > MaxKey {
			return nil, fmt.Errorf("step size %q must be an integer from 1 to %d", boundedText(name), MaxKey)
		}
		values := map[string]int{}
		for field, value := range fields {
			if !slices.Contains(tileKeys, field) {
				return nil, fmt.Errorf("step size %d: %q is not a tile setting", key, boundedText(field))
			}
			var number int
			if json.Unmarshal(value, &number) != nil {
				return nil, fmt.Errorf("step size %d: %s must be an integer", key, field)
			}
			values[field] = number
		}
		for _, field := range tileKeys {
			if _, ok := values[field]; !ok && field != "SPLIT_K" {
				return nil, fmt.Errorf("step size %d: %s is missing", key, field)
			}
		}
		config := TileConfig{BlockSizeM: values["BLOCK_SIZE_M"], BlockSizeN: values["BLOCK_SIZE_N"], BlockSizeK: values["BLOCK_SIZE_K"],
			GroupSizeM: values["GROUP_SIZE_M"], NumWarps: values["num_warps"], NumStages: values["num_stages"]}
		if split, ok := values["SPLIT_K"]; ok {
			config.SplitK = &split
		}
		if err = config.validate(shape); err != nil {
			return nil, fmt.Errorf("step size %d: %w", key, err)
		}
		entries[key] = config
	}
	return entries, nil
}

// NewFile builds one file of a set from its entries.
func NewFile(shape FusedMoEShape, device string, entries map[int]TileConfig) (domain.KernelConfigFile, error) {
	if err := shape.Valid(); err != nil {
		return domain.KernelConfigFile{}, err
	}
	for key, config := range entries {
		if err := config.validate(shape); err != nil {
			return domain.KernelConfigFile{}, fmt.Errorf("step size %d: %w", key, err)
		}
	}
	content, err := CanonicalFile(entries)
	if err != nil {
		return domain.KernelConfigFile{}, err
	}
	sum := sha256.Sum256(content)
	return domain.KernelConfigFile{Name: FileName(shape, device), Content: string(content), SHA256: hex.EncodeToString(sum[:])}, nil
}

// NewSet assembles a set and its digest. Validate it before use.
func NewSet(runtime, version, device string, files []domain.KernelConfigFile) domain.KernelConfigSet {
	set := domain.KernelConfigSet{Operator: OperatorFusedMoE, Runtime: runtime, RuntimeVersion: version, Device: device, Tuner: Tuner, Files: slices.Clone(files)}
	sort.Slice(set.Files, func(a, b int) bool { return set.Files[a].Name < set.Files[b].Name })
	set.Digest = Digest(set)
	return set
}

// Digest names a set: its operator, engine, device, tuner and every file's
// name and sha256, in name order.
func Digest(set domain.KernelConfigSet) string {
	type file struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	files := make([]file, len(set.Files))
	for index, entry := range set.Files {
		files[index] = file{entry.Name, entry.SHA256}
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Name < files[b].Name })
	body, _ := json.Marshal(struct {
		Operator       string `json:"operator"`
		Runtime        string `json:"runtime"`
		RuntimeVersion string `json:"runtime_version"`
		Device         string `json:"device"`
		Tuner          string `json:"tuner"`
		Files          []file `json:"files"`
	}{set.Operator, set.Runtime, set.RuntimeVersion, set.Device, set.Tuner, files})
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var devicePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate admits a set only when every fact in it is one the engine
// version's source was read for: a reviewed engine, the fused-MoE operator,
// file names in the engine's form for the set's device, canonical files of
// tile settings only, and matching digests.
func Validate(set domain.KernelConfigSet) error {
	if set.Operator != OperatorFusedMoE {
		return fmt.Errorf("kernel configs operator must be %s", OperatorFusedMoE)
	}
	engine, ok := lookupEngine(set.Runtime, set.RuntimeVersion)
	if !ok {
		return fmt.Errorf("kernel configs are not reviewed for %s %s", boundedText(set.Runtime), boundedText(set.RuntimeVersion))
	}
	if !devicePattern.MatchString(set.Device) {
		return errors.New("kernel configs device must be the GPU name in the engine's file-name form")
	}
	if set.Tuner != Tuner {
		return fmt.Errorf("kernel configs tuner must be %s", Tuner)
	}
	if len(set.Files) == 0 || len(set.Files) > MaxFiles {
		return fmt.Errorf("kernel configs hold 1 to %d files", MaxFiles)
	}
	seen := map[string]bool{}
	for _, file := range set.Files {
		shape, device, err := ParseFileName(file.Name)
		if err != nil {
			return err
		}
		if device != set.Device {
			return fmt.Errorf("file %s is for device %s, not the set's %s", file.Name, device, set.Device)
		}
		if _, reviewed := engine.boundaries[shape.dtypeClass()]; !reviewed {
			return fmt.Errorf("file %s: %s %s fused-MoE files are reviewed only for %s", file.Name, set.Runtime, set.RuntimeVersion, strings.Join(reviewedClasses(engine), ", "))
		}
		if seen[file.Name] {
			return fmt.Errorf("file %s appears twice", file.Name)
		}
		seen[file.Name] = true
		entries, err := ParseFile([]byte(file.Content), shape)
		if err != nil {
			return fmt.Errorf("file %s: %w", file.Name, err)
		}
		canonical, err := CanonicalFile(entries)
		if err != nil {
			return fmt.Errorf("file %s: %w", file.Name, err)
		}
		if !bytes.Equal(canonical, []byte(file.Content)) {
			return fmt.Errorf("file %s is not in canonical form", file.Name)
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return fmt.Errorf("file %s does not match its sha256", file.Name)
		}
	}
	if Digest(set) != set.Digest {
		return errors.New("kernel configs do not match their digest")
	}
	return nil
}

func reviewedClasses(engine reviewedEngine) []string {
	classes := make([]string, 0, len(engine.boundaries))
	for class := range engine.boundaries {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	return classes
}

// DeviceForGPU is the engine's file-name form of the GPU a revision asks
// for: the name the NVIDIA driver reports for it (as RunPod lists GPUs by
// it), with spaces as underscores. A GPU the table does not know keeps its
// own name and so matches no tuned set.
func DeviceForGPU(gpu string) string {
	return EngineDeviceName(provideridentity.GPUTypeID("runpod", strings.TrimSpace(gpu)))
}

// ValidateForRevision checks a set against the revision that would serve it:
// the same engine and version, and the GPU the set was tuned on.
func ValidateForRevision(set domain.KernelConfigSet, runtime, version, gpu string) error {
	if err := Validate(set); err != nil {
		return err
	}
	if set.Runtime != runtime || set.RuntimeVersion != version {
		return fmt.Errorf("kernel configs were tuned for %s %s, not the revision's %s %s", set.Runtime, set.RuntimeVersion, boundedText(runtime), boundedText(version))
	}
	if device := DeviceForGPU(gpu); device != set.Device {
		return fmt.Errorf("kernel configs were tuned on %s, not the revision's GPU %s (%s)", set.Device, boundedText(gpu), device)
	}
	return nil
}

// ValidateForDeployment checks the optional set a deployment spec or
// revision carries. version may be empty for the engine's default pin. The
// set is reviewed only for the engine's own pinned image, never a custom
// workload, because its file names and tile limits belong to that image's
// kernel.
func ValidateForDeployment(set *domain.KernelConfigSet, runtime, version, gpu string, pinnedImage bool) error {
	if set == nil {
		return nil
	}
	if !pinnedImage {
		return errors.New("kernel configs are reviewed only for the engine's pinned image, not a custom workload")
	}
	if strings.TrimSpace(runtime) == "" {
		runtime = support.DefaultRuntime
	}
	if version = support.NormalizeRuntimeVersion(version); version == "" {
		version = support.CurrentRuntimeVersion(runtime)
	}
	return ValidateForRevision(*set, runtime, version, gpu)
}
