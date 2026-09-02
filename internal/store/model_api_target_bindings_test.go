package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/modelapitarget"
)

func TestModelAPITargetBindingRepositoryRejectsInvalidInputsBeforeDatabaseAccess(t *testing.T) {
	ctx := context.Background()
	s := &Store{}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	binding, err := modelapitarget.NewBinding(modelapitarget.Draft{
		ID: "binding-1", OperatorTenantID: "operator-a", ProductID: "glm-5.2", Kind: modelapitarget.KindUpstream,
		OfferID: "offer-1", OfferVersion: 1, Adapter: "openai", SupplierModelID: "supplier/model",
		EndpointReference: "registry/model", EndpointConfigDigest: "sha256:" + strings.Repeat("a", 64), Region: "global",
		CreatedAt: now, ValidFrom: now.Add(time.Minute), ValidUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishModelAPITargetBinding(ctx, "operator-b", binding); err == nil {
		t.Fatal("cross-operator target binding was not rejected before database access")
	}
	if _, err = s.CurrentModelAPITargetBindings(ctx, "operator-a", binding.ProductID, time.Time{}); err == nil {
		t.Fatal("zero evaluation time was not rejected before database access")
	}

	draft := modelapitarget.Draft{
		ID: "binding-submicro", OperatorTenantID: "operator-a", ProductID: "glm-5.2", Kind: modelapitarget.KindUpstream,
		OfferID: "offer-1", OfferVersion: 1, Adapter: "openai", SupplierModelID: "supplier/model",
		EndpointReference: "registry/model", EndpointConfigDigest: "sha256:" + strings.Repeat("b", 64), Region: "global",
		CreatedAt: now.Add(time.Nanosecond), ValidFrom: now.Add(time.Minute + time.Nanosecond), ValidUntil: now.Add(time.Hour + time.Nanosecond),
	}
	binding, err = modelapitarget.NewBinding(draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishModelAPITargetBinding(ctx, "operator-a", binding); err == nil || !strings.Contains(err.Error(), "microsecond") {
		t.Fatalf("sub-microsecond target binding was not rejected before database access: %v", err)
	}
}

func TestModelAPITargetBindingsPersistExactImmutableOfferRevisions(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	operator := "target-operator-" + suffix
	otherOperator := "target-other-" + suffix
	for _, tenant := range []string{operator, otherOperator} {
		if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
			t.Fatal(err)
		}
	}
	secret, err := s.CreateSecretReference(ctx, operator, "target-secret-"+suffix, "env", "TARGET_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	product, err := s.ManagedModelAPIProduct(ctx, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	offer := modelapisupply.Offer{
		ID: "target-offer-" + suffix, Version: 7, OperatorTenantID: operator, ProductID: product.ID,
		Supplier: "fixture", Adapter: "openai", SupplierModelID: "fixture/glm-5.2", Protocol: product.Protocol,
		TupleKey: "sha256:" + strings.Repeat("c", 64), Region: "global", CredentialReference: secret.ID,
		State: modelapisupply.OfferActive, Capabilities: []string{"chat-completions", "streaming"},
		Access: "ready", Availability: "available", Health: "healthy", ObservedAt: now,
		CostRate:   modelapisupply.CostRate{Currency: "USD"},
		Commercial: modelapisupply.CommercialAuthorization{State: modelapisupply.CommercialPending},
	}
	if _, err = s.PublishModelAPISupplierOffer(ctx, operator, offer); err != nil {
		t.Fatal(err)
	}

	bindings := make([]modelapitarget.Binding, 0, 4)
	for index, kind := range []modelapitarget.Kind{modelapitarget.KindUpstream, modelapitarget.KindServerlessGPU, modelapitarget.KindDedicated, modelapitarget.KindBYOC} {
		binding, bindingErr := modelapitarget.NewBinding(modelapitarget.Draft{
			ID: fmt.Sprintf("target-binding-%d-%s", index, suffix), OperatorTenantID: operator, ProductID: product.ID, Kind: kind,
			OfferID: offer.ID, OfferVersion: offer.Version, Adapter: offer.Adapter, SupplierModelID: offer.SupplierModelID,
			EndpointReference: fmt.Sprintf("endpoint-registry/%s/%d", kind, index), EndpointConfigDigest: "sha256:" + strings.Repeat(fmt.Sprint(index+1), 64), Region: offer.Region,
			CreatedAt: now, ValidFrom: now.Add(time.Minute), ValidUntil: now.Add(time.Hour),
		})
		if bindingErr != nil {
			t.Fatal(bindingErr)
		}
		stored, publishErr := s.PublishModelAPITargetBinding(ctx, operator, binding)
		if publishErr != nil || stored.ContractDigest != binding.ContractDigest {
			t.Fatalf("kind=%s stored=%+v err=%v", kind, stored, publishErr)
		}
		bindings = append(bindings, binding)
	}

	replayed, err := s.PublishModelAPITargetBinding(ctx, operator, bindings[0])
	if err != nil || replayed.ContractDigest != bindings[0].ContractDigest {
		t.Fatalf("exact replay=%+v err=%v", replayed, err)
	}
	history, err := s.ModelAPITargetBindings(ctx, operator, product.ID)
	if err != nil || len(history) != len(bindings) {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	current, err := s.CurrentModelAPITargetBindings(ctx, operator, product.ID, now.Add(2*time.Minute))
	if err != nil || len(current) != len(bindings) {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	expired, err := s.CurrentModelAPITargetBindings(ctx, operator, product.ID, now.Add(time.Hour))
	if err != nil || len(expired) != 0 {
		t.Fatalf("expired=%+v err=%v", expired, err)
	}
	if _, err = s.ModelAPITargetBinding(ctx, otherOperator, bindings[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-operator read error=%v, want not found", err)
	}

	conflict, err := modelapitarget.NewBinding(modelapitarget.Draft{
		ID: bindings[0].ID, OperatorTenantID: operator, ProductID: product.ID, Kind: bindings[0].Kind,
		OfferID: offer.ID, OfferVersion: offer.Version, Adapter: offer.Adapter, SupplierModelID: offer.SupplierModelID,
		EndpointReference: "endpoint-registry/changed", EndpointConfigDigest: bindings[0].EndpointConfigDigest, Region: offer.Region,
		CreatedAt: now, ValidFrom: now.Add(time.Minute), ValidUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishModelAPITargetBinding(ctx, operator, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting immutable binding error=%v", err)
	}

	mismatch, err := modelapitarget.NewBinding(modelapitarget.Draft{
		ID: "target-mismatch-" + suffix, OperatorTenantID: operator, ProductID: product.ID, Kind: modelapitarget.KindUpstream,
		OfferID: offer.ID, OfferVersion: offer.Version, Adapter: "different-adapter", SupplierModelID: offer.SupplierModelID,
		EndpointReference: "endpoint-registry/mismatch", EndpointConfigDigest: "sha256:" + strings.Repeat("f", 64), Region: offer.Region,
		CreatedAt: now, ValidFrom: now.Add(time.Minute), ValidUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishModelAPITargetBinding(ctx, operator, mismatch); err == nil || !strings.Contains(err.Error(), "exactly match") {
		t.Fatalf("offer mismatch error=%v", err)
	}

	if _, err = s.ExecContext(ctx, `UPDATE model_api_target_bindings SET endpoint_reference='mutated' WHERE id=?`, bindings[0].ID); err == nil {
		t.Fatal("immutable target binding accepted an update")
	}
	if _, err = s.ExecContext(ctx, `DELETE FROM model_api_target_bindings WHERE id=?`, bindings[0].ID); err == nil {
		t.Fatal("immutable target binding accepted a delete")
	}
}
