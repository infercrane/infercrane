package benchmark

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (fn roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestRunMeasuresSuccessfulRequests(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if request.Header.Get("Authorization") != "Bearer secret" {
			status = http.StatusUnauthorized
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`)), Header: make(http.Header)}, nil
	})}
	result, err := Run(context.Background(), Config{Endpoint: "http://infercrane", APIKey: "secret", Model: "model", Requests: 10, Concurrency: 3, Client: client})
	if err != nil || result.Succeeded != 10 || result.Failed != 0 || result.P95Milliseconds < 0 {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}
