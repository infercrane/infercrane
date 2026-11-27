package proofgate_test

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/proofgate"
	"github.com/infercrane/infercrane/internal/testtools/proofgatefixture"
)

func signingKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// verified decides a scenario's signed report the way the CLI does: it
// verifies the saved bytes first.
func verified(t *testing.T, scenario string, policy proofgate.Policy) proofgate.Result {
	t.Helper()
	document, err := proofgatefixture.Document(scenario, signingKey(t))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	signature := proofgate.Signature{UnsignedReason: document.UnsignedReason}
	if document.Signature != nil {
		checked, verifyErr := optimizationreport.Verify(raw, nil)
		if verifyErr != nil {
			t.Fatalf("%s: fixture report does not verify: %v", scenario, verifyErr)
		}
		signature = proofgate.Signature{Present: true, Verified: true, KeyID: checked.KeyID}
	}
	return proofgate.Decide(document, signature, policy)
}

func TestEveryScenarioGetsItsVerdictAndConclusion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		scenario, verdict, conclusion, reason string
	}{
		{proofgatefixture.Pass, proofgate.Pass, proofgate.ConclusionSuccess, "no measurable difference"},
		{proofgatefixture.Gain, proofgate.Pass, proofgate.ConclusionSuccess, "on fresh servers"},
		{proofgatefixture.Watch, proofgate.Watch, proofgate.ConclusionNeutral, "watch band"},
		{proofgatefixture.Block, proofgate.Block, proofgate.ConclusionFailure, "+14.1% against a 5.1% noise floor"},
		{proofgatefixture.AnswersBlocked, proofgate.Block, proofgate.ConclusionFailure, "answer check blocked"},
		{proofgatefixture.Unknown, proofgate.Unknown, proofgate.ConclusionFailure, "drifted"},
		{proofgatefixture.Running, proofgate.Unknown, proofgate.ConclusionFailure, "has not finished"},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			t.Parallel()
			result := verified(t, tc.scenario, proofgate.Policy{})
			if result.Verdict != tc.verdict || result.Conclusion != tc.conclusion {
				t.Fatalf("verdict %s/%s, want %s/%s; reasons %v", result.Verdict, result.Conclusion, tc.verdict, tc.conclusion, result.Reasons)
			}
			if !strings.Contains(strings.Join(result.Reasons, "\n"), tc.reason) {
				t.Fatalf("reasons %q do not mention %q", result.Reasons, tc.reason)
			}
			if result.Setup == nil || result.Setup.ID != "proposed" || result.Baseline == nil || result.Baseline.ID != "current" {
				t.Fatalf("setups %+v against %+v", result.Setup, result.Baseline)
			}
		})
	}
}

func TestOnlyAGainThatHeldOnFreshServersIsShown(t *testing.T) {
	t.Parallel()
	gain := verified(t, proofgatefixture.Gain, proofgate.Policy{})
	if gain.Speed.Claim != proofgate.ClaimShown || gain.Speed.Confirmation == nil {
		t.Fatalf("confirmed gain: claim %q, confirmation %+v", gain.Speed.Claim, gain.Speed.Confirmation)
	}
	if *gain.Speed.ChangePercent > -17.9 || *gain.Speed.ChangePercent < -18.1 || *gain.Speed.NoiseFloorPercent != 4 {
		t.Fatalf("measured change %v against noise %v", *gain.Speed.ChangePercent, *gain.Speed.NoiseFloorPercent)
	}

	document, err := proofgatefixture.Document(proofgatefixture.Gain, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range document.Report.Candidates {
		document.Report.Candidates[index].Confirmation = nil
	}
	unconfirmed := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{})
	if unconfirmed.Verdict != proofgate.Pass || unconfirmed.Speed.Claim != proofgate.ClaimNotShown {
		t.Fatalf("a gain measured once passes the regression gate but is not shown: %s, %q", unconfirmed.Verdict, unconfirmed.Speed.Claim)
	}

	for index := range document.Report.Candidates {
		if document.Report.Candidates[index].ID == "proposed" {
			document.Report.Candidates[index].Confirmation = &optimizationreport.Comparison{Metric: "ttft_p95_ms", ChangePercent: 6, NoiseFloorPercent: 4, Decision: optimizationreport.DecisionRegressed}
		}
	}
	disagree := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{})
	if disagree.Verdict != proofgate.Unknown || !strings.Contains(strings.Join(disagree.Reasons, " "), "windows disagree") {
		t.Fatalf("better then worse on fresh servers must stay unknown: %s %v", disagree.Verdict, disagree.Reasons)
	}
}

func TestWatchConclusionFollowsThePolicy(t *testing.T) {
	t.Parallel()
	for onWatch, want := range map[string]string{"": proofgate.ConclusionNeutral, "neutral": proofgate.ConclusionNeutral, "fail": proofgate.ConclusionFailure, "pass": proofgate.ConclusionSuccess} {
		result := verified(t, proofgatefixture.Watch, proofgate.Policy{OnWatch: onWatch})
		if result.Verdict != proofgate.Watch || result.Conclusion != want {
			t.Fatalf("on-watch %q: %s/%s, want WATCH/%s", onWatch, result.Verdict, result.Conclusion, want)
		}
	}
	if _, err := proofgate.NormalizeOnWatch("ignore"); err == nil {
		t.Fatal("an unknown on-watch value must be refused")
	}
	// The policy never softens anything but WATCH.
	for _, scenario := range []string{proofgatefixture.Block, proofgatefixture.Unknown} {
		if result := verified(t, scenario, proofgate.Policy{OnWatch: "pass"}); result.Conclusion != proofgate.ConclusionFailure {
			t.Fatalf("%s with on-watch pass concluded %s", scenario, result.Conclusion)
		}
	}
}

func TestUnknownNeverPasses(t *testing.T) {
	t.Parallel()
	pass, err := proofgatefixture.Document(proofgatefixture.Pass, nil)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*optimizationreport.Document) proofgate.Signature{
		"answer check missing": func(d *optimizationreport.Document) proofgate.Signature {
			setup(d).AnswerCheck = nil
			return proofgate.Signature{}
		},
		"answer check unknown": func(d *optimizationreport.Document) proofgate.Signature {
			setup(d).AnswerCheck = &optimizationreport.AnswerCheck{Verdict: proofgate.Unknown, Reason: "the answer check did not run for this setup"}
			return proofgate.Signature{}
		},
		"setup was the answer reference": func(d *optimizationreport.Document) proofgate.Signature {
			setup(d).AnswerCheck.Reference = true
			return proofgate.Signature{}
		},
		"no comparison with the current setup": func(d *optimizationreport.Document) proofgate.Signature {
			setup(d).VersusBaseline = nil
			return proofgate.Signature{}
		},
		"stopped by infrastructure": func(d *optimizationreport.Document) proofgate.Signature {
			setup(d).FailureCode = "optimization_machine_lost_repeatedly"
			return proofgate.Signature{}
		},
		"no current setup": func(d *optimizationreport.Document) proofgate.Signature {
			d.Report.Candidates = d.Report.Candidates[1:]
			return proofgate.Signature{}
		},
		"unsupported schema": func(d *optimizationreport.Document) proofgate.Signature {
			d.Report.SchemaVersion = "infercrane.optimization-report/v9"
			return proofgate.Signature{}
		},
		"signature did not verify": func(*optimizationreport.Document) proofgate.Signature {
			return proofgate.Signature{Present: true, Error: "the report was changed"}
		},
	}
	for name, mutate := range mutations {
		document := clone(t, pass)
		signature := mutate(&document)
		result := proofgate.Decide(document, signature, proofgate.Policy{OnWatch: "pass"})
		if result.Verdict != proofgate.Unknown || result.Conclusion != proofgate.ConclusionFailure {
			t.Fatalf("%s: %s/%s, want UNKNOWN/failure; reasons %v", name, result.Verdict, result.Conclusion, result.Reasons)
		}
	}
	unsigned := proofgate.Decide(clone(t, pass), proofgate.Signature{UnsignedReason: optimizationreport.UnsignedNoSigningKey}, proofgate.Policy{RequireSignature: true})
	if unsigned.Verdict != proofgate.Unknown || !strings.Contains(strings.Join(unsigned.Reasons, " "), "no signing key") {
		t.Fatalf("a required signature that is missing is unknown: %s %v", unsigned.Verdict, unsigned.Reasons)
	}
	if allowed := proofgate.Decide(clone(t, pass), proofgate.Signature{UnsignedReason: optimizationreport.UnsignedNoSigningKey}, proofgate.Policy{}); allowed.Verdict != proofgate.Pass {
		t.Fatalf("an unsigned report passes when no signature is required: %s", allowed.Verdict)
	}
}

func TestARegressionBlocksEvenWhenSomethingElseIsUnknown(t *testing.T) {
	t.Parallel()
	document, err := proofgatefixture.Document(proofgatefixture.Block, nil)
	if err != nil {
		t.Fatal(err)
	}
	setup(&document).AnswerCheck = nil
	result := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{})
	if result.Verdict != proofgate.Block || !strings.Contains(result.Reasons[0], "worse beyond the measured noise") {
		t.Fatalf("a measured regression is decisive: %s %v", result.Verdict, result.Reasons)
	}
	// A guard (more failed requests, shorter answers) blocks whatever the
	// objective gained.
	pass, _ := proofgatefixture.Document(proofgatefixture.Pass, nil)
	setup(&pass).Guards = []optimizationreport.Guard{{Code: "measured_error_rate_regression", Metric: "error_rate_percent", Baseline: 0, Value: 4, Reason: "failed 4.0% of its requests against 0.0% for the current setup"}}
	if guarded := proofgate.Decide(pass, proofgate.Signature{}, proofgate.Policy{}); guarded.Verdict != proofgate.Block {
		t.Fatalf("a guard must block: %s", guarded.Verdict)
	}
}

func TestSetupSelection(t *testing.T) {
	t.Parallel()
	document, err := proofgatefixture.Document(proofgatefixture.Pass, nil)
	if err != nil {
		t.Fatal(err)
	}
	extra := *setup(&document)
	extra.ID, extra.Label = "another", "vllm, --enable-chunked-prefill"
	document.Report.Candidates = append(document.Report.Candidates, extra)
	if result := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{}); result.Verdict != proofgate.Unknown || !strings.Contains(result.Reasons[0], "name the one this change introduced") {
		t.Fatalf("several setups need a name: %s %v", result.Verdict, result.Reasons)
	}
	if result := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{Setup: "proposed"}); result.Verdict != proofgate.Pass || result.Setup.ID != "proposed" {
		t.Fatalf("named setup: %s %+v", result.Verdict, result.Setup)
	}
	if result := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{Setup: "current"}); result.Verdict != proofgate.Unknown {
		t.Fatalf("the current setup is not a change: %s", result.Verdict)
	}
	if result := proofgate.Decide(document, proofgate.Signature{}, proofgate.Policy{Setup: "missing"}); result.Verdict != proofgate.Unknown {
		t.Fatalf("a missing setup is unknown: %s", result.Verdict)
	}
}

func TestMarkdownIsOneConciseInertComment(t *testing.T) {
	t.Parallel()
	document, err := proofgatefixture.Document(proofgatefixture.Block, signingKey(t))
	if err != nil {
		t.Fatal(err)
	}
	setup(&document).Label = "vllm, --served-model-name @octocat|`x`\n<script>"
	result := proofgate.Decide(document, proofgate.Signature{Present: true, Verified: true, KeyID: document.Signature.KeyID}, proofgate.Policy{})
	result.EvidenceURL = "https://console.example/optimization/run-block"
	markdown := proofgate.Markdown(result, "deploy/qwen.yaml")
	for _, want := range []string{
		"<!-- infercrane-proof-gate:deploy/qwen.yaml -->", "### ❌ BLOCK · InferCrane proof gate",
		"+14.1% against a ±5.1% noise floor measured in the same window · **worse**",
		"| Answer check | PASS · answers matched the reference 99% across 64 prompts",
		"[Run `run-block`](https://console.example/optimization/run-block)", "signed by key `" + document.Signature.KeyID + "`",
		"infercrane optimize report verify report.json",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("comment lacks %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{"\n<script", "`x`"} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("comment carries %q from a setup label:\n%s", forbidden, markdown)
		}
	}
	if strings.Count(markdown, "@octocat") != 1 || !strings.Contains(markdown, "`vllm, --served-model-name @octocat\\|'x' <script>`") {
		t.Fatalf("a label renders only as code, where a mention is inert:\n%s", markdown)
	}
	if lines := strings.Count(markdown, "\n"); lines > 30 {
		t.Fatalf("comment has %d lines; keep it concise", lines)
	}
	if proofgate.CommentMarker("") != "<!-- infercrane-proof-gate:default -->" || proofgate.CommentMarker("a b-->") != "<!-- infercrane-proof-gate:a-b--- -->" {
		t.Fatalf("markers: %q %q", proofgate.CommentMarker(""), proofgate.CommentMarker("a b-->"))
	}
}

func setup(document *optimizationreport.Document) *optimizationreport.CandidateResult {
	for index := range document.Report.Candidates {
		if document.Report.Candidates[index].ID == "proposed" {
			return &document.Report.Candidates[index]
		}
	}
	panic("fixture has no proposed setup")
}

func clone(t *testing.T, document optimizationreport.Document) optimizationreport.Document {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var out optimizationreport.Document
	if err = json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
