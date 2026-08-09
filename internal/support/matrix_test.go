package support

import (
	"strings"
	"testing"
)

func TestV01QualificationIsExplicit(t *testing.T) {
	matrix := V01()
	if err := matrix.Validate("vllm", "runpod", "elastic"); err != nil {
		t.Fatal(err)
	}
	if err := matrix.Validate("vllm", "runpod", "serverless"); err != nil {
		t.Fatal(err)
	}
	if err := matrix.Validate("vllm", "another-cloud", "elastic"); err == nil || !strings.Contains(err.Error(), "not qualified") {
		t.Fatalf("expected qualification error, got %v", err)
	}
}

func TestMatrixCanQualifyNewAdaptersWithoutChangingValidationCode(t *testing.T) {
	matrix := New([]string{"vllm", "future-runtime"}, map[string][]string{
		"runpod":       {"elastic", "serverless"},
		"future-cloud": {"elastic"},
	})
	if err := matrix.Validate("future-runtime", "future-cloud", "elastic"); err != nil {
		t.Fatal(err)
	}
	if err := matrix.Validate("future-runtime", "future-cloud", "serverless"); err == nil {
		t.Fatal("unqualified provider mode was accepted")
	}
}
