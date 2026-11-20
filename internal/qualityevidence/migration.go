package qualityevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// MigrationVerdictSchema is the signed verdict that a candidate answers a
// graded suite as well as the closed API it would replace. A staged switch
// from that closed API to the candidate's deployment cites it in place of a
// same-model check.
const MigrationVerdictSchema = "infercrane.dev/migration-quality-verdict/v1"

// MigrationVerdict is the signed payload. It binds both evidence rows it was
// computed from by their payload digests, the policy that decided it, and
// the result.
type MigrationVerdict struct {
	Schema                  string                            `json:"schema"`
	CampaignID              string                            `json:"campaign_id"`
	Endpoint                string                            `json:"endpoint"`
	BindingID               string                            `json:"binding_id"`
	CandidateDeployment     string                            `json:"candidate_deployment"`
	CandidateRevisionID     string                            `json:"candidate_revision_id"`
	ReferenceEvidenceDigest string                            `json:"reference_evidence_digest"`
	CandidateEvidenceDigest string                            `json:"candidate_evidence_digest"`
	Suite                   string                            `json:"suite"`
	SuiteVersion            string                            `json:"suite_version"`
	Evaluator               string                            `json:"evaluator"`
	EvaluatorVersion        string                            `json:"evaluator_version"`
	MarginPercent           float64                           `json:"margin_percent"`
	Verdict                 string                            `json:"verdict"`
	Code                    string                            `json:"code"`
	ReferenceScore          *float64                          `json:"reference_score,omitempty"`
	CandidateScore          float64                           `json:"candidate_score"`
	Bootstrap               *releaseguard.BootstrapComparison `json:"bootstrap,omitempty"`
	EvaluatedAt             time.Time                         `json:"evaluated_at"`
}

// Validate checks a verdict's identity fields and its verdict value.
func (v MigrationVerdict) Validate() error {
	if v.Schema != MigrationVerdictSchema {
		return fmt.Errorf("migration verdict schema must be %q", MigrationVerdictSchema)
	}
	for name, value := range map[string]string{"campaign_id": v.CampaignID, "endpoint": v.Endpoint, "binding_id": v.BindingID, "candidate_deployment": v.CandidateDeployment, "candidate_revision_id": v.CandidateRevisionID, "suite": v.Suite, "evaluator": v.Evaluator} {
		if strings.TrimSpace(value) == "" || len(value) > 256 {
			return fmt.Errorf("migration verdict %s must be non-empty and at most 256 characters", name)
		}
	}
	for name, value := range map[string]string{"reference_evidence_digest": v.ReferenceEvidenceDigest, "candidate_evidence_digest": v.CandidateEvidenceDigest} {
		if !sha256Pattern.MatchString(value) {
			return fmt.Errorf("migration verdict %s must be a SHA-256 digest", name)
		}
	}
	switch v.Verdict {
	case releaseguard.QualityPass, releaseguard.QualityBlock, releaseguard.QualityUnknown, releaseguard.QualityWatch:
	default:
		return errors.New("migration verdict must be PASS, WATCH, BLOCK, or UNKNOWN")
	}
	if v.MarginPercent <= 0 || v.MarginPercent > 50 || v.EvaluatedAt.IsZero() {
		return errors.New("migration verdict needs a margin and an evaluation time")
	}
	return nil
}

// DecodeMigrationVerdict verifies a signed verdict and strictly decodes it.
func DecodeMigrationVerdict(envelope passport.Envelope) (MigrationVerdict, error) {
	if err := passport.Verify(envelope); err != nil {
		return MigrationVerdict{}, fmt.Errorf("verify migration verdict: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(envelope.PayloadJSON)))
	decoder.DisallowUnknownFields()
	var verdict MigrationVerdict
	if err := decoder.Decode(&verdict); err != nil {
		return MigrationVerdict{}, fmt.Errorf("decode migration verdict: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return MigrationVerdict{}, errors.New("migration verdict contains trailing JSON")
	}
	return verdict, verdict.Validate()
}
