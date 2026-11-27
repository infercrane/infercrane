package technique

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ResultSchemaV1 is the record of one measured candidate. It carries the five
// fields every performance number needs (hardware and software, workload,
// precision, baseline, correctness method) and what it takes to reproduce
// it. A missing field leaves the number "not shown", never a gain.
const ResultSchemaV1 = "infercrane.dev/technique-result/v1"

// Decisions a result can carry.
const (
	DecisionImproved = "improved"
	DecisionSame     = "no_measurable_difference"
	DecisionWorse    = "regressed"
	DecisionUnknown  = "unknown"
)

// Claims a result supports.
const (
	ClaimShown    = "shown"
	ClaimNotShown = "not shown"
)

// EvidenceFreshMachineConfirmation is the evidence every result needs before
// it is shown, whatever its technique: the sha256 of the record of a second
// window on a fresh machine that measured the gain again. A gain from one
// machine and one run can be that machine; Unknown names it until a fresh
// machine repeated it.
const EvidenceFreshMachineConfirmation = "fresh-machine-confirmation"

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Result is one candidate's measured outcome at one load.
type Result struct {
	SchemaVersion    string `json:"schema_version"`
	Technique        string `json:"technique"`
	TechniqueVersion string `json:"technique_version"`
	Candidate        string `json:"candidate"`

	Hardware    Hardware    `json:"hardware"`
	Software    Software    `json:"software"`
	Workload    Workload    `json:"workload"`
	Precision   Precision   `json:"precision"`
	Baseline    Baseline    `json:"baseline"`
	Correctness Correctness `json:"correctness"`

	Measured     Measured     `json:"measured"`
	Reproduction Reproduction `json:"reproduction"`

	// RequiredEvidence is the technique's manifest evidence, copied when the
	// record is made; Manifest.ClaimOf checks against the manifest itself, so
	// a record cannot drop a requirement by leaving it out.
	RequiredEvidence []string       `json:"required_evidence,omitempty"`
	Evidence         []EvidenceItem `json:"evidence,omitempty"`
	// Disclosures are the manifest's, carried with the number word for word;
	// Manifest.ClaimOf shows nothing that drops one.
	Disclosures []string `json:"disclosures,omitempty"`
}

// EvidenceItem is one piece of technique-specific evidence: its name, the
// sha256 of the file that holds it, and a short summary.
type EvidenceItem struct {
	Name    string `json:"name"`
	Digest  string `json:"digest"`
	Summary string `json:"summary,omitempty"`
}

// Hardware is the GPU the number was measured on.
type Hardware struct {
	Accelerator string `json:"accelerator"`
	Count       int    `json:"count"`
	Driver      string `json:"driver,omitempty"`
}

// Software is the engine and the exact image that served.
type Software struct {
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	ImageDigest   string `json:"image_digest"`
}

// Workload is the content-free traffic and the load it was held at.
type Workload struct {
	// ShapeDigest commits to the traffic shape (lengths and order, never
	// text).
	ShapeDigest string `json:"shape_digest"`
	Kind        string `json:"kind"`
	// Load is requests at once.
	Load     int `json:"load"`
	Requests int `json:"requests"`
}

// Precision is the numbers the setup served.
type Precision struct {
	Weights string `json:"weights"`
	KVCache string `json:"kv_cache"`
}

// Baseline is what the candidate was compared with, and how.
type Baseline struct {
	Setup string `json:"setup"`
	// Basis is how the arms were placed, for example same-machine (in turn
	// on one machine), interleaved (paired runs on two servers), with
	// +fresh-servers when a fresh machine measured it again.
	Basis string `json:"basis"`
}

// Correctness is how answers were checked, and the verdict.
type Correctness struct {
	// Method is, for example, task-score, answer-agreement, or
	// task-score+kl.
	Method  string `json:"method"`
	Verdict string `json:"verdict"`
}

// Measured is the outcome on the metric the claim is about.
type Measured struct {
	Metric string `json:"metric"`
	// Change is relative to the baseline, for example 0.12 for +12%.
	Change float64 `json:"change"`
	// NoiseFloor is the measured noise the change had to clear, from the
	// same machine or the copy of the current setup, never a constant.
	NoiseFloor float64 `json:"noise_floor"`
	Decision   string  `json:"decision"`
}

// Reproduction is what another party needs to run it again.
type Reproduction struct {
	Command     string `json:"command"`
	Commit      string `json:"commit"`
	ImageDigest string `json:"image_digest"`
	// TrafficShape names or links the content-free traffic shape file.
	TrafficShape string `json:"traffic_shape"`
}

// Unknown names every field a claim needs that the result does not carry.
func (r Result) Unknown() []string {
	var out []string
	missing := func(name, value string) {
		if strings.TrimSpace(value) == "" || strings.EqualFold(strings.TrimSpace(value), DecisionUnknown) {
			out = append(out, name)
		}
	}
	missing("hardware.accelerator", r.Hardware.Accelerator)
	if r.Hardware.Count < 1 {
		out = append(out, "hardware.count")
	}
	missing("software.engine", r.Software.Engine)
	missing("software.engine_version", r.Software.EngineVersion)
	if !digestPattern.MatchString(r.Software.ImageDigest) {
		out = append(out, "software.image_digest")
	}
	if !digestPattern.MatchString(r.Workload.ShapeDigest) {
		out = append(out, "workload.shape_digest")
	}
	missing("workload.kind", r.Workload.Kind)
	if r.Workload.Load < 1 {
		out = append(out, "workload.load")
	}
	missing("precision.weights", r.Precision.Weights)
	missing("precision.kv_cache", r.Precision.KVCache)
	missing("baseline.setup", r.Baseline.Setup)
	missing("baseline.basis", r.Baseline.Basis)
	missing("correctness.method", r.Correctness.Method)
	missing("correctness.verdict", r.Correctness.Verdict)
	missing("reproduction.command", r.Reproduction.Command)
	missing("reproduction.commit", r.Reproduction.Commit)
	missing("reproduction.traffic_shape", r.Reproduction.TrafficShape)
	if !digestPattern.MatchString(r.Reproduction.ImageDigest) {
		out = append(out, "reproduction.image_digest")
	}
	// Every result needs the fresh machine's confirmation; no manifest can
	// leave it out.
	required := r.RequiredEvidence
	if !slices.Contains(required, EvidenceFreshMachineConfirmation) {
		required = append(slices.Clone(required), EvidenceFreshMachineConfirmation)
	}
	for _, name := range required {
		if !slices.ContainsFunc(r.Evidence, func(item EvidenceItem) bool { return item.Name == name && digestPattern.MatchString(item.Digest) }) {
			out = append(out, "evidence."+name)
		}
	}
	return out
}

// ClaimOf judges a record of this technique against the manifest's own
// evidence list, whatever the record copied.
func (m Manifest) ClaimOf(r Result) string {
	if r.Technique != m.ID || r.TechniqueVersion != m.Version {
		return ClaimNotShown
	}
	for _, sentence := range m.Disclosures {
		if !slices.Contains(r.Disclosures, sentence) {
			return ClaimNotShown
		}
	}
	for _, name := range m.Evidence {
		if !slices.Contains(r.RequiredEvidence, name) {
			r.RequiredEvidence = append(slices.Clone(r.RequiredEvidence), name)
		}
	}
	return r.Claim()
}

// Claim is "shown" only for a measured improvement beyond the measured noise
// with every field present, answers that held, and a fresh machine's
// confirmation (EvidenceFreshMachineConfirmation); anything else is "not
// shown".
func (r Result) Claim() string {
	if len(r.Unknown()) > 0 || r.Measured.Decision != DecisionImproved || r.Measured.Change <= r.Measured.NoiseFloor || r.Measured.NoiseFloor <= 0 || r.Correctness.Verdict != "passed" {
		return ClaimNotShown
	}
	return ClaimShown
}

// Validate checks the record's shape. A valid record can still be "not
// shown"; Unknown says why.
func (r Result) Validate() error {
	if r.SchemaVersion != ResultSchemaV1 {
		return fmt.Errorf("technique result schema must be %q", ResultSchemaV1)
	}
	if !idPattern.MatchString(r.Technique) || !versionPattern.MatchString(r.TechniqueVersion) || strings.TrimSpace(r.Candidate) == "" {
		return errors.New("technique result names its technique, version, and candidate")
	}
	switch r.Measured.Decision {
	case DecisionImproved, DecisionSame, DecisionWorse, DecisionUnknown:
	default:
		return fmt.Errorf("technique result decision %q is not one of improved, no_measurable_difference, regressed, unknown", r.Measured.Decision)
	}
	if !tokenPattern.MatchString(r.Measured.Metric) || r.Measured.NoiseFloor < 0 || r.Hardware.Count < 0 || r.Workload.Load < 0 || r.Workload.Requests < 0 {
		return errors.New("technique result names its metric, and counts and noise are not negative")
	}
	if len(r.RequiredEvidence) > maxEvidence || len(r.Evidence) > maxEvidence {
		return fmt.Errorf("technique result carries more than %d kinds of evidence", maxEvidence)
	}
	if len(r.Disclosures) > maxDisclosures {
		return fmt.Errorf("technique result carries more than %d disclosures", maxDisclosures)
	}
	for _, sentence := range r.Disclosures {
		if strings.TrimSpace(sentence) == "" || len(sentence) > maxDisclosureLength {
			return errors.New("technique result carries an empty or long disclosure")
		}
	}
	for _, name := range r.RequiredEvidence {
		if !tokenPattern.MatchString(name) {
			return fmt.Errorf("technique result requires evidence %q, not a lowercase token", name)
		}
	}
	for _, item := range r.Evidence {
		if !tokenPattern.MatchString(item.Name) || !digestPattern.MatchString(item.Digest) || len(item.Summary) > 256 {
			return fmt.Errorf("technique result evidence %q needs a lowercase name, a sha256 digest, and at most 256 characters of summary", item.Name)
		}
	}
	return nil
}
