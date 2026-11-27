package optimizationreport

import (
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/bandverdict"
	"github.com/infercrane/infercrane/internal/domain"
)

// studyBand is the winner fixture as one campaign of a study at a load.
func studyBand(t *testing.T, id string, load int) (Report, domain.OptimizationCampaign) {
	t.Helper()
	evidence := winnerFixture().evidence()
	evidence.Campaign.ID = id
	report := Assemble(evidence)
	report.CampaignID, report.Workload.Concurrency = id, &load
	if report.Outcome.Kind != OutcomeWinnerSelected || report.Outcome.WinnerCandidateID == "" {
		t.Fatalf("the winner fixture has no winner: %+v", report.Outcome)
	}
	return report, evidence.Campaign
}

func TestABandStudyReadsEachCampaignAsItsLoadBand(t *testing.T) {
	light, lightCampaign := studyBand(t, "campaign-16", 16)
	heavy, heavyCampaign := studyBand(t, "campaign-64", 64)
	heavy.Outcome.Kind, heavy.Outcome.WinnerCandidateID, heavy.Outcome.Headline = OutcomeBaselineKept, "", "kept"
	study, err := BandStudyOf([]Report{heavy, light}, []domain.OptimizationCampaign{heavyCampaign, lightCampaign})
	if err != nil {
		t.Fatal(err)
	}
	verdict := study.Verdict
	if verdict.Summary != bandverdict.SummaryPerBand || len(verdict.Bands) != 2 || verdict.Bands[0].Load != 16 || verdict.Bands[1].Load != 64 {
		t.Fatalf("verdict = %+v", verdict)
	}
	winner := verdict.Bands[0].Winner
	if !strings.HasPrefix(winner, "setup-") || winner == study.Current || verdict.Bands[1].Decision != bandverdict.DecisionKeep {
		t.Fatalf("bands = %+v current=%s", verdict.Bands, study.Current)
	}
	if len(verdict.SwitchBands) != 2 || verdict.SwitchBands[0] != (bandverdict.SwitchBand{UpTo: 16, Engine: winner}) || verdict.SwitchBands[1] != (bandverdict.SwitchBand{Engine: study.Current}) {
		t.Fatalf("table = %+v", verdict.SwitchBands)
	}
	if study.Setups[winner].Label == "" || study.Setups[study.Current].Engine == "" || len(study.Campaigns) != 2 || study.SchemaVersion != BandStudySchema {
		t.Fatalf("study = %+v", study)
	}
	// The same setup has one key in both campaigns, whatever its candidate IDs.
	if heavyKey, _, _ := setupKey(heavyCampaign.Candidates[1]); heavyKey != func() string { key, _, _ := setupKey(lightCampaign.Candidates[1]); return key }() {
		t.Fatal("one setup got two keys")
	}
}

func TestABandStudyRefusesCampaignsThatAreNotOneStudy(t *testing.T) {
	light, lightCampaign := studyBand(t, "campaign-16", 16)
	other, otherCampaign := studyBand(t, "campaign-64", 64)
	other.ModelIdentity = "acme/another-model"
	if _, err := BandStudyOf([]Report{light, other}, []domain.OptimizationCampaign{lightCampaign, otherCampaign}); !errors.Is(err, ErrStudyMismatch) {
		t.Fatalf("another model: %v", err)
	}
	twice, twiceCampaign := studyBand(t, "campaign-16b", 16)
	if _, err := BandStudyOf([]Report{light, twice}, []domain.OptimizationCampaign{lightCampaign, twiceCampaign}); !errors.Is(err, ErrStudyMismatch) || !strings.Contains(err.Error(), "16 requests at once") {
		t.Fatalf("one load twice: %v", err)
	}
	ranked, rankedCampaign := studyBand(t, "campaign-32", 32)
	ranked.ObjectiveMetric = "ttft_p95_ms"
	if _, err := BandStudyOf([]Report{light, ranked}, []domain.OptimizationCampaign{lightCampaign, rankedCampaign}); !errors.Is(err, ErrStudyMismatch) {
		t.Fatalf("another objective: %v", err)
	}
}

func TestABandStudyWithARunningCampaignIsIncomplete(t *testing.T) {
	light, lightCampaign := studyBand(t, "campaign-16", 16)
	running, runningCampaign := studyBand(t, "campaign-64", 64)
	running.Outcome.Kind = OutcomeInProgress
	study, err := BandStudyOf([]Report{light, running}, []domain.OptimizationCampaign{lightCampaign, runningCampaign})
	if err != nil {
		t.Fatal(err)
	}
	if study.Verdict.Summary != bandverdict.SummaryIncomplete || study.Verdict.SwitchBands != nil || study.Verdict.Bands[1].Decision != bandverdict.DecisionUnknown {
		t.Fatalf("verdict = %+v", study.Verdict)
	}
}
