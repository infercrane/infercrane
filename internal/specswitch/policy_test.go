package specswitch

import (
	"strings"
	"testing"
	"time"
)

func testTiming() Timing {
	return Timing{Sample: Duration(time.Second), Smooth: Duration(2 * time.Second), RaiseAfter: Duration(3 * time.Second),
		LowerAfter: Duration(10 * time.Second), MinDwell: Duration(5 * time.Second), DrainTimeout: Duration(time.Minute), WakeTimeout: Duration(time.Minute)}
}

var throughputBands = []Band{{UpTo: 58, Engine: "drafts-3"}, {Engine: "drafts-1"}}

func TestDeciderRaisesOnceTheBandHolds(t *testing.T) {
	d := newDecider(testTiming(), throughputBands)
	start := time.Unix(1000, 0)
	awakeSince := start.Add(-time.Minute)
	for second := range 3 {
		if target, swap := d.observe(start.Add(time.Duration(second)*time.Second), 96, "drafts-3", awakeSince); swap {
			t.Fatalf("swapped to %s after %d s, before the 3 s hold", target, second)
		}
	}
	target, swap := d.observe(start.Add(3*time.Second), 96, "drafts-3", awakeSince)
	if !swap || target != "drafts-1" {
		t.Fatalf("after 3 s at 96 got (%q, %t), want drafts-1", target, swap)
	}
}

func TestDeciderLowersOnlyAfterTheLongerHold(t *testing.T) {
	d := newDecider(testTiming(), throughputBands)
	start := time.Unix(1000, 0)
	awakeSince := start.Add(-time.Minute)
	for second := 0; second < 10; second++ {
		if _, swap := d.observe(start.Add(time.Duration(second)*time.Second), 20, "drafts-1", awakeSince); swap {
			t.Fatalf("lowered after %d s, before the 10 s hold", second)
		}
	}
	if target, swap := d.observe(start.Add(10*time.Second), 20, "drafts-1", awakeSince); !swap || target != "drafts-3" {
		t.Fatalf("after 10 s at 20 got (%q, %t), want drafts-3", target, swap)
	}
}

func TestDeciderIgnoresDipsInsideTheSmoothingWindow(t *testing.T) {
	d := newDecider(testTiming(), throughputBands)
	start := time.Unix(1000, 0)
	awakeSince := start.Add(-time.Minute)
	// A closed loop at 96 reads 96 most of the time and, between one answer
	// and the next request, a little less; the band stays drafts-1.
	loads := []int{96, 40, 96, 30, 96, 96, 50, 96, 96, 96, 96, 96, 96}
	for second, load := range loads {
		if _, swap := d.observe(start.Add(time.Duration(second)*time.Second), load, "drafts-1", awakeSince); swap {
			t.Fatalf("a dip to %d at second %d swapped engines", load, second)
		}
	}
}

func TestDeciderHoldsALowerForMinDwellButNeverARaise(t *testing.T) {
	start := time.Unix(1000, 0)
	// A raise right after a swap goes ahead after RaiseAfter alone.
	d := newDecider(testTiming(), throughputBands)
	for second := 0; second < 3; second++ {
		d.observe(start.Add(time.Duration(second)*time.Second), 96, "drafts-3", start)
	}
	if target, swap := d.observe(start.Add(3*time.Second), 96, "drafts-3", start); !swap || target != "drafts-1" {
		t.Fatalf("a raise 3 s after a swap was held: (%q, %t)", target, swap)
	}
	// A lower waits for the dwell even once the lower band has held.
	timing := testTiming()
	timing.MinDwell = Duration(30 * time.Second)
	d = newDecider(timing, throughputBands)
	for second := 0; second < 30; second++ {
		if _, swap := d.observe(start.Add(time.Duration(second)*time.Second), 20, "drafts-1", start); swap {
			t.Fatalf("lowered %d s after a swap, before the 30 s dwell", second)
		}
	}
	if _, swap := d.observe(start.Add(30*time.Second), 20, "drafts-1", start); !swap {
		t.Fatal("did not lower once the dwell had passed")
	}
}

func TestDeciderRestartsTheHoldWhenTheBandChanges(t *testing.T) {
	bands := []Band{{UpTo: 58, Engine: "drafts-3"}, {UpTo: 111, Engine: "drafts-1"}, {Engine: "drafts-0"}}
	d := newDecider(testTiming(), bands)
	start := time.Unix(1000, 0)
	awakeSince := start.Add(-time.Minute)
	d.observe(start, 96, "drafts-3", awakeSince)
	d.observe(start.Add(time.Second), 96, "drafts-3", awakeSince)
	// The load moves to the top band before the hold for drafts-1 ends.
	for second := 4; second < 7; second++ {
		if target, swap := d.observe(start.Add(time.Duration(second)*time.Second), 128, "drafts-3", awakeSince); swap {
			t.Fatalf("swapped to %s %d s into a band that began at 4 s", target, second-4)
		}
	}
	if target, swap := d.observe(start.Add(7*time.Second), 128, "drafts-3", awakeSince); !swap || target != "drafts-0" {
		t.Fatalf("got (%q, %t), want drafts-0 after 3 s at 128", target, swap)
	}
}

func TestBandForChoosesTheFirstBandThatHoldsTheLoad(t *testing.T) {
	bands := []Band{{UpTo: 58, Engine: "a"}, {UpTo: 111, Engine: "b"}, {Engine: "c"}}
	for load, want := range map[int]string{0: "a", 58: "a", 59: "b", 111: "b", 112: "c", 4000: "c"} {
		if got := bands[bandFor(bands, load)].Engine; got != want {
			t.Errorf("load %d: got %s, want %s", load, got, want)
		}
	}
}

func validConfig() Config {
	return Config{
		Listen: ":8000",
		Engines: []Engine{
			{Name: "drafts-3", Drafts: 3, URL: "http://127.0.0.1:8101", Room: 58},
			{Name: "drafts-1", Drafts: 1, URL: "http://127.0.0.1:8102", Room: 111},
		},
		Policies: map[string][]Band{"throughput": throughputBands},
		Policy:   "throughput",
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := validConfig().Normalize(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	cases := map[string]func(*Config){
		"engine off loopback": func(c *Config) { c.Engines[0].URL = "http://10.0.0.5:8101" },
		"engine with a path":  func(c *Config) { c.Engines[0].URL = "http://127.0.0.1:8101/v1" },
		"one engine":          func(c *Config) { c.Engines = c.Engines[:1] },
		"duplicate engine":    func(c *Config) { c.Engines[1].Name = "drafts-3" },
		"unknown band engine": func(c *Config) { c.Policies["throughput"] = []Band{{UpTo: 10, Engine: "x"}, {Engine: "drafts-1"}} },
		"bands that fall": func(c *Config) {
			c.Policies["throughput"] = []Band{{UpTo: 58, Engine: "drafts-3"}, {UpTo: 40, Engine: "drafts-1"}, {Engine: "drafts-1"}}
		},
		"closed last band": func(c *Config) {
			c.Policies["throughput"] = []Band{{UpTo: 58, Engine: "drafts-3"}, {UpTo: 200, Engine: "drafts-1"}}
		},
		"missing policy":     func(c *Config) { c.Policy = "latency" },
		"admin off loopback": func(c *Config) { c.AdminListen = "0.0.0.0:9000" },
		"lower faster than raise": func(c *Config) {
			c.Timing.RaiseAfter, c.Timing.LowerAfter = Duration(time.Minute), Duration(time.Second)
		},
		"queue too long":    func(c *Config) { c.QueueLimit = MaxQueue + 1 },
		"argument too long": func(c *Config) { c.Engines[0].Command = []string{"vllm", strings.Repeat("x", MaxArgBytes+1)} },
	}
	for name, change := range cases {
		cfg := validConfig()
		cfg.Policies = map[string][]Band{"throughput": append([]Band(nil), throughputBands...)}
		change(&cfg)
		if _, err := cfg.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
