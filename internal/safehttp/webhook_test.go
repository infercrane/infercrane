package safehttp

import (
	"context"
	"net"
	"net/http"
	"testing"
)

func TestWebhookClientRejectsPrivateResolvedAddress(t *testing.T) {
	client := WebhookClient(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}, false)
	request, _ := http.NewRequest(http.MethodPost, "https://example.invalid/hook", nil)
	if _, err := client.Do(request); err == nil {
		t.Fatal("private webhook address accepted")
	}
}
func TestWebhookClientRejectsRedirects(t *testing.T) {
	client := WebhookClient(nil, false)
	if err := client.CheckRedirect(&http.Request{}, nil); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestProhibitedRejectsSharedAndReservedDestinations(t *testing.T) {
	for _, value := range []string{
		"100.64.0.1",
		"192.0.2.1",
		"198.18.0.1",
		"203.0.113.1",
		"240.0.0.1",
		"2001:db8::1",
	} {
		if !prohibited(net.ParseIP(value)) {
			t.Fatalf("non-public destination %s was allowed", value)
		}
	}
	if prohibited(net.ParseIP("8.8.8.8")) {
		t.Fatal("public destination was prohibited")
	}
}
