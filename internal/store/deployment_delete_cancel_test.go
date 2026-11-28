package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

var deployKinds = []string{"deployment.converge", "deployment.serverless.converge"}

// The rule needs no database: a deletion cancels a deploy that no worker is
// running, and refuses every other unfinished operation.
func TestRefuseOpenOperationCancelsOnlyADeployNoWorkerRuns(t *testing.T) {
	for _, test := range []struct {
		kind, status string
		leased       bool
		refused      bool
		running      bool
	}{
		{"deployment.converge", "pending", false, false, false},
		{"deployment.converge", "waiting", false, false, false},
		{"deployment.serverless.converge", "waiting", false, false, false},
		// An expired lease belongs to no worker; any worker may claim it.
		{"deployment.converge", "running", false, false, false},
		{"deployment.converge", "leased", false, false, false},
		{"deployment.converge", "cancelling", false, false, false},
		{"deployment.converge", "running", true, true, true},
		{"deployment.converge", "leased", true, true, true},
		{"deployment.converge", "cancelling", true, true, true},
		{"deployment.scale", "waiting", false, true, false},
		{"rollout.promote", "running", true, true, true},
		{"deployment.delete", "pending", false, true, false},
	} {
		refused := refuseOpenOperation("op-1", test.kind, test.status, test.leased, deployKinds)
		if (refused != nil) != test.refused || (refused != nil && (refused.Running != test.running || refused.ID != "op-1" || !errors.Is(refused, ErrConflict))) {
			t.Fatalf("%+v: refused=%+v", test, refused)
		}
	}
	if refuseOpenOperation("op-1", "deployment.converge", "waiting", false, nil) == nil {
		t.Fatal("a deletion that cancels nothing cancelled a deploy")
	}
}

func submitStuckDeploy(t *testing.T, ctx context.Context, s *Store, name string) (domain.Deployment, domain.Operation) {
	t.Helper()
	deployment, operation, _, err := s.SubmitCloudDeployment(ctx,
		domain.Deployment{Name: name, Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1},
		domain.Operation{Kind: "deployment.converge", IdempotencyKey: "create-" + name, RequestJSON: `{"name":"` + name + `","model":"Qwen/Qwen3-8B","cloud":"kubernetes","gpu":"NVIDIA-A10G","tenant_id":"global"}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	return deployment, operation
}

func deleteRequest(deployment domain.Deployment) domain.Operation {
	return domain.Operation{Kind: "deployment.delete", IdempotencyKey: "delete-" + deployment.Name, RequestJSON: `{"deployment_id":"` + deployment.ID + `","name":"` + deployment.Name + `","tenant_id":"global"}`}
}

// Written for PostgreSQL (INFERCRANE_TEST_DATABASE_URL); it skips without it.
func TestDeleteCancelsAWaitingDeployAndQueuesCleanup(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	for _, status := range []string{"pending", "waiting"} {
		name := "stuck-" + status + "-" + time.Now().UTC().Format("150405.000000000")
		deployment, deploy := submitStuckDeploy(t, ctx, s, name)
		if status == "waiting" {
			// A failed attempt leaves the deploy waiting for its next one,
			// with no lease.
			if _, err := s.ExecContext(ctx, `UPDATE operations SET status='waiting',attempt=1,message='runtime_starting',next_attempt_at=NOW()+INTERVAL '1 minute' WHERE id=?`, deploy.ID); err != nil {
				t.Fatal(err)
			}
		}
		deletion, err := s.SubmitDeploymentDeleteCancelling(ctx, "global", name, deployment.ID, deleteRequest(deployment), deployKinds)
		if err != nil || !deletion.Created || deletion.Operation.Status != "pending" || deletion.Cancelled == nil || deletion.Cancelled.ID != deploy.ID {
			t.Fatalf("%s: deletion=%+v err=%v", status, deletion, err)
		}
		cancelled, err := s.Operation(ctx, deploy.ID)
		if err != nil || cancelled.Status != "cancelled" || !cancelled.CancelRequested || cancelled.CompletedAt == nil {
			t.Fatalf("%s: deploy after delete=%+v err=%v", status, cancelled, err)
		}
		var desired, observed string
		if err = s.QueryRowContext(ctx, `SELECT desired_state,observed_state FROM deployments WHERE id=?`, deployment.ID).Scan(&desired, &observed); err != nil || desired != "deleted" || observed != "deleting" {
			t.Fatalf("%s: state=(%s,%s,%v)", status, desired, observed, err)
		}
		// A retry with the same key returns the same deletion.
		again, err := s.SubmitDeploymentDeleteCancelling(ctx, "global", name, deployment.ID, deleteRequest(deployment), deployKinds)
		if err != nil || again.Created || again.Operation.ID != deletion.Operation.ID || again.Cancelled != nil {
			t.Fatalf("%s: retry=%+v err=%v", status, again, err)
		}
	}
}

// Written for PostgreSQL (INFERCRANE_TEST_DATABASE_URL); it skips without it.
func TestDeleteRefusesADeployAWorkerIsRunningAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "running-" + time.Now().UTC().Format("150405.000000000")
	deployment, deploy := submitStuckDeploy(t, ctx, s, name)
	if _, err := s.ExecContext(ctx, `UPDATE operations SET status='running',attempt=1,lease_owner='worker-a',lease_generation=1,lease_expires_at=NOW()+INTERVAL '1 minute' WHERE id=?`, deploy.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.SubmitDeploymentDeleteCancelling(ctx, "global", name, deployment.ID, deleteRequest(deployment), deployKinds)
	var open *OpenOperationError
	if !errors.As(err, &open) || !open.Running || open.ID != deploy.ID || !errors.Is(err, ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	current, err := s.Operation(ctx, deploy.ID)
	if err != nil || current.Status != "running" || current.CancelRequested {
		t.Fatalf("running deploy was changed: %+v err=%v", current, err)
	}
	var desired string
	if err = s.QueryRowContext(ctx, `SELECT desired_state FROM deployments WHERE id=?`, deployment.ID).Scan(&desired); err != nil || desired != "running" {
		t.Fatalf("desired=%s err=%v", desired, err)
	}
	// Once its lease has expired, no worker runs it, and the delete goes on.
	if _, err = s.ExecContext(ctx, `UPDATE operations SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=?`, deploy.ID); err != nil {
		t.Fatal(err)
	}
	deletion, err := s.SubmitDeploymentDeleteCancelling(ctx, "global", name, deployment.ID, deleteRequest(deployment), deployKinds)
	if err != nil || !deletion.Created || deletion.Cancelled == nil || deletion.Cancelled.Status != "cancelled" {
		t.Fatalf("deletion=%+v err=%v", deletion, err)
	}
}

// Written for PostgreSQL (INFERCRANE_TEST_DATABASE_URL); it skips without it.
func TestDeleteRefusesAnUnfinishedOperationOfAnotherKind(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "scaling-" + time.Now().UTC().Format("150405.000000000")
	deployment, _ := submitStuckDeploy(t, ctx, s, name)
	if _, err := s.ExecContext(ctx, `UPDATE operations SET status='succeeded',completed_at=NOW() WHERE resource_name=?`, name); err != nil {
		t.Fatal(err)
	}
	scale, _, err := s.EnqueueOperation(ctx, domain.Operation{TenantID: "global", Kind: "deployment.scale", ResourceType: "deployment", ResourceName: name, IdempotencyKey: "scale-" + name, RequestJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, kinds := range [][]string{deployKinds, nil} {
		_, err = s.SubmitDeploymentDeleteCancelling(ctx, "global", name, deployment.ID, deleteRequest(deployment), kinds)
		var open *OpenOperationError
		if !errors.As(err, &open) || open.Running || open.ID != scale.ID || open.Kind != "deployment.scale" {
			t.Fatalf("kinds=%v err=%v", kinds, err)
		}
	}
	if current, lookupErr := s.Operation(ctx, scale.ID); lookupErr != nil || current.Status != "pending" || current.CancelRequested {
		t.Fatalf("scale was changed: %+v err=%v", current, lookupErr)
	}
	if _, _, err = s.SubmitDeploymentDelete(ctx, "global", name, deployment.ID, deleteRequest(deployment)); !errors.Is(err, ErrConflict) {
		t.Fatalf("plain delete err=%v", err)
	}
}
