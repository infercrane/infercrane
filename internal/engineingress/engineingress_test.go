package engineingress

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEngineIngressSources(t *testing.T) {
	peers, disabled, err := Parse(" pods:app.kubernetes.io/name=infercrane , 10.8.0.1/16, namespace:kubernetes.io/metadata.name=gateway;pods:app=router+tier=edge ")
	if err != nil || disabled {
		t.Fatalf("peers=%v disabled=%v err=%v", peers, disabled, err)
	}
	want := []Peer{
		{PodLabels: map[string]string{"app.kubernetes.io/name": "infercrane"}},
		{CIDR: "10.8.0.0/16"},
		{NamespaceLabels: map[string]string{"kubernetes.io/metadata.name": "gateway"}, PodLabels: map[string]string{"app": "router", "tier": "edge"}},
	}
	if !reflect.DeepEqual(peers, want) {
		t.Fatalf("peers=%#v", peers)
	}
	if peers, disabled, err := Parse("none"); err != nil || !disabled || peers != nil {
		t.Fatalf("none: %v %v %v", peers, disabled, err)
	}
	for value, want := range map[string]string{
		"":                   "no engine ingress sources",
		"gateway":            "not an address range",
		"pods:app":           "key=value",
		"pods:app=bad value": "key=value",
		"pods:a=b;pods:c=d":  "repeated selector",
		"services:a=b":       "unknown or repeated selector",
		strings.Repeat("10.0.0.0/8,", MaxPeers) + "10.0.0.0/8": "at most 16",
	} {
		if _, _, err := Parse(value); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err=%v, want %q", value, err, want)
		}
	}
}
