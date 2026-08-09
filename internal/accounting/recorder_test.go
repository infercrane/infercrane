package accounting

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type failingSink struct{}

func (failingSink) RecordRequest(context.Context, domain.InferenceRecord) error {
	return errors.New("database unavailable")
}

func TestRecorderExportsQueueDropAndPersistenceFailureEvidence(t *testing.T) {
	backpressured := &Recorder{queue: make(chan record, 1)}
	_ = backpressured.RecordRequest(context.Background(), domain.InferenceRecord{RequestID: "queued"})
	_ = backpressured.RecordRequest(context.Background(), domain.InferenceRecord{RequestID: "dropped"})
	var queueOutput bytes.Buffer
	backpressured.WritePrometheus(&queueOutput)
	if !strings.Contains(queueOutput.String(), "infercrane_accounting_queue_depth 1") || !strings.Contains(queueOutput.String(), "infercrane_accounting_records_dropped_total 1") {
		t.Fatalf("queue metrics=%s", queueOutput.String())
	}
	close(backpressured.queue)

	recorder := New(failingSink{}, nil, 1, 1)
	if err := recorder.RecordRequest(context.Background(), domain.InferenceRecord{RequestID: "request-1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	recorder.WritePrometheus(&output)
	for _, expected := range []string{"infercrane_accounting_queue_capacity 1", "infercrane_accounting_persist_failures_total 1"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, output.String())
		}
	}
}
