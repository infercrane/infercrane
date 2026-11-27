package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// A plan starts from what an endpoint runs: the active revision of the one
// deployment it routes to.
func TestEndpointActiveSetupIsTheActiveRevisionOfItsOneDeployment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "setup-" + time.Now().UTC().Format("150405.000000000")
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://setup.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target}); err != nil {
		t.Fatal(err)
	}
	deployment, revision, spec, err := s.EndpointActiveSetup(ctx, "global", name)
	if err != nil || deployment != name || revision != 1 || spec.Runtime != "vllm" || spec.Model != "org/model" {
		t.Fatalf("deployment=%s revision=%d spec=%+v err=%v", deployment, revision, spec, err)
	}
	if _, _, _, err = s.EndpointActiveSetup(ctx, "global", name+"-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown endpoint err=%v", err)
	}
	if _, _, _, err = s.EndpointActiveSetup(ctx, "another-tenant", name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant err=%v", err)
	}
}
