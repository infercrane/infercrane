package artifactcache

import "testing"

func TestRequestIsProviderNeutralAndFailClosed(t *testing.T) {
	if (Request{ArtifactID: "a", ModelIdentity: "m@c", Provider: "aws", Location: "s3://cache"}.Validate()) != nil {
		t.Fatal("valid request rejected")
	}
	if (Request{Provider: "aws"}.Validate()) == nil {
		t.Fatal("partial request accepted")
	}
}
