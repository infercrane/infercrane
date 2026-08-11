// Package safehttp owns hardened transports for user-configured destinations.
package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

type Resolver func(context.Context, string) ([]net.IPAddr, error)

func WebhookClient(resolve Resolver, allowPrivate bool) *http.Client {
	if resolve == nil {
		resolve = net.DefaultResolver.LookupIPAddr
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolve(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("webhook destination DNS could not be validated")
		}
		var last error
		for _, candidate := range addresses {
			if !allowPrivate && prohibited(candidate.IP) {
				continue
			}
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			last = dialErr
		}
		if last != nil {
			return nil, last
		}
		return nil, errors.New("webhook destination has no permitted address")
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("webhook redirects are not permitted") }}
}

func prohibited(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
