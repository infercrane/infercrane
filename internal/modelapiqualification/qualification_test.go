package modelapiqualification

import (
	"strings"
	"testing"
	"time"
)

func TestMeasureBuildsDeterministicNearestRankEvidence(t *testing.T) {
	started := time.Date(2026, 9, 2, 12, 0, 0, 0, time.FixedZone("fixture", 2*60*60))
	target := fixtureTarget()
	samples := make([]Sample, 20)
	for index := range samples {
		ttft := time.Duration(index+1) * 10 * time.Millisecond
		generation := time.Duration(index+1) * 100 * time.Millisecond
		samples[index] = Sample{
			RequestID: "request-" + string(rune('a'+index)), StartedAt: started.Add(time.Duration(index) * time.Second),
			FirstTokenAt: started.Add(time.Duration(index)*time.Second + ttft),
			CompletedAt:  started.Add(time.Duration(index)*time.Second + ttft + generation),
			InputTokens:  100, OutputTokens: 20,
		}
	}

	evidence, err := Measure(target, samples, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SchemaVersion != SchemaVersion || evidence.SampleCount != 20 {
		t.Fatalf("unexpected evidence metadata: %#v", evidence)
	}
	if evidence.TTFTP95MS != 190 {
		t.Fatalf("ttft p95=%v want=190", evidence.TTFTP95MS)
	}
	if evidence.OutputTokensPerSecondP5 != 10 {
		t.Fatalf("output tokens/s p5=%v want=10", evidence.OutputTokensPerSecondP5)
	}
	if evidence.ObservedAt.Location() != time.UTC || evidence.ValidUntil.Sub(evidence.ObservedAt) != time.Hour {
		t.Fatalf("unexpected validity window: %s to %s", evidence.ObservedAt, evidence.ValidUntil)
	}
	if !strings.HasPrefix(evidence.Digest, "sha256:") || len(evidence.Digest) != len("sha256:")+64 {
		t.Fatalf("unexpected digest %q", evidence.Digest)
	}
	if !evidence.ValidAt(evidence.ObservedAt) || evidence.ValidAt(evidence.ValidUntil) {
		t.Fatal("validity window must be half-open")
	}

	reversed := append([]Sample(nil), samples...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	second, err := Measure(target, reversed, time.Hour)
	if err != nil || second.Digest != evidence.Digest {
		t.Fatalf("sample order changed evidence digest: %q err=%v", second.Digest, err)
	}

	samples[0].OutputTokens = 999
	target.Capabilities[0] = "mutated"
	if evidence.Target.Capabilities[0] != "chat-completions" || second.Digest != evidence.Digest {
		t.Fatal("result retained caller-owned mutable data")
	}
}

func TestMeasureRejectsIncompleteOrAmbiguousSamples(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	valid := Sample{RequestID: "request", StartedAt: now, FirstTokenAt: now.Add(time.Second), CompletedAt: now.Add(2 * time.Second), OutputTokens: 1}
	tests := []struct {
		name     string
		target   Target
		samples  []Sample
		validFor time.Duration
	}{
		{name: "missing tuple", target: Target{}, samples: []Sample{valid}, validFor: time.Hour},
		{name: "no samples", target: fixtureTarget(), validFor: time.Hour},
		{name: "expired immediately", target: fixtureTarget(), samples: []Sample{valid}},
		{name: "excess validity", target: fixtureTarget(), samples: []Sample{valid}, validFor: MaximumValidity + time.Nanosecond},
		{name: "duplicate request", target: fixtureTarget(), samples: []Sample{valid, valid}, validFor: time.Hour},
		{name: "missing first token", target: fixtureTarget(), samples: []Sample{{RequestID: "request", StartedAt: now, CompletedAt: now.Add(time.Second), OutputTokens: 1}}, validFor: time.Hour},
		{name: "zero generation", target: fixtureTarget(), samples: []Sample{{RequestID: "request", StartedAt: now, FirstTokenAt: now.Add(time.Second), CompletedAt: now.Add(time.Second), OutputTokens: 1}}, validFor: time.Hour},
		{name: "missing usage", target: fixtureTarget(), samples: []Sample{{RequestID: "request", StartedAt: now, FirstTokenAt: now.Add(time.Second), CompletedAt: now.Add(2 * time.Second)}}, validFor: time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Measure(test.target, test.samples, test.validFor); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func fixtureTarget() Target {
	return Target{
		TupleKey: "deepseek|deepseek-v4-flash|chat-completions|openai|global", Supplier: "deepseek",
		Adapter: "deepseek-openai", SupplierModelID: "deepseek-v4-flash", Operation: "chat-completions",
		Protocol: "openai", Region: "global", Capabilities: []string{"streaming", "chat-completions"},
	}
}
