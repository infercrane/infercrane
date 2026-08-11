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
