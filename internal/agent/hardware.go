package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/nodediscovery"
)

// HardwareProbe reads the host inventory. Every field is injectable so tests
// do not depend on the machine that runs them.
type HardwareProbe struct {
	Runner nodediscovery.Runner
	GOOS   string
	GOARCH string
	// SysRoot is the root for /sys reads ("" means "/").
	SysRoot string
}

// Inventory reports accelerators with unknowns explicit. NVIDIA comes from
// nvidia-smi (which reads NVML). Apple Silicon and AMD are reported as
// detected; their telemetry is not collected yet, and that is said, not
// guessed around.
func (p HardwareProbe) Inventory(ctx context.Context) agentprotocol.Hardware {
	goos, goarch := p.GOOS, p.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	runner := p.Runner
	if runner == nil {
		runner = nodediscovery.CommandRunner{}
	}
	out := agentprotocol.Hardware{State: "unavailable", Accelerators: []agentprotocol.Accelerator{}, Limitations: []string{}}
	nvidia, err := nodediscovery.DiscoverLocal(ctx, runner)
	if err == nil && len(nvidia.GPUs) > 0 {
		out.State = "ready"
		groups := map[string]*agentprotocol.Accelerator{}
		order := []string{}
		for _, gpu := range nvidia.GPUs {
			key := gpu.Name + "\x00" + strconv.Itoa(gpu.MemoryTotalMiB) + "\x00" + gpu.DriverVersion
			if group, ok := groups[key]; ok {
				group.Count++
				continue
			}
			memory := gpu.MemoryTotalMiB
			groups[key] = &agentprotocol.Accelerator{Vendor: "nvidia", Name: gpu.Name, Count: 1, MemoryMiB: &memory, Driver: gpu.DriverVersion, Source: "nvidia-smi"}
			order = append(order, key)
		}
		for _, key := range order {
			out.Accelerators = append(out.Accelerators, *groups[key])
		}
	} else if err == nil && nvidia.State == "degraded" {
		out.Limitations = append(out.Limitations, "nvidia-smi is installed but did not return an inventory")
	}
	if apple, ok := p.apple(ctx, runner, goos, goarch); ok {
		out.Accelerators = append(out.Accelerators, apple)
		out.Limitations = append(out.Limitations, "Apple Silicon is reported as detected. InferCrane does not collect Apple GPU telemetry or run optimizations on it yet.")
	}
	if amd, ok := p.amd(ctx, runner, goos); ok {
		out.Accelerators = append(out.Accelerators, amd)
		out.Limitations = append(out.Limitations, "AMD GPUs are reported as detected. InferCrane does not collect AMD GPU telemetry or run optimizations on AMD yet.")
	}
	if out.State != "ready" && len(out.Accelerators) > 0 {
		out.State = "partial"
	}
	if len(out.Accelerators) == 0 {
		out.Limitations = append(out.Limitations, "No supported accelerator was found. Servers on this machine are still observed.")
	}
	out.Limitations = append(out.Limitations, "GPU presence does not prove model fit, runtime compatibility, or performance.")
	return out
}

func (p HardwareProbe) apple(ctx context.Context, runner nodediscovery.Runner, goos, goarch string) (agentprotocol.Accelerator, bool) {
	if goos != "darwin" || goarch != "arm64" {
		return agentprotocol.Accelerator{}, false
	}
	accelerator := agentprotocol.Accelerator{Vendor: "apple", Name: "Apple Silicon GPU", Count: 1, Source: "sysctl", Unknown: []string{"gpu_core_count", "memory_bandwidth", "gpu_usable_memory"}}
	if brand, err := runner.Run(ctx, "sysctl", "-n", "machdep.cpu.brand_string"); err == nil {
		if name := strings.TrimSpace(string(brand)); name != "" && len(name) < 128 {
			accelerator.Name = name + " GPU"
		}
	} else {
		accelerator.Unknown = append(accelerator.Unknown, "name")
	}
	if memory, err := runner.Run(ctx, "sysctl", "-n", "hw.memsize"); err == nil {
		if bytes, parseErr := strconv.ParseInt(strings.TrimSpace(string(memory)), 10, 64); parseErr == nil && bytes > 0 {
			// Unified memory is shared with the CPU; the GPU's usable share
			// is set by macOS and is reported as unknown.
			mib := int(bytes / (1 << 20))
			accelerator.MemoryMiB = &mib
		}
	}
	if accelerator.MemoryMiB == nil {
		accelerator.Unknown = append(accelerator.Unknown, "memory_mib")
	}
	return accelerator, true
}

func (p HardwareProbe) amd(ctx context.Context, runner nodediscovery.Runner, goos string) (agentprotocol.Accelerator, bool) {
	if goos != "linux" {
		return agentprotocol.Accelerator{}, false
	}
	root := p.SysRoot
	if root == "" {
		root = "/"
	}
	cards, _ := filepath.Glob(filepath.Join(root, "sys/class/drm/card[0-9]*/device/vendor"))
	sort.Strings(cards)
	count := 0
	var vram int64
	vramKnown := true
	for _, vendorPath := range cards {
		vendor, err := os.ReadFile(vendorPath)
		if err != nil || strings.TrimSpace(string(vendor)) != "0x1002" {
			continue
		}
		device := filepath.Dir(vendorPath)
		if class, classErr := os.ReadFile(filepath.Join(device, "class")); classErr == nil && !strings.HasPrefix(strings.TrimSpace(string(class)), "0x03") {
			continue
		}
		count++
		if total, readErr := os.ReadFile(filepath.Join(device, "mem_info_vram_total")); readErr == nil {
			if bytes, parseErr := strconv.ParseInt(strings.TrimSpace(string(total)), 10, 64); parseErr == nil && bytes > 0 {
				if vram == 0 || bytes < vram {
					vram = bytes
				}
				continue
			}
		}
		vramKnown = false
	}
	if count == 0 {
		return agentprotocol.Accelerator{}, false
	}
	accelerator := agentprotocol.Accelerator{Vendor: "amd", Name: "AMD GPU", Count: count, Source: "sysfs"}
	if vramKnown && vram > 0 {
		mib := int(vram / (1 << 20))
		accelerator.MemoryMiB = &mib
	} else {
		accelerator.Unknown = append(accelerator.Unknown, "memory_mib")
	}
	if output, err := runner.Run(ctx, "rocm-smi", "--showproductname", "--csv"); err == nil {
		if name := amdProductName(string(output)); name != "" {
			accelerator.Name, accelerator.Source = name, "sysfs, rocm-smi"
		}
	}
	if accelerator.Name == "AMD GPU" {
		accelerator.Unknown = append(accelerator.Unknown, "name")
	}
	accelerator.Unknown = append(accelerator.Unknown, "driver")
	return accelerator, true
}

// amdProductName reads the first "Card series" or "Card SKU" value from
// rocm-smi CSV output.
func amdProductName(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return ""
	}
	header := strings.Split(lines[0], ",")
	values := strings.Split(lines[1], ",")
	for _, wanted := range []string{"Card series", "Card SKU", "Card model"} {
		for index, column := range header {
			if strings.EqualFold(strings.TrimSpace(column), wanted) && index < len(values) {
				if value := strings.TrimSpace(values[index]); value != "" && len(value) < 128 {
					return value
				}
			}
		}
	}
	return ""
}
