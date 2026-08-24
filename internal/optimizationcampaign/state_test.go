package optimizationcampaign

import "testing"

func TestAggregateStateIsDeterministicAcrossMixedCandidates(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		want   string
	}{
		{"work remains", []string{CandidateRejected, CandidateMeasuring}, CampaignRunning},
		{"ranked evidence", []string{CandidateRejected, CandidateRanked}, CampaignRanked},
		{"ranking persisted before guard", []string{CandidateRejected, CandidateGuarding}, CampaignRanked},
		{"qualified candidate", []string{CandidateRejected, CandidateGuardPassed}, CampaignGuardPassed},
		{"qualified new endpoint", []string{CandidateRejected, CandidateQualified}, CampaignQualified},
		{"promotion wins", []string{CandidateObserved, CandidatePromoted}, CampaignPromoted},
		{"observed result", []string{CandidateRejected, CandidateObserved}, CampaignObserved},
		{"failed terminal set", []string{CandidateRejected, CandidateFailed}, CampaignFailed},
		{"inconclusive terminal set", []string{CandidateRejected, CandidateInconclusive}, CampaignInconclusive},
		{"all cancelled", []string{CandidateCancelled, CandidateCancelled}, CampaignCancelled},
		{"rejected terminal set", []string{CandidateRejected, CandidateCancelled}, CampaignRejected},
		{"all cleaned", []string{CandidateCleaned, CandidateCleaned}, CampaignCleaned},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := AggregateState(test.states)
			if err != nil || got != test.want {
				t.Fatalf("AggregateState(%v) = %q, %v; want %q", test.states, got, err, test.want)
			}
		})
	}
	if _, err := AggregateState(nil); err == nil {
		t.Fatal("empty campaign state should fail")
	}
	if _, err := AggregateState([]string{"future_state"}); err == nil {
		t.Fatal("unknown candidate state should fail closed")
	}
}

func TestNewEndpointCanQualifyWithoutInventingReleaseGuardBaseline(t *testing.T) {
	if err := ValidateCandidateTransition(CandidateRanked, CandidateQualified); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCandidateTransition(CandidateQualified, CandidatePromoted); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCandidateTransition(CandidateProposed, CandidateQualified); err == nil {
		t.Fatal("new endpoint skipped measured and quality proof boundaries")
	}
}

func TestCandidateStateMachineRejectsSkippingProofBoundaries(t *testing.T) {
	valid := []string{CandidateProposed, CandidateProvisioning, CandidateReady, CandidateMeasuring, CandidateValidating, CandidateRanked, CandidateGuarding, CandidateGuardPassed, CandidatePromoted, CandidateObserved, CandidateCleaned}
	for index := 0; index < len(valid)-1; index++ {
		if err := ValidateCandidateTransition(valid[index], valid[index+1]); err != nil {
			t.Fatalf("valid transition rejected: %v", err)
		}
	}
	for _, transition := range [][2]string{{CandidateProposed, CandidateGuardPassed}, {CandidateMeasuring, CandidatePromoted}, {CandidateRejected, CandidatePromoted}, {CandidateCleaned, CandidateProvisioning}} {
		if err := ValidateCandidateTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("illegal transition accepted: %v", transition)
		}
	}
}
