package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtime"
	"github.com/infercrane/infercrane/internal/testtools/providerfixture"
)

func elasticProfile() integration.ProviderProfile {
	return integration.ProviderProfile{Adapter: "fixture-elastic", Cloud: "fixture", ContractVersion: integration.ProviderContractV1, AdapterVersion: "test", Modes: []integration.ComputeMode{integration.ElasticMode}, Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic"}}}
}

func serverlessProfile() integration.ProviderProfile {
	return integration.ProviderProfile{Adapter: "fixture-serverless", Cloud: "fixture", ContractVersion: integration.ProviderContractV1, AdapterVersion: "test", Modes: []integration.ComputeMode{integration.ServerlessMode}, Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic"}}}
}

func TestElasticLifecycleConformance(t *testing.T) {
	provider := providerfixture.NewElastic()
	report := ElasticLifecycle(context.Background(), elasticProfile(), provider, provision.ReplicaSpec{ExternalKey: "deployment-r0", Model: "model", Cloud: "fixture", GPU: "fixture"}, 8000)
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if provider.CreatedResourceCount != 1 || provider.DeleteCalls != 1 {
		t.Fatalf("unexpected mutations: created=%d deleted=%d", provider.CreatedResourceCount, provider.DeleteCalls)
	}
}

func TestLostEnsureResponseConformanceAdoptsOneResource(t *testing.T) {
	provider := providerfixture.NewElastic()
	provider.FailAfterCreateOnce = true
	report := LostEnsureResponse(context.Background(), elasticProfile(), provider, provision.ReplicaSpec{ExternalKey: "deployment-r0", Model: "model", Cloud: "fixture", GPU: "fixture"}, 8000)
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if provider.CreatedResourceCount != 1 || provider.EnsureCalls != 2 {
		t.Fatalf("lost response duplicated resource: created=%d ensures=%d", provider.CreatedResourceCount, provider.EnsureCalls)
	}
}

func TestElasticDeleteRecoveryConformance(t *testing.T) {
	provider := providerfixture.NewElastic()
	provider.FailDeleteOnce = true
	report := ElasticDeleteRecovery(context.Background(), elasticProfile(), provider, provision.ReplicaSpec{ExternalKey: "deployment-r0", Model: "model", Cloud: "fixture", GPU: "fixture"}, 8000)
	if err := report.Err(); err != nil || provider.CreatedResourceCount != 1 || provider.DeleteCalls != 1 {
		t.Fatalf("report=%+v created=%d deleted=%d err=%v", report, provider.CreatedResourceCount, provider.DeleteCalls, err)
	}
}

func TestElasticTimeoutConformance(t *testing.T) {
	provider := providerfixture.NewElastic()
	provider.BlockEnsureUntilDone = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	report := ElasticTimeout(ctx, elasticProfile(), provider, provision.ReplicaSpec{ExternalKey: "deployment-r0"})
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestElasticFailureRedactionConformance(t *testing.T) {
	const secret = "credential-must-not-escape"
	provider := providerfixture.NewElastic()
	provider.EnsureFailure = errors.New("provider rejected request")
	if report := ElasticFailureRedaction(context.Background(), elasticProfile(), provider, provision.ReplicaSpec{ExternalKey: "deployment-r0"}, secret); report.Err() != nil {
		t.Fatalf("safe failure rejected: %+v", report)
	}
	provider.EnsureFailure = errors.New("provider rejected " + secret)
	report := ElasticFailureRedaction(context.Background(), elasticProfile(), provider, provision.ReplicaSpec{ExternalKey: "deployment-r0"}, secret)
	if report.Err() == nil || strings.Contains(fmt.Sprintf("%+v", report), secret) {
		t.Fatalf("leaking failure was not detected safely: %+v", report)
	}
}

func TestServerlessLifecycleConformance(t *testing.T) {
	provider := providerfixture.NewServerless()
	spec := provision.ServerlessEndpointSpec{ExternalKey: "deployment-revision", Model: "model", ModelRevision: "immutable", GPU: "fixture", WorkersMax: 4}
	report := ServerlessLifecycle(context.Background(), serverlessProfile(), provider, spec)
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if provider.CreatedEndpointCount != 1 || provider.DeleteCalls != 1 {
		t.Fatalf("unexpected mutations: created=%d deleted=%d", provider.CreatedEndpointCount, provider.DeleteCalls)
	}
}

func TestLostServerlessResponseConformanceAdoptsOneEndpoint(t *testing.T) {
	provider := providerfixture.NewServerless()
	provider.FailAfterCreateOnce = true
	spec := provision.ServerlessEndpointSpec{ExternalKey: "deployment-revision", Model: "model", ModelRevision: "immutable", GPU: "fixture", WorkersMax: 4}
	report := LostServerlessEnsureResponse(context.Background(), serverlessProfile(), provider, spec)
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if provider.CreatedEndpointCount != 1 || provider.EnsureCalls != 2 {
		t.Fatalf("lost response duplicated endpoint: created=%d ensures=%d", provider.CreatedEndpointCount, provider.EnsureCalls)
	}
}

func TestRuntimeReadinessConformance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3-8B"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	profile := integration.RuntimeProfile{Runtime: "vllm-fixture", ContractVersion: integration.RuntimeContractV1, AdapterVersion: "test", Protocol: "openai", Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic"}}}
	report := RuntimeReadiness(context.Background(), profile, runtime.VLLM{Client: server.Client()}, server.URL, "Qwen/Qwen3-8B")
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestRuntimeCapabilityConformance(t *testing.T) {
	registry, err := integration.V02Catalog()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Runtime("vllm")
	if err != nil {
		t.Fatal(err)
	}
	report := RuntimeCapabilities(profile, "readiness", "buffered_chat", "streaming_chat", "cancellation", "graceful_drain", "telemetry")
	if err := report.Err(); err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestAWSEC2ProviderContractConformance(t *testing.T) {
	runner := &providerfixture.AWSCLI{}
	provider := provision.AWSEC2{Runner: runner, RoleARN: "arn:aws:iam::123456789012:role/infercrane", Region: "eu-central-1", SubnetID: "subnet-private", SecurityGroupIDs: []string{"sg-worker"}, AMIID: "ami-gpu", InstanceType: "g6e.xlarge", GPU: "L40S", InstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/worker", WorkerSecretARN: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:worker", ImageDigest: "vllm/vllm-openai@sha256:c48cf118e1e6e39d7790e174d6014f7af5d06f79c2d29d984d11cbe2e8d414e7"}
	profile := integration.ProviderProfile{Adapter: "aws-ec2", Cloud: "aws", ContractVersion: integration.ProviderContractV1, AdapterVersion: "test", Modes: []integration.ComputeMode{integration.ElasticMode}, Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic-aws-cli"}}}
	spec := provision.ReplicaSpec{ExternalKey: "deployment-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "aws", GPU: "L40S", Region: "eu-central-1", Port: 8000}
	report := ElasticLifecycle(context.Background(), profile, provider, spec, 8000)
	if err := report.Err(); err != nil || runner.CreateCalls != 1 || runner.DeleteCalls != 1 {
		t.Fatalf("report=%+v creates=%d deletes=%d err=%v", report, runner.CreateCalls, runner.DeleteCalls, err)
	}
}

func TestAWSEC2LostCreateResponseConformance(t *testing.T) {
	runner := &providerfixture.AWSCLI{FailAfterCreateOnce: true}
	provider := provision.AWSEC2{Runner: runner, RoleARN: "arn:aws:iam::123456789012:role/infercrane", Region: "eu-central-1", SubnetID: "subnet-private", SecurityGroupIDs: []string{"sg-worker"}, AMIID: "ami-gpu", InstanceType: "g6e.xlarge", GPU: "L40S", InstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/worker", WorkerSecretARN: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:worker", ImageDigest: "vllm/vllm-openai@sha256:c48cf118e1e6e39d7790e174d6014f7af5d06f79c2d29d984d11cbe2e8d414e7"}
	profile := integration.ProviderProfile{Adapter: "aws-ec2", Cloud: "aws", ContractVersion: integration.ProviderContractV1, AdapterVersion: "test", Modes: []integration.ComputeMode{integration.ElasticMode}, Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic-aws-cli"}}}
	spec := provision.ReplicaSpec{ExternalKey: "deployment-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "aws", GPU: "L40S", Region: "eu-central-1", Port: 8000}
	report := LostEnsureResponse(context.Background(), profile, provider, spec, 8000)
	if err := report.Err(); err != nil || runner.CreateCalls != 1 {
		t.Fatalf("report=%+v creates=%d err=%v", report, runner.CreateCalls, err)
	}
}
