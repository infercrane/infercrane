package continualoptimizer

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
)

// PriceChangeThreshold is the relative move in the setup's hourly price that
// counts as a price change.
const PriceChangeThreshold = .10

// ModelRevisionCheckInterval bounds how often a check asks the model host for
// the newest revision of a pinned model.
const ModelRevisionCheckInterval = 24 * time.Hour

// Observed is what the last check of one endpoint saw. It is persisted with
// the endpoint's check state so that every change is acted on once: a
// trigger's key is recorded here after the check that handled it.
type Observed struct {
	ScheduledAt         *time.Time `json:"scheduled_at,omitempty"`
	EngineRelease       string     `json:"engine_release,omitempty"`
	ModelRevision       string     `json:"model_revision,omitempty"`
	ModelCheckedAt      *time.Time `json:"model_checked_at,omitempty"`
	HourlyUSDPerReplica float64    `json:"hourly_usd_per_replica,omitempty"`
	PriceSource         string     `json:"price_source,omitempty"`
}

// Setup is the running revision of an endpoint, as its deployment spec
// records it. Empty fields are unknown.
type Setup struct {
	Model          string
	ModelRevision  string
	Runtime        string
	RuntimeVersion string
	RuntimeImage   string
	GPU            string
}

// ScheduleDue reports whether the scheduled check is due.
func ScheduleDue(observed Observed, policy Policy, now time.Time) bool {
	return observed.ScheduledAt == nil || !now.Before(observed.ScheduledAt.Add(policy.CheckInterval()))
}

// ScheduleTrigger is the weekly (or policy-interval) check.
func ScheduleTrigger(policy Policy, now time.Time) Trigger {
	return Trigger{Kind: TriggerSchedule, Key: now.UTC().Format("2006-01-02"), Detail: fmt.Sprintf("scheduled check (every %s)", humanInterval(policy.CheckInterval()))}
}

// ManualTrigger is a person pressing Check now.
func ManualTrigger(actor string) Trigger {
	detail := "checked on request"
	if actor = strings.TrimSpace(actor); actor != "" && len(actor) <= 128 {
		detail = "checked on request by " + actor
	}
	return Trigger{Kind: TriggerManual, Detail: detail}
}

// PinnedEngine is the engine release InferCrane currently pins for a runtime
// on the setup's GPU platform.
func PinnedEngine(setup Setup) (support.RuntimeImage, bool) {
	runtime := strings.ToLower(strings.TrimSpace(setup.Runtime))
	if runtime == "" {
		runtime = support.DefaultRuntime
	}
	platform := provideridentity.AcceleratorPlatform(setup.GPU)
	for _, image := range support.RuntimeImages() {
		if image.Runtime == runtime && image.Platform == platform {
			return image, true
		}
	}
	return support.RuntimeImage{}, false
}

// EngineReleaseKey names one pinned engine release.
func EngineReleaseKey(image support.RuntimeImage) string {
	return image.Runtime + "@" + image.EngineVersion
}

// DetectEngineRelease fires when InferCrane pins an engine release the
// endpoint does not run yet. The running release is read from the revision's
// engine version, else its image; with neither recorded it is unknown and
// nothing fires.
func DetectEngineRelease(setup Setup, observed Observed) (Trigger, bool) {
	pinned, ok := PinnedEngine(setup)
	if !ok {
		return Trigger{}, false
	}
	key := EngineReleaseKey(pinned)
	if observed.EngineRelease == key {
		return Trigger{}, false
	}
	running := strings.TrimPrefix(strings.TrimSpace(setup.RuntimeVersion), "v")
	switch {
	case running != "":
		if running == pinned.EngineVersion {
			return Trigger{}, false
		}
	case strings.TrimSpace(setup.RuntimeImage) != "":
		if setup.RuntimeImage == pinned.Image {
			return Trigger{}, false
		}
		running = "an earlier image"
	default:
		return Trigger{}, false
	}
	return Trigger{Kind: TriggerEngineRelease, Key: key, Detail: fmt.Sprintf("%s %s is the new pinned engine release; this endpoint runs %s", pinned.Runtime, pinned.EngineVersion, running)}, true
}

// ModelRevisionKey names one model revision.
func ModelRevisionKey(model, revision string) string {
	return model + "@" + revision
}

// DetectModelRevision fires when the model host's newest revision differs
// from the one the endpoint pins. An unpinned model or an unknown newest
// revision fires nothing.
func DetectModelRevision(setup Setup, latest string, observed Observed) (Trigger, bool) {
	pinned, latest := strings.TrimSpace(setup.ModelRevision), strings.TrimSpace(latest)
	if setup.Model == "" || pinned == "" || latest == "" || strings.EqualFold(pinned, latest) {
		return Trigger{}, false
	}
	key := ModelRevisionKey(setup.Model, latest)
	if observed.ModelRevision == key {
		return Trigger{}, false
	}
	return Trigger{Kind: TriggerModelRevision, Key: key, Detail: fmt.Sprintf("%s has a new revision %s; this endpoint runs %s", setup.Model, short(latest), short(pinned))}, true
}

// DetectPriceChange fires when the setup's sourced hourly price moved by at
// least PriceChangeThreshold since the last check. An unknown price on
// either side fires nothing.
func DetectPriceChange(previous, current float64) (Trigger, bool) {
	if previous <= 0 || current <= 0 || math.IsNaN(previous) || math.IsNaN(current) {
		return Trigger{}, false
	}
	change := (current - previous) / previous
	if math.Abs(change) < PriceChangeThreshold {
		return Trigger{}, false
	}
	return Trigger{Kind: TriggerPriceChange, Key: fmt.Sprintf("%.4f", current), Detail: fmt.Sprintf("the hourly GPU price for this setup moved %+.0f%% (from $%.2f to $%.2f per replica)", change*100, previous, current)}, true
}

func short(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func humanInterval(interval time.Duration) string {
	switch {
	case interval%(7*24*time.Hour) == 0:
		weeks := int(interval / (7 * 24 * time.Hour))
		if weeks == 1 {
			return "week"
		}
		return fmt.Sprintf("%d weeks", weeks)
	case interval%(24*time.Hour) == 0:
		days := int(interval / (24 * time.Hour))
		if days == 1 {
			return "day"
		}
		return fmt.Sprintf("%d days", days)
	default:
		return interval.String()
	}
}
