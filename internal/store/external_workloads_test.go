package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestSandboxReferenceIssuesEndpointRestrictedExpiringCredential(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "sandbox-target", URL: "http://sandbox-target", Provider: "existing", Runtime: "vllm", UpstreamModel: "model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "coder-production", Model: "model"}, []string{"sandbox-target"}); err != nil {
		t.Fatal(err)
	}
	reference, token, err := s.CreateSandboxReference(ctx, "global", domain.SandboxReference{Provider: "e2b", ExternalID: "sandbox-42", ExternalRevision: "template-v3", EndpointName: "coder-production", MetadataJSON: `{"team":"agents"}`}, 30*time.Minute)
	if err != nil || token == "" || reference.PrincipalID == "" {
		t.Fatalf("reference=%#v token=%t err=%v", reference, token != "", err)
	}
	principal, err := s.AuthenticatePrincipal(ctx, token)
	if err != nil || principal.Kind != "inference_token" || len(principal.EndpointNames) != 1 || principal.EndpointNames[0] != "coder-production" || principal.ExpiresAt == nil {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	if _, _, err = s.CreateSandboxReference(ctx, "global", domain.SandboxReference{Provider: "e2b", ExternalID: "sandbox-42", EndpointName: "coder-production", MetadataJSON: `{}`}, 30*time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate error=%v, want conflict", err)
	}
	if _, _, err = s.CreateSandboxReference(ctx, "global", domain.SandboxReference{Provider: "e2b", ExternalID: "sandbox-content", EndpointName: "coder-production", MetadataJSON: `{"commands":["cat /etc/passwd"]}`}, 30*time.Minute); err == nil {
		t.Fatal("command content entered sandbox metadata")
	}
	if _, _, err = s.CreateSandboxReference(ctx, "global", domain.SandboxReference{Provider: "e2b", ExternalID: "sandbox-nested-content", EndpointName: "coder-production", MetadataJSON: `{"labels":{"Secret":"must-not-persist"}}`}, 30*time.Minute); err == nil {
		t.Fatal("nested secret entered sandbox metadata")
	}
	rotated, err := s.RotateSandboxCredential(ctx, "global", reference.ID)
	if err != nil || rotated == "" || rotated == token {
		t.Fatalf("rotation token=%t changed=%t err=%v", rotated != "", rotated != token, err)
	}
	if _, err = s.AuthenticatePrincipal(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token error=%v, want not found", err)
	}
	if err = s.RevokeSandboxReference(ctx, "global", reference.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticatePrincipal(ctx, rotated); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token error=%v, want not found", err)
	}
	reconnected, replacementToken, err := s.CreateSandboxReference(ctx, "global", domain.SandboxReference{Provider: "e2b", ExternalID: "sandbox-42", ExternalRevision: "template-v4", EndpointName: "coder-production", MetadataJSON: `{"team":"agents"}`}, 30*time.Minute)
	if err != nil {
		t.Fatalf("reconnect revoked sandbox reference: %v", err)
	}
	if reconnected.ID == reference.ID || reconnected.PrincipalID == reference.PrincipalID || replacementToken == "" || replacementToken == token || replacementToken == rotated {
		t.Fatalf("reconnect did not issue fresh identity: old=%+v new=%+v", reference, reconnected)
	}
	rows, err := s.SandboxReferences(ctx, "global")
	if err != nil || len(rows) != 2 || rows[0].Status != "referenced" || rows[1].Status != "stopped" {
		t.Fatalf("references=%#v err=%v", rows, err)
	}
}

func TestTrainingHandoffIsRevisionBoundImmutableAndTenantSafe(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.AddTarget(ctx, domain.Target{Name: "training-target", URL: "http://training-target", Provider: "existing", Runtime: "vllm", UpstreamModel: "base"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: "trained-coder", Model: "base"}, []string{"training-target"}); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateCandidateRevision(ctx, "global", "trained-coder", `{"model":"trained","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1,"autoscaling_enabled":false}`)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	handoff := domain.TrainingArtifactHandoff{RevisionID: candidate.ID, Provider: "mlflow", ExternalRunID: "run-42", Repository: "mlflow://models/coder/42", ImmutableRevision: "42", ArtifactDigest: digest, BaseModelIdentity: "acme/base@1", Method: "lora", Framework: "transformers", FrameworkVersion: "5.0.0", DatasetFingerprint: "sha256:" + strings.Repeat("b", 64), PayloadDigest: "sha256:" + strings.Repeat("c", 64), Signature: "signature", PublicKey: "public", Algorithm: "Ed25519-SHA256", KeyID: "sha256:key"}
	artifact := domain.ModelArtifact{Source: "training-handoff", Repository: handoff.Repository, ImmutableRevision: handoff.ImmutableRevision, ModelIdentity: handoff.Repository + "@" + handoff.ImmutableRevision, RuntimeCompatibilityJSON: `{}`}
	created, attached, err := s.AttachTrainingArtifactHandoff(ctx, "global", "trained-coder", handoff, artifact)
	if err != nil || created.ModelArtifactID == "" || attached.ID != created.ModelArtifactID {
		t.Fatalf("handoff=%#v artifact=%#v err=%v", created, attached, err)
	}
	again, againArtifact, err := s.AttachTrainingArtifactHandoff(ctx, "global", "trained-coder", handoff, artifact)
	if err != nil || again.ID != created.ID || againArtifact.ID != attached.ID {
		t.Fatalf("idempotent handoff=%#v artifact=%#v err=%v", again, againArtifact, err)
	}
	if again.ExternalRunID != handoff.ExternalRunID || again.Provider != handoff.Provider || again.Repository != handoff.Repository || again.CreatedAt.IsZero() {
		t.Fatal("idempotent handoff returned incomplete persisted evidence")
	}
	changed := handoff
	changed.PayloadDigest = "sha256:" + strings.Repeat("d", 64)
	if _, _, err = s.AttachTrainingArtifactHandoff(ctx, "global", "trained-coder", changed, artifact); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed handoff error=%v, want conflict", err)
	}
	if _, _, err = s.AttachTrainingArtifactHandoff(ctx, "other", "trained-coder", handoff, artifact); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant error=%v, want not found", err)
	}
	rows, err := s.TrainingArtifactHandoffs(ctx, "global", "trained-coder")
	if err != nil || len(rows) != 1 || rows[0].ArtifactDigest != digest {
		t.Fatalf("handoffs=%#v err=%v", rows, err)
	}
}
