// Package specswitch serves one model from several engines on one GPU, one
// engine per speculation setup, and keeps awake the engine whose setup suits
// the present load.
//
// Speculation that pays at a light load can cost more than it gives at a heavy
// one. On a model with recurrent-state layers each draft token keeps another
// copy of the state for every running request, so the draft length decides how
// many requests fit in the cache at once. vLLM and SGLang can lower the draft
// length with the batch size inside one engine, but both keep the memory
// reserved for the longest draft length they may use, so neither frees that
// room. The switch does: each engine is started with its own draft length and
// its own cache, all but one sleep (weights in host memory, cache discarded),
// and the switch swaps the awake engine when the load has moved to another band
// and stayed there.
//
// The switch is the server's own front: it holds no credentials, reads no
// database and makes no routing decision beyond this one server. Every engine
// listens on the loopback interface only.
package specswitch

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Limits on what a configuration may hold. They bound the switch's queue,
// memory and process count whatever the configuration file says.
const (
	MaxEngines      = 4
	MaxQueue        = 4096
	MaxRoom         = 4096
	MaxCommandArgs  = 128
	MaxArgBytes     = 4096
	MaxRequestBytes = 16 << 20
	// MaxQueuedBytes bounds the request bodies the switch holds while
	// requests wait for room.
	MaxQueuedBytes = 512 << 20
)

// Config is one switch: its engines, the load bands that choose between them,
// and how long a new band must hold before a swap.
type Config struct {
	// Listen is the serving address, for example ":8000".
	Listen string `json:"listen"`
	// AdminListen is an optional loopback address for the state and policy
	// endpoints. Empty disables them.
	AdminListen string   `json:"admin_listen,omitempty"`
	Engines     []Engine `json:"engines"`
	// Policies maps a policy name to its bands; Policy names the one in force.
	// A policy is chosen per serving goal: the bands that maximize all output
	// differ from those that keep first tokens within a limit.
	Policies map[string][]Band `json:"policies"`
	Policy   string            `json:"policy"`
	Timing   Timing            `json:"timing"`
	// QueueLimit bounds the requests that wait in the switch for room.
	QueueLimit int `json:"queue_limit"`
	// Memory sizes the GPU memory share of engines whose command holds
	// MemoryShareToken.
	Memory Memory `json:"memory,omitzero"`
}

// MemoryShareToken in an engine's command is replaced, before the engine
// starts, by the share of GPU memory it may take: everything the sleeping
// engines do not keep, less a margin. An engine left to size its cache from
// all free memory (vLLM 0.34.0's default) could not wake beside the others.
const MemoryShareToken = "{memory_share}"

// Memory says how the command measures what a sleeping engine keeps.
type Memory struct {
	// Probe is the argv that prints the GPU's used and total memory in MiB
	// as "used, total", for example
	// nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader,nounits.
	Probe []string `json:"probe,omitempty"`
	// MarginMiB is left free beyond what the sleeping engines keep.
	MarginMiB float64 `json:"margin_mib,omitempty"`
	// MaxShare caps the share an engine is given.
	MaxShare float64 `json:"max_share,omitempty"`
}

// sized says whether the engine's command takes the computed share.
func (e Engine) sized() bool {
	for _, arg := range e.Command {
		if strings.Contains(arg, MemoryShareToken) {
			return true
		}
	}
	return false
}

// StartOrder is the order the command starts engines in: those with a share
// of their own first, then those sized from what the first ones keep asleep,
// each in configuration order. The last one started stays awake; when it is
// not the first band's engine, the command swaps to that one before serving.
func StartOrder(cfg Config) []Engine {
	var own, sized []Engine
	for _, engine := range cfg.Engines {
		if engine.sized() {
			sized = append(sized, engine)
		} else {
			own = append(own, engine)
		}
	}
	return append(own, sized...)
}

// Engine is one engine process the switch fronts.
type Engine struct {
	// Name identifies the engine in metrics and logs, for example "drafts-3".
	Name string `json:"name"`
	// Drafts is the engine's speculation draft length, 0 for none.
	Drafts int `json:"drafts"`
	// URL is the engine's loopback base URL, for example http://127.0.0.1:8101.
	URL string `json:"url"`
	// Room is the most requests the switch sends this engine at once: the
	// requests its cache holds. Requests beyond it wait in the switch, where a
	// swap need not drain them. Zero sends every request at once.
	Room int `json:"room"`
	// Command is the argv that starts the engine; only the command uses it.
	Command []string `json:"command,omitempty"`
	// Env holds extra environment variables for the engine process.
	Env map[string]string `json:"env,omitempty"`
}

// Band is one band of load and the engine that serves it.
type Band struct {
	// UpTo is the band's largest load, in requests at once on this server
	// (running plus waiting in the switch). Zero marks the last band, above
	// every other.
	UpTo   int    `json:"up_to,omitempty"`
	Engine string `json:"engine"`
}

// Timing sets how the switch reads the load and when it swaps.
type Timing struct {
	// Sample is how often the load is read.
	Sample Duration `json:"sample"`
	// Smooth is the window whose highest reading is the load: requests that
	// end and start again in a closed loop leave short dips the band ignores.
	Smooth Duration `json:"smooth"`
	// RaiseAfter is how long a band above the awake engine's must hold.
	RaiseAfter Duration `json:"raise_after"`
	// LowerAfter is how long a band below it must hold; longer than
	// RaiseAfter, because a pause between bursts is not a lighter load.
	LowerAfter Duration `json:"lower_after"`
	// MinDwell is the least time an engine stays awake before a lower band
	// may replace it, and the wait after a swap that failed. A raise is never
	// held by it.
	MinDwell Duration `json:"min_dwell"`
	// DrainTimeout bounds the wait for requests in flight before a swap; when
	// it passes, the swap is called off and the awake engine keeps serving.
	DrainTimeout Duration `json:"drain_timeout"`
	// WakeTimeout bounds putting one engine to sleep and waking another.
	WakeTimeout Duration `json:"wake_timeout"`
}

// Duration is a time.Duration written as a Go duration string ("3s").
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// D returns the duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// DefaultTiming suits short and medium answers: a band must hold 3 seconds to
// raise and 2 minutes to lower, an engine stays awake at least a minute before
// a lower, and a swap waits up to a minute for answers in flight. A swap holds new requests
// for its whole length, so it is made quickly where staying costs every
// request (a light setup under a heavy load queues them all) and slowly where
// staying costs only some speed.
func DefaultTiming() Timing {
	return Timing{
		Sample:       Duration(250 * time.Millisecond),
		Smooth:       Duration(2 * time.Second),
		RaiseAfter:   Duration(3 * time.Second),
		LowerAfter:   Duration(2 * time.Minute),
		MinDwell:     Duration(time.Minute),
		DrainTimeout: Duration(60 * time.Second),
		WakeTimeout:  Duration(2 * time.Minute),
	}
}

// withDefaults fills unset timings: a zero value takes the default.
func (t Timing) withDefaults() Timing {
	defaults := DefaultTiming()
	for _, pair := range []struct{ value, fallback *Duration }{
		{&t.Sample, &defaults.Sample}, {&t.Smooth, &defaults.Smooth}, {&t.RaiseAfter, &defaults.RaiseAfter},
		{&t.LowerAfter, &defaults.LowerAfter}, {&t.MinDwell, &defaults.MinDwell}, {&t.DrainTimeout, &defaults.DrainTimeout},
		{&t.WakeTimeout, &defaults.WakeTimeout},
	} {
		if *pair.value <= 0 {
			*pair.value = *pair.fallback
		}
	}
	return t
}

// Normalize fills defaults and validates the configuration.
func (c Config) Normalize() (Config, error) {
	c.Timing = c.Timing.withDefaults()
	if c.QueueLimit == 0 {
		c.QueueLimit = 1024
	}
	return c, c.Validate()
}

// Validate rejects a configuration the switch cannot run safely.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen is required")
	}
	if c.AdminListen != "" && !loopbackAddress(c.AdminListen) {
		return errors.New("admin_listen must be a loopback address")
	}
	if len(c.Engines) < 2 || len(c.Engines) > MaxEngines {
		return fmt.Errorf("a switch needs 2 to %d engines", MaxEngines)
	}
	if c.QueueLimit < 1 || c.QueueLimit > MaxQueue {
		return fmt.Errorf("queue_limit must be between 1 and %d", MaxQueue)
	}
	names := map[string]bool{}
	for index, engine := range c.Engines {
		if engine.Name == "" || names[engine.Name] {
			return fmt.Errorf("engine %d needs a unique name", index)
		}
		names[engine.Name] = true
		if engine.Drafts < 0 || engine.Drafts > 16 {
			return fmt.Errorf("engine %s: drafts must be between 0 and 16", engine.Name)
		}
		if engine.Room < 0 || engine.Room > MaxRoom {
			return fmt.Errorf("engine %s: room must be between 0 and %d", engine.Name, MaxRoom)
		}
		parsed, err := url.Parse(engine.URL)
		if err != nil || parsed.Scheme != "http" || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || !loopbackAddress(parsed.Host) {
			return fmt.Errorf("engine %s: url must be http on a loopback address with no path", engine.Name)
		}
		if len(engine.Command) > MaxCommandArgs {
			return fmt.Errorf("engine %s: command exceeds %d arguments", engine.Name, MaxCommandArgs)
		}
		for _, arg := range engine.Command {
			if len(arg) > MaxArgBytes || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("engine %s: command arguments must be under %d bytes without NUL", engine.Name, MaxArgBytes)
			}
		}
	}
	if len(c.Policies) == 0 {
		return errors.New("at least one policy is required")
	}
	if _, ok := c.Policies[c.Policy]; !ok {
		return fmt.Errorf("policy %q is not one of the policies", c.Policy)
	}
	for name, bands := range c.Policies {
		if err := validateBands(bands, names); err != nil {
			return fmt.Errorf("policy %s: %w", name, err)
		}
	}
	sized := 0
	for _, engine := range c.Engines {
		if engine.sized() {
			sized++
		}
	}
	if sized > 0 {
		if len(c.Memory.Probe) == 0 || len(c.Memory.Probe) > MaxCommandArgs {
			return errors.New("memory.probe is required when an engine's command takes the memory share")
		}
		if c.Memory.MarginMiB < 0 || c.Memory.MaxShare < 0.5 || c.Memory.MaxShare > 1 {
			return errors.New("memory needs margin_mib >= 0 and max_share between 0.5 and 1")
		}
		if StartOrder(c)[0].sized() {
			return errors.New("the first engine to start must set its own memory share: the share of the others is measured from it asleep")
		}
	}
	t := c.Timing
	if t.Sample <= 0 || t.Smooth < t.Sample || t.RaiseAfter <= 0 || t.LowerAfter < t.RaiseAfter || t.MinDwell < 0 || t.DrainTimeout <= 0 || t.WakeTimeout <= 0 {
		return errors.New("timing must have sample > 0, smooth >= sample, raise_after > 0, lower_after >= raise_after and positive timeouts")
	}
	if t.LowerAfter.D() > time.Hour || t.DrainTimeout.D() > 30*time.Minute || t.WakeTimeout.D() > 30*time.Minute {
		return errors.New("timing must hold lower_after under an hour and timeouts under 30 minutes")
	}
	return nil
}

// validateBands checks that bands rise, end in an open band and name engines.
func validateBands(bands []Band, engines map[string]bool) error {
	if len(bands) < 1 || len(bands) > MaxEngines {
		return fmt.Errorf("needs 1 to %d bands", MaxEngines)
	}
	previous := 0
	for index, band := range bands {
		if !engines[band.Engine] {
			return fmt.Errorf("band %d names unknown engine %q", index, band.Engine)
		}
		last := index == len(bands)-1
		switch {
		case last && band.UpTo != 0:
			return errors.New("the last band must be open (no up_to)")
		case !last && band.UpTo <= previous:
			return errors.New("bands must rise: each up_to above the one before")
		}
		previous = band.UpTo
	}
	return nil
}

// loopbackAddress accepts host:port on 127.0.0.1, ::1 or localhost.
func loopbackAddress(hostPort string) bool {
	host := hostPort
	if index := strings.LastIndex(hostPort, ":"); index >= 0 {
		host = hostPort[:index]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// engineIndex returns the position of the named engine.
func (c Config) engineIndex(name string) int {
	for index, engine := range c.Engines {
		if engine.Name == name {
			return index
		}
	}
	return -1
}
