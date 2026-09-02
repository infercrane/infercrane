package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapiproduct"
)

func TestModelAPICatalogQueriesKeepPrivateReadsTenantScoped(t *testing.T) {
	for name, test := range map[string]struct {
		query string
		want  []string
	}{
		"operator publication": {modelAPIOperatorPublicationForTenantQuery, []string{"operator_tenant_id=?", "product_id=?"}},
		"customer entitlement": {modelAPIEntitlementForTenantQuery, []string{"customer_tenant_id=?", "product_id=?"}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, predicate := range test.want {
				if !strings.Contains(test.query, predicate) {
					t.Fatalf("private query is missing isolation predicate %q: %s", predicate, test.query)
				}
			}
		})
	}
}

func TestModelAPICatalogRepositoryRejectsCrossTenantInputsBeforeDatabaseAccess(t *testing.T) {
	store := &Store{}
	if _, err := store.SaveModelAPIOperatorPublication(context.Background(), "operator-a", modelapiproduct.OperatorPublication{OperatorWorkspaceID: "operator-b"}); err == nil {
		t.Fatal("operator mismatch must fail before database access")
	}
	if _, err := store.SaveModelAPIProductEntitlement(context.Background(), "customer-a", modelapiproduct.ProductEntitlement{CustomerWorkspaceID: "customer-b"}); err == nil {
		t.Fatal("customer mismatch must fail before database access")
	}
	if _, err := store.CurrentModelAPIRetailRate(context.Background(), "glm-5.3", time.Time{}); err == nil {
		t.Fatal("zero evaluation time must fail before database access")
	}
	stamp := time.Date(2030, 1, 1, 0, 0, 0, 123, time.UTC)
	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: "rate-1", ProductID: "glm-5.3", Version: 1,
		InputMicrousdPerMillion: 1, OutputMicrousdPerMillion: 1,
		PublishedAt: stamp, ValidFrom: stamp.Add(time.Hour), ValidUntil: stamp.Add(2 * time.Hour), PublicProvenance: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.PublishModelAPIRetailRate(context.Background(), rate); err == nil || !strings.Contains(err.Error(), "microsecond") {
		t.Fatalf("sub-microsecond immutable rate was not rejected before database access: %v", err)
	}
}

func TestModelAPICatalogRepositoryPersistsRedactsAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	operatorTenant := "model-api-operator-" + suffix
	otherOperator := "model-api-other-operator-" + suffix
	customerTenant := "model-api-customer-" + suffix
	otherCustomer := "model-api-other-customer-" + suffix
	for _, tenant := range []string{operatorTenant, otherOperator, customerTenant, otherCustomer} {
		if err := s.CreateTenant(ctx, tenant, tenant); err != nil {
			t.Fatal(err)
		}
	}

	product, err := s.ManagedModelAPIProduct(ctx, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	// A previous interrupted integration test must not leave the global seeded
	// product callable for this isolated scenario.
	product.Availability = modelapiproduct.AvailabilityCatalogOnly
	if product, err = s.SaveManagedModelAPIProduct(ctx, product); err != nil {
		t.Fatal(err)
	}
	product.Availability = modelapiproduct.AvailabilityAvailable
	if _, err = s.SaveManagedModelAPIProduct(ctx, product); err == nil {
		t.Fatal("product became available without an operator publication")
	}
	product.Availability = modelapiproduct.AvailabilityCatalogOnly
	t.Cleanup(func() {
		product.Availability = modelapiproduct.AvailabilityCatalogOnly
		if _, cleanupErr := s.SaveManagedModelAPIProduct(context.Background(), product); cleanupErr != nil {
			t.Errorf("restore catalog-only product: %v", cleanupErr)
		}
	})

	target, err := s.AddTargetForTenant(ctx, operatorTenant, domain.Target{
		Name: "model-api-target-" + suffix, URL: "http://model-api-" + suffix + ":8000",
		Provider: "existing", Runtime: "vllm", UpstreamModel: product.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeploymentForTenant(ctx, operatorTenant, domain.Deployment{Name: "model-api-deployment-" + suffix, Model: product.ID}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	var servingPlanID string
	if err = s.QueryRowContext(ctx, `SELECT active_serving_plan_id FROM endpoints WHERE tenant_id=? AND id=?`, operatorTenant, deployment.ID+"-endpoint").Scan(&servingPlanID); err != nil {
		t.Fatal(err)
	}

	var version int
	if err = s.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM model_api_retail_rate_cards WHERE product_id=?`, product.ID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	capabilityUntil := now.Add(time.Hour)
	for index := range product.Capabilities {
		if product.Capabilities[index].Name == "chat-completions" || product.Capabilities[index].Name == "streaming" {
			product.Capabilities[index].State = modelapiproduct.ClaimQualified
			product.Capabilities[index].EvidenceID = "capability-evidence-" + product.Capabilities[index].Name + "-" + suffix
			product.Capabilities[index].EvidenceUntil = &capabilityUntil
		}
	}
	if product, err = s.SaveManagedModelAPIProduct(ctx, product); err != nil {
		t.Fatal(err)
	}
	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: product.ID + "-rate-" + fmt.Sprint(version), ProductID: product.ID, Version: version,
		InputMicrousdPerMillion: 100_000, OutputMicrousdPerMillion: 400_000,
		PublishedAt: now.Add(-2 * time.Hour), ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(2 * time.Hour),
		PublicProvenance: "InferCrane integration-test retail rate",
	})
	if err != nil {
		t.Fatal(err)
	}
	storedRate, err := s.PublishModelAPIRetailRate(ctx, rate)
	if err != nil || storedRate.ContractDigest != rate.ContractDigest {
		t.Fatalf("publish rate=%+v err=%v", storedRate, err)
	}
	replayedRate, err := s.PublishModelAPIRetailRate(ctx, rate)
	if err != nil || replayedRate.ID != storedRate.ID {
		t.Fatalf("idempotent rate replay=%+v err=%v", replayedRate, err)
	}
	currentRate, err := s.CurrentModelAPIRetailRate(ctx, product.ID, now)
	if err != nil || currentRate.ID != storedRate.ID {
		t.Fatalf("current rate=%+v err=%v", currentRate, err)
	}
	if _, err = s.CurrentModelAPIRetailRate(ctx, product.ID, rate.ValidUntil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired current rate error=%v, want not found", err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE model_api_retail_rate_cards SET output_microusd_per_million=output_microusd_per_million+1 WHERE id=?`, rate.ID); err == nil {
		t.Fatal("immutable retail rate accepted an update")
	}

	supplyPlanID := "model-api-supply-plan-" + suffix
	if _, err = s.ExecContext(ctx, `INSERT INTO model_api_supply_plans(id,operator_tenant_id,managed_product_id,protocol,schema_version,digest,status,ranking_basis,request_json,plan_json,generated_at,created_at) VALUES(?,?,?,?,?,?, 'ready','test','{}'::jsonb,'{}'::jsonb,?,?)`,
		supplyPlanID, operatorTenant, product.ID, product.Protocol, "managed-model-supply-plan/v1", "sha256:test-"+suffix, now, now); err != nil {
		t.Fatal(err)
	}
	evidenceUntil := now.Add(time.Hour)
	publication := modelapiproduct.OperatorPublication{
		SchemaVersion: modelapiproduct.OperatorProjectionSchemaVersion,
		ProductID:     product.ID, OperatorWorkspaceID: operatorTenant, ServingPlanID: servingPlanID, SupplyPlanID: supplyPlanID,
		Qualification: modelapiproduct.RouteQualification{State: modelapiproduct.QualificationQualified, EvidenceID: "private-evidence-" + suffix, EvidenceUntil: &evidenceUntil},
		RetailRate:    &storedRate,
	}
	publication, err = s.SaveModelAPIOperatorPublication(ctx, operatorTenant, publication)
	if err != nil {
		t.Fatal(err)
	}
	if publication.OperatorWorkspaceID != operatorTenant || publication.RetailRate == nil || publication.RetailRate.ID != rate.ID {
		t.Fatalf("stored operator publication=%+v", publication)
	}
	if _, err = s.ModelAPIOperatorPublication(ctx, otherOperator, product.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other operator read error=%v, want not found", err)
	}
	takeover := publication
	takeover.OperatorWorkspaceID = otherOperator
	if _, err = s.SaveModelAPIOperatorPublication(ctx, otherOperator, takeover); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-operator publication takeover error=%v", err)
	}

	product.Availability = modelapiproduct.AvailabilityAvailable
	product, err = s.SaveManagedModelAPIProduct(ctx, product)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := s.PublicModelAPIProduct(ctx, product.ID, now)
	if err != nil || !projection.Callable || projection.RetailRate == nil {
		t.Fatalf("public projection=%+v err=%v", projection, err)
	}
	publicJSON, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{operatorTenant, servingPlanID, supplyPlanID, publication.Qualification.EvidenceID} {
		if strings.Contains(string(publicJSON), private) {
			t.Fatalf("public projection leaked %q: %s", private, publicJSON)
		}
	}

	rpm, monthly, maximum := int64(60), int64(10_000_000), int64(100_000)
	entitlement := modelapiproduct.ProductEntitlement{
		SchemaVersion:       modelapiproduct.EntitlementSchemaVersion,
		CustomerWorkspaceID: customerTenant, ProductID: product.ID,
		OperatorWorkspaceID: operatorTenant, ServingPlanID: servingPlanID,
		RetailRateID: rate.ID, RetailRateVersion: rate.Version, State: modelapiproduct.EntitlementActive,
		Limits:    modelapiproduct.CustomerLimits{RequestsPerMinute: &rpm, MonthlySpendMicrousd: &monthly, MaxRequestMicrousd: &maximum},
		ValidFrom: now.Add(-time.Minute),
	}
	entitlement, err = s.SaveModelAPIProductEntitlement(ctx, customerTenant, entitlement)
	if err != nil || entitlement.ID == "" {
		t.Fatalf("entitlement=%+v err=%v", entitlement, err)
	}
	if _, err = s.ModelAPIProductEntitlement(ctx, otherCustomer, product.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other customer read error=%v, want not found", err)
	}
	crossTenant := entitlement
	crossTenant.CustomerWorkspaceID = customerTenant
	if _, err = s.SaveModelAPIProductEntitlement(ctx, otherCustomer, crossTenant); err == nil {
		t.Fatal("cross-customer entitlement write succeeded")
	}
	access, err := s.ModelAPIProductAccess(ctx, customerTenant, product.ID, now)
	if err != nil || !access.Authorized || access.Entitlement == nil {
		t.Fatalf("customer access=%+v err=%v", access, err)
	}
	accessJSON, err := json.Marshal(access)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{operatorTenant, servingPlanID, supplyPlanID, publication.Qualification.EvidenceID} {
		if strings.Contains(string(accessJSON), private) {
			t.Fatalf("customer access leaked %q: %s", private, accessJSON)
		}
	}
	otherAccess, err := s.ModelAPIProductAccess(ctx, otherCustomer, product.ID, now)
	if err != nil || otherAccess.Authorized || otherAccess.Entitlement != nil {
		t.Fatalf("other customer access=%+v err=%v", otherAccess, err)
	}

	// Stale evidence can exist in storage during an incident, but public and
	// customer reads must immediately stop authorizing traffic.
	if _, err = s.ExecContext(ctx, `UPDATE model_api_operator_publications SET qualification_valid_until=? WHERE product_id=? AND operator_tenant_id=?`, now.Add(-time.Second), product.ID, operatorTenant); err != nil {
		t.Fatal(err)
	}
	staleProjection, err := s.PublicModelAPIProduct(ctx, product.ID, now)
	if err != nil || staleProjection.Callable || staleProjection.Availability != modelapiproduct.AvailabilityUnavailable || staleProjection.RetailRate == nil {
		t.Fatalf("stale public projection=%+v err=%v", staleProjection, err)
	}
	staleAccess, err := s.ModelAPIProductAccess(ctx, customerTenant, product.ID, now)
	if err != nil || staleAccess.Authorized {
		t.Fatalf("stale customer access=%+v err=%v", staleAccess, err)
	}
}
