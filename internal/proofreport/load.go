package proofreport

import (
	"context"
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/technique"
)

// Request names the runs of one engagement and how the report is headed.
type Request struct {
	Title       string
	PreparedFor string
	CampaignIDs []string
}

// Check accepts a request: 1 to MaxRuns distinct runs, and a title and name
// of one printable line each.
func (r Request) Check() (Request, error) {
	out := Request{}
	var err error
	if out.Title, err = CheckText("title", r.Title); err != nil {
		return Request{}, err
	}
	if out.PreparedFor, err = CheckText("prepared_for", r.PreparedFor); err != nil {
		return Request{}, err
	}
	seen := map[string]bool{}
	for _, id := range r.CampaignIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if seen[id] {
			return Request{}, fmt.Errorf("%w: run %s is named twice", ErrInvalidRequest, id)
		}
		seen[id] = true
		out.CampaignIDs = append(out.CampaignIDs, id)
	}
	if len(out.CampaignIDs) == 0 || len(out.CampaignIDs) > MaxRuns {
		return Request{}, fmt.Errorf("%w: a proof report reads 1 to %d runs", ErrInvalidRequest, MaxRuns)
	}
	return out, nil
}

// Load reads each run inside the tenant and assembles the report. Every read
// goes through the campaign report's tenant-scoped store, so a report never
// reads another tenant's records. domain.ErrNotFound means a run does not
// exist for the tenant; a missing evidence record is not an error, the field
// that needs it stays unknown.
func Load(ctx context.Context, store optimizationreport.Store, tenant string, request Request) (Report, error) {
	request, err := request.Check()
	if err != nil {
		return Report{}, err
	}
	in := Input{Title: request.Title, PreparedFor: request.PreparedFor}
	for _, id := range request.CampaignIDs {
		report, err := optimizationreport.Load(ctx, store, tenant, id)
		if err != nil {
			return Report{}, err
		}
		campaign, err := store.OptimizationCampaign(ctx, tenant, id)
		if err != nil {
			return Report{}, err
		}
		evidence := RunEvidence{Campaign: campaign, Report: report, Claim: technique.ClaimNotShown}
		for _, run := range campaign.Candidates {
			if report.Outcome.Kind != optimizationreport.OutcomeWinnerSelected || run.ID != report.Outcome.WinnerCandidateID {
				continue
			}
			if run.QualityEvidenceID != "" {
				rows, err := store.QualityEvidenceByIDs(ctx, tenant, []string{run.QualityEvidenceID})
				if err != nil {
					return Report{}, fmt.Errorf("read the chosen setup's quality evidence: %w", err)
				}
				if row, found := rows[run.QualityEvidenceID]; found {
					evidence.Answers = &row
				}
			}
			evidence.Result, evidence.Claim = optimizationreport.SetupResult(campaign, run, evidence.Answers)
		}
		in.Runs = append(in.Runs, evidence)
	}
	return Assemble(in), nil
}
