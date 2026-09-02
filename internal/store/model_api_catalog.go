package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
)

const modelAPIProductSelect = `SELECT id,display_name,publisher,description,protocol,tasks_json::text,capability_contract_json::text,input_modalities_json::text,output_modalities_json::text,context_window_tokens,availability,self_host_eligibility FROM model_api_products`
const modelAPIRateSelect = `SELECT id,product_id,version,currency,input_microusd_per_million,cached_input_microusd_per_million,output_microusd_per_million,valid_from,valid_until,published_at,public_provenance,contract_digest FROM model_api_retail_rate_cards`
const modelAPIOperatorPublicationSelect = `SELECT product_id,operator_tenant_id,serving_plan_id,supply_plan_id,qualification_state,qualification_evidence_id,qualification_valid_until,active_retail_rate_card_id,updated_at FROM model_api_operator_publications`
const modelAPIEntitlementSelect = `SELECT id,customer_tenant_id,product_id,operator_tenant_id,serving_plan_id,retail_rate_card_id,retail_rate_version,state,requests_per_minute,tokens_per_minute,monthly_spend_microusd,max_request_microusd,valid_from,valid_until,created_at,updated_at FROM model_api_product_entitlements`

const modelAPIOperatorPublicationForTenantQuery = modelAPIOperatorPublicationSelect + ` WHERE operator_tenant_id=? AND product_id=?`
const modelAPIEntitlementForTenantQuery = modelAPIEntitlementSelect + ` WHERE customer_tenant_id=? AND product_id=?`

type ModelAPIProductAccess struct {
	Product     modelapiproduct.PublicProjection               `json:"product"`
	Entitlement *modelapiproduct.CustomerEntitlementProjection `json:"entitlement,omitempty"`
	Authorized  bool                                           `json:"authorized"`
}

// SaveManagedModelAPIProduct persists the supplier-neutral product contract.
// Moving a product into a callable display state requires an already persisted,
// current operator publication; catalog metadata alone can never activate it.
func (s *Store) SaveManagedModelAPIProduct(ctx context.Context, product modelapiproduct.Product) (modelapiproduct.Product, error) {
	if err := product.Validate(); err != nil {
		return modelapiproduct.Product{}, err
	}
	tasks, capabilities, inputs, outputs, err := encodeModelAPIProductJSON(product)
	if err != nil {
		return modelapiproduct.Product{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return modelapiproduct.Product{}, err
	}
	defer tx.Rollback()

	if product.Availability == modelapiproduct.AvailabilityAvailable || product.Availability == modelapiproduct.AvailabilityDegraded {
		publication, publicationErr := modelAPIOperatorPublicationForProduct(ctx, tx, product.ID)
		if publicationErr != nil {
			if errors.Is(publicationErr, ErrNotFound) {
				return modelapiproduct.Product{}, errors.New("callable product availability requires an operator publication")
			}
			return modelapiproduct.Product{}, publicationErr
		}
		if err = publication.ValidateAt(product, time.Now().UTC()); err != nil {
			return modelapiproduct.Product{}, fmt.Errorf("callable product publication is stale or incomplete: %w", err)
		}
	}

	stamp := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO model_api_products(id,display_name,publisher,description,protocol,tasks_json,capability_contract_json,input_modalities_json,output_modalities_json,context_window_tokens,availability,self_host_eligibility,created_at,updated_at) VALUES(?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?::jsonb,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET display_name=EXCLUDED.display_name,publisher=EXCLUDED.publisher,description=EXCLUDED.description,protocol=EXCLUDED.protocol,tasks_json=EXCLUDED.tasks_json,capability_contract_json=EXCLUDED.capability_contract_json,input_modalities_json=EXCLUDED.input_modalities_json,output_modalities_json=EXCLUDED.output_modalities_json,context_window_tokens=EXCLUDED.context_window_tokens,availability=EXCLUDED.availability,self_host_eligibility=EXCLUDED.self_host_eligibility,updated_at=EXCLUDED.updated_at`,
		product.ID, product.DisplayName, product.Publisher, product.Description, product.Protocol,
		tasks, capabilities, inputs, outputs, nullableInt64(product.ContextWindowTokens), product.Availability, product.SelfHostEligibility, stamp, stamp)
	if err != nil {
		return modelapiproduct.Product{}, err
	}
	if err = tx.Commit(); err != nil {
		return modelapiproduct.Product{}, err
	}
	return s.ManagedModelAPIProduct(ctx, product.ID)
}

func (s *Store) ManagedModelAPIProduct(ctx context.Context, productID string) (modelapiproduct.Product, error) {
	if productID == "" {
		return modelapiproduct.Product{}, errors.New("product id is required")
	}
	return scanModelAPIProduct(s.QueryRowContext(ctx, modelAPIProductSelect+` WHERE id=?`, productID))
}

func (s *Store) ManagedModelAPIProducts(ctx context.Context) ([]modelapiproduct.Product, error) {
	rows, err := s.QueryContext(ctx, modelAPIProductSelect+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := make([]modelapiproduct.Product, 0, 6)
	for rows.Next() {
		product, scanErr := scanModelAPIProduct(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		products = append(products, product)
	}
	return products, rows.Err()
}

// PublishModelAPIRetailRate inserts one immutable version. Replays of the exact
// contract are idempotent; conflicting IDs or versions fail without mutation.
func (s *Store) PublishModelAPIRetailRate(ctx context.Context, rate modelapiproduct.RetailRate) (modelapiproduct.RetailRate, error) {
	if err := rate.Validate(); err != nil {
		return modelapiproduct.RetailRate{}, err
	}
	// PostgreSQL stores timestamptz at microsecond precision. Reject a contract
	// that would be rounded on write, because that would invalidate its digest.
	for _, value := range []time.Time{rate.ValidFrom, rate.ValidUntil, rate.PublishedAt} {
		if !value.Equal(value.Truncate(time.Microsecond)) {
			return modelapiproduct.RetailRate{}, errors.New("retail rate timestamps must use PostgreSQL-safe microsecond precision")
		}
	}
	_, err := s.ExecContext(ctx, `INSERT INTO model_api_retail_rate_cards(id,product_id,version,currency,input_microusd_per_million,cached_input_microusd_per_million,output_microusd_per_million,valid_from,valid_until,published_at,public_provenance,contract_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		rate.ID, rate.ProductID, rate.Version, rate.Currency, rate.InputMicrousdPerMillion,
		nullableInt64(rate.CachedInputMicrousdPerMillion), rate.OutputMicrousdPerMillion,
		rate.ValidFrom.UTC(), rate.ValidUntil.UTC(), rate.PublishedAt.UTC(), rate.PublicProvenance, rate.ContractDigest)
	if err == nil {
		return s.ModelAPIRetailRate(ctx, rate.ProductID, rate.Version)
	}
	if !isUniqueViolation(err) {
		return modelapiproduct.RetailRate{}, err
	}
	existing, lookupErr := s.ModelAPIRetailRate(ctx, rate.ProductID, rate.Version)
	if lookupErr == nil && modelAPIRatesEqual(existing, rate) {
		return existing, nil
	}
	return modelapiproduct.RetailRate{}, fmt.Errorf("%w: retail rate id or version already has a different contract", ErrConflict)
}

func (s *Store) ModelAPIRetailRate(ctx context.Context, productID string, version int) (modelapiproduct.RetailRate, error) {
	if productID == "" || version <= 0 {
		return modelapiproduct.RetailRate{}, errors.New("product id and positive retail rate version are required")
	}
	return scanModelAPIRetailRate(s.QueryRowContext(ctx, modelAPIRateSelect+` WHERE product_id=? AND version=?`, productID, version))
}

func (s *Store) CurrentModelAPIRetailRate(ctx context.Context, productID string, at time.Time) (modelapiproduct.RetailRate, error) {
	if productID == "" || at.IsZero() {
		return modelapiproduct.RetailRate{}, errors.New("product id and evaluation time are required")
	}
	rate, err := scanModelAPIRetailRate(s.QueryRowContext(ctx, modelAPIRateSelect+` WHERE product_id=? AND valid_from<=? AND valid_until>? ORDER BY version DESC LIMIT 1`, productID, at.UTC(), at.UTC()))
	if err != nil {
		return modelapiproduct.RetailRate{}, err
	}
	if !rate.CurrentAt(at) {
		return modelapiproduct.RetailRate{}, ErrNotFound
	}
	return rate, nil
}

// SaveModelAPIOperatorPublication is operator-scoped. A second operator cannot
// take over a product publication, and referenced plans must belong to the same
// operator and public product.
func (s *Store) SaveModelAPIOperatorPublication(ctx context.Context, operatorTenant string, publication modelapiproduct.OperatorPublication) (modelapiproduct.OperatorPublication, error) {
	if operatorTenant == "" || publication.OperatorWorkspaceID != operatorTenant {
		return modelapiproduct.OperatorPublication{}, errors.New("operator tenant must own the publication")
	}
	product, err := s.ManagedModelAPIProduct(ctx, publication.ProductID)
	if err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	publication.UpdatedAt = time.Now().UTC()
	if err = publication.ValidateAt(product, publication.UpdatedAt); err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	defer tx.Rollback()
	var planExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM serving_plans WHERE id=? AND tenant_id=?)`, publication.ServingPlanID, operatorTenant).Scan(&planExists); err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	if !planExists {
		return modelapiproduct.OperatorPublication{}, ErrNotFound
	}
	var supplyPlanExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_api_supply_plans WHERE id=? AND operator_tenant_id=? AND managed_product_id=?)`, publication.SupplyPlanID, operatorTenant, publication.ProductID).Scan(&supplyPlanExists); err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	if !supplyPlanExists {
		return modelapiproduct.OperatorPublication{}, ErrNotFound
	}
	if publication.RetailRate != nil {
		storedRate, rateErr := modelAPIRetailRateByVersion(ctx, tx, publication.ProductID, publication.RetailRate.Version)
		if rateErr != nil {
			return modelapiproduct.OperatorPublication{}, rateErr
		}
		if !modelAPIRatesEqual(storedRate, *publication.RetailRate) {
			return modelapiproduct.OperatorPublication{}, fmt.Errorf("%w: publication retail rate does not match the immutable stored contract", ErrConflict)
		}
	}
	var existingOperator sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT operator_tenant_id FROM model_api_operator_publications WHERE product_id=? FOR UPDATE`, publication.ProductID).Scan(&existingOperator)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return modelapiproduct.OperatorPublication{}, err
	}
	if existingOperator.Valid && existingOperator.String != operatorTenant {
		return modelapiproduct.OperatorPublication{}, fmt.Errorf("%w: product publication belongs to another operator", ErrConflict)
	}
	stamp := publication.UpdatedAt
	result, err := tx.ExecContext(ctx, `INSERT INTO model_api_operator_publications(product_id,operator_tenant_id,serving_plan_id,supply_plan_id,qualification_state,qualification_evidence_id,qualification_valid_until,active_retail_rate_card_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(product_id) DO UPDATE SET serving_plan_id=EXCLUDED.serving_plan_id,supply_plan_id=EXCLUDED.supply_plan_id,qualification_state=EXCLUDED.qualification_state,qualification_evidence_id=EXCLUDED.qualification_evidence_id,qualification_valid_until=EXCLUDED.qualification_valid_until,active_retail_rate_card_id=EXCLUDED.active_retail_rate_card_id,updated_at=EXCLUDED.updated_at WHERE model_api_operator_publications.operator_tenant_id=EXCLUDED.operator_tenant_id`,
		publication.ProductID, operatorTenant, publication.ServingPlanID, publication.SupplyPlanID,
		publication.Qualification.State, publication.Qualification.EvidenceID, nullableTime(publication.Qualification.EvidenceUntil),
		nullableRateID(publication.RetailRate), stamp, stamp)
	if err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
		return modelapiproduct.OperatorPublication{}, affectedErr
	} else if affected == 0 {
		return modelapiproduct.OperatorPublication{}, fmt.Errorf("%w: product publication belongs to another operator", ErrConflict)
	}
	if err = tx.Commit(); err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	return s.ModelAPIOperatorPublication(ctx, operatorTenant, publication.ProductID)
}

func (s *Store) ModelAPIOperatorPublication(ctx context.Context, operatorTenant, productID string) (modelapiproduct.OperatorPublication, error) {
	if operatorTenant == "" || productID == "" {
		return modelapiproduct.OperatorPublication{}, errors.New("operator tenant and product id are required")
	}
	return modelAPIOperatorPublicationFromRow(ctx, s, s.QueryRowContext(ctx, modelAPIOperatorPublicationForTenantQuery, operatorTenant, productID))
}

// SaveModelAPIProductEntitlement persists the customer-owned authorization to
// shared operator supply. Active grants must match the current publication,
// serving plan, and immutable retail rate exactly.
func (s *Store) SaveModelAPIProductEntitlement(ctx context.Context, customerTenant string, entitlement modelapiproduct.ProductEntitlement) (modelapiproduct.ProductEntitlement, error) {
	if customerTenant == "" || entitlement.CustomerWorkspaceID != customerTenant {
		return modelapiproduct.ProductEntitlement{}, errors.New("customer tenant must own the entitlement")
	}
	stamp := time.Now().UTC()
	if entitlement.ID == "" {
		id, err := newID()
		if err != nil {
			return modelapiproduct.ProductEntitlement{}, err
		}
		entitlement.ID = id
	}
	if entitlement.CreatedAt.IsZero() {
		entitlement.CreatedAt = stamp
	}
	entitlement.UpdatedAt = stamp
	if err := entitlement.Validate(); err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	product, err := s.ManagedModelAPIProduct(ctx, entitlement.ProductID)
	if err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	publication, err := s.ModelAPIOperatorPublication(ctx, entitlement.OperatorWorkspaceID, entitlement.ProductID)
	if err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	if publication.ServingPlanID != entitlement.ServingPlanID || publication.RetailRate == nil || publication.RetailRate.ID != entitlement.RetailRateID || publication.RetailRate.Version != entitlement.RetailRateVersion {
		return modelapiproduct.ProductEntitlement{}, fmt.Errorf("%w: entitlement does not match the operator publication and retail rate", ErrConflict)
	}
	if entitlement.State == modelapiproduct.EntitlementActive {
		if err = publication.ValidateAt(product, stamp); err != nil || !entitlement.ActiveAt(stamp) {
			return modelapiproduct.ProductEntitlement{}, errors.New("active entitlement requires a current callable publication and validity window")
		}
		projection, projectionErr := modelapiproduct.PublicProjectionAt(product, &publication, stamp)
		if projectionErr != nil || !projection.Callable {
			return modelapiproduct.ProductEntitlement{}, errors.New("active entitlement requires a callable public product")
		}
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	defer tx.Rollback()
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM model_api_product_entitlements WHERE customer_tenant_id=? AND product_id=? FOR UPDATE`, customerTenant, entitlement.ProductID).Scan(&existingID)
	if err == nil && existingID != entitlement.ID {
		return modelapiproduct.ProductEntitlement{}, fmt.Errorf("%w: customer product already has a different entitlement identity", ErrConflict)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return modelapiproduct.ProductEntitlement{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO model_api_product_entitlements(id,customer_tenant_id,product_id,operator_tenant_id,serving_plan_id,retail_rate_card_id,retail_rate_version,state,requests_per_minute,tokens_per_minute,monthly_spend_microusd,max_request_microusd,valid_from,valid_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(customer_tenant_id,product_id) DO UPDATE SET operator_tenant_id=EXCLUDED.operator_tenant_id,serving_plan_id=EXCLUDED.serving_plan_id,retail_rate_card_id=EXCLUDED.retail_rate_card_id,retail_rate_version=EXCLUDED.retail_rate_version,state=EXCLUDED.state,requests_per_minute=EXCLUDED.requests_per_minute,tokens_per_minute=EXCLUDED.tokens_per_minute,monthly_spend_microusd=EXCLUDED.monthly_spend_microusd,max_request_microusd=EXCLUDED.max_request_microusd,valid_from=EXCLUDED.valid_from,valid_until=EXCLUDED.valid_until,updated_at=EXCLUDED.updated_at WHERE model_api_product_entitlements.id=EXCLUDED.id`,
		entitlement.ID, customerTenant, entitlement.ProductID, entitlement.OperatorWorkspaceID, entitlement.ServingPlanID,
		entitlement.RetailRateID, entitlement.RetailRateVersion, entitlement.State,
		nullableInt64(entitlement.Limits.RequestsPerMinute), nullableInt64(entitlement.Limits.TokensPerMinute),
		nullableInt64(entitlement.Limits.MonthlySpendMicrousd), nullableInt64(entitlement.Limits.MaxRequestMicrousd),
		entitlement.ValidFrom.UTC(), nullableTime(entitlement.ValidUntil), entitlement.CreatedAt.UTC(), stamp)
	if err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
		return modelapiproduct.ProductEntitlement{}, affectedErr
	} else if affected == 0 {
		return modelapiproduct.ProductEntitlement{}, fmt.Errorf("%w: customer product already has a different entitlement identity", ErrConflict)
	}
	if err = tx.Commit(); err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	return s.ModelAPIProductEntitlement(ctx, customerTenant, entitlement.ProductID)
}

func (s *Store) ModelAPIProductEntitlement(ctx context.Context, customerTenant, productID string) (modelapiproduct.ProductEntitlement, error) {
	if customerTenant == "" || productID == "" {
		return modelapiproduct.ProductEntitlement{}, errors.New("customer tenant and product id are required")
	}
	return scanModelAPIProductEntitlement(s.QueryRowContext(ctx, modelAPIEntitlementForTenantQuery, customerTenant, productID))
}

func (s *Store) PublicModelAPIProduct(ctx context.Context, productID string, at time.Time) (modelapiproduct.PublicProjection, error) {
	if productID == "" || at.IsZero() {
		return modelapiproduct.PublicProjection{}, errors.New("product id and evaluation time are required")
	}
	product, err := s.ManagedModelAPIProduct(ctx, productID)
	if err != nil {
		return modelapiproduct.PublicProjection{}, err
	}
	publication, err := modelAPIOperatorPublicationForProduct(ctx, s, productID)
	if errors.Is(err, ErrNotFound) {
		return modelapiproduct.PublicProjectionAt(product, nil, at)
	}
	if err != nil {
		return modelapiproduct.PublicProjection{}, err
	}
	return modelapiproduct.PublicProjectionAt(product, &publication, at)
}

func (s *Store) PublicModelAPIProducts(ctx context.Context, at time.Time) ([]modelapiproduct.PublicProjection, error) {
	if at.IsZero() {
		return nil, errors.New("evaluation time is required")
	}
	products, err := s.ManagedModelAPIProducts(ctx)
	if err != nil {
		return nil, err
	}
	projections := make([]modelapiproduct.PublicProjection, 0, len(products))
	for _, product := range products {
		projection, projectionErr := s.PublicModelAPIProduct(ctx, product.ID, at)
		if projectionErr != nil {
			return nil, projectionErr
		}
		projections = append(projections, projection)
	}
	return projections, nil
}

// ModelAPIProductAccess combines only customer-safe projections. Internal plan
// references never leave the repository through this result.
func (s *Store) ModelAPIProductAccess(ctx context.Context, customerTenant, productID string, at time.Time) (ModelAPIProductAccess, error) {
	product, err := s.PublicModelAPIProduct(ctx, productID, at)
	if err != nil {
		return ModelAPIProductAccess{}, err
	}
	access := ModelAPIProductAccess{Product: product}
	entitlement, err := s.ModelAPIProductEntitlement(ctx, customerTenant, productID)
	if errors.Is(err, ErrNotFound) {
		return access, nil
	}
	if err != nil {
		return ModelAPIProductAccess{}, err
	}
	projection, err := entitlement.CustomerProjection()
	if err != nil {
		return ModelAPIProductAccess{}, err
	}
	access.Entitlement = &projection
	access.Authorized = product.Callable && entitlement.ActiveAt(at) && product.RetailRate != nil && product.RetailRate.ID == entitlement.RetailRateID && product.RetailRate.Version == entitlement.RetailRateVersion
	return access, nil
}

type modelAPICatalogQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type modelAPIRowScanner interface {
	Scan(...any) error
}

func scanModelAPIProduct(row modelAPIRowScanner) (modelapiproduct.Product, error) {
	product := modelapiproduct.Product{SchemaVersion: modelapiproduct.ProductSchemaVersion}
	var tasksJSON, capabilitiesJSON, inputsJSON, outputsJSON string
	var contextTokens sql.NullInt64
	err := row.Scan(&product.ID, &product.DisplayName, &product.Publisher, &product.Description, &product.Protocol,
		&tasksJSON, &capabilitiesJSON, &inputsJSON, &outputsJSON, &contextTokens, &product.Availability, &product.SelfHostEligibility)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapiproduct.Product{}, ErrNotFound
	}
	if err != nil {
		return modelapiproduct.Product{}, err
	}
	if err = json.Unmarshal([]byte(tasksJSON), &product.Tasks); err != nil {
		return modelapiproduct.Product{}, fmt.Errorf("decode model API product tasks: %w", err)
	}
	if err = json.Unmarshal([]byte(capabilitiesJSON), &product.Capabilities); err != nil {
		return modelapiproduct.Product{}, fmt.Errorf("decode model API product capabilities: %w", err)
	}
	if err = json.Unmarshal([]byte(inputsJSON), &product.InputModalities); err != nil {
		return modelapiproduct.Product{}, fmt.Errorf("decode model API product input modalities: %w", err)
	}
	if err = json.Unmarshal([]byte(outputsJSON), &product.OutputModalities); err != nil {
		return modelapiproduct.Product{}, fmt.Errorf("decode model API product output modalities: %w", err)
	}
	if contextTokens.Valid {
		product.ContextWindowTokens = &contextTokens.Int64
	}
	if err = product.Validate(); err != nil {
		return modelapiproduct.Product{}, fmt.Errorf("stored model API product is invalid: %w", err)
	}
	return product, nil
}

func scanModelAPIRetailRate(row modelAPIRowScanner) (modelapiproduct.RetailRate, error) {
	var rate modelapiproduct.RetailRate
	var cached sql.NullInt64
	err := row.Scan(&rate.ID, &rate.ProductID, &rate.Version, &rate.Currency, &rate.InputMicrousdPerMillion,
		&cached, &rate.OutputMicrousdPerMillion, &rate.ValidFrom, &rate.ValidUntil, &rate.PublishedAt,
		&rate.PublicProvenance, &rate.ContractDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapiproduct.RetailRate{}, ErrNotFound
	}
	if err != nil {
		return modelapiproduct.RetailRate{}, err
	}
	if cached.Valid {
		rate.CachedInputMicrousdPerMillion = &cached.Int64
	}
	if err = rate.Validate(); err != nil {
		return modelapiproduct.RetailRate{}, fmt.Errorf("stored model API retail rate is invalid: %w", err)
	}
	return rate, nil
}

func scanModelAPIProductEntitlement(row modelAPIRowScanner) (modelapiproduct.ProductEntitlement, error) {
	entitlement := modelapiproduct.ProductEntitlement{SchemaVersion: modelapiproduct.EntitlementSchemaVersion}
	var requests, tokens, monthly, maximum sql.NullInt64
	var validUntil sql.NullTime
	err := row.Scan(&entitlement.ID, &entitlement.CustomerWorkspaceID, &entitlement.ProductID, &entitlement.OperatorWorkspaceID,
		&entitlement.ServingPlanID, &entitlement.RetailRateID, &entitlement.RetailRateVersion, &entitlement.State,
		&requests, &tokens, &monthly, &maximum, &entitlement.ValidFrom, &validUntil, &entitlement.CreatedAt, &entitlement.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapiproduct.ProductEntitlement{}, ErrNotFound
	}
	if err != nil {
		return modelapiproduct.ProductEntitlement{}, err
	}
	entitlement.Limits = modelapiproduct.CustomerLimits{
		RequestsPerMinute: nullableInt64Pointer(requests), TokensPerMinute: nullableInt64Pointer(tokens),
		MonthlySpendMicrousd: nullableInt64Pointer(monthly), MaxRequestMicrousd: nullableInt64Pointer(maximum),
	}
	if validUntil.Valid {
		value := validUntil.Time.UTC()
		entitlement.ValidUntil = &value
	}
	if err = entitlement.Validate(); err != nil {
		return modelapiproduct.ProductEntitlement{}, fmt.Errorf("stored model API entitlement is invalid: %w", err)
	}
	return entitlement, nil
}

func modelAPIOperatorPublicationForProduct(ctx context.Context, query modelAPICatalogQueryer, productID string) (modelapiproduct.OperatorPublication, error) {
	return modelAPIOperatorPublicationFromRow(ctx, query, query.QueryRowContext(ctx, modelAPIOperatorPublicationSelect+` WHERE product_id=?`, productID))
}

func modelAPIOperatorPublicationFromRow(ctx context.Context, query modelAPICatalogQueryer, row modelAPIRowScanner) (modelapiproduct.OperatorPublication, error) {
	publication := modelapiproduct.OperatorPublication{SchemaVersion: modelapiproduct.OperatorProjectionSchemaVersion}
	var evidenceUntil sql.NullTime
	var rateID sql.NullString
	err := row.Scan(&publication.ProductID, &publication.OperatorWorkspaceID, &publication.ServingPlanID, &publication.SupplyPlanID,
		&publication.Qualification.State, &publication.Qualification.EvidenceID, &evidenceUntil, &rateID, &publication.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapiproduct.OperatorPublication{}, ErrNotFound
	}
	if err != nil {
		return modelapiproduct.OperatorPublication{}, err
	}
	if evidenceUntil.Valid {
		value := evidenceUntil.Time.UTC()
		publication.Qualification.EvidenceUntil = &value
	}
	if rateID.Valid {
		rate, rateErr := scanModelAPIRetailRate(query.QueryRowContext(ctx, modelAPIRateSelect+` WHERE id=? AND product_id=?`, rateID.String, publication.ProductID))
		if rateErr != nil {
			return modelapiproduct.OperatorPublication{}, rateErr
		}
		publication.RetailRate = &rate
	}
	return publication, nil
}

func modelAPIRetailRateByVersion(ctx context.Context, query modelAPICatalogQueryer, productID string, version int) (modelapiproduct.RetailRate, error) {
	return scanModelAPIRetailRate(query.QueryRowContext(ctx, modelAPIRateSelect+` WHERE product_id=? AND version=?`, productID, version))
}

func encodeModelAPIProductJSON(product modelapiproduct.Product) (string, string, string, string, error) {
	values := []any{product.Tasks, product.Capabilities, product.InputModalities, product.OutputModalities}
	encoded := make([]string, len(values))
	for index, value := range values {
		body, err := json.Marshal(value)
		if err != nil {
			return "", "", "", "", err
		}
		encoded[index] = string(body)
	}
	return encoded[0], encoded[1], encoded[2], encoded[3], nil
}

func modelAPIRatesEqual(left, right modelapiproduct.RetailRate) bool {
	return left.ID == right.ID && left.ProductID == right.ProductID && left.Version == right.Version && left.Currency == right.Currency &&
		left.InputMicrousdPerMillion == right.InputMicrousdPerMillion && equalOptionalInt64(left.CachedInputMicrousdPerMillion, right.CachedInputMicrousdPerMillion) &&
		left.OutputMicrousdPerMillion == right.OutputMicrousdPerMillion && left.ValidFrom.Equal(right.ValidFrom) && left.ValidUntil.Equal(right.ValidUntil) &&
		left.PublishedAt.Equal(right.PublishedAt) && left.PublicProvenance == right.PublicProvenance && left.ContractDigest == right.ContractDigest
}

func equalOptionalInt64(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	copy := value.Int64
	return &copy
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func nullableRateID(rate *modelapiproduct.RetailRate) any {
	if rate == nil {
		return nil
	}
	return rate.ID
}
