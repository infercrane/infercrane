package optimizer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/spec"
	"gopkg.in/yaml.v3"
)

const recipeTestDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

func recipeRequest() Request {
	return Request{
		ModelIdentity: "acme/novel-model", ModelRevision: strings.Repeat("a", 40), Provider: "runpod-pods",
		GPU: "H200", Objective: "throughput", MaxCandidates: 20, Search: SearchRecipes,
		ModelFeatures: []string{FeatureNativeMTP}, ModelConfigDigest: recipeTestDigest,
	}
}

func armIDs(candidates []Candidate) []string {
	var ids []string
	for _, candidate := range candidates {
		for _, feature := range candidate.Features {
			if feature.Name == "recipe_arm" {
				ids = append(ids, feature.Source)
			}
		}
	}
	return ids
}

func candidateForArm(t *testing.T, proposal Proposal, arm string) Candidate {
	t.Helper()
	for _, candidate := range proposal.Candidates {
		for _, feature := range candidate.Features {
			if feature.Name == "recipe_arm" && feature.Source == arm {
				return candidate
			}
		}
	}
	t.Fatalf("arm %q missing from %v", arm, armIDs(proposal.Candidates))
	return Candidate{}
}

func TestInspectModelConfigDetectsDeclaredFeatureNotModelName(t *testing.T) {
	tests := []struct {
		name, config string
		want         []string
	}{
		{"nested mtp head", `{"text_config":{"mtp_num_hidden_layers":1}}`, []string{FeatureNativeMTP}},
		{"top-level nextn layers", `{"num_nextn_predict_layers":1}`, []string{FeatureNativeMTP}},
		{"zero layers", `{"mtp_num_hidden_layers":0}`, []string{}},
		{"undeclared stays unknown", `{"model_type":"qwen3_5"}`, []string{}},
	}
	for _, test := range tests {
		evidence, err := InspectModelConfig([]byte(test.config))
		if err != nil || !reflect.DeepEqual(evidence.Features, test.want) || !sha256Digest(evidence.Digest) || !strings.HasPrefix(evidence.Digest, "sha256:") {
			t.Fatalf("%s: evidence=%+v err=%v", test.name, evidence, err)
		}
	}
	for _, invalid := range [][]byte{nil, []byte("not json"), []byte(`[]`), make([]byte, maxModelConfigBytes+1)} {
		if _, err := InspectModelConfig(invalid); err == nil {
			t.Fatalf("invalid config accepted: %.20q", invalid)
		}
	}
}

func TestRecipeSearchProposesControlledUnmeasuredExperimentsForAnyPinnedModel(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), recipeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("search proposal failed validation: %v", err)
	}
	if arms := armIDs(proposal.Candidates); len(arms) == 0 || arms[0] != "control" {
		t.Fatalf("control must rank first: %v", arms)
	}
	control := proposal.Candidates[0]
	if proposal.StartingPoint == nil || proposal.StartingPoint.CandidateID != control.ID {
		t.Fatalf("starting point must be the untuned control: %+v", proposal.StartingPoint)
	}
	image := control.Deployment.Runtime.Workload.Image
	for _, candidate := range proposal.Candidates {
		if candidate.Source.Name != recipeSearchSource {
			continue
		}
		if candidate.EvidenceState != EvidenceUnmeasured || candidate.Status != "proposed-unmeasured" || candidate.Deployment.Runtime.Workload.Image != image || candidate.Deployment.Model.ID != "acme/novel-model" {
			t.Fatalf("experiment crossed its evidence boundary: %+v", candidate)
		}
		if !strings.Contains(strings.Join(candidate.Limitations, " "), "carry no performance claim") {
			t.Fatalf("experiment omitted its claim boundary: %+v", candidate.Limitations)
		}
	}
	command := strings.Join(candidateForArm(t, proposal, "nextn-k4-bounded-graphs").Deployment.Runtime.Workload.Command, " ")
	for _, want := range []string{"--speculative-algorithm NEXTN", "--speculative-num-draft-tokens 4", "--cuda-graph-bs-decode 1 2 4 8 12 16 24 32 33", "${MODEL_REVISION}"} {
		if !strings.Contains(command, want) {
			t.Fatalf("composite arm omitted %q: %s", want, command)
		}
	}
	for _, flag := range []string{"--speculative-algorithm", "--kv-cache-dtype", "--cuda-graph-bs-decode", "--chunked-prefill-size"} {
		if strings.Contains(strings.Join(control.Deployment.Runtime.Workload.Command, " "), flag) {
			t.Fatalf("control carries recipe flag %s", flag)
		}
	}
	precision := candidateForArm(t, proposal, "kv-fp8")
	if !strings.Contains(strings.Join(precision.RequiredEvidence, " "), "semantic quality evidence") {
		t.Fatalf("precision arm must require quality evidence: %+v", precision.RequiredEvidence)
	}
}

func TestRecipeSearchIsDeterministicAndKeepsControlUnderBudget(t *testing.T) {
	request := recipeRequest()
	first, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("search must be deterministic: err=%v", err)
	}
	request.MaxCandidates = 4
	bounded, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	arms := armIDs(bounded.Candidates)
	want := []string{"control", "nextn-k4-bounded-graphs", "nextn-k4", "bounded-graphs"}
	if !reflect.DeepEqual(arms, want) {
		t.Fatalf("small budgets must keep the control and isolate each mechanism: %v", arms)
	}
	if err = ValidateProposal(bounded); err != nil {
		t.Fatal(err)
	}
}

func TestRecipeSearchSkipsSpeculationWithoutDeclaredModelFeature(t *testing.T) {
	request := recipeRequest()
	request.ModelFeatures, request.ModelConfigDigest = nil, ""
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range armIDs(proposal.Candidates) {
		if strings.Contains(arm, "nextn") {
			t.Fatalf("speculative arm generated without evidence: %v", arm)
		}
	}
	if !strings.Contains(strings.Join(proposal.Warnings, " "), "Speculative-decoding arms were skipped") {
		t.Fatalf("skip was not explained: %v", proposal.Warnings)
	}
}

func TestRecipeSearchGatesPrefillArmsOnWorkloadShape(t *testing.T) {
	request := recipeRequest()
	request.WorkloadProfile = "interactive"
	short, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range armIDs(short.Candidates) {
		if strings.Contains(arm, "chunk") {
			t.Fatalf("prefill arm generated for a short-prompt workload: %v", arm)
		}
	}
	request.WorkloadProfile = "long-context"
	long, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	candidateForArm(t, long, "chunk4k")
}

func TestRecipeSearchFailsClosedOutsideItsReviewedBoundary(t *testing.T) {
	request := recipeRequest()
	request.GPU = "L40S"
	unsupported, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(armIDs(unsupported.Candidates)) != 0 || !strings.Contains(strings.Join(unsupported.Warnings, " "), "no reviewed") {
		t.Fatalf("unsupported GPU: %+v err=%v", unsupported, err)
	}
	request = recipeRequest()
	request.Provider, request.Region = "aws", "eu-central-1"
	unqualified, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(armIDs(unqualified.Candidates)) != 0 || !strings.Contains(strings.Join(unqualified.Warnings, " "), "pinned-image runtime") {
		t.Fatalf("unqualified provider: %+v err=%v", unqualified, err)
	}
	request = recipeRequest()
	request.Runtimes = []string{"vllm"}
	excluded, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(armIDs(excluded.Candidates)) != 0 {
		t.Fatalf("runtime allowlist ignored: %+v err=%v", excluded, err)
	}
	request = recipeRequest()
	request.ModelRevision = ""
	unpinned, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(unpinned.Candidates) != 0 || !reflect.DeepEqual(unpinned.Missing, []string{"immutable_model_revision"}) {
		t.Fatalf("unpinned model: %+v err=%v", unpinned, err)
	}
}

func TestRecipeSearchExtendsCatalogModelsWithoutRelaxingTheirBoundary(t *testing.T) {
	request := Request{ModelIdentity: "qwen3.8-27b-fp8", Provider: "runpod-pods", GPU: "B200", Objective: "throughput", MaxCandidates: 20, Search: SearchRecipes, ModelFeatures: []string{FeatureNativeMTP}, ModelConfigDigest: recipeTestDigest}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(proposal.Missing) != 0 || len(armIDs(proposal.Candidates)) == 0 {
		t.Fatalf("catalog search: %+v err=%v", proposal, err)
	}
	if proposal.Candidates[0].Deployment.Model.Revision != "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a" {
		t.Fatalf("catalog pin was not used: %+v", proposal.Candidates[0].Deployment.Model)
	}
	if !strings.Contains(strings.Join(proposal.Warnings, " "), "reviewed_runtime_profile") {
		t.Fatalf("missing reviewed boundary was hidden: %v", proposal.Warnings)
	}
	request.Search, request.ModelFeatures, request.ModelConfigDigest = "", nil, ""
	plain, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(plain.Candidates) != 0 || !reflect.DeepEqual(plain.Missing, []string{"reviewed_runtime_profile"}) {
		t.Fatalf("default catalog boundary changed: %+v err=%v", plain, err)
	}
}

func TestDefaultProposalIsUnchangedWhenSearchIsOff(t *testing.T) {
	request := recipeRequest()
	request.Search, request.ModelFeatures, request.ModelConfigDigest = "", nil, ""
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(armIDs(proposal.Candidates)) != 0 {
		t.Fatalf("search ran without opt-in: %v", armIDs(proposal.Candidates))
	}
	encoded, err := json.Marshal(proposal.Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"search"`, `"model_features"`, `"model_config_digest"`} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("default input digest would change through %s: %s", field, encoded)
		}
	}
}

func TestRecipeSearchRejectsInvalidInputs(t *testing.T) {
	mutations := map[string]func(*Request){
		"unknown search":          func(r *Request) { r.Search = "everything" },
		"unknown feature":         func(r *Request) { r.ModelFeatures = []string{"telepathy"} },
		"feature without digest":  func(r *Request) { r.ModelConfigDigest = "" },
		"malformed digest":        func(r *Request) { r.ModelConfigDigest = "sha256:abc" },
		"features without search": func(r *Request) { r.Search = "" },
		"digest without search":   func(r *Request) { r.Search, r.ModelFeatures = "", nil },
	}
	for name, mutate := range mutations {
		request := recipeRequest()
		mutate(&request)
		if _, err := catalogSource(t).Propose(context.Background(), request); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestRecipeCandidatesPassDeploymentSpecValidation(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), recipeRequest())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for _, candidate := range proposal.Candidates {
		if candidate.Source.Name != recipeSearchSource {
			continue
		}
		encoded, marshalErr := yaml.Marshal(candidate.Deployment)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		path := filepath.Join(directory, candidate.Deployment.Name+".yaml")
		if err = os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err = spec.Load(path); err != nil {
			t.Fatalf("%s failed spec validation: %v", candidate.Deployment.Name, err)
		}
	}
}

const pairedH200RawReceipt = "../../docs/testing/evidence/qwen38-public-decode-saturation-modal-2026-09-23T044530Z-raw.json"

func TestWinningArmMatchesRecordedLaunchOnSameImage(t *testing.T) {
	body, err := os.ReadFile(pairedH200RawReceipt)
	if err != nil {
		t.Fatal(err)
	}
	var runs []struct {
		CandidateID string `json:"candidate_id"`
		Recipe      struct {
			Command []string `json:"command"`
			Image   string   `json:"image"`
		} `json:"recipe"`
	}
	if err = json.Unmarshal(body, &runs); err != nil {
		t.Fatal(err)
	}
	template := recipeTemplates()[0]
	var arm recipeArm
	for _, candidate := range template.arms {
		if candidate.id == "nextn-k4-bounded-graphs" {
			arm = candidate
		}
	}
	found := false
	for _, run := range runs {
		if run.CandidateID != arm.exercisedBy {
			continue
		}
		found = true
		if run.Recipe.Image != template.image {
			t.Fatalf("template image drifted from receipt: %s", run.Recipe.Image)
		}
		recorded := strings.Join(run.Recipe.Command, " ")
		if !strings.HasSuffix(recorded, strings.Join(arm.args, " ")) || !strings.HasPrefix(recorded, strings.Join(template.launcher, " ")) {
			t.Fatalf("arm drifted from receipt:\n%s\n%v", recorded, arm.args)
		}
	}
	if !found {
		t.Fatalf("receipt does not contain %s", arm.exercisedBy)
	}
}

func TestEveryArmNamesAnExercisedReceiptCandidate(t *testing.T) {
	files, err := filepath.Glob("../../docs/testing/evidence/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("evidence files: %v err=%v", files, err)
	}
	exercised := map[string]struct{}{}
	for _, file := range files {
		body, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var document struct {
			SchemaVersion string `json:"schema_version"`
			Candidates    []struct {
				ID string `json:"id"`
			} `json:"candidates"`
		}
		if json.Unmarshal(body, &document) != nil || document.SchemaVersion == "" {
			continue
		}
		for _, candidate := range document.Candidates {
			exercised[candidate.ID] = struct{}{}
		}
	}
	for _, template := range recipeTemplates() {
		for _, arm := range template.arms {
			if _, ok := exercised[arm.exercisedBy]; !ok {
				t.Fatalf("arm %s names an unrecorded candidate %s", arm.id, arm.exercisedBy)
			}
		}
	}
}
