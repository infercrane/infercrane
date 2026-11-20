package optimizationreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

func answerLength(value float64) *float64 { return &value }

// withRuns gives a measured window C X C X C X C of 100 requests per run.
func withRuns(measured *optimizationcampaign.MeasuredEvidence, controlFailed, candidateFailed int, controlTokens, candidateTokens float64) *optimizationcampaign.MeasuredEvidence {
	measured.Runs = nil
	for index := range 7 {
		if index%2 == 0 {
			measured.Runs = append(measured.Runs, optimizationcampaign.RunSummary{Arm: optimizationcampaign.ArmControl, Requests: 100, Failed: controlFailed, OutputTokensPerRequest: answerLength(controlTokens)})
		} else {
			measured.Runs = append(measured.Runs, optimizationcampaign.RunSummary{Arm: optimizationcampaign.ArmCandidate, Requests: 100, Failed: candidateFailed, OutputTokensPerRequest: answerLength(candidateTokens)})
		}
	}
	return measured
}

// guardedFixture is winnerFixture with every setup's failed requests and
// answer length recorded. The winner answered as fully as the current setup;
// "same" failed 4% of its requests and answered 25% shorter.
func guardedFixture() fixture {
	f := winnerFixture()
	f.arms[0].measured.Runs = []optimizationcampaign.RunSummary{
		{Arm: optimizationcampaign.ArmCandidate, Requests: 100, OutputTokensPerRequest: answerLength(200)},
		{Arm: optimizationcampaign.ArmCandidate, Requests: 100, OutputTokensPerRequest: answerLength(204)},
	}
	f.arms[1].measured = withRuns(f.arms[1].measured, 0, 0, 200, 198)
	f.arms[2].measured = withRuns(f.arms[2].measured, 0, 4, 200, 150)
	f.arms[2].failure = optimizationcampaign.FailureErrorRateRegression
	return f
}

func candidateByID(t *testing.T, report Report, id string) CandidateResult {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.ID == id {
			return candidate
		}
	}
	t.Fatalf("report has no candidate %s", id)
	return CandidateResult{}
}

func TestReportShowsFailedRequestsAndAnswerLengthAndWarnsWhenAGuardFired(t *testing.T) {
	report := Assemble(guardedFixture().evidence())
	control, winner, held := candidateByID(t, report, "control"), candidateByID(t, report, "winner"), candidateByID(t, report, "same")
	if !near(control.ErrorRatePercent, 0) || !near(control.OutputTokensPerRequest, 202) || len(control.Guards) != 0 {
		t.Fatalf("baseline = %+v", control)
	}
	if !near(winner.ErrorRatePercent, 0) || !near(winner.OutputTokensPerRequest, 198) || len(winner.Guards) != 0 {
		t.Fatalf("winner = %+v", winner)
	}
	if !near(held.ErrorRatePercent, 4) || !near(held.OutputTokensPerRequest, 150) || len(held.Guards) != 2 {
		t.Fatalf("held-back setup = %+v", held)
	}
	failed, shorter := held.Guards[0], held.Guards[1]
	if failed.Code != optimizationcampaign.FailureErrorRateRegression || failed.Metric != "error_rate_percent" || failed.Baseline != 0 || failed.Value != 4 || failed.Basis != optimizationcampaign.GuardBasisSameWindow {
		t.Fatalf("failed-request guard = %+v", failed)
	}
	if shorter.Code != optimizationcampaign.FailureOutputLengthRegression || shorter.Metric != "output_tokens_per_request" || shorter.Baseline != 200 || shorter.Value != 150 {
		t.Fatalf("answer-length guard = %+v", shorter)
	}
	want := "was held back: failed 4.0% of requests; the current setup failed 0.0% · answered 25% shorter than the current setup; a speed or cost gain may come from shorter answers."
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], want) {
		t.Fatalf("warnings = %q", report.Warnings)
	}

	text := Text(Document{Report: report})
	orderedIn(t, text, report.Outcome.Headline, "Warning: “", want, "Cost per 1M output tokens", "Every setup measured", "server removed · failed 4.0% of requests; the current setup failed 0.0%", "4.0% of requests failed · 150 output tokens per request")
	if !strings.Contains(text, "0.0% of requests failed · 198 output tokens per request") {
		t.Fatalf("text lacks the winner's answers:\n%s", text)
	}
	page, err := HTML(Document{Report: report})
	if err != nil || !strings.Contains(string(page), `<p class="warning" role="note">`) {
		t.Fatalf("html lacks the warning: err=%v", err)
	}

	// A chosen setup ranked before the guards existed is not hidden: the
	// report says its gain may come from its failed requests.
	f := guardedFixture()
	f.arms[1].measured = withRuns(f.arms[1].measured, 0, 3, 200, 198)
	report = Assemble(f.evidence())
	if len(report.Warnings) != 2 || !strings.Contains(report.Warnings[0], "”, the chosen setup, failed 3.0% of requests; the current setup failed 0.0%. Part of its gain may come from that.") {
		t.Fatalf("warnings = %q", report.Warnings)
	}
}

// TestReportKeepsUnrecordedFailuresAndAnswerLengthUnknown covers evidence
// stored before answer length was recorded: nothing reads as zero, and no
// guard or warning is invented.
func TestReportKeepsUnrecordedFailuresAndAnswerLengthUnknown(t *testing.T) {
	report := Assemble(winnerFixture().evidence())
	for _, candidate := range report.Candidates {
		if candidate.ErrorRatePercent != nil || candidate.OutputTokensPerRequest != nil || len(candidate.Guards) != 0 {
			t.Fatalf("unrecorded evidence was filled in: %+v", candidate)
		}
	}
	if report.Warnings == nil || len(report.Warnings) != 0 {
		t.Fatalf("warnings = %#v", report.Warnings)
	}
	encoded, err := json.Marshal(report)
	if err != nil || !strings.Contains(string(encoded), `"error_rate_percent":null`) || !strings.Contains(string(encoded), `"output_tokens_per_request":null`) || !strings.Contains(string(encoded), `"warnings":[]`) {
		t.Fatalf("unknown values were not null: %s err=%v", encoded, err)
	}
	if strings.Contains(Text(Document{Report: report}), "of requests failed") {
		t.Fatal("text stated a failure rate that was not recorded")
	}

	// Older rows without runs still give a failure rate from their pooled
	// counts; the answer length stays unknown.
	evidence := guardedFixture().evidence()
	for id, row := range evidence.Benchmarks {
		var document map[string]any
		_ = json.Unmarshal([]byte(row.WorkloadJSON), &document)
		measured := document["measured"].(map[string]any)
		delete(measured, "runs")
		encoded, _ := json.Marshal(document)
		row.WorkloadJSON, row.RequestCount, row.Failed = string(encoded), 300, 0
		evidence.Benchmarks[id] = row
	}
	report = Assemble(evidence)
	if held := candidateByID(t, report, "same"); !near(held.ErrorRatePercent, 0) || held.OutputTokensPerRequest != nil || len(held.Guards) != 0 {
		t.Fatalf("pooled fallback = %+v", held)
	}
}

func cutOffTrace(limitReached *bool, arrivals ...int) *domain.ReplayTrace {
	shapes := []map[string]any{{"operation": "embeddings", "arrival_ms": arrivals[0]}}
	for index, output := range []int{400, 600} {
		shapes = append(shapes, map[string]any{"operation": "chat", "arrival_ms": arrivals[index+1], "input_tokens": 100, "output_tokens": output})
	}
	encoded, _ := json.Marshal(shapes)
	start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	return &domain.ReplayTrace{ID: "trace-1", ShapeJSON: string(encoded), ShapeDigest: traceDigest, RequestCount: len(shapes), WindowStart: start, WindowEnd: start.Add(time.Hour), RequestLimitReached: limitReached}
}

func TestReportProjectsACutOffCaptureOverItsKeptRequests(t *testing.T) {
	reached, complete := true, false
	t.Run("cut off at its limit", func(t *testing.T) {
		// The capture kept its newest three requests, which arrived over ten
		// minutes of a one-hour window. Dividing by the hour would project
		// one sixth of the traffic.
		f := winnerFixture()
		f.replay = cutOffTrace(&reached, 3_000_000, 3_300_000, 3_600_000)
		report := Assemble(f.evidence())
		monthly := report.Monthly
		if monthly.Basis != BasisReplayVolume || !near(monthly.WindowSeconds, 600) || !near(monthly.CaptureWindowSeconds, 3600) || monthly.CaptureCutOff == nil || !*monthly.CaptureCutOff || !near(monthly.MonthlyOutputTokens, 1000.0/600*730*3600) {
			t.Fatalf("monthly = %+v", monthly)
		}
		uncut := Assemble(winnerFixture().evidence()).Monthly
		if !near(monthly.BaselineUSD, 6**uncut.BaselineUSD) || !near(uncut.WindowSeconds, 3600) {
			t.Fatalf("cut off %+v against complete %+v", monthly, uncut)
		}
		if strings.Contains(strings.Join(report.Unknown, ","), "monthly") {
			t.Fatalf("unknown = %v", report.Unknown)
		}
		if text := Text(Document{Report: report}); !strings.Contains(text, "Capture: it reached its request limit and kept only the newest requests, so the projection uses the 10m 0s from the first to the last kept request, not the whole 1h 0m window.") {
			t.Fatalf("text does not say the capture was cut off:\n%s", text)
		}
	})
	t.Run("cut off with no time between kept requests", func(t *testing.T) {
		f := winnerFixture()
		f.replay = cutOffTrace(&reached, 5000, 5000, 5000)
		report := Assemble(f.evidence())
		if report.Monthly.Basis != BasisUnknown || report.Monthly.MonthlyOutputTokens != nil || !strings.Contains(report.Monthly.Reason, "arrived at one instant") {
			t.Fatalf("monthly = %+v", report.Monthly)
		}
		assertUnknown(t, report, "monthly")
	})
	t.Run("kept every request", func(t *testing.T) {
		f := winnerFixture()
		f.replay = cutOffTrace(&complete, 3_000_000, 3_300_000, 3_600_000)
		monthly := Assemble(f.evidence()).Monthly
		if !near(monthly.WindowSeconds, 3600) || monthly.CaptureCutOff == nil || *monthly.CaptureCutOff || !near(monthly.MonthlyOutputTokens, 730_000) {
			t.Fatalf("monthly = %+v", monthly)
		}
	})
	t.Run("captured before the cut-off was recorded", func(t *testing.T) {
		f := winnerFixture()
		f.replay = cutOffTrace(nil, 3_000_000, 3_300_000, 3_600_000)
		report := Assemble(f.evidence())
		if !near(report.Monthly.WindowSeconds, 3600) || report.Monthly.CaptureCutOff != nil {
			t.Fatalf("monthly = %+v", report.Monthly)
		}
		assertUnknown(t, report, "monthly.capture_cut_off")
		if text := Text(Document{Report: report}); !strings.Contains(text, "Monthly cost: the capture did not record whether it reached its request limit; if it did, the projection is too low") {
			t.Fatalf("text does not explain the unknown cut-off:\n%s", text)
		}
	})
}
