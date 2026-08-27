package passport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
)

func TestCanonicalSignedPassportRejectsTampering(t *testing.T) {
	_, privateKey, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := struct {
		Schema   string `json:"schema"`
		Revision string `json:"revision"`
	}{"infercrane.passport/v1", "rev-1"}
	first, err := Sign(payload, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Sign(payload, privateKey)
	if first != second {
		t.Fatalf("signing is not deterministic: %#v %#v", first, second)
	}
	if err = Verify(first); err != nil {
		t.Fatal(err)
	}
	first.PayloadJSON = strings.Replace(first.PayloadJSON, "rev-1", "rev-2", 1)
	if err = Verify(first); err == nil {
		t.Fatal("tampered payload verified")
	}
}

func TestSignedPassportSurvivesPresentationFormatting(t *testing.T) {
	_, privateKey, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Sign(map[string]any{"schema": "infercrane.passport/v1", "revision": "rev-1", "ratio": 1.25}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	var indented bytes.Buffer
	if err = json.Indent(&indented, []byte(envelope.PayloadJSON), "", "  "); err != nil {
		t.Fatal(err)
	}
	envelope.PayloadJSON = indented.String()
	if err = Verify(envelope); err != nil {
		t.Fatalf("presentation-only formatting invalidated signed evidence: %v", err)
	}

	envelope.PayloadJSON = strings.Replace(envelope.PayloadJSON, `"ratio": 1.25`, `"ratio": 1.5`, 1)
	if err = Verify(envelope); err == nil {
		t.Fatal("semantic payload mutation verified")
	}
}

func TestQualificationSelectionIsExactAndDoesNotPromoteDeferredEvidence(t *testing.T) {
	registry, err := integration.PortableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	selected := SelectQualification(registry.Snapshot(), domain.DeploymentRevisionSpec{Runtime: "vllm", Cloud: "runpod", ComputeMode: "elastic"})
	if selected.ProviderContract == "" || selected.RuntimeContract == "" || len(selected.Providers) != 1 || selected.Providers[0].Cloud != "runpod" || len(selected.Runtimes) != 1 || selected.Runtimes[0].Runtime != "vllm" || len(selected.Compatibility) != 1 {
		t.Fatalf("selected=%+v", selected)
	}
	custom := SelectQualification(registry.Snapshot(), domain.DeploymentRevisionSpec{Runtime: "custom-oci", Cloud: "runpod", ProviderAdapter: "runpod-pods", ComputeMode: "elastic"})
	if len(custom.Providers) != 1 || custom.Providers[0].Adapter != "runpod-pods" || len(custom.Compatibility) != 1 || custom.Compatibility[0].Adapter != "runpod-pods" {
		t.Fatalf("custom=%+v", custom)
	}
}

func TestCompletenessListsEvidenceGapsDeterministically(t *testing.T) {
	payload := Payload{RevisionSpec: domain.DeploymentRevisionSpec{Runtime: "vllm", Cloud: "runpod", ComputeMode: "elastic"}, Benchmarks: []Benchmark{}, MissingEvidence: []string{}}
	FinalizeEvidence(&payload)
	want := []string{"immutable_model_artifact", "revision_benchmark", "release_guard_evaluation", "runtime_qualification", "provider_qualification", "runtime_provider_compatibility"}
	if strings.Join(payload.MissingEvidence, ",") != strings.Join(want, ",") {
		t.Fatalf("missing=%#v", payload.MissingEvidence)
	}
	FinalizeEvidence(&payload)
	if strings.Join(payload.MissingEvidence, ",") != strings.Join(want, ",") {
		t.Fatalf("finalization is not idempotent: missing=%#v", payload.MissingEvidence)
	}
}

func TestPrivateKeyEncodingRoundTrip(t *testing.T) {
	_, privateKey, _ := GenerateKey()
	decoded, err := DecodePrivateKey(EncodePrivateKey(privateKey))
	if err != nil || string(decoded) != string(privateKey) {
		t.Fatalf("round trip: %v", err)
	}
}
