package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeAWSRunner struct {
	instanceID         string
	externalKey        string
	state              string
	failAfterCreate    bool
	createCalls        int
	deleteCalls        int
	apiEnvironments    [][]string
	runInstanceArgs    []string
	roleFailurePayload string
}

func (f *fakeAWSRunner) Run(_ context.Context, environment []string, args ...string) ([]byte, error) {
	if len(args) >= 2 && args[0] == "sts" && args[1] == "assume-role" {
		if f.roleFailurePayload != "" {
			return []byte(f.roleFailurePayload), errors.New("assume failed")
		}
		return []byte(`{"Credentials":{"AccessKeyId":"temporary-access","SecretAccessKey":"temporary-secret","SessionToken":"temporary-token"}}`), nil
	}
	f.apiEnvironments = append(f.apiEnvironments, append([]string(nil), environment...))
	if len(args) < 2 || args[0] != "ec2" {
		return nil, errors.New("unexpected AWS command")
	}
	switch args[1] {
	case "describe-instances":
		if f.instanceID == "" {
			return []byte(`{"Reservations":[]}`), nil
		}
		response := map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{"InstanceId": f.instanceID, "PrivateIpAddress": "10.0.1.12", "State": map[string]string{"Name": f.state}, "Tags": []map[string]string{{"Key": "infercrane:external-key", "Value": f.externalKey}}}}}}}
		return json.Marshal(response)
	case "run-instances":
		f.createCalls++
		f.runInstanceArgs = append([]string(nil), args...)
		f.instanceID, f.state = "i-fixture", "running"
		f.externalKey = awsArgumentTagExternalKey(args)
		if f.failAfterCreate {
			f.failAfterCreate = false
			return []byte("transport lost after create"), errors.New("transport lost")
		}
		return []byte(`{"Instances":[{"InstanceId":"i-fixture"}]}`), nil
	case "terminate-instances":
		f.deleteCalls++
		f.state = "terminated"
		return []byte(`{"TerminatingInstances":[{"InstanceId":"i-fixture"}]}`), nil
	default:
		return nil, fmt.Errorf("unexpected EC2 command %q", args[1])
	}
}

func awsArgumentTagExternalKey(args []string) string {
	for i, arg := range args {
		if arg != "--tag-specifications" || i+1 >= len(args) {
			continue
		}
		var specifications []struct {
			Tags []struct {
				Key, Value string
			}
		}
		_ = json.Unmarshal([]byte(args[i+1]), &specifications)
		for _, specification := range specifications {
			for _, tag := range specification.Tags {
				if tag.Key == "infercrane:external-key" {
					return tag.Value
				}
			}
		}
	}
	return ""
}

func testAWSEC2(runner CommandRunner) AWSEC2 {
	return AWSEC2{
		Runner: runner, RoleARN: "arn:aws:iam::123456789012:role/infercrane", Region: "eu-central-1",
		SubnetID: "subnet-private", SecurityGroupIDs: []string{"sg-inference"}, AMIID: "ami-gpu",
		InstanceType: "g6e.xlarge", GPU: "L40S", InstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/infercrane-worker",
		WorkerSecretARN: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:worker-key",
		ImageDigest:     "vllm/vllm-openai@sha256:c48cf118e1e6e39d7790e174d6014f7af5d06f79c2d29d984d11cbe2e8d414e7",
	}
}

func awsReplicaSpec() ReplicaSpec {
	return ReplicaSpec{ExternalKey: "deployment-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "aws", GPU: "L40S", Region: "eu-central-1", Port: 8000}
}

func TestAWSEC2LifecycleIsIdempotentPrivateAndTagged(t *testing.T) {
	runner := &fakeAWSRunner{}
	provider := testAWSEC2(runner)
	first, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err != nil || first.ResourceID != second.ResourceID || runner.createCalls != 1 {
		t.Fatalf("first=%#v second=%#v creates=%d err=%v", first, second, runner.createCalls, err)
	}
	observation, err := provider.ObserveReplica(context.Background(), second, 8000)
	if err != nil || !observation.Exists || observation.State != "ready" || observation.Endpoint != "http://10.0.1.12:8000" || !strings.Contains(observation.Details, `"cost_state":"unknown"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	joined := strings.Join(runner.runInstanceArgs, " ")
	if !strings.Contains(joined, `"AssociatePublicIpAddress":false`) || !strings.Contains(joined, "infercrane:external-key") || !strings.Contains(joined, "--client-token") || strings.Contains(joined, "temporary-secret") {
		t.Fatalf("unsafe or non-idempotent run-instances args: %s", joined)
	}
	if err := provider.DeleteReplica(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteReplica(context.Background(), second); err != nil || runner.deleteCalls != 1 {
		t.Fatalf("delete calls=%d err=%v", runner.deleteCalls, err)
	}
}

func TestAWSEC2AdoptsAfterLostCreateResponse(t *testing.T) {
	runner := &fakeAWSRunner{failAfterCreate: true}
	provider := testAWSEC2(runner)
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || strings.Contains(err.Error(), "temporary-secret") {
		t.Fatalf("lost response was not safely surfaced: %v", err)
	}
	handle, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err != nil || handle.ResourceID != "i-fixture" || runner.createCalls != 1 {
		t.Fatalf("handle=%#v creates=%d err=%v", handle, runner.createCalls, err)
	}
}

func TestAWSEC2RejectsMutableImageBeforeAWSCall(t *testing.T) {
	runner := &fakeAWSRunner{}
	provider := testAWSEC2(runner)
	provider.ImageDigest = "vllm/vllm-openai:latest"
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || runner.createCalls != 0 {
		t.Fatalf("mutable image accepted: creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2RedactsRoleFailureOutput(t *testing.T) {
	const secret = "role-error-contained-secret"
	provider := testAWSEC2(&fakeAWSRunner{roleFailurePayload: secret})
	_, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("role failure leaked credential material: %v", err)
	}
}
