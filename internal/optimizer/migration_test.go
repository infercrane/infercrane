package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func migrationRequest() Request {
	return Request{ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", WorkloadFingerprint: "sha256:" + strings.Repeat("a", 64), QualityEvalSet: "sha256:" + strings.Repeat("b", 64), Migration: &MigrationBaseline{Endpoint: "support-bot", Binding: "primary"}}
}

// The closed API is the baseline, never a candidate: the candidates still
// share one open base model, so a migration proposal passes lineage exactly
// like any other and the closed API's model never becomes a lineage subject.
func TestMigrationProposalKeepsLineageAmongCandidates(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), migrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Input.Migration == nil || proposal.Input.Migration.Dialect != MigrationDialectOpenAICompatible || proposal.Input.Migration.MaxQualityRegressionPercent != DefaultMigrationQualityMargin {
		t.Fatalf("migration defaults: %+v", proposal.Input.Migration)
	}
	if proposal.Input.WorkloadSource != WorkloadSourceCustomerObserved {
		t.Fatalf("workload source=%q", proposal.Input.WorkloadSource)
	}
	if err := ValidateProposal(proposal); err != nil {
		t.Fatalf("migration proposal refused: %v", err)
	}
	lineage, err := ProposalLineage(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lineage.Base, "support-bot") || !strings.HasPrefix(lineage.Base, proposal.Candidates[0].Deployment.Model.ID) {
		t.Fatalf("lineage base=%q", lineage.Base)
	}
	// A candidate on another open model is still refused.
	proposal.Candidates[len(proposal.Candidates)-1].Deployment.Model.ID = "Qwen/Qwen3.8-27B"
	if len(proposal.Candidates) > 1 {
		if err := ValidateProposal(proposal); !errors.Is(err, ErrDifferentBaseModel) {
			t.Fatalf("mixed open models accepted: %v", err)
		}
	}
}

func TestMigrationRequestNeedsObservedTrafficAndGradedQuality(t *testing.T) {
	for name, edit := range map[string]func(*Request){
		"no fingerprint":   func(r *Request) { r.WorkloadFingerprint = "" },
		"public profile":   func(r *Request) { r.WorkloadSource = WorkloadSourcePublicPrior },
		"no graded set":    func(r *Request) { r.QualityEvalSet = "" },
		"unknown dialect":  func(r *Request) { r.Migration.Dialect = "anthropic-native" },
		"margin too large": func(r *Request) { r.Migration.MaxQualityRegressionPercent = 60 },
		"no binding":       func(r *Request) { r.Migration.Binding = "" },
	} {
		request := migrationRequest()
		edit(&request)
		if _, err := catalogSource(t).Propose(context.Background(), request); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	customer := migrationRequest()
	customer.QualityEvalSet, customer.QualityCheck = "", QualityCheckCustomer
	if _, err := catalogSource(t).Propose(context.Background(), customer); err != nil {
		t.Fatalf("customer evidence migration refused: %v", err)
	}
}
