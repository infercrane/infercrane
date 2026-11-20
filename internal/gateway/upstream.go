package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// upstreamErrorLimit bounds how much of a server-error body the gateway reads
// to tell a model server's error from an error written in between.
const upstreamErrorLimit = 64 << 10

// upstreamLogLimit bounds how much of that body reaches the server log.
const upstreamLogLimit = 512

// upstreamClient returns the client that sends inference requests upstream,
// with g.Client's settings and an upstreamTransport in front of its transport.
func (g *Gateway) upstreamClient() *http.Client {
	client := g.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 128, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 5 * time.Minute}}
	}
	guarded := *client
	guarded.Transport = upstreamTransport{next: client.Transport, logger: g.Logger}
	return &guarded
}

// upstreamTransport keeps what went wrong upstream in the server log and out of
// the caller's response.
//
// Model servers answer an error with a JSON object, or inside the event
// stream they opened. A 5xx with any other body was written by something in
// between: the instance-local vLLM Router answers a worker it cannot reach with
// 500 "Request failed: error sending request for url (http://<service>.<namespace>.svc:8080/...)",
// and a proxy answers with an HTML page. That text names internal hosts and is
// not the caller's to read, and it means no model server answered. So the
// transport returns it as a transport error, which proxyInference already
// handles like its own connection failure: it may retry within the endpoint's
// retry budget, fail over to a primary-fallback endpoint's next binding, and
// otherwise answers 503 upstream_unavailable with its own message.
// 502, 503 and 504 are left alone; proxyInference never forwards their bodies.
type upstreamTransport struct {
	next   http.RoundTripper
	logger *slog.Logger
}

// upstreamServerError is a server error that no model server wrote.
type upstreamServerError struct{ status int }

func (e upstreamServerError) Error() string {
	return "upstream answered HTTP " + strconv.Itoa(e.status) + " without a model server error body"
}

func (t upstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if err != nil {
		// A request the caller cancelled or that ran out of time is recorded as
		// such; only failures of the upstream itself are logged.
		if req.Context().Err() == nil {
			t.log(req, "error", err)
		}
		return nil, err
	}
	switch {
	case resp.StatusCode < http.StatusInternalServerError,
		resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout,
		isEventStream(resp.Header.Get("Content-Type")):
		return resp, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorLimit+1))
	if readErr == nil && len(body) <= upstreamErrorLimit && isJSONObject(body) {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{bytes.NewReader(body), resp.Body}
		return resp, nil
	}
	_ = resp.Body.Close()
	t.log(req, "upstream_status", resp.StatusCode, "upstream_body", strings.ToValidUTF8(string(body[:min(len(body), upstreamLogLimit)]), "?"))
	return nil, upstreamServerError{status: resp.StatusCode}
}

func (t upstreamTransport) log(req *http.Request, attributes ...any) {
	if t.logger == nil {
		return
	}
	attributes = append([]any{"request_id", req.Header.Get("X-Request-ID"), "attempt", req.Header.Get("X-InferCrane-Attempt"), "upstream_url", req.URL.Redacted()}, attributes...)
	t.logger.Warn("inference upstream failed", attributes...)
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "text/event-stream"
}

func isJSONObject(body []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(body, &object) == nil && object != nil
}
