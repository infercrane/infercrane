package resultmetrics

import (
	"bytes"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\.)*")(,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\.)*")*\})? (\S+)$`)
	typeLine   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (gauge|counter)$`)
)

func ptr[T any](value T) *T { return &value }

func render(t *testing.T, s Snapshot) string {
	t.Helper()
	var out bytes.Buffer
	if err := WritePrometheus(&out, s); err != nil {
		t.Fatal(err)
	}
	validExposition(t, out.String())
	return out.String()
}

// validExposition checks every line against the text format and that each
// sample belongs to the family declared right above it.
func validExposition(t *testing.T, text string) {
	t.Helper()
	family := ""
	declared := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
		case strings.HasPrefix(line, "# TYPE "):
			match := typeLine.FindStringSubmatch(line)
			if match == nil {
				t.Fatalf("bad TYPE line %q", line)
			}
			if declared[match[1]] {
				t.Fatalf("family %s declared twice", match[1])
			}
			family, declared[match[1]] = match[1], true
		default:
			match := sampleLine.FindStringSubmatch(line)
			if match == nil {
				t.Fatalf("bad sample line %q", line)
			}
			if match[1] != family {
				t.Fatalf("sample %q outside its family %s", line, family)
			}
			if match[len(match)-1] == "NaN" {
				t.Fatalf("NaN sample %q", line)
			}
		}
	}
}

func fixture() Snapshot {
	evaluated := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	rolledBack := evaluated.Add(5 * time.Minute)
	return Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   evaluated.Add(time.Hour),
		Deployments: []Deployment{
			{
				Name:            "qwen-prod",
				LatestGuard:     &GuardResult{Decision: "REJECT", QualityVerdict: ptr("BLOCK"), EvaluatedAt: evaluated},
				GuardDecisions:  map[string]int{"ACCEPT": 3, "REJECT": 1},
				QualityVerdicts: map[string]int{"PASS": 2, "WATCH": 1, "BLOCK": 1},
				Observation:     &Observation{State: "rolled_back", UpdatedAt: rolledBack},
				Rollbacks:       map[string]int{"automatic": 1},
				LastRollbackAt:  &rolledBack,
			},
			{Name: "never-evaluated"},
		},
		Endpoints: []Endpoint{{
			Name: "support-chat", RoutingPolicy: "weighted", CandidateStaged: true,
			Bindings:       []BindingShare{{Name: "openai", Kind: "external", Share: 0.95}, {Name: "owned", Kind: "deployment", Share: 0.05}},
			LatestGuard:    &EndpointGuardResult{Decision: "PASS", EvaluatedAt: evaluated},
			GuardDecisions: map[string]int{"PASS": 1},
		}},
		Campaigns: []Campaign{{
			Endpoint: "qwen-prod", ID: "camp-1", State: "promoted", UpdatedAt: evaluated,
			Outcome: ptr("winner_selected"), BaselineCostPerMillionOutputTokensUSD: ptr(1.8), ChosenCostPerMillionOutputTokensUSD: ptr(0.9),
		}},
		CampaignStates: map[string]int{"promoted": 1, "running": 2},
	}
}

func TestWritePrometheusExportsResults(t *testing.T) {
	text := render(t, fixture())
	for _, want := range []string{
		`infercrane_release_guard_decision{deployment="qwen-prod",decision="REJECT"} 1`,
		`infercrane_release_guard_quality_verdict{deployment="qwen-prod",verdict="BLOCK"} 1`,
		`infercrane_release_guard_evaluation_timestamp_seconds{deployment="qwen-prod"} 1795687200`,
		`infercrane_release_guard_decisions_total{deployment="qwen-prod",decision="ACCEPT"} 3`,
		`infercrane_release_guard_quality_verdicts_total{deployment="qwen-prod",verdict="WATCH"} 1`,
		`infercrane_release_observation_state{deployment="qwen-prod",state="rolled_back"} 1`,
		`infercrane_rollbacks_total{deployment="qwen-prod",trigger="automatic"} 1`,
		`infercrane_rollbacks_total{deployment="qwen-prod",trigger="operator"} 0`,
		`infercrane_rollback_timestamp_seconds{deployment="qwen-prod"} 1795687500`,
		`infercrane_endpoint_guard_decision{endpoint="support-chat",decision="PASS"} 1`,
		`infercrane_endpoint_guard_decisions_total{endpoint="support-chat",decision="REJECT"} 0`,
		`infercrane_endpoint_routing_policy{endpoint="support-chat",policy="weighted"} 1`,
		`infercrane_endpoint_traffic_share{endpoint="support-chat",binding="owned",kind="deployment"} 0.05`,
		`infercrane_endpoint_traffic_share{endpoint="support-chat",binding="openai",kind="external"} 0.95`,
		`infercrane_endpoint_candidate_staged{endpoint="support-chat"} 1`,
		`infercrane_campaign_state{endpoint="qwen-prod",state="promoted"} 1`,
		`infercrane_campaign_outcome{endpoint="qwen-prod",outcome="winner_selected"} 1`,
		`infercrane_campaign_cost_per_million_output_tokens_usd{endpoint="qwen-prod",setup="baseline"} 1.8`,
		`infercrane_campaign_cost_per_million_output_tokens_usd{endpoint="qwen-prod",setup="chosen"} 0.9`,
		`infercrane_campaigns{state="running"} 2`,
		`infercrane_campaigns{state="cleaned"} 0`,
		`infercrane_results_truncated 0`,
		`infercrane_results_generated_timestamp_seconds 1795690800`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
}

// TestUnknownIsAbsentNotZero pins the truth boundary: a deployment with no
// evaluation, observation, or rollback has counters at zero (stored records
// were counted) but no decision, verdict, or timestamp series.
func TestUnknownIsAbsentNotZero(t *testing.T) {
	text := render(t, fixture())
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, `deployment="never-evaluated"`) {
			continue
		}
		if !strings.HasPrefix(line, "infercrane_release_guard_decisions_total") && !strings.HasPrefix(line, "infercrane_release_guard_quality_verdicts_total") && !strings.HasPrefix(line, "infercrane_rollbacks_total") {
			t.Errorf("unknown value exported: %s", line)
		}
		if !strings.HasSuffix(line, " 0") {
			t.Errorf("counter without records is not zero: %s", line)
		}
	}
	if got := strings.Count(text, `deployment="never-evaluated"`); got != len(GuardDecisions)+len(QualityVerdicts)+len(RollbackTriggers) {
		t.Fatalf("never-evaluated series = %d", got)
	}

	noQuality := fixture()
	noQuality.Deployments[0].LatestGuard.QualityVerdict = nil
	noQuality.Campaigns[0].Outcome, noQuality.Campaigns[0].ChosenCostPerMillionOutputTokensUSD = nil, nil
	text = render(t, noQuality)
	for _, absent := range []string{"infercrane_release_guard_quality_verdict{", "infercrane_campaign_outcome{", `setup="chosen"`} {
		if strings.Contains(text, absent) {
			t.Errorf("unknown value exported as %s", absent)
		}
	}
	if !strings.Contains(text, `setup="baseline"} 1.8`) {
		t.Error("known baseline cost dropped with the unknown chosen cost")
	}
}

func TestValuesOutsideTheListsAreDropped(t *testing.T) {
	s := fixture()
	s.Deployments[0].LatestGuard.Decision = "MAYBE"
	s.Deployments[0].Observation.State = "dreaming"
	s.Deployments[0].GuardDecisions["MAYBE"] = 7
	s.Endpoints[0].Bindings = append(s.Endpoints[0].Bindings, BindingShare{Name: "x", Kind: "satellite", Share: 0.5}, BindingShare{Name: "y", Kind: "external", Share: math.NaN()}, BindingShare{Name: "z", Kind: "external", Share: 1.5})
	s.Campaigns = append(s.Campaigns, Campaign{Endpoint: "other", State: "exploding"})
	s.Campaigns[0].BaselineCostPerMillionOutputTokensUSD = ptr(math.Inf(1))
	text := render(t, s)
	for _, unwanted := range []string{"MAYBE", "dreaming", "satellite", `binding="y"`, `binding="z"`, "exploding", `endpoint="other"`, `setup="baseline"`, "infercrane_release_guard_evaluation_timestamp_seconds{"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("exported %s:\n%s", unwanted, text)
		}
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	text := render(t, Snapshot{Deployments: []Deployment{{Name: "a\"b\\c\nd"}}})
	if !strings.Contains(text, `deployment="a\"b\\c\nd"`) {
		t.Fatalf("label not escaped:\n%s", text)
	}
}

func TestEmptySnapshotDeclaresEveryFamily(t *testing.T) {
	text := render(t, Snapshot{})
	if got := strings.Count(text, "# TYPE "); got != 21 {
		t.Fatalf("families = %d", got)
	}
	if strings.Contains(text, "infercrane_results_generated_timestamp_seconds 0") {
		t.Fatal("zero generation time exported")
	}
}

func TestSharesFollowRoutingPolicy(t *testing.T) {
	weighted := Shares("weighted", []PlanBinding{{Name: "closed", Kind: "external", Priority: 0, Weight: 19}, {Name: "owned", Kind: "deployment", Priority: 1, Weight: 1}})
	if len(weighted) != 2 || weighted[0].Share != 0.95 || weighted[1].Share != 0.05 {
		t.Fatalf("weighted = %+v", weighted)
	}
	fallback := Shares("primary-fallback", []PlanBinding{{Name: "backup", Priority: 2, Weight: 100}, {Name: "primary", Priority: 1, Weight: 100}})
	if fallback[0].Share != 0 || fallback[1].Share != 1 {
		t.Fatalf("primary-fallback = %+v", fallback)
	}
	if manual := Shares("manual", []PlanBinding{{Name: "only", Weight: 100}}); len(manual) != 1 || manual[0].Share != 1 {
		t.Fatalf("manual = %+v", manual)
	}
	if Shares("weighted", []PlanBinding{{Name: "a", Weight: 0}}) != nil || Shares("random", []PlanBinding{{Name: "a", Weight: 1}}) != nil || Shares("weighted", nil) != nil {
		t.Fatal("unknown share was invented")
	}
}

func TestCountsStartAtZero(t *testing.T) {
	counts := Counts(GuardDecisions)
	if len(counts) != 3 || counts["ACCEPT"] != 0 {
		t.Fatalf("counts = %v", counts)
	}
}
