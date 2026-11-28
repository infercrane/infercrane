package controlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/workflows"
)

func deleteDeployment(t *testing.T, fake *fakeStore) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/deployments/qwen", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Idempotency-Key", "delete-qwen")
	response := httptest.NewRecorder()
	(API{Store: fake, APIKey: "secret"}).Handler().ServeHTTP(response, request)
	return response
}

// A deploy that never became ready no longer blocks its own deletion: the
// delete cancels it, records the cancellation as operation cancel does, and
// queues the cleanup.
func TestDeleteCancelsTheUnfinishedDeploy(t *testing.T) {
	cancelled := domain.Operation{ID: "deploy-1", Kind: workflows.ConvergeKind, ResourceType: "deployment", ResourceName: "qwen", Status: "cancelled", CancelRequested: true, Message: "cancelled by deployment delete"}
	fake := &fakeStore{created: true, deleteCancelled: &cancelled, activeOperation: domain.Operation{ID: "deploy-1", Kind: workflows.ConvergeKind, Status: "waiting"}}
	response := deleteDeployment(t, fake)
	if response.Code != http.StatusAccepted || fake.operation.Kind != workflows.DeleteKind {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), fake.operation)
	}
	if !slices.Equal(fake.deleteCancelKinds, []string{workflows.ConvergeKind, workflows.ServerlessConvergeKind}) {
		t.Fatalf("delete may cancel %v", fake.deleteCancelKinds)
	}
	var body struct {
		Operation          domain.Operation  `json:"operation"`
		CancelledOperation *domain.Operation `json:"cancelled_operation"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Operation.Kind != workflows.DeleteKind || body.CancelledOperation == nil || body.CancelledOperation.ID != "deploy-1" || body.CancelledOperation.Status != "cancelled" {
		t.Fatalf("body=%s", response.Body.String())
	}
	if len(fake.audited) != 1 || fake.audited[0].Action != "operation.cancel" || fake.audited[0].ResourceType != "operation" || fake.audited[0].ResourceName != "deploy-1" || fake.audited[0].Outcome != "accepted" {
		t.Fatalf("audit=%+v", fake.audited)
	}
}

// A serverless deploy cancelled before its endpoint registered a target
// still gets the serverless cleanup.
func TestDeleteOfAnUnfinishedServerlessDeployUsesServerlessCleanup(t *testing.T) {
	cancelled := domain.Operation{ID: "deploy-1", Kind: workflows.ServerlessConvergeKind, Status: "cancelled"}
	fake := &fakeStore{created: true, deleteCancelled: &cancelled, activeOperation: domain.Operation{ID: "deploy-1", Kind: workflows.ServerlessConvergeKind, Status: "waiting"}}
	if response := deleteDeployment(t, fake); response.Code != http.StatusAccepted || fake.operation.Kind != workflows.ServerlessDeleteKind {
		t.Fatalf("response=%d %s operation=%#v", response.Code, response.Body.String(), fake.operation)
	}
}

func TestDeleteWithoutAnOpenOperationCancelsNothing(t *testing.T) {
	fake := &fakeStore{created: true}
	response := deleteDeployment(t, fake)
	if response.Code != http.StatusAccepted || strings.Contains(response.Body.String(), "cancelled_operation") || len(fake.audited) != 0 {
		t.Fatalf("response=%d %s audit=%+v", response.Code, response.Body.String(), fake.audited)
	}
}

// Only a deploy is cancelled, and only while no worker runs it. Every other
// case changes nothing and says why.
func TestDeleteRefusesWhereCancellingIsUnsafe(t *testing.T) {
	for _, test := range []struct {
		open store.OpenOperationError
		want string
	}{
		{store.OpenOperationError{ID: "deploy-1", Kind: workflows.ConvergeKind, Status: "running", Running: true}, "running on a worker right now, so deleting now could race the provider calls"},
		{store.OpenOperationError{ID: "deploy-1", Kind: workflows.ConvergeKind, Status: "cancelling", Running: true}, "being cancelled, and a worker is running its cleanup now"},
		{store.OpenOperationError{ID: "scale-1", Kind: workflows.ScaleKind, Status: "waiting"}, "Delete cancels an unfinished deploy by itself, but not other changes"},
	} {
		open := test.open
		fake := &fakeStore{deleteErr: fmt.Errorf("submit: %w", &open)}
		response := deleteDeployment(t, fake)
		var body struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusConflict || body.Error.Code != "unresolved_operation" || !strings.Contains(body.Error.Message, test.want) || !strings.Contains(body.Error.Message, open.ID) || !strings.Contains(body.Error.Message, "Nothing was changed") {
			t.Fatalf("%+v: response=%d %s", open, response.Code, response.Body.String())
		}
		if len(fake.audited) != 0 {
			t.Fatalf("%+v: a refused delete recorded %+v", open, fake.audited)
		}
	}
}
