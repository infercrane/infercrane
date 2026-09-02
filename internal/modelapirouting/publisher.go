package modelapirouting

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// RouteSourceStore returns one fully validated, secret-free generation. A
// malformed durable generation is an error, never a partially usable result.
type RouteSourceStore interface {
	PublishedModelAPIRoutes(context.Context, time.Time) ([]RouteSource, error)
}

// TargetResolver is the narrow trusted boundary that maps an operator-owned
// adapter and secret reference to a callable target. Store never sees secret
// values and Directory never sees secret references.
type TargetResolver interface {
	ResolveHostedModelTarget(ctx context.Context, operatorTenantID, supplier, adapter, credentialReference string) (ResolvedTarget, error)
}

type ResolvedTarget struct {
	Endpoint   string
	Credential string
}

type SecretReferenceStore interface {
	SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error)
}

type SecretValueResolver interface {
	Resolve(context.Context, domain.SecretReference) (string, error)
}

// RegistryTargetResolver is the production trusted-boundary implementation.
// Adapter base URLs come from operator configuration; a database row cannot
// inject an arbitrary upstream URL. Secret IDs are resolved tenant-scoped.
type RegistryTargetResolver struct {
	endpoints  map[string]string
	references SecretReferenceStore
	secrets    SecretValueResolver
}

func NewRegistryTargetResolver(endpoints map[string]string, references SecretReferenceStore, secrets SecretValueResolver) (*RegistryTargetResolver, error) {
	if references == nil || secrets == nil || len(endpoints) == 0 {
		return nil, errors.New("hosted target resolver requires endpoints, reference store, and secret resolver")
	}
	validated := make(map[string]string, len(endpoints))
	for supplierAdapter, endpoint := range endpoints {
		supplierAdapter = strings.TrimSpace(supplierAdapter)
		parsed, err := url.Parse(strings.TrimSpace(endpoint))
		if supplierAdapter == "" || !strings.Contains(supplierAdapter, "/") || err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("hosted adapter endpoints must be named absolute HTTPS URLs without query or fragment")
		}
		validated[supplierAdapter] = strings.TrimRight(parsed.String(), "/")
	}
	return &RegistryTargetResolver{endpoints: validated, references: references, secrets: secrets}, nil
}

func (r *RegistryTargetResolver) ResolveHostedModelTarget(ctx context.Context, operatorTenantID, supplier, adapter, credentialReference string) (ResolvedTarget, error) {
	if r == nil || operatorTenantID == "" || supplier == "" || adapter == "" || credentialReference == "" {
		return ResolvedTarget{}, errors.New("operator, supplier, adapter, and credential reference are required")
	}
	endpoint, exists := r.endpoints[supplier+"/"+adapter]
	if !exists {
		return ResolvedTarget{}, errors.New("hosted supplier adapter is not configured")
	}
	reference, err := r.references.SecretReferenceForTenant(ctx, operatorTenantID, credentialReference)
	if err != nil {
		return ResolvedTarget{}, err
	}
	credential, err := r.secrets.Resolve(ctx, reference)
	if err != nil {
		return ResolvedTarget{}, err
	}
	if strings.TrimSpace(credential) == "" {
		return ResolvedTarget{}, errors.New("hosted supplier credential resolved empty")
	}
	return ResolvedTarget{Endpoint: endpoint, Credential: credential}, nil
}

// Publisher periodically compiles a complete generation and swaps it into the
// request-path Directory atomically. Any source, secret, or validation failure
// retains the previous known-good snapshot.
type Publisher struct {
	Store     RouteSourceStore
	Resolver  TargetResolver
	Directory *Directory
	Interval  time.Duration
	Logger    *slog.Logger
	Now       func() time.Time
}

func (p *Publisher) PublishOnce(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Resolver == nil || p.Directory == nil {
		return errors.New("hosted route publisher requires store, target resolver, and directory")
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	at := now().UTC()
	sources, err := p.Store.PublishedModelAPIRoutes(ctx, at)
	if err != nil {
		return err
	}
	routes := make([]PublishedRoute, 0, len(sources))
	for _, source := range sources {
		if !source.Entitlement.activeAt(at) || !source.Publication.currentAt(at) || !source.Rate.currentAt(at) {
			return errors.New("hosted route source is expired or inactive")
		}
		route := PublishedRoute{Entitlement: source.Entitlement, Publication: source.Publication, Rate: source.Rate}
		for _, candidateSource := range source.Candidates {
			if strings.TrimSpace(candidateSource.Adapter) == "" || strings.TrimSpace(candidateSource.CredentialReference) == "" {
				return errors.New("published hosted candidate lacks adapter or credential reference")
			}
			target, resolveErr := p.Resolver.ResolveHostedModelTarget(ctx, source.Publication.OperatorTenantID, candidateSource.Candidate.Supplier, candidateSource.Adapter, candidateSource.CredentialReference)
			if resolveErr != nil {
				return resolveErr
			}
			candidate := candidateSource.Candidate
			candidate.Endpoint, candidate.Credential = target.Endpoint, target.Credential
			if strings.TrimSpace(candidate.Credential) == "" {
				return errors.New("resolved hosted candidate credential is empty")
			}
			if !candidate.validFor(source.Publication, at) || candidate.CompatibilityKey != source.Publication.CompatibilityKey {
				return errors.New("resolved hosted candidate is expired or incompatible")
			}
			route.Candidates = append(route.Candidates, candidate)
		}
		routes = append(routes, route)
	}
	return p.Directory.Publish(routes)
}

func (p *Publisher) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	p.publishAndLog(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.publishAndLog(ctx)
		}
	}
}

func (p *Publisher) publishAndLog(ctx context.Context) {
	if err := p.PublishOnce(ctx); err != nil && p.Logger != nil {
		p.Logger.Error("retain previous hosted model API route generation", "error", err)
	}
}
