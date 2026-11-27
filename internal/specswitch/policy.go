package specswitch

import "time"

// sample is one reading of the load.
type sample struct {
	at   time.Time
	load int
}

// decider chooses the engine for the load. It is not safe for concurrent use;
// the switch's control loop owns it.
type decider struct {
	timing Timing
	bands  []Band
	// samples within the smoothing window, oldest first.
	samples []sample
	// candidate is the band the load has been in since candidateSince while
	// another engine is awake; empty when the awake engine's band holds.
	candidate      string
	candidateSince time.Time
}

func newDecider(timing Timing, bands []Band) *decider {
	return &decider{timing: timing, bands: bands}
}

// setBands replaces the bands, for a policy change, and forgets a pending
// candidate so the new bands must hold on their own.
func (d *decider) setBands(bands []Band) {
	d.bands = bands
	d.candidate = ""
}

// load is the highest reading within the smoothing window.
func (d *decider) load() int {
	highest := 0
	for _, s := range d.samples {
		highest = max(highest, s.load)
	}
	return highest
}

// bandFor returns the index of the band that holds load.
func bandFor(bands []Band, load int) int {
	for index, band := range bands {
		if band.UpTo == 0 || load <= band.UpTo {
			return index
		}
	}
	return len(bands) - 1
}

// bandOf returns the index of the band served by engine, or -1. An engine
// serving several bands is placed at the lowest.
func bandOf(bands []Band, engine string) int {
	for index, band := range bands {
		if band.Engine == engine {
			return index
		}
	}
	return -1
}

// observe records one reading and returns the engine to swap to, if the load
// has stayed in another engine's band long enough. A raise (to a band above
// the awake engine's, or away from an engine the policy no longer names)
// waits RaiseAfter: staying costs every request. A lower waits LowerAfter, and
// only once the awake engine has been awake MinDwell since awakeSince:
// staying costs only some speed.
func (d *decider) observe(now time.Time, load int, awake string, awakeSince time.Time) (string, bool) {
	d.samples = append(d.samples, sample{at: now, load: load})
	cutoff := now.Add(-d.timing.Smooth.D())
	drop := 0
	for drop < len(d.samples)-1 && d.samples[drop].at.Before(cutoff) {
		drop++
	}
	d.samples = d.samples[drop:]

	want := d.bands[bandFor(d.bands, d.load())]
	if want.Engine == awake {
		d.candidate = ""
		return "", false
	}
	if want.Engine != d.candidate {
		d.candidate, d.candidateSince = want.Engine, now
	}
	current := bandOf(d.bands, awake)
	raising := current < 0 || bandOf(d.bands, want.Engine) > current
	if raising {
		return want.Engine, now.Sub(d.candidateSince) >= d.timing.RaiseAfter.D()
	}
	if now.Sub(d.candidateSince) < d.timing.LowerAfter.D() || now.Sub(awakeSince) < d.timing.MinDwell.D() {
		return "", false
	}
	return want.Engine, true
}

// reset forgets readings and a pending candidate, after a swap or a failed one.
func (d *decider) reset() {
	d.samples = d.samples[:0]
	d.candidate = ""
}
