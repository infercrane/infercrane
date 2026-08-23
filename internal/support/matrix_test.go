package support

import (
	"slices"
	"strings"
	"testing"
)

func TestSGLangWorkloadPinsResolvedModelRevision(t *testing.T) {
	command := SGLangWorkload().Command
	for _, pair := range [][2]string{{"--model-path", "${MODEL}"}, {"--revision", "${MODEL_REVISION}"}} {
		index := slices.Index(command, pair[0])
		if index < 0 || index+1 >= len(command) || command[index+1] != pair[1] {
			t.Fatalf("SGLang command does not preserve %s identity: %v", pair[0], command)
		}
	}
}

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

func TestV03QualifiesOnlyNarrowAWSElasticPath(t *testing.T) {
	matrix := V03()
	if err := matrix.Validate("vllm", "aws", "elastic"); err != nil {
		t.Fatal(err)
	}
	if err := matrix.Validate("vllm", "aws", "serverless"); err == nil {
		t.Fatal("AWS serverless must remain outside the v0.3 qualification policy")
	}
}

func TestV06QualifiesExactPortableRuntimeCombinations(t *testing.T) {
	matrix := V06()
	for _, combination := range [][3]string{{"sglang", "aws", "elastic"}, {"custom-oci", "aws", "elastic"}, {"vllm", "aws", "elastic"}} {
		if err := matrix.Validate(combination[0], combination[1], combination[2]); err != nil {
			t.Fatalf("%v: %v", combination, err)
		}
	}
	for _, combination := range [][3]string{{"sglang", "runpod", "elastic"}, {"sglang", "runpod", "serverless"}, {"custom-oci", "runpod", "serverless"}} {
		if err := matrix.Validate(combination[0], combination[1], combination[2]); err == nil {
			t.Fatalf("unqualified combination accepted: %v", combination)
		}
	}
}

func TestV09QualifiesOnlyKubernetesElasticRuntimeCombinations(t *testing.T) {
	matrix := V09()
	for _, runtime := range []string{"vllm", "sglang", "custom-oci"} {
		if err := matrix.Validate(runtime, "kubernetes", ElasticMode); err != nil {
			t.Fatalf("%s Kubernetes elastic: %v", runtime, err)
		}
	}
	if err := matrix.Validate("vllm", "kubernetes", ServerlessMode); err == nil {
		t.Fatal("Kubernetes serverless must remain unqualified")
	}
}
