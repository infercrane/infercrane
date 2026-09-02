package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/infercrane/infercrane/internal/modelapitarget"
)

const modelAPITargetBindingSelect = `SELECT schema_version,id,operator_tenant_id,managed_product_id,target_kind,offer_id,offer_version,adapter,supplier_model_id,endpoint_reference,endpoint_config_digest,region,valid_from,valid_until,created_at,contract_digest FROM model_api_target_bindings`

// PublishModelAPITargetBinding stores one immutable execution contract. Exact
// replays are idempotent; changing any pinned field requires a new binding ID.
func (s *Store) PublishModelAPITargetBinding(ctx context.Context, operatorTenant string, binding modelapitarget.Binding) (modelapitarget.Binding, error) {
	if operatorTenant == "" || binding.OperatorTenantID != operatorTenant {
		return modelapitarget.Binding{}, errors.New("operator tenant must own the target binding")
	}
	if err := binding.Validate(); err != nil {
		return modelapitarget.Binding{}, err
	}
	if err := requirePostgresSafeTargetBindingTimes(binding); err != nil {
		return modelapitarget.Binding{}, err
	}
	offer, err := s.ModelAPISupplierOffer(ctx, operatorTenant, binding.OfferID, binding.OfferVersion)
	if err != nil {
		return modelapitarget.Binding{}, fmt.Errorf("target binding supplier offer: %w", err)
	}
	if offer.ProductID != binding.ProductID || offer.Adapter != binding.Adapter || offer.SupplierModelID != binding.SupplierModelID || offer.Region != binding.Region {
		return modelapitarget.Binding{}, errors.New("target binding must exactly match the pinned supplier offer revision")
	}
	result, err := s.ExecContext(ctx, `INSERT INTO model_api_target_bindings(schema_version,id,operator_tenant_id,managed_product_id,target_kind,offer_id,offer_version,adapter,supplier_model_id,endpoint_reference,endpoint_config_digest,region,valid_from,valid_until,created_at,contract_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		binding.SchemaVersion, binding.ID, operatorTenant, binding.ProductID, binding.Kind, binding.OfferID, binding.OfferVersion,
		binding.Adapter, binding.SupplierModelID, binding.EndpointReference, binding.EndpointConfigDigest, binding.Region,
		binding.ValidFrom.UTC(), binding.ValidUntil.UTC(), binding.CreatedAt.UTC(), binding.ContractDigest)
	if err != nil {
		return modelapitarget.Binding{}, err
	}
	stored, loadErr := s.ModelAPITargetBinding(ctx, operatorTenant, binding.ID)
	if loadErr != nil {
		if affected, _ := result.RowsAffected(); affected == 0 && errors.Is(loadErr, ErrNotFound) {
			return modelapitarget.Binding{}, fmt.Errorf("%w: target binding identity belongs to another operator", ErrConflict)
		}
		return modelapitarget.Binding{}, loadErr
	}
	if affected, _ := result.RowsAffected(); affected == 0 && !reflect.DeepEqual(stored, binding) {
		return modelapitarget.Binding{}, fmt.Errorf("%w: target binding identity already has a different immutable contract", ErrConflict)
	}
	return stored, nil
}

func (s *Store) ModelAPITargetBinding(ctx context.Context, operatorTenant, bindingID string) (modelapitarget.Binding, error) {
	if operatorTenant == "" || bindingID == "" {
		return modelapitarget.Binding{}, errors.New("operator tenant and target binding id are required")
	}
	return scanModelAPITargetBinding(s.QueryRowContext(ctx, modelAPITargetBindingSelect+` WHERE operator_tenant_id=? AND id=?`, operatorTenant, bindingID))
}

// ModelAPITargetBindings returns the complete immutable history for one
// operator product, newest validity windows first.
func (s *Store) ModelAPITargetBindings(ctx context.Context, operatorTenant, productID string) ([]modelapitarget.Binding, error) {
	if operatorTenant == "" || productID == "" {
		return nil, errors.New("operator tenant and product id are required")
	}
	return s.queryModelAPITargetBindings(ctx, modelAPITargetBindingSelect+` WHERE operator_tenant_id=? AND managed_product_id=? ORDER BY valid_from DESC,created_at DESC,id`, operatorTenant, productID)
}

// CurrentModelAPITargetBindings returns every binding whose half-open validity
// window contains at. Multiple current bindings are allowed so a later route
// snapshot can choose primaries, fallbacks, or weighted targets explicitly.
func (s *Store) CurrentModelAPITargetBindings(ctx context.Context, operatorTenant, productID string, at time.Time) ([]modelapitarget.Binding, error) {
	if operatorTenant == "" || productID == "" || at.IsZero() {
		return nil, errors.New("operator tenant, product id, and evaluation time are required")
	}
	return s.queryModelAPITargetBindings(ctx, modelAPITargetBindingSelect+` WHERE operator_tenant_id=? AND managed_product_id=? AND valid_from<=? AND valid_until>? ORDER BY valid_from DESC,created_at DESC,id`, operatorTenant, productID, at.UTC(), at.UTC())
}

func (s *Store) queryModelAPITargetBindings(ctx context.Context, query string, args ...any) ([]modelapitarget.Binding, error) {
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]modelapitarget.Binding, 0)
	for rows.Next() {
		binding, scanErr := scanModelAPITargetBinding(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, binding)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scanModelAPITargetBinding(row interface{ Scan(...any) error }) (modelapitarget.Binding, error) {
	var binding modelapitarget.Binding
	err := row.Scan(&binding.SchemaVersion, &binding.ID, &binding.OperatorTenantID, &binding.ProductID, &binding.Kind,
		&binding.OfferID, &binding.OfferVersion, &binding.Adapter, &binding.SupplierModelID, &binding.EndpointReference,
		&binding.EndpointConfigDigest, &binding.Region, &binding.ValidFrom, &binding.ValidUntil, &binding.CreatedAt, &binding.ContractDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return modelapitarget.Binding{}, ErrNotFound
	}
	if err != nil {
		return modelapitarget.Binding{}, err
	}
	binding.ValidFrom = binding.ValidFrom.UTC()
	binding.ValidUntil = binding.ValidUntil.UTC()
	binding.CreatedAt = binding.CreatedAt.UTC()
	return binding, binding.Validate()
}

func requirePostgresSafeTargetBindingTimes(binding modelapitarget.Binding) error {
	for _, value := range []time.Time{binding.ValidFrom, binding.ValidUntil, binding.CreatedAt} {
		if !value.Equal(value.Truncate(time.Microsecond)) {
			return errors.New("target binding timestamps must use PostgreSQL-safe microsecond precision")
		}
	}
	return nil
}
