// Package resultmetrics turns a workspace's persisted optimization results
// into Prometheus series: Release Guard decisions and quality verdicts,
// post-promotion observation, rollbacks, endpoint traffic shares, and the
// latest optimization run per endpoint.
//
// Every label value comes from a fixed list or from a name the workspace
// chose (deployment, endpoint, binding), so a workspace has a bounded number
// of series. A value outside a list is dropped rather than exported. Missing
// evidence is never written as zero: a gauge whose value is unknown has no
// series. Counters start at zero, because a zero count of stored records is
// a measured value.
package resultmetrics

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion names the JSON form of a Snapshot.
const SchemaVersion = "infercrane.result-metrics/v1"

// MaxSubjects bounds how many deployments, endpoints, and optimization runs
// one snapshot carries. Above it, the snapshot keeps the first names in
// alphabetical order and sets Truncated.
const MaxSubjects = 500

// Fixed label values. A value not listed here is never exported.
var (
	GuardDecisions         = []string{"ACCEPT", "REJECT", "WAIT"}
	QualityVerdicts        = []string{"PASS", "WATCH", "BLOCK", "UNKNOWN"}
	EndpointGuardDecisions = []string{"PASS", "REJECT", "INCONCLUSIVE"}
	ObservationStates      = []string{"observing", "accepted", "rolled_back", "failed"}
	RollbackTriggers       = []string{"automatic", "operator"}
	RoutingPolicies        = []string{"manual", "primary-fallback", "weighted"}
	BindingKinds           = []string{"deployment", "external"}
	CampaignStates         = []string{"awaiting_approval", "approved", "running", "ranked", "qualified", "guard_passed", "rejected", "inconclusive", "promoted", "observed", "cancelled", "failed", "cleaned"}
	CampaignOutcomes       = []string{"winner_selected", "baseline_kept", "no_winner", "in_progress", "cancelled"}
)

// Snapshot is one workspace's results at GeneratedAt.
type Snapshot struct {
	SchemaVersion string    `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	// Truncated is true when the workspace has more than MaxSubjects
	// deployments, endpoints, or optimized endpoints.
	Truncated   bool         `json:"truncated"`
	Deployments []Deployment `json:"deployments"`
	Endpoints   []Endpoint   `json:"endpoints"`
	// Campaigns holds the latest optimization run of each endpoint a run
	// targeted.
	Campaigns []Campaign `json:"campaigns"`
	// CampaignStates counts the workspace's optimization runs by state.
	CampaignStates map[string]int `json:"campaign_states"`
}

// Deployment is the release history of one deployment.
type Deployment struct {
	Name string `json:"name"`
	// LatestGuard is the newest Release Guard evaluation of a candidate
	// revision; nil when none was ever recorded.
	LatestGuard *GuardResult `json:"latest_guard"`
	// GuardDecisions and QualityVerdicts count every stored evaluation.
	GuardDecisions  map[string]int `json:"guard_decisions"`
	QualityVerdicts map[string]int `json:"quality_verdicts"`
	// Observation is the newest post-promotion observation window; nil when
	// no promotion was observed.
	Observation    *Observation   `json:"observation"`
	Rollbacks      map[string]int `json:"rollbacks"`
	LastRollbackAt *time.Time     `json:"last_rollback_at"`
}

// GuardResult is one Release Guard evaluation of a deployment candidate.
type GuardResult struct {
	Decision string `json:"decision"`
	// QualityVerdict is nil when the policy does not gate on answer quality.
	QualityVerdict *string   `json:"quality_verdict"`
	EvaluatedAt    time.Time `json:"evaluated_at"`
}

// Observation is a post-promotion window that rolls back automatically
// when the promoted revision regresses.
type Observation struct {
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Endpoint is the routing and endpoint Release Guard state of one endpoint.
type Endpoint struct {
	Name string `json:"name"`
	// RoutingPolicy is empty when the endpoint has no active serving plan.
	RoutingPolicy string         `json:"routing_policy"`
	Bindings      []BindingShare `json:"bindings"`
	// CandidateStaged is true while a candidate serving plan waits for the
	// endpoint Release Guard and a promotion.
	CandidateStaged bool                 `json:"candidate_staged"`
	LatestGuard     *EndpointGuardResult `json:"latest_guard"`
	GuardDecisions  map[string]int       `json:"guard_decisions"`
}

// BindingShare is the configured share of new requests the active serving
// plan sends to one binding: its weight over the plan's total for a weighted
// plan, and all of it to the first binding otherwise. Fallback traffic after
// a failure is not included.
type BindingShare struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Share float64 `json:"share"`
}

// EndpointGuardResult is one endpoint Release Guard evaluation.
type EndpointGuardResult struct {
	Decision    string    `json:"decision"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// Campaign is the latest optimization run that targeted an endpoint.
type Campaign struct {
	Endpoint  string    `json:"endpoint"`
	ID        string    `json:"id"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
	// Outcome and the costs come from the run's report. They are nil until
	// the report has been read, and a cost stays nil when the report does
	// not know it.
	Outcome                               *string  `json:"outcome"`
	BaselineCostPerMillionOutputTokensUSD *float64 `json:"baseline_cost_per_million_output_tokens_usd"`
	ChosenCostPerMillionOutputTokensUSD   *float64 `json:"chosen_cost_per_million_output_tokens_usd"`
}

// PlanBinding is one binding of a serving plan as stored.
type PlanBinding struct {
	Name     string
	Kind     string
	Priority int
	Weight   int
}

// Shares returns the configured share of new requests for each binding of
// a serving plan. A weighted plan splits by weight. Manual and
// primary-fallback plans send every new request to the binding with the
// lowest priority number; the others receive traffic only after a failure,
// which a configured share does not describe. An unknown policy or a plan
// without positive weights returns nil.
func Shares(policy string, bindings []PlanBinding) []BindingShare {
	if len(bindings) == 0 {
		return nil
	}
	out := make([]BindingShare, 0, len(bindings))
	switch policy {
	case "weighted":
		total := 0
		for _, b := range bindings {
			if b.Weight > 0 {
				total += b.Weight
			}
		}
		if total <= 0 {
			return nil
		}
		for _, b := range bindings {
			weight := max(b.Weight, 0)
			out = append(out, BindingShare{Name: b.Name, Kind: b.Kind, Share: float64(weight) / float64(total)})
		}
	case "manual", "primary-fallback":
		first := 0
		for i, b := range bindings {
			if b.Priority < bindings[first].Priority {
				first = i
			}
		}
		for i, b := range bindings {
			out = append(out, BindingShare{Name: b.Name, Kind: b.Kind, Share: boolean(i == first)})
		}
	default:
		return nil
	}
	return out
}

// Counts returns a map with every listed value at zero, so a counter series
// exists before its first increment.
func Counts(values []string) map[string]int {
	out := make(map[string]int, len(values))
	for _, value := range values {
		out[value] = 0
	}
	return out
}

// Listed reports whether value is one of values.
func Listed(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

type family struct {
	name, kind, help string
	samples          []sample
}

type sample struct {
	labels [][2]string
	value  float64
}

func (f *family) add(value float64, labels ...string) {
	pairs := make([][2]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		pairs = append(pairs, [2]string{labels[i], labels[i+1]})
	}
	f.samples = append(f.samples, sample{labels: pairs, value: value})
}

// WritePrometheus writes the snapshot in the Prometheus text exposition
// format, version 0.0.4.
func WritePrometheus(w io.Writer, s Snapshot) error {
	families := buildFamilies(s)
	out := bufio.NewWriter(w)
	for _, f := range families {
		out.WriteString("# HELP " + f.name + " " + f.help + "\n")
		out.WriteString("# TYPE " + f.name + " " + f.kind + "\n")
		for _, item := range f.samples {
			out.WriteString(f.name)
			if len(item.labels) > 0 {
				out.WriteByte('{')
				for i, pair := range item.labels {
					if i > 0 {
						out.WriteByte(',')
					}
					out.WriteString(pair[0] + `="` + escapeLabel(pair[1]) + `"`)
				}
				out.WriteByte('}')
			}
			out.WriteString(" " + strconv.FormatFloat(item.value, 'f', -1, 64) + "\n")
		}
	}
	return out.Flush()
}

func buildFamilies(s Snapshot) []*family {
	guardDecision := &family{name: "infercrane_release_guard_decision", kind: "gauge", help: "Latest Release Guard decision on a deployment's candidate revision, 1 on the current decision. No series: never evaluated."}
	qualityVerdict := &family{name: "infercrane_release_guard_quality_verdict", kind: "gauge", help: "Quality verdict of the latest Release Guard evaluation, 1 on the current verdict. No series: never evaluated, or the policy does not gate on quality."}
	guardTime := &family{name: "infercrane_release_guard_evaluation_timestamp_seconds", kind: "gauge", help: "When the latest Release Guard evaluation of a deployment was recorded."}
	guardDecisions := &family{name: "infercrane_release_guard_decisions_total", kind: "counter", help: "Stored Release Guard evaluations of a deployment by decision."}
	qualityVerdicts := &family{name: "infercrane_release_guard_quality_verdicts_total", kind: "counter", help: "Stored Release Guard evaluations of a deployment by quality verdict."}
	observation := &family{name: "infercrane_release_observation_state", kind: "gauge", help: "State of the latest post-promotion observation window, 1 on the current state. No series: no promotion was observed."}
	rollbacks := &family{name: "infercrane_rollbacks_total", kind: "counter", help: "Rollbacks of a deployment to a retained revision, by trigger: automatic (Release Guard observation) or operator."}
	rollbackTime := &family{name: "infercrane_rollback_timestamp_seconds", kind: "gauge", help: "When the deployment last rolled back. No series: it never rolled back."}
	endpointDecision := &family{name: "infercrane_endpoint_guard_decision", kind: "gauge", help: "Latest endpoint Release Guard decision on a candidate serving plan, 1 on the current decision. No series: never evaluated."}
	endpointTime := &family{name: "infercrane_endpoint_guard_evaluation_timestamp_seconds", kind: "gauge", help: "When the latest endpoint Release Guard evaluation was recorded."}
	endpointDecisions := &family{name: "infercrane_endpoint_guard_decisions_total", kind: "counter", help: "Stored endpoint Release Guard evaluations by decision."}
	routing := &family{name: "infercrane_endpoint_routing_policy", kind: "gauge", help: "Routing policy of the endpoint's active serving plan, 1 on the current policy. A weighted plan splits traffic, as a canary does."}
	share := &family{name: "infercrane_endpoint_traffic_share", kind: "gauge", help: "Configured share of new requests (0 to 1) the active serving plan sends to a binding. Fallback traffic after a failure is not included."}
	staged := &family{name: "infercrane_endpoint_candidate_staged", kind: "gauge", help: "1 while a candidate serving plan waits for the endpoint Release Guard and a promotion, else 0."}
	campaignState := &family{name: "infercrane_campaign_state", kind: "gauge", help: "State of the latest optimization run that targeted the endpoint, 1 on the current state."}
	campaignOutcome := &family{name: "infercrane_campaign_outcome", kind: "gauge", help: "Outcome in the report of the latest optimization run that targeted the endpoint, 1 on the current outcome. No series: the report has not been read yet."}
	campaignTime := &family{name: "infercrane_campaign_updated_timestamp_seconds", kind: "gauge", help: "When the latest optimization run that targeted the endpoint last changed."}
	campaignCost := &family{name: "infercrane_campaign_cost_per_million_output_tokens_usd", kind: "gauge", help: "Cost per million output tokens measured by the latest optimization run, for the baseline and the chosen setup. No series: the run's report does not know it."}
	campaigns := &family{name: "infercrane_campaigns", kind: "gauge", help: "Optimization runs in the workspace by state."}
	truncated := &family{name: "infercrane_results_truncated", kind: "gauge", help: "1 when the workspace has more deployments, endpoints, or optimized endpoints than one scrape exports (500 each), else 0."}
	generated := &family{name: "infercrane_results_generated_timestamp_seconds", kind: "gauge", help: "When the control plane read these results."}

	deployments := append([]Deployment(nil), s.Deployments...)
	sort.Slice(deployments, func(i, j int) bool { return deployments[i].Name < deployments[j].Name })
	for _, d := range deployments {
		if d.Name == "" {
			continue
		}
		if g := d.LatestGuard; g != nil && Listed(GuardDecisions, g.Decision) {
			guardDecision.add(1, "deployment", d.Name, "decision", g.Decision)
			if g.QualityVerdict != nil && Listed(QualityVerdicts, *g.QualityVerdict) {
				qualityVerdict.add(1, "deployment", d.Name, "verdict", *g.QualityVerdict)
			}
			if !g.EvaluatedAt.IsZero() {
				guardTime.add(seconds(g.EvaluatedAt), "deployment", d.Name)
			}
		}
		for _, decision := range GuardDecisions {
			guardDecisions.add(float64(d.GuardDecisions[decision]), "deployment", d.Name, "decision", decision)
		}
		for _, verdict := range QualityVerdicts {
			qualityVerdicts.add(float64(d.QualityVerdicts[verdict]), "deployment", d.Name, "verdict", verdict)
		}
		if o := d.Observation; o != nil && Listed(ObservationStates, o.State) {
			observation.add(1, "deployment", d.Name, "state", o.State)
		}
		for _, trigger := range RollbackTriggers {
			rollbacks.add(float64(d.Rollbacks[trigger]), "deployment", d.Name, "trigger", trigger)
		}
		if d.LastRollbackAt != nil && !d.LastRollbackAt.IsZero() {
			rollbackTime.add(seconds(*d.LastRollbackAt), "deployment", d.Name)
		}
	}

	endpoints := append([]Endpoint(nil), s.Endpoints...)
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].Name < endpoints[j].Name })
	for _, e := range endpoints {
		if e.Name == "" {
			continue
		}
		if g := e.LatestGuard; g != nil && Listed(EndpointGuardDecisions, g.Decision) {
			endpointDecision.add(1, "endpoint", e.Name, "decision", g.Decision)
			if !g.EvaluatedAt.IsZero() {
				endpointTime.add(seconds(g.EvaluatedAt), "endpoint", e.Name)
			}
		}
		for _, decision := range EndpointGuardDecisions {
			endpointDecisions.add(float64(e.GuardDecisions[decision]), "endpoint", e.Name, "decision", decision)
		}
		if Listed(RoutingPolicies, e.RoutingPolicy) {
			routing.add(1, "endpoint", e.Name, "policy", e.RoutingPolicy)
			bindings := append([]BindingShare(nil), e.Bindings...)
			sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
			for _, b := range bindings {
				if b.Name == "" || !Listed(BindingKinds, b.Kind) || math.IsNaN(b.Share) || b.Share < 0 || b.Share > 1 {
					continue
				}
				share.add(b.Share, "endpoint", e.Name, "binding", b.Name, "kind", b.Kind)
			}
		}
		staged.add(boolean(e.CandidateStaged), "endpoint", e.Name)
	}

	runs := append([]Campaign(nil), s.Campaigns...)
	sort.Slice(runs, func(i, j int) bool { return runs[i].Endpoint < runs[j].Endpoint })
	for _, c := range runs {
		if c.Endpoint == "" || !Listed(CampaignStates, c.State) {
			continue
		}
		campaignState.add(1, "endpoint", c.Endpoint, "state", c.State)
		if c.Outcome != nil && Listed(CampaignOutcomes, *c.Outcome) {
			campaignOutcome.add(1, "endpoint", c.Endpoint, "outcome", *c.Outcome)
		}
		if !c.UpdatedAt.IsZero() {
			campaignTime.add(seconds(c.UpdatedAt), "endpoint", c.Endpoint)
		}
		if cost, ok := usd(c.BaselineCostPerMillionOutputTokensUSD); ok {
			campaignCost.add(cost, "endpoint", c.Endpoint, "setup", "baseline")
		}
		if cost, ok := usd(c.ChosenCostPerMillionOutputTokensUSD); ok {
			campaignCost.add(cost, "endpoint", c.Endpoint, "setup", "chosen")
		}
	}
	for _, state := range CampaignStates {
		campaigns.add(float64(s.CampaignStates[state]), "state", state)
	}
	truncated.add(boolean(s.Truncated))
	if !s.GeneratedAt.IsZero() {
		generated.add(seconds(s.GeneratedAt))
	}
	return []*family{guardDecision, qualityVerdict, guardTime, guardDecisions, qualityVerdicts, observation, rollbacks, rollbackTime, endpointDecision, endpointTime, endpointDecisions, routing, share, staged, campaignState, campaignOutcome, campaignTime, campaignCost, campaigns, truncated, generated}
}

func usd(value *float64) (float64, bool) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, false
	}
	return *value, true
}

func seconds(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func boolean(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(value string) string { return labelEscaper.Replace(value) }
