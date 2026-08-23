package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
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
	instanceType       string
	rootVolumeGiB      int
	rootEncrypted      bool
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
		instanceType := f.instanceType
		if instanceType == "" {
			instanceType = "g6e.xlarge"
		}
		rootVolumeGiB, rootEncrypted := f.rootVolumeGiB, f.rootEncrypted
		if rootVolumeGiB == 0 {
			rootVolumeGiB, rootEncrypted = 100, true
		}
		response := map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{"InstanceId": f.instanceID, "ImageId": "ami-gpu", "InstanceType": instanceType, "SubnetId": "subnet-private", "PrivateIpAddress": "10.0.1.12", "IamInstanceProfile": map[string]string{"Arn": "arn:aws:iam::123456789012:instance-profile/infercrane-worker"}, "SecurityGroups": []map[string]string{{"GroupId": "sg-inference"}}, "State": map[string]string{"Name": f.state}, "Tags": []map[string]string{{"Key": "infercrane:external-key", "Value": f.externalKey}, {"Key": "infercrane:root-volume-gib", "Value": fmt.Sprint(rootVolumeGiB)}, {"Key": "infercrane:root-volume-encrypted", "Value": fmt.Sprint(rootEncrypted)}}}}}}}
		return json.Marshal(response)
	case "run-instances":
		f.createCalls++
		f.runInstanceArgs = append([]string(nil), args...)
		f.instanceID, f.state = "i-fixture", "running"
		f.externalKey = awsArgumentTagExternalKey(args)
		f.rootVolumeGiB, f.rootEncrypted = awsArgumentRootVolume(args)
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

func awsArgumentTagResourceTypes(args []string) []string {
	for i, arg := range args {
		if arg != "--tag-specifications" || i+1 >= len(args) {
			continue
		}
		var specifications []struct {
			ResourceType string
		}
		if json.Unmarshal([]byte(args[i+1]), &specifications) != nil {
			return nil
		}
		result := make([]string, 0, len(specifications))
		for _, specification := range specifications {
			result = append(result, specification.ResourceType)
		}
		return result
	}
	return nil
}

func awsArgumentRootVolume(args []string) (int, bool) {
	for i, arg := range args {
		if arg != "--block-device-mappings" || i+1 >= len(args) {
			continue
		}
		var mappings []struct {
			DeviceName string
			Ebs        struct {
				VolumeSize int
				Encrypted  bool
			}
		}
		if json.Unmarshal([]byte(args[i+1]), &mappings) == nil && len(mappings) == 1 && mappings[0].DeviceName == "/dev/xvda" {
			return mappings[0].Ebs.VolumeSize, mappings[0].Ebs.Encrypted
		}
	}
	return 0, false
}

func testAWSEC2(runner CommandRunner) AWSEC2 {
	return AWSEC2{
		Runner: runner, RoleARN: "arn:aws:iam::123456789012:role/infercrane", Region: "eu-central-1",
		SubnetID: "subnet-private", SecurityGroupIDs: []string{"sg-inference"}, AMIID: "ami-gpu",
		InstanceType: "g6e.xlarge", GPU: "L40S", InstanceProfileARN: "arn:aws:iam::123456789012:instance-profile/infercrane-worker",
		WorkerSecretARN: "arn:aws:secretsmanager:eu-central-1:123456789012:secret:worker-key",
		ImageDigest:     "vllm/vllm-openai@sha256:0fec7ec5f3e6bc168e54899935fb0557da908a4832a1dbc88e2debcf2f889416",
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
	if !strings.Contains(joined, `"AssociatePublicIpAddress":false`) || !strings.Contains(joined, "infercrane:external-key") || !strings.Contains(joined, "--client-token") || !strings.Contains(joined, "--count 1") || !strings.Contains(joined, `"VolumeSize":100`) || !strings.Contains(joined, `"VolumeType":"gp3"`) || !strings.Contains(joined, `"Encrypted":true`) || !strings.Contains(joined, `"DeleteOnTermination":true`) || strings.Contains(joined, "--min-count") || strings.Contains(joined, "--max-count") || strings.Contains(joined, "temporary-secret") {
		t.Fatalf("unsafe or non-idempotent run-instances args: %s", joined)
	}
	resourceTypes := awsArgumentTagResourceTypes(runner.runInstanceArgs)
	if len(resourceTypes) != 2 || resourceTypes[0] != "instance" || resourceTypes[1] != "volume" {
		t.Fatalf("instance and billable root volume must share ownership tags: %#v", resourceTypes)
	}
	userData := awsUserData(runner.runInstanceArgs)
	if !strings.Contains(userData, `-e VLLM_API_KEY="$worker_key"`) || !strings.Contains(userData, "'Qwen/Qwen3-8B' '--port' '8000'") || strings.Contains(userData, "--api-key") || strings.Contains(userData, "--model") {
		t.Fatalf("vLLM credential must be injected through a non-argv environment variable:\n%s", userData)
	}
	if !strings.Contains(userData, "infercrane_stage identity_start") || !strings.Contains(userData, "infercrane_stage identity_ready") || !strings.Contains(userData, "docker image inspect '") || !strings.Contains(userData, "infercrane_stage image_cache_hit") || !strings.Contains(userData, "infercrane_stage image_pull_complete") || !strings.Contains(userData, "infercrane_stage runtime_start") {
		t.Fatalf("AWS bootstrap does not reuse prewarmed images or expose startup stages:\n%s", userData)
	}
	command := exec.Command("sh", "-n")
	command.Stdin = strings.NewReader(userData)
	if output, syntaxErr := command.CombinedOutput(); syntaxErr != nil {
		t.Fatalf("AWS user data is not valid POSIX shell: %v: %s\n%s", syntaxErr, output, userData)
	}
	if err := provider.DeleteReplica(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteReplica(context.Background(), second); err != nil || runner.deleteCalls != 1 {
		t.Fatalf("delete calls=%d err=%v", runner.deleteCalls, err)
	}
}

func TestAWSEC2PortableWorkloadUsesDigestArgvAndSecretEnvironment(t *testing.T) {
	runner := &fakeAWSRunner{}
	spec := awsReplicaSpec()
	spec.Workload = runtimecontract.Workload{Image: "registry.example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"serve", "--model", "${MODEL}", "--label", "two words", "--port", "${PORT}"}, Protocol: "openai", Port: 9000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
	if _, err := testAWSEC2(runner).EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	userData := ""
	for i, arg := range runner.runInstanceArgs {
		if arg == "--user-data" && i+1 < len(runner.runInstanceArgs) {
			userData = runner.runInstanceArgs[i+1]
		}
	}
	for _, want := range []string{"docker image inspect '" + spec.Workload.Image + "'", "docker pull '" + spec.Workload.Image + "'", "infercrane_stage image_cache_hit", `-e INFERCRANE_WORKER_API_KEY="$worker_key"`, "'serve' '--model' 'Qwen/Qwen3-8B' '--label' 'two words' '--port' '9000'", `docker logs --follow "$container_id" >/dev/console 2>&1 &`} {
		if !strings.Contains(userData, want) {
			t.Fatalf("user data missing %q:\n%s", want, userData)
		}
	}
	if strings.Contains(userData, "temporary-secret") {
		t.Fatal("temporary AWS credentials leaked into user data")
	}
}

func awsUserData(args []string) string {
	for i, arg := range args {
		if arg == "--user-data" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
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

func TestAWSEC2RefusesMismatchedInstanceDuringAdoption(t *testing.T) {
	runner := &fakeAWSRunner{instanceID: "i-stale", externalKey: awsReplicaSpec().ExternalKey, state: "running", instanceType: "p5.48xlarge"}
	_, err := testAWSEC2(runner).EnsureReplica(context.Background(), awsReplicaSpec())
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched instance was adopted: %v", err)
	}
	if runner.createCalls != 0 {
		t.Fatalf("provider created replacement while conflicting resource exists: %d", runner.createCalls)
	}
}

func TestAWSEC2RefusesUnencryptedOrUndersizedRootVolumeDuringAdoption(t *testing.T) {
	runner := &fakeAWSRunner{instanceID: "i-stale", externalKey: awsReplicaSpec().ExternalKey, state: "running", rootVolumeGiB: 30, rootEncrypted: false}
	_, err := testAWSEC2(runner).EnsureReplica(context.Background(), awsReplicaSpec())
	if err == nil || !strings.Contains(err.Error(), "encrypted root volume") || runner.createCalls != 0 {
		t.Fatalf("unsafe root volume was adopted: creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2RejectsUnsafeRootVolumeBeforeAWSCall(t *testing.T) {
	runner := &fakeAWSRunner{}
	provider := testAWSEC2(runner)
	provider.RootVolumeGiB = 30
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || runner.createCalls != 0 {
		t.Fatalf("unsafe root volume accepted: creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2ClientTokenIsStableBoundedAndZonallyScoped(t *testing.T) {
	provider := testAWSEC2(&fakeAWSRunner{})
	token := provider.clientToken("deployment-r0")
	if len(token) != 64 || token != provider.clientToken("deployment-r0") {
		t.Fatalf("client token must be a stable 64-character digest: %q", token)
	}
	changedKey := provider.clientToken("deployment-r1")
	changedRegionProvider := provider
	changedRegionProvider.Region = "us-east-1"
	changedSubnetProvider := provider
	changedSubnetProvider.SubnetID = "subnet-other-az"
	if token == changedKey || token == changedRegionProvider.clientToken("deployment-r0") || token == changedSubnetProvider.clientToken("deployment-r0") {
		t.Fatal("client token did not change across resource, region, or subnet scope")
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
