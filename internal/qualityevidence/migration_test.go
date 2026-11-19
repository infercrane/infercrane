package qualityevidence

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
)

func TestMigrationVerdictIsSignedAndTamperEvident(t *testing.T) {
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	verdict := MigrationVerdict{Schema: MigrationVerdictSchema, CampaignID: "c", Endpoint: "support-bot", BindingID: "b", CandidateDeployment: "qwen-opt", CandidateRevisionID: "rev", ReferenceEvidenceDigest: "sha256:" + strings.Repeat("a", 64), CandidateEvidenceDigest: "sha256:" + strings.Repeat("b", 64), Suite: "s", SuiteVersion: "1", Evaluator: "infercrane-graded", EvaluatorVersion: "1", MarginPercent: 5, Verdict: "PASS", Code: "quality_within_policy", CandidateScore: 0.97, EvaluatedAt: time.Now().UTC().Truncate(time.Second)}
	envelope, err := passport.Sign(verdict, key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMigrationVerdict(envelope)
	if err != nil || decoded.Verdict != "PASS" || decoded.CandidateRevisionID != "rev" {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	tampered := envelope
	tampered.PayloadJSON = strings.Replace(envelope.PayloadJSON, `"PASS"`, `"BLOCK"`, 1)
	if _, err = DecodeMigrationVerdict(tampered); err == nil {
		t.Fatal("a tampered verdict verified")
	}
	for name, edit := range map[string]func(*MigrationVerdict){
		"schema":  func(v *MigrationVerdict) { v.Schema = "other" },
		"verdict": func(v *MigrationVerdict) { v.Verdict = "MAYBE" },
		"digest":  func(v *MigrationVerdict) { v.ReferenceEvidenceDigest = "nope" },
		"margin":  func(v *MigrationVerdict) { v.MarginPercent = 0 },
		"binding": func(v *MigrationVerdict) { v.BindingID = "" },
	} {
		invalid := verdict
		edit(&invalid)
		signed, _ := passport.Sign(invalid, key)
		if _, err = DecodeMigrationVerdict(signed); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
