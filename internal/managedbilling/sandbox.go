package managedbilling

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

const (
	DefaultManagedSandboxActiveHourlyMicrousd = int64(300_000)
	DefaultManagedSandboxVCPU                 = 4
	DefaultManagedSandboxMemoryMiB            = 8_192
	DefaultManagedSandboxWorkspaceGiB         = 40
	DefaultManagedSandboxMaxActive            = 1
	DefaultManagedSandboxMaxRetained          = 10
)

// SandboxPolicy is the server-owned commercial contract for qualified Brezel
// capacity. Browser requests can choose a lifetime, but never a price, size,
// quota, or billing mode.
type SandboxPolicy struct {
	Enabled                               bool
	SupplierHourlyMicrousd                int64
	ActiveHourlyMicrousd                  int64
	WorkspaceStorageMicrousdPerGiBMonth   int64
	VCPU, MemoryMiB, IncludedWorkspaceGiB int
	MaxActive, MaxRetained                int
}

func (p SandboxPolicy) Normalize() SandboxPolicy {
	if p.ActiveHourlyMicrousd == 0 {
		p.ActiveHourlyMicrousd = DefaultManagedSandboxActiveHourlyMicrousd
	}
	if p.VCPU == 0 {
		p.VCPU = DefaultManagedSandboxVCPU
	}
	if p.MemoryMiB == 0 {
		p.MemoryMiB = DefaultManagedSandboxMemoryMiB
	}
	if p.IncludedWorkspaceGiB == 0 {
		p.IncludedWorkspaceGiB = DefaultManagedSandboxWorkspaceGiB
	}
	if p.MaxActive == 0 {
		p.MaxActive = DefaultManagedSandboxMaxActive
	}
	if p.MaxRetained == 0 {
		p.MaxRetained = DefaultManagedSandboxMaxRetained
	}
	return p
}

func (p SandboxPolicy) Validate() error {
	p = p.Normalize()
	if p.ActiveHourlyMicrousd < 1 || p.ActiveHourlyMicrousd > 100_000_000 {
		return errors.New("managed sandbox active hourly price is invalid")
	}
	if p.SupplierHourlyMicrousd < 1 || p.SupplierHourlyMicrousd > p.ActiveHourlyMicrousd {
		return errors.New("managed sandbox supplier hourly cost is invalid")
	}
	if p.WorkspaceStorageMicrousdPerGiBMonth < 0 || p.WorkspaceStorageMicrousdPerGiBMonth > 10_000_000 {
		return errors.New("managed sandbox storage price is invalid")
	}
	if p.VCPU < 1 || p.VCPU > 256 || p.MemoryMiB < 128 || p.MemoryMiB > 1<<20 || p.IncludedWorkspaceGiB < 1 || p.IncludedWorkspaceGiB > 1<<20 {
		return errors.New("managed sandbox size is invalid")
	}
	if p.MaxActive < 1 || p.MaxActive > 10_000 || p.MaxRetained < p.MaxActive || p.MaxRetained > 100_000 {
		return errors.New("managed sandbox quota is invalid")
	}
	return nil
}

func (p SandboxPolicy) Reservation(runtime time.Duration, resourceName string) (domain.ManagedSpendReservation, error) {
	p = p.Normalize()
	if !p.Enabled {
		return domain.ManagedSpendReservation{}, errors.New("managed sandbox billing is disabled")
	}
	if err := p.Validate(); err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	if resourceName == "" || runtime < 30*time.Second || runtime > 30*24*time.Hour {
		return domain.ManagedSpendReservation{}, errors.New("managed sandbox identity or lifetime is invalid")
	}
	reserved, err := DurationCostMicrousd(p.ActiveHourlyMicrousd, runtime)
	if err != nil || reserved < 1 {
		return domain.ManagedSpendReservation{}, errors.New("managed sandbox reservation could not be priced")
	}
	pricing, err := json.Marshal(map[string]any{
		"billing_mode":                             "prepaid_usage",
		"active_hourly_microusd":                   p.ActiveHourlyMicrousd,
		"standby_hourly_microusd":                  0,
		"workspace_storage_microusd_per_gib_month": p.WorkspaceStorageMicrousdPerGiBMonth,
		"included_workspace_gib":                   p.IncludedWorkspaceGiB,
		"max_active":                               p.MaxActive,
		"max_retained":                             p.MaxRetained,
	})
	if err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	return domain.ManagedSpendReservation{
		ResourceType: "sandbox", ResourceName: resourceName, Provider: "brezel",
		SupplierHourlyMicrousd: p.SupplierHourlyMicrousd, RetailHourlyMicrousd: p.ActiveHourlyMicrousd,
		ReservedMicrousd: reserved, RuntimeLimitSeconds: int(runtime / time.Second),
		GrossMarginBPS: int((p.ActiveHourlyMicrousd - p.SupplierHourlyMicrousd) * 10_000 / p.ActiveHourlyMicrousd),
		PricingJSON:    string(pricing),
	}, nil
}
