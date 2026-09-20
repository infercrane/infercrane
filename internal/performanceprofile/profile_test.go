package performanceprofile

import "testing"

func TestProfilesAreBoundedAndCoverDistinctObjectives(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range Names() {
		profile, err := Get(name)
		if err != nil || profile.Name != name || profile.Requests < profile.Concurrency || profile.Concurrency < 1 || profile.InputTokens < 1 || profile.OutputTokens < 1 || profile.Requests > 10_000 {
			t.Fatalf("profile=%#v err=%v", profile, err)
		}
		seen[profile.Objective] = true
	}
	for _, objective := range []string{"latency", "throughput", "long_context", "long_generation", "bounded_overload"} {
		if !seen[objective] {
			t.Fatalf("missing objective %q", objective)
		}
	}
}

func TestPublicPriorProfilesRemainBoundedAndDiscoverable(t *testing.T) {
	for _, name := range PublicPriorNames() {
		profile, err := Get(name)
		if err != nil || !IsPublicPrior(name) || profile.InputTokens+profile.OutputTokens > 8192 {
			t.Fatalf("profile=%+v err=%v", profile, err)
		}
	}
	if IsPublicPrior("interactive") || IsPublicPrior("public-unknown") {
		t.Fatal("non-public workload profile was labeled as a public prior")
	}
}

func TestUnknownProfileFailsClosed(t *testing.T) {
	if _, err := Get("fastest"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}
