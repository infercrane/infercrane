// Package engineingress reads which sources may reach an engine's port on
// Kubernetes: the control plane's gateway and router, named by the operator,
// and nothing else.
package engineingress

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// MaxPeers bounds the configured sources.
const MaxPeers = 16

// Peer is one source allowed to reach an engine pod: an address range, or
// pods selected by labels, optionally in namespaces selected by labels.
// Pods without a namespace selector are in the engine's own namespace.
type Peer struct {
	CIDR            string            `json:"cidr,omitempty"`
	NamespaceLabels map[string]string `json:"namespace_labels,omitempty"`
	PodLabels       map[string]string `json:"pod_labels,omitempty"`
}

var (
	labelKey   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelValue = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
)

// Parse reads the setting: "none" turns the restriction off; otherwise a
// comma-separated list of sources, each an address range ("10.8.0.0/16"),
// "pods:key=value[+key=value]", "namespace:key=value[+key=value]", or
// "namespace:…;pods:…". An empty setting is not a choice and is refused.
func Parse(value string) ([]Peer, bool, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "":
		return nil, false, errors.New("no engine ingress sources are set")
	case "none":
		return nil, true, nil
	}
	entries := strings.Split(value, ",")
	if len(entries) > MaxPeers {
		return nil, false, fmt.Errorf("at most %d engine ingress sources are allowed", MaxPeers)
	}
	peers := make([]Peer, 0, len(entries))
	for _, entry := range entries {
		peer, err := parsePeer(strings.TrimSpace(entry))
		if err != nil {
			return nil, false, err
		}
		peers = append(peers, peer)
	}
	return peers, false, nil
}

func parsePeer(entry string) (Peer, error) {
	if prefix, err := netip.ParsePrefix(entry); err == nil {
		return Peer{CIDR: prefix.Masked().String()}, nil
	}
	var peer Peer
	for _, part := range strings.Split(entry, ";") {
		name, selector, found := strings.Cut(strings.TrimSpace(part), ":")
		if !found {
			return Peer{}, fmt.Errorf("engine ingress source %q is not an address range, pods:…, or namespace:…", bounded(entry))
		}
		labels, err := parseLabels(selector)
		if err != nil {
			return Peer{}, fmt.Errorf("engine ingress source %q: %w", bounded(entry), err)
		}
		switch {
		case name == "pods" && peer.PodLabels == nil:
			peer.PodLabels = labels
		case name == "namespace" && peer.NamespaceLabels == nil:
			peer.NamespaceLabels = labels
		default:
			return Peer{}, fmt.Errorf("engine ingress source %q has an unknown or repeated selector %q", bounded(entry), bounded(name))
		}
	}
	return peer, nil
}

func parseLabels(selector string) (map[string]string, error) {
	labels := map[string]string{}
	for _, pair := range strings.Split(selector, "+") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || !labelKey.MatchString(key) || !labelValue.MatchString(value) {
			return nil, fmt.Errorf("label %q must be key=value with Kubernetes label syntax", bounded(pair))
		}
		labels[key] = value
	}
	return labels, nil
}

func bounded(value string) string {
	if len(value) > 80 {
		return value[:80] + "…"
	}
	return value
}
