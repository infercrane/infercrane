package optimizationevidence

import (
	"os"
	"strings"
	"testing"
)

func TestFastPathImportCannotQualifyUntilInferCraneBindsEvidence(t *testing.T) {
	body, err := os.ReadFile("testdata/qwen38-fastpath-qualified.json")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := DecodeFastPathManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	if imported.QualificationEligible || imported.ImportedEvidenceState != StateExternalUnverified || imported.ModelIdentity != "Qwen/Qwen3.8-27B-FP8@017b9c7af6b5689d5dd426a76e0bc077eb5ca20a" || !strings.Contains(imported.RuntimeIdentity, "sglang@0.5.18") || imported.Campaign.HardwareIdentity != "modal:NVIDIA H100 80GB HBM3:1" || !isSHA256(imported.Campaign.Candidates[0].RecipeDigest) {
		t.Fatalf("unsafe or incomplete import: %+v", imported)
	}
	if imported.Campaign.Candidates[0].ErrorRate != 0 || imported.Campaign.Candidates[0].PromptTokenMismatchRate != 0 || imported.Campaign.Candidates[0].Lanes[2].Requests != 1656 {
		t.Fatalf("qualification measurements were not preserved exactly: %+v", imported.Campaign.Candidates[0])
	}
	unbound, err := Evaluate(imported.Campaign)
	if err != nil {
		t.Fatal(err)
	}
	if unbound.WinnerID != "" || resultByID(t, unbound, imported.CandidateID).Score != nil {
		t.Fatalf("unbound import received a winner: %+v", unbound)
	}
	bound, err := imported.BindReproduced(ReproductionBinding{ModelIdentity: imported.ModelIdentity, RevisionID: "infercrane-revision-1", QualityEvidenceID: "signed-quality-1", QualityPassed: true})
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := Evaluate(bound)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.WinnerID != imported.CandidateID {
		t.Fatalf("bound reproduced candidate was not selected: %+v", evaluation)
	}
}

func TestFastPathBindingFailsClosed(t *testing.T) {
	body, err := os.ReadFile("testdata/qwen38-fastpath-qualified.json")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := DecodeFastPathManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	for name, binding := range map[string]ReproductionBinding{
		"wrong model":      {ModelIdentity: "other@revision", RevisionID: "revision", QualityEvidenceID: "quality", QualityPassed: true},
		"missing revision": {ModelIdentity: imported.ModelIdentity, QualityEvidenceID: "quality", QualityPassed: true},
		"failed quality":   {ModelIdentity: imported.ModelIdentity, RevisionID: "revision", QualityEvidenceID: "quality", QualityPassed: false},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := imported.BindReproduced(binding); err == nil {
				t.Fatal("unsafe binding was accepted")
			}
		})
	}
}

func TestFastPathImportRejectsSecretsAndMutableIdentity(t *testing.T) {
	body, err := os.ReadFile("testdata/qwen38-fastpath-qualified.json")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Replace(string(body), `"secret_values_included":false`, `"secret_values_included":true`, 1)
	if _, err = DecodeFastPathManifest([]byte(secret)); err == nil {
		t.Fatal("manifest claiming embedded secrets was accepted")
	}
	mutable := strings.Replace(string(body), "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a", "main", 1)
	if _, err = DecodeFastPathManifest([]byte(mutable)); err == nil {
		t.Fatal("mutable model identity was accepted")
	}
}
