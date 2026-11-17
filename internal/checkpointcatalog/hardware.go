package checkpointcatalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/provideridentity"
)

// CUDAVendor is the hardware vendor of the default CUDA platform. Its GPUs are
// not in the reviewed accelerator table: the catalog names them by
// architecture family, and the policy's runnable_hardware lists the families
// the product runs.
const CUDAVendor = "nvidia"

// cudaArchitectures maps markers in a CUDA GPU name to the architecture family
// the catalog uses.
var cudaArchitectures = []struct {
	family  string
	markers []string
}{
	{"blackwell", []string{"gb200", "gb300", "b200", "b300", "rtxpro6000", "rtx5090"}},
	{"hopper", []string{"gh200", "h100", "h200", "h800", "h20"}},
}

// AcceleratorArchitecture maps a concrete GPU name to the architecture the
// catalog matches builds against: a reviewed accelerator's compiler target
// (gfx942 for MI300X and MI325X, gfx950 for MI350X and MI355X), or the
// family of a CUDA GPU (hopper, blackwell). Other names return "".
func AcceleratorArchitecture(gpu string) string {
	if accelerator, reviewed := provideridentity.LookupAccelerator(gpu); reviewed {
		return accelerator.Target
	}
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(gpu))
	for _, architecture := range cudaArchitectures {
		for _, marker := range architecture.markers {
			if strings.Contains(normalized, marker) {
				return architecture.family
			}
		}
	}
	return ""
}

// RunnableArchitectures returns the architectures of a vendor's hardware that
// AcceleratorArchitecture can match a GPU to: the CUDA vendor's architecture
// families, or the compiler targets of the vendor's reviewed accelerators
// (provideridentity.Accelerators). A vendor with neither returns nil: the
// product cannot run its hardware.
func RunnableArchitectures(vendor string) []string {
	var out []string
	if vendor == CUDAVendor {
		for _, architecture := range cudaArchitectures {
			out = append(out, architecture.family)
		}
		return out
	}
	for _, accelerator := range provideridentity.Accelerators() {
		if accelerator.Vendor == vendor && !slices.Contains(out, accelerator.Target) {
			out = append(out, accelerator.Target)
		}
	}
	return out
}

// RunnableVendor reports whether the product runs any of a vendor's hardware.
func RunnableVendor(vendor string) bool {
	return len(RunnableArchitectures(vendor)) > 0
}

// ResolveArchitectures maps the architectures a reviewed policy names for a
// vendor to the ones entries record and Match compares. For a vendor with
// reviewed accelerators, a name is a compiler target ("gfx942") or the
// architecture of reviewed accelerators ("cdna3" for CDNA 3), which resolves
// to their compiler targets. A name that is neither is kept as written, so
// the target stays unreviewed rather than guessed, and so are other vendors'
// names.
func ResolveArchitectures(vendor string, architectures []string) []string {
	out := []string{}
	add := func(value string) {
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	accelerators := provideridentity.Accelerators()
	for _, name := range architectures {
		key, resolved := architectureKey(name), false
		for _, accelerator := range accelerators {
			if accelerator.Vendor == vendor && (key == architectureKey(accelerator.Target) || key == architectureKey(accelerator.Architecture)) {
				add(accelerator.Target)
				resolved = true
			}
		}
		if !resolved {
			add(name)
		}
	}
	return out
}

func architectureKey(name string) string {
	return strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(name)))
}

// validateRunnable rejects runnable hardware that Match could never pair with
// a GPU: no architectures, or one the vendor's runnable architectures lack.
func validateRunnable(hardware Hardware) error {
	if len(hardware.Architectures) == 0 {
		return errors.New("runnable hardware requires explicit architectures")
	}
	runnable := RunnableArchitectures(hardware.Vendor)
	if len(runnable) == 0 {
		return fmt.Errorf("the product cannot run %s hardware: it has no reviewed accelerators", hardware.Vendor)
	}
	for _, architecture := range hardware.Architectures {
		if !slices.Contains(runnable, architecture) {
			return fmt.Errorf("runnable %s hardware must name one of %s, not %q", hardware.Vendor, strings.Join(runnable, ", "), architecture)
		}
	}
	return nil
}
