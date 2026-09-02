package modelapitarget

import (
	"strings"
	"testing"
	"time"
)

func TestBindingSupportsEveryCapacityKind(t *testing.T) {
	for _, kind := range []Kind{KindUpstream, KindServerlessGPU, KindDedicated, KindBYOC} {
		t.Run(string(kind), func(t *testing.T) {
			binding, err := NewBinding(validDraft(kind))
			if err != nil {
				t.Fatal(err)
			}
			if binding.Kind != kind || !binding.HasCanonicalDigest() || !strings.HasPrefix(binding.ContractDigest, "sha256:") {
				t.Fatalf("binding=%+v", binding)
			}
			if !binding.CurrentAt(binding.ValidFrom) || binding.CurrentAt(binding.ValidUntil) {
				t.Fatal("binding validity interval is not half-open")
			}
		})
	}
}

func TestBindingRejectsIncompleteAndMutableContracts(t *testing.T) {
	tests := map[string]func(*Draft){
		"unknown kind":     func(draft *Draft) { draft.Kind = "spot" },
		"missing endpoint": func(draft *Draft) { draft.EndpointReference = "" },
		"padded adapter":   func(draft *Draft) { draft.Adapter = " openai" },
		"invalid digest":   func(draft *Draft) { draft.EndpointConfigDigest = "sha256:not-a-digest" },
		"zero revision":    func(draft *Draft) { draft.OfferVersion = 0 },
		"created too late": func(draft *Draft) { draft.CreatedAt = draft.ValidFrom.Add(time.Second) },
		"empty interval":   func(draft *Draft) { draft.ValidUntil = draft.ValidFrom },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			draft := validDraft(KindUpstream)
			mutate(&draft)
			if _, err := NewBinding(draft); err == nil {
				t.Fatal("invalid target binding was accepted")
			}
		})
	}

	binding, err := NewBinding(validDraft(KindUpstream))
	if err != nil {
		t.Fatal(err)
	}
	binding.SupplierModelID = "supplier/tampered"
	if binding.Validate() == nil || binding.HasCanonicalDigest() || binding.CurrentAt(binding.ValidFrom) {
		t.Fatal("a mutated immutable binding retained a valid digest")
	}
}

func TestBindingNormalizesTimesBeforeDigesting(t *testing.T) {
	draft := validDraft(KindServerlessGPU)
	location := time.FixedZone("fixture", -7*60*60)
	draft.CreatedAt = draft.CreatedAt.In(location)
	draft.ValidFrom = draft.ValidFrom.In(location)
	draft.ValidUntil = draft.ValidUntil.In(location)
	binding, err := NewBinding(draft)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CreatedAt.Location() != time.UTC || binding.ValidFrom.Location() != time.UTC || binding.ValidUntil.Location() != time.UTC {
		t.Fatalf("binding times were not normalized: %+v", binding)
	}
}

func validDraft(kind Kind) Draft {
	created := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	return Draft{
		ID: "binding-1", OperatorTenantID: "operator-1", ProductID: "glm-5.2", Kind: kind,
		OfferID: "offer-1", OfferVersion: 3, Adapter: "openai", SupplierModelID: "supplier/glm-5.2",
		EndpointReference: "endpoint-registry/glm-primary", EndpointConfigDigest: "sha256:" + strings.Repeat("a", 64), Region: "global",
		CreatedAt: created, ValidFrom: created.Add(time.Minute), ValidUntil: created.Add(time.Hour),
	}
}
