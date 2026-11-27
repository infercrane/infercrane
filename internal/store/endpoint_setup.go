package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

// EndpointActiveSetup returns the active revision of the one deployment an
// endpoint routes to: its name, its revision number, and its spec. An
// endpoint that routes to no deployment with an active revision, or to more
// than one, has no single setup, and the result is ErrNotFound.
func (s *Store) EndpointActiveSetup(ctx context.Context, tenant, endpointName string) (string, int, domain.DeploymentRevisionSpec, error) {
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return "", 0, domain.DeploymentRevisionSpec{}, err
	}
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT d.name,r.revision_number,r.spec_json::text FROM backend_bindings b JOIN deployments d ON d.id=b.deployment_id AND d.tenant_id=b.tenant_id AND d.desired_state<>'deleted' JOIN deployment_revisions r ON r.id=d.active_revision_id WHERE b.tenant_id=? AND b.endpoint_id=? AND b.deployment_id IS NOT NULL LIMIT 2`, tenant, resolved.Endpoint.ID)
	if err != nil {
		return "", 0, domain.DeploymentRevisionSpec{}, err
	}
	defer rows.Close()
	var name, spec string
	var revision, found int
	for rows.Next() {
		found++
		if err = rows.Scan(&name, &revision, &spec); err != nil {
			return "", 0, domain.DeploymentRevisionSpec{}, err
		}
	}
	if err = rows.Err(); err != nil {
		return "", 0, domain.DeploymentRevisionSpec{}, err
	}
	if found != 1 {
		return "", 0, domain.DeploymentRevisionSpec{}, ErrNotFound
	}
	var decoded domain.DeploymentRevisionSpec
	if err = json.Unmarshal([]byte(spec), &decoded); err != nil {
		return "", 0, domain.DeploymentRevisionSpec{}, fmt.Errorf("decode the active revision of %s: %w", name, err)
	}
	return name, revision, decoded, nil
}
