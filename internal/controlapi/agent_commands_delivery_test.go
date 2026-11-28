package controlapi

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

// deliveryWriter is a response writer whose write or flush can fail, or that
// cannot flush at all.
type deliveryWriter struct {
	header   http.Header
	body     bytes.Buffer
	writeErr error
}

func (d *deliveryWriter) Header() http.Header { return d.header }
func (d *deliveryWriter) WriteHeader(int)     {}
func (d *deliveryWriter) Write(p []byte) (int, error) {
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	return d.body.Write(p)
}

type flushFailingWriter struct {
	deliveryWriter
	flushErr error
}

func (f *flushFailingWriter) FlushError() error { return f.flushErr }

// A long-poll answer counts as delivered only when it reached the
// connection, so an answer whose write or flush fails releases its commands.
func TestCommandAnswersReportWhetherTheyReachedTheConnection(t *testing.T) {
	answer := agentprotocol.CommandsResponse{Commands: []agentprotocol.Command{{ID: "command-0001", Kind: agentprotocol.CommandRunCandidate, Attempt: 1}}}
	recorder := httptest.NewRecorder()
	if err := writeDeliveredJSON(recorder, http.StatusOK, answer); err != nil || recorder.Code != http.StatusOK || !recorder.Flushed || !strings.Contains(recorder.Body.String(), `"id":"command-0001"`) {
		t.Fatalf("delivered answer: %v %d %q", err, recorder.Code, recorder.Body.String())
	}
	broken := errors.New("write: broken pipe")
	if err := writeDeliveredJSON(&deliveryWriter{header: http.Header{}, writeErr: broken}, http.StatusOK, answer); !errors.Is(err, broken) {
		t.Fatalf("failed write: %v", err)
	}
	reset := errors.New("write: connection reset by peer")
	if err := writeDeliveredJSON(&flushFailingWriter{deliveryWriter: deliveryWriter{header: http.Header{}}, flushErr: reset}, http.StatusOK, answer); !errors.Is(err, reset) {
		t.Fatalf("failed flush: %v", err)
	}
	// A writer that cannot flush shows no failure; the answer is sent when
	// the handler returns.
	plain := &deliveryWriter{header: http.Header{}}
	if err := writeDeliveredJSON(plain, http.StatusOK, answer); err != nil || !strings.Contains(plain.body.String(), `"attempt":1`) {
		t.Fatalf("unflushable writer: %v %q", err, plain.body.String())
	}
}
