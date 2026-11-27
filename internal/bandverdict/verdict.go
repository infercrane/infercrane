// Package bandverdict gives a verdict per load band from a screen: setups
// measured in turn on one machine at two or more loads, the current setup
// first and last. Servers of one GPU model differ by more than most setups
// do, so every comparison here is on one machine, and the noise a gain must
// clear is measured in the same screen: the current setup's two runs, and a
// copy of the current setup when the screen ran one. A setup that wins at a
// light load can lose at a heavy one, so each band gets its own verdict,
// judged by default on useful output (output of requests whose first token
// came within the limit).
//
// It reads a content-free summary (schemas/band-screen-v1.schema.json) and
// runs anywhere: the harness screen, the CLI (`infercrane optimize
// band-verdict`), or a band study in the product. A verdict is a screen's
// finding, not a proof; finalists still go to fresh servers.
package bandverdict

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// SchemaV1 is the input this package reads.
const SchemaV1 = "infercrane.dev/band-screen/v1"

// Metrics a verdict can be judged on.
const (
	MetricUsefulOutput = "useful_output"
	MetricOutput       = "output"
)

// Band decisions.
const (
	// DecisionWinner: a setup beat the current setup by more than the noise.
	DecisionWinner = "winner"
	// DecisionKeep: no setup beat the current setup by more than the noise.
	DecisionKeep = "keep_current"
	// DecisionNoDecision: the copy of the current setup measured outside the
	// current setup's own repeat noise, so the band cannot separate setups.
	DecisionNoDecision = "no_decision"
	// DecisionUnknown: the band lacks what a verdict needs (two runs of the
	// current setup, or repeats within the agreed spread).
	DecisionUnknown = "unknown"
)

// Summary kinds across bands.
const (
	// SummaryOneSetup: every band decided, and one setup wins (or the
	// current setup is kept) in all of them.
	SummaryOneSetup = "one_setup"
	// SummaryPerBand: every band decided, with different setups.
	SummaryPerBand = "setup_per_band"
	// SummaryIncomplete: at least one band has no verdict.
	SummaryIncomplete = "incomplete"
)

// Screen is the input: one summary per band.
type Screen struct {
	SchemaVersion string `json:"schema_version"`
	// Metric is what the verdict is judged on: useful_output (default) or
	// output.
	Metric string `json:"metric,omitempty"`
	// LimitMS is the first-token limit useful output counted against.
	LimitMS float64 `json:"limit_ms"`
	// Current is the setup every other setup is compared with; Copy is a
	// copy of it (its flags with only a seed added), when the screen ran one.
	Current string `json:"current"`
	Copy    string `json:"copy,omitempty"`
	Rules   Rules  `json:"rules"`
	Bands   []Band `json:"bands"`
}

// Rules are the pass bars written before the screen ran.
type Rules struct {
	// MaxCurrentSpread is how far the current setup's first and last runs
	// may differ (a share of their mean) for the machine to count as steady.
	MaxCurrentSpread float64 `json:"max_current_spread"`
	// MinNoiseFloor guards against two repeats that agree by chance; it is
	// never the allowance itself, which comes from the screen.
	MinNoiseFloor float64 `json:"min_noise_floor"`
}

// Band is one load of one kind of traffic.
type Band struct {
	Kind string `json:"kind"`
	// Load is requests at once.
	Load int `json:"load"`
	// CurrentRuns are the current setup's runs in order: first and last.
	CurrentRuns []Measure `json:"current_runs"`
	Setups      []Setup   `json:"setups"`
}

// Setup is one other setup's runs in the band.
type Setup struct {
	ID   string    `json:"id"`
	Runs []Measure `json:"runs"`
	// HeldBack keeps a setup from winning whatever it gained, for example
	// "task set worse"; the reason is reported.
	HeldBack string `json:"held_back,omitempty"`
}

// Measure is one run in one band, in output tokens per second.
type Measure struct {
	Output       float64  `json:"output"`
	UsefulOutput *float64 `json:"useful_output,omitempty"`
}

// Verdict is the decision per band and across them.
type Verdict struct {
	Metric  string        `json:"metric"`
	Bands   []BandVerdict `json:"bands"`
	Summary string        `json:"summary"`
	// Winners names the decision per band, keyed kind/load: a setup ID, or
	// keep_current.
	Winners map[string]string `json:"winners"`
	Reason  string            `json:"reason"`
	// SwitchBands is the band table a per-server switch serves from (the
	// shape of specswitch's policy bands): each band's largest load and the
	// setup that serves it, the current setup where it was kept. It is set
	// only when every band decided and all bands are one kind of traffic.
	SwitchBands []SwitchBand `json:"switch_bands,omitempty"`
}

// SwitchBand is one row of the band table. UpTo is the band's largest load in
// requests at once; 0 marks the last band, above every other. A load between
// two measured bands is served by the higher band's setup: the verdict knows
// nothing between them, and the higher band's setup was measured under more
// load.
type SwitchBand struct {
	UpTo   int    `json:"up_to,omitempty"`
	Engine string `json:"engine"`
}

// BandVerdict is one band's decision with everything it rests on.
type BandVerdict struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	Load int    `json:"load"`
	// Current is the mean of the current setup's runs on the metric.
	Current float64 `json:"current"`
	// Spread is how far its first and last runs differ, a share of the mean.
	Spread float64 `json:"spread"`
	// CopyGain is the copy's change from the current setup, when measured.
	CopyGain *float64 `json:"copy_gain,omitempty"`
	// NoiseFloor is what a gain had to clear: the largest of the spread, the
	// copy's change, and the rules' minimum.
	NoiseFloor float64 `json:"noise_floor"`
	// RepeatsSteady and CopyInsideNoise are the band's two checks.
	RepeatsSteady   bool          `json:"repeats_steady"`
	CopyInsideNoise *bool         `json:"copy_inside_noise,omitempty"`
	Decision        string        `json:"decision"`
	Winner          string        `json:"winner,omitempty"`
	Setups          []SetupResult `json:"setups"`
	Reason          string        `json:"reason"`
}

// SetupResult is one setup's change from the current setup in a band.
type SetupResult struct {
	ID string `json:"id"`
	// Gain is the change on the verdict's metric; OutputGain on all output.
	Gain       *float64 `json:"gain"`
	OutputGain *float64 `json:"output_gain"`
	Ahead      bool     `json:"ahead"`
	HeldBack   string   `json:"held_back,omitempty"`
}

// Decode reads and validates a screen summary.
func Decode(body []byte) (Screen, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var screen Screen
	if err := decoder.Decode(&screen); err != nil {
		return Screen{}, fmt.Errorf("decode band screen: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Screen{}, errors.New("band screen holds more than one JSON value")
	}
	return screen, screen.Validate()
}

// Validate checks a screen summary's shape.
func (s Screen) Validate() error {
	if s.SchemaVersion != SchemaV1 {
		return fmt.Errorf("band screen schema must be %q", SchemaV1)
	}
	if s.Metric != "" && s.Metric != MetricUsefulOutput && s.Metric != MetricOutput {
		return fmt.Errorf("band screen metric is %q or %q", MetricUsefulOutput, MetricOutput)
	}
	if strings.TrimSpace(s.Current) == "" || s.Copy == s.Current {
		return errors.New("band screen names its current setup, and a copy that is another setup")
	}
	if !finite(s.LimitMS) || s.LimitMS < 0 || !finite(s.Rules.MaxCurrentSpread) || s.Rules.MaxCurrentSpread <= 0 || s.Rules.MaxCurrentSpread >= 1 || !finite(s.Rules.MinNoiseFloor) || s.Rules.MinNoiseFloor < 0 || s.Rules.MinNoiseFloor >= 1 {
		return errors.New("band screen rules need a current-setup spread above 0 and below 1, and a noise floor from 0 to below 1")
	}
	if len(s.Bands) == 0 || len(s.Bands) > 64 {
		return errors.New("band screen holds 1 to 64 bands")
	}
	seen := map[string]bool{}
	for _, band := range s.Bands {
		key := band.key()
		if strings.TrimSpace(band.Kind) == "" || band.Load < 1 || seen[key] {
			return fmt.Errorf("band %q needs a kind, a load of at least 1, and must appear once", key)
		}
		seen[key] = true
		ids := map[string]bool{}
		for _, setup := range band.Setups {
			if strings.TrimSpace(setup.ID) == "" || setup.ID == s.Current || ids[setup.ID] {
				return fmt.Errorf("band %s: each setup has its own ID, other than the current setup's", key)
			}
			ids[setup.ID] = true
			for _, run := range setup.Runs {
				if !run.valid() {
					return fmt.Errorf("band %s: setup %s has a run that is not a finite, non-negative number", key, setup.ID)
				}
			}
		}
		for _, run := range band.CurrentRuns {
			if !run.valid() {
				return fmt.Errorf("band %s: the current setup has a run that is not a finite, non-negative number", key)
			}
		}
	}
	return nil
}

func (b Band) key() string { return fmt.Sprintf("%s/%d", b.Kind, b.Load) }

func (m Measure) valid() bool {
	return finite(m.Output) && m.Output >= 0 && (m.UsefulOutput == nil || finite(*m.UsefulOutput) && *m.UsefulOutput >= 0)
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (m Measure) value(metric string) (float64, bool) {
	if metric == MetricOutput {
		return m.Output, true
	}
	if m.UsefulOutput == nil {
		return 0, false
	}
	return *m.UsefulOutput, true
}

// mean is the metric's mean over runs, when every run has it.
func mean(runs []Measure, metric string) (float64, bool) {
	if len(runs) == 0 {
		return 0, false
	}
	total := 0.0
	for _, run := range runs {
		value, ok := run.value(metric)
		if !ok {
			return 0, false
		}
		total += value
	}
	return total / float64(len(runs)), true
}

func gain(runs []Measure, metric string, current float64) *float64 {
	value, ok := mean(runs, metric)
	if !ok || current <= 0 {
		return nil
	}
	change := value/current - 1
	return &change
}

// Decide gives the verdict. Bands are reported in load order within each
// kind.
func Decide(screen Screen) (Verdict, error) {
	if err := screen.Validate(); err != nil {
		return Verdict{}, err
	}
	metric := screen.Metric
	if metric == "" {
		metric = MetricUsefulOutput
	}
	bands := append([]Band(nil), screen.Bands...)
	sort.SliceStable(bands, func(i, j int) bool {
		if bands[i].Kind != bands[j].Kind {
			return bands[i].Kind < bands[j].Kind
		}
		return bands[i].Load < bands[j].Load
	})
	decided := make([]BandVerdict, 0, len(bands))
	for _, band := range bands {
		decided = append(decided, decideBand(screen, band, metric))
	}
	return Summarize(metric, screen.Current, decided), nil
}

// Summarize gives the verdict across bands that were each decided already: by
// a screen (Decide), or by a campaign per load (a band study in the product).
// current names the setup a kept band keeps.
func Summarize(metric, current string, bands []BandVerdict) Verdict {
	verdict := Verdict{Metric: metric, Bands: bands, Winners: map[string]string{}}
	complete, winners := true, map[string]bool{}
	for _, decided := range bands {
		switch decided.Decision {
		case DecisionWinner:
			verdict.Winners[decided.Key] = decided.Winner
			winners[decided.Winner] = true
		case DecisionKeep:
			verdict.Winners[decided.Key] = DecisionKeep
			winners[DecisionKeep] = true
		default:
			complete = false
		}
	}
	switch {
	case !complete || len(bands) == 0:
		verdict.Summary, verdict.Reason = SummaryIncomplete, "At least one band has no verdict; its reason says why."
	case len(winners) == 1:
		verdict.Summary = SummaryOneSetup
		for winner := range winners {
			if winner == DecisionKeep {
				verdict.Reason = "No setup beat the current setup by more than the noise in any band."
			} else {
				verdict.Reason = fmt.Sprintf("%s beat the current setup by more than the noise in every band.", winner)
			}
		}
	default:
		verdict.Summary, verdict.Reason = SummaryPerBand, "Different setups win different bands. A deployment serves one setup unless a per-server switch serves the band table."
	}
	if complete && len(bands) > 0 {
		verdict.SwitchBands = switchBands(current, verdict.Bands)
	}
	return verdict
}

// switchBands turns decided bands of one kind into a band table, merging
// neighbours that the same setup serves.
func switchBands(current string, bands []BandVerdict) []SwitchBand {
	if len(bands) == 0 {
		return nil
	}
	for _, band := range bands {
		if band.Kind != bands[0].Kind {
			return nil
		}
	}
	var out []SwitchBand
	for _, band := range bands {
		engine := band.Winner
		if band.Decision == DecisionKeep {
			engine = current
		}
		if len(out) > 0 && out[len(out)-1].Engine == engine {
			out[len(out)-1].UpTo = band.Load
			continue
		}
		out = append(out, SwitchBand{UpTo: band.Load, Engine: engine})
	}
	out[len(out)-1].UpTo = 0
	return out
}

func decideBand(screen Screen, band Band, metric string) BandVerdict {
	out := BandVerdict{Key: band.key(), Kind: band.Kind, Load: band.Load, Setups: []SetupResult{}}
	if len(band.CurrentRuns) < 2 {
		out.Decision, out.Reason = DecisionUnknown, "The current setup ran fewer than twice in this band, so the machine's own noise is unknown."
		return out
	}
	current, ok := mean(band.CurrentRuns, metric)
	first, firstOK := band.CurrentRuns[0].value(metric)
	last, lastOK := band.CurrentRuns[len(band.CurrentRuns)-1].value(metric)
	if !ok || !firstOK || !lastOK || current <= 0 {
		out.Decision, out.Reason = DecisionUnknown, fmt.Sprintf("The current setup has no %s in this band.", strings.ReplaceAll(metric, "_", " "))
		return out
	}
	out.Current, out.Spread = current, math.Abs(first-last)/current
	out.RepeatsSteady = out.Spread <= screen.Rules.MaxCurrentSpread
	out.NoiseFloor = math.Max(out.Spread, screen.Rules.MinNoiseFloor)
	for _, setup := range band.Setups {
		if setup.ID != screen.Copy {
			continue
		}
		if change := gain(setup.Runs, metric, current); change != nil {
			inside := math.Abs(*change) <= math.Max(out.Spread, screen.Rules.MinNoiseFloor)
			out.CopyGain, out.CopyInsideNoise = change, &inside
			out.NoiseFloor = math.Max(out.NoiseFloor, math.Abs(*change))
		}
	}
	best := -1
	for _, setup := range band.Setups {
		result := SetupResult{ID: setup.ID, Gain: gain(setup.Runs, metric, current), HeldBack: setup.HeldBack}
		if outputCurrent, ok := mean(band.CurrentRuns, MetricOutput); ok {
			result.OutputGain = gain(setup.Runs, MetricOutput, outputCurrent)
		}
		result.Ahead = setup.ID != screen.Copy && setup.HeldBack == "" && result.Gain != nil && *result.Gain > out.NoiseFloor
		out.Setups = append(out.Setups, result)
		if result.Ahead && (best < 0 || *result.Gain > *out.Setups[best].Gain) {
			best = len(out.Setups) - 1
		}
	}
	switch {
	case !out.RepeatsSteady:
		out.Decision = DecisionUnknown
		out.Reason = fmt.Sprintf("The current setup's first and last runs differ by %.1f%%, above the agreed %.1f%%: the machine was not steady.", 100*out.Spread, 100*screen.Rules.MaxCurrentSpread)
	case out.CopyInsideNoise != nil && !*out.CopyInsideNoise:
		out.Decision = DecisionNoDecision
		out.Reason = fmt.Sprintf("The copy of the current setup changed %+.1f%%, outside the current setup's own %.1f%%: this band cannot separate setups.", 100**out.CopyGain, 100*math.Max(out.Spread, screen.Rules.MinNoiseFloor))
	case best < 0:
		out.Decision = DecisionKeep
		out.Reason = fmt.Sprintf("No setup beat the current setup by more than %.1f%%.", 100*out.NoiseFloor)
	default:
		out.Decision, out.Winner = DecisionWinner, out.Setups[best].ID
		out.Reason = fmt.Sprintf("%s changed %+.1f%%, beyond the %.1f%% noise.", out.Winner, 100**out.Setups[best].Gain, 100*out.NoiseFloor)
	}
	return out
}
