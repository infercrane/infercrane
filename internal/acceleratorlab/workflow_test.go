package acceleratorlab

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

type checkpointFixture struct {
	stages []string
}

func (fixture *checkpointFixture) CheckpointClaimedOperation(_ context.Context, _, _ string, _ int64, step, _, checkpoint string, _ int, _ string) error {
	if strings.Contains(checkpoint, "tenant") {
		return errors.New("tenant identity must not be copied into progress payloads")
	}
	fixture.stages = append(fixture.stages, step)
	return nil
}

func TestHandlerPersistsContentFreeProgressAndReturnsEvidence(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.TenantID = "untrusted"
	encoded, _ := json.Marshal(request)
	checkpoint := &checkpointFixture{}
	engine := Engine{Profiler: fixtureProfiler(0), Builder: fixtureBuilder(0), Qualifier: fixtureQualifier(nil)}
	handler := Handlers(engine, checkpoint)[ExecuteKind]
	result, err := handler(context.Background(), domain.Operation{ID: "operation-1", TenantID: "tenant-1", RequestJSON: string(encoded), LeaseOwner: "worker-1", LeaseGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"state":"qualified"`) || strings.Contains(result, "untrusted") || strings.Join(checkpoint.stages, ",") != "profile,search,build,measure,prove" {
		t.Fatalf("result=%s stages=%v", result, checkpoint.stages)
	}
}

func TestHandlerTreatsExpiredAuthorityAsPermanent(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Policy.AuthorizedUntil = request.Policy.AuthorizedUntil.Add(-2 * time.Hour)
	encoded, _ := json.Marshal(request)
	handler := Handlers(Engine{Profiler: fixtureProfiler(0), Builder: fixtureBuilder(0), Qualifier: fixtureQualifier(nil)}, nil)[ExecuteKind]
	_, err := handler(context.Background(), domain.Operation{TenantID: "tenant-1", RequestJSON: string(encoded)})
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Retryable || failure.Code != "accelerator_lab_authority_expired" {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
}
