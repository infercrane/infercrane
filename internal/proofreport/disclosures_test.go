package proofreport

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"html"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/technique"
)

func oneEngineManifest(t *testing.T) technique.Manifest {
	t.Helper()
	manifest, known := optimizer.TechniqueManifest(optimizer.TechniqueAdaptiveSpeculationOneEngine)
	if !known {
		t.Fatal("the one-engine technique is not in this build")
	}
	return manifest
}

// A technique's shown claim carries its disclosures word for word: in the
// report, under its signature, and in every rendering, next to the claim.
func TestATechniqueClaimCarriesItsDisclosures(t *testing.T) {
	manifest := oneEngineManifest(t)
	report := Assemble(Input{Title: "Proof Report", Runs: []RunEvidence{asTechnique(t, goodRun("run-a", 16, 0.30).evidence(t), manifest.Disclosures)}})
	run := report.Workloads[0].Runs[0]
	if run.Claim.Claim != technique.ClaimShown || run.Claim.Technique != manifest.ID+"@"+manifest.Version || len(run.Claim.Missing) != 0 {
		t.Fatalf("claim = %+v", run.Claim)
	}
	if !slices.Equal(run.Claim.Disclosures, manifest.Disclosures) {
		t.Fatalf("disclosures = %q", run.Claim.Disclosures)
	}
	if run.Saving.State != SavingVerified {
		t.Fatalf("saving = %+v", run.Saving)
	}

	seed := sha256.Sum256([]byte("proof report test key"))
	key := ed25519.NewKeyFromSeed(seed[:])
	document, err := Sign(report, key)
	if err != nil || document.Signature == nil {
		t.Fatalf("sign: %v %+v", err, document)
	}
	markdown, text := string(Markdown(document)), Text(document)
	page, err := HTML(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentence := range manifest.Disclosures {
		if !strings.Contains(markdown, "Disclosed with this claim: "+md(sentence)) {
			t.Errorf("markdown lacks %q", sentence)
		}
		if !strings.Contains(string(page), "<li>"+html.EscapeString(sentence)+"</li>") {
			t.Errorf("HTML lacks %q", sentence)
		}
		if !strings.Contains(text, "Disclosed with this claim: "+sentence) {
			t.Errorf("text lacks %q", sentence)
		}
	}

	// The disclosures are signed with the report: changing one fails.
	saved, _ := json.MarshalIndent(document, "", "  ")
	if _, err := Verify(saved, key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	encoded, _ := json.Marshal(manifest.Disclosures[0])
	tampered := strings.Replace(string(saved), string(encoded), `"It held at every load."`, 1)
	if tampered == string(saved) {
		t.Fatal("the saved report does not carry the first disclosure")
	}
	if _, err := Verify([]byte(tampered), nil); err == nil || !strings.Contains(err.Error(), "the report was changed") {
		t.Fatalf("tampered: %v", err)
	}
}

// A technique's claim without every disclosure its technique requires is
// "not shown", and says so, even when the claim it was read with said
// "shown"; so is one whose technique this build does not know at the version
// that ran. Its saving is not verified.
func TestATechniqueClaimWithoutItsDisclosuresIsNotShown(t *testing.T) {
	manifest := oneEngineManifest(t)
	dropped := asTechnique(t, goodRun("run-a", 16, 0.30).evidence(t), manifest.Disclosures[1:])
	if dropped.Claim != technique.ClaimNotShown {
		t.Fatalf("the manifest showed a claim without a disclosure: %s", dropped.Claim)
	}
	stale := asTechnique(t, goodRun("run-a", 16, 0.30).evidence(t), manifest.Disclosures[1:])
	stale.Claim = technique.ClaimShown
	otherVersion := asTechnique(t, goodRun("run-a", 16, 0.30).evidence(t), manifest.Disclosures)
	result := *otherVersion.Result
	result.TechniqueVersion = "9.9.9"
	otherVersion.Result, otherVersion.Claim = &result, technique.ClaimShown
	for name, test := range map[string]struct {
		evidence RunEvidence
		lacks    string
	}{
		"a dropped disclosure":       {dropped, "every disclosure its technique requires, word for word"},
		"a stale shown claim":        {stale, "every disclosure its technique requires, word for word"},
		"a version this build lacks": {otherVersion, "the technique's contract at the version that ran"},
	} {
		run := Assemble(Input{Title: "Proof Report", Runs: []RunEvidence{test.evidence}}).Workloads[0].Runs[0]
		if run.Claim.Claim != technique.ClaimNotShown || !slices.Contains(run.Claim.Missing, test.lacks) {
			t.Errorf("%s: claim = %+v", name, run.Claim)
		}
		if !slices.Equal(run.Claim.Disclosures, test.evidence.Result.Disclosures) {
			t.Errorf("%s: disclosures %q, carried %q", name, run.Claim.Disclosures, test.evidence.Result.Disclosures)
		}
		if run.Saving.State != SavingNotShown || !slices.ContainsFunc(run.Saving.Reasons, func(reason string) bool { return strings.Contains(reason, test.lacks) }) {
			t.Errorf("%s: saving = %+v", name, run.Saving)
		}
	}
}

// A setup no technique made carries no disclosures, as an empty list.
func TestASetupWithoutATechniqueCarriesNoDisclosures(t *testing.T) {
	report := assemble(t, goodRun("run-a", 16, 0.30))
	run := report.Workloads[0].Runs[0]
	if run.Claim.Technique != "" || run.Claim.Disclosures == nil || len(run.Claim.Disclosures) != 0 {
		t.Fatalf("claim = %+v", run.Claim)
	}
	encoded, _ := json.Marshal(run.Claim)
	if !strings.Contains(string(encoded), `"disclosures":[]`) {
		t.Fatalf("claim JSON %s", encoded)
	}
	if strings.Contains(string(Markdown(Document{Report: report})), "Disclosed with this claim") {
		t.Fatal("a setup without a technique printed a disclosure")
	}
}
