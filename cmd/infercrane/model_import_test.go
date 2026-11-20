package main

import (
	"strings"
	"testing"
)

func TestParseAdapterFlag(t *testing.T) {
	commit := strings.Repeat("a", 40)
	adapter, err := parseAdapterFlag("support-bot=acme/support-lora@" + commit)
	if err != nil || adapter.Name != "support-bot" || adapter.Repository != "acme/support-lora" || adapter.Revision != commit || adapter.Source() != "huggingface" {
		t.Fatalf("adapter=%#v err=%v", adapter, err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	adapter, err = parseAdapterFlag("bot=s3://acme-adapters/support@" + digest)
	if err != nil || adapter.Repository != "s3://acme-adapters/support" || adapter.Revision != digest || adapter.Source() != "s3" {
		t.Fatalf("bucket adapter=%#v err=%v", adapter, err)
	}
	for _, invalid := range []string{"acme/support-lora@" + commit, "bot=acme/support-lora", "bot=acme/support-lora@", "bot=@" + commit} {
		if _, err = parseAdapterFlag(invalid); err == nil {
			t.Fatalf("%q accepted", invalid)
		}
	}
}
