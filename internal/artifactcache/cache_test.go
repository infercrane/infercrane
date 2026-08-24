package artifactcache

import (
	"errors"
	"strings"
	"testing"
)

func TestRequestIsProviderNeutralAndFailClosed(t *testing.T) {
	if (Request{ArtifactID: "a", ModelIdentity: "m@c", Provider: "aws", Location: "s3://cache", IdempotencyKey: "prefetch-a"}.Validate()) != nil {
		t.Fatal("valid request rejected")
	}
	if (Request{Provider: "aws"}.Validate()) == nil {
		t.Fatal("partial request accepted")
	}
}

func TestFailureClassificationDefaultsToAdoptionAndAllowsDefiniteRejection(t *testing.T) {
	if code, unknown := Classify(errors.New("response lost")); code != "provider_result_unknown" || !unknown {
		t.Fatalf("untyped provider failure was not safely replayable: code=%q unknown=%t", code, unknown)
	}
	err := Definitive("cache_not_configured", errors.New("mapping absent"))
	if code, unknown := Classify(err); code != "cache_not_configured" || unknown || !strings.Contains(err.Error(), "mapping absent") {
		t.Fatalf("definite rejection was not preserved: code=%q unknown=%t err=%v", code, unknown, err)
	}
	if (Request{ArtifactID: strings.Repeat("a", 1025), ModelIdentity: "m@c", Provider: "aws", Location: "cache", IdempotencyKey: "key"}.Validate()) == nil {
		t.Fatal("unbounded artifact cache identity accepted")
	}
}
