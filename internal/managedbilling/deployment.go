package managedbilling

import (
	"errors"
	"math"
	"time"
)

const (
	DefaultManagedDeploymentGrossMarginBPS         = 4_000
	DefaultManagedDeploymentRuntime                = time.Hour
	DefaultManagedDeploymentCleanupAllowance       = 10 * time.Minute
	MaximumManagedDeploymentRuntime                = 24 * time.Hour
	minimumManagedDeploymentRuntime                = 15 * time.Minute
	microusdPerUSD                           int64 = 1_000_000
)

// DeploymentPolicy is the server-side commercial boundary for InferCrane
// Cloud. It is never accepted from a browser request.
type DeploymentPolicy struct {
	Enabled          bool
	Provider         string
	GrossMarginBPS   int
	CleanupAllowance time.Duration
}

func (p DeploymentPolicy) Normalize() DeploymentPolicy {
	if p.Provider == "" {
		p.Provider = "runpod"
	}
	if p.GrossMarginBPS == 0 {
		p.GrossMarginBPS = DefaultManagedDeploymentGrossMarginBPS
	}
	if p.CleanupAllowance == 0 {
		p.CleanupAllowance = DefaultManagedDeploymentCleanupAllowance
	}
	return p
}

func (p DeploymentPolicy) Validate() error {
	p = p.Normalize()
	if p.GrossMarginBPS < 0 || p.GrossMarginBPS >= 10_000 {
		return errors.New("managed deployment gross margin must be between 0 and 9999 basis points")
	}
	if p.Provider == "" {
		return errors.New("managed deployment provider is required")
	}
	if p.CleanupAllowance < time.Minute || p.CleanupAllowance > time.Hour {
		return errors.New("managed deployment cleanup allowance must be between one minute and one hour")
	}
	return nil
}

type DeploymentQuote struct {
	State                   string    `json:"state"`
	Provider                string    `json:"provider"`
	Region                  string    `json:"region,omitempty"`
	GPU                     string    `json:"gpu"`
	GPUCount                int       `json:"gpu_count"`
	Currency                string    `json:"currency"`
	SupplierHourlyMicrousd  int64     `json:"supplier_hourly_microusd"`
	RetailHourlyMicrousd    int64     `json:"retail_hourly_microusd"`
	ReservedMicrousd        int64     `json:"reserved_microusd"`
	RuntimeLimitSeconds     int       `json:"runtime_limit_seconds"`
	CleanupAllowanceSeconds int       `json:"cleanup_allowance_seconds"`
	GrossMarginBPS          int       `json:"gross_margin_bps"`
	Source                  string    `json:"source"`
	ObservedAt              time.Time `json:"observed_at"`
	ValidUntil              time.Time `json:"valid_until"`
}

// QuoteDeployment turns a current complete supplier rate into an integer,
// prepaid retail authorization. The cleanup allowance is held but is charged
// only when provider cleanup actually takes time.
func QuoteDeployment(policy DeploymentPolicy, provider, region, gpu, source string, gpuCount int, supplierHourlyUSD float64, runtime time.Duration, observedAt, validUntil time.Time) (DeploymentQuote, error) {
	policy = policy.Normalize()
	if err := policy.Validate(); err != nil {
		return DeploymentQuote{}, err
	}
	if !policy.Enabled {
		return DeploymentQuote{}, errors.New("managed deployments are disabled")
	}
	if provider != policy.Provider || gpu == "" || gpuCount != 1 || source == "" || observedAt.IsZero() || validUntil.IsZero() || !validUntil.After(time.Now().UTC()) {
		return DeploymentQuote{}, errors.New("a current exact managed deployment price is required")
	}
	if runtime < minimumManagedDeploymentRuntime || runtime > MaximumManagedDeploymentRuntime || runtime%time.Minute != 0 {
		return DeploymentQuote{}, errors.New("managed runtime must be whole minutes between 15 minutes and 24 hours")
	}
	if math.IsNaN(supplierHourlyUSD) || math.IsInf(supplierHourlyUSD, 0) || supplierHourlyUSD <= 0 || supplierHourlyUSD > float64(math.MaxInt64)/float64(microusdPerUSD) {
		return DeploymentQuote{}, errors.New("supplier hourly price is invalid")
	}
	supplierHourlyMicrousd := int64(math.Ceil(supplierHourlyUSD * float64(microusdPerUSD)))
	retailHourlyMicrousd, err := MinimumRetailPriceMicrousd(supplierHourlyMicrousd, policy.GrossMarginBPS)
	if err != nil {
		return DeploymentQuote{}, err
	}
	reservedMicrousd, err := DurationCostMicrousd(retailHourlyMicrousd, runtime+policy.CleanupAllowance)
	if err != nil {
		return DeploymentQuote{}, err
	}
	return DeploymentQuote{
		State:                   "current",
		Provider:                provider,
		Region:                  region,
		GPU:                     gpu,
		GPUCount:                gpuCount,
		Currency:                "USD",
		SupplierHourlyMicrousd:  supplierHourlyMicrousd,
		RetailHourlyMicrousd:    retailHourlyMicrousd,
		ReservedMicrousd:        reservedMicrousd,
		RuntimeLimitSeconds:     int(runtime / time.Second),
		CleanupAllowanceSeconds: int(policy.CleanupAllowance / time.Second),
		GrossMarginBPS:          policy.GrossMarginBPS,
		Source:                  source,
		ObservedAt:              observedAt.UTC(),
		ValidUntil:              validUntil.UTC(),
	}, nil
}

// DurationCostMicrousd rounds up to the nearest micro-dollar. Authorization
// and settlement therefore never under-collect because of truncation.
func DurationCostMicrousd(hourlyMicrousd int64, duration time.Duration) (int64, error) {
	if hourlyMicrousd < 0 || duration < 0 {
		return 0, errors.New("hourly price and duration must be non-negative")
	}
	if hourlyMicrousd == 0 || duration == 0 {
		return 0, nil
	}
	seconds := int64(math.Ceil(duration.Seconds()))
	if seconds > (math.MaxInt64-3_599)/hourlyMicrousd {
		return 0, errors.New("duration cost overflows int64")
	}
	return (hourlyMicrousd*seconds + 3_599) / 3_600, nil
}
