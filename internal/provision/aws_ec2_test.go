package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type fakeAWSRunner struct {
	instanceID             string
	externalKey            string
	state                  string
	failAfterCreate        bool
	createCalls            int
	deleteCalls            int
	apiEnvironments        [][]string
	runInstanceArgs        []string
	runInstanceHistory     [][]string
	roleFailurePayload     string
	createFailurePayload   string
	createFailureBySubnet  map[string]string
	instanceType           string
	instanceSubnet         string
	rootVolumeGiB          int
	rootEncrypted          bool
	rootDeviceName         string
	tagRootVolumeGiB       int
	tagRootEncrypted       *bool
	tagRootDeviceName      string
	amiRootDeviceName      string
	amiRootVolumeGiB       int
	amiOccupiedDevices     []string
	consoleOutput          string
	snapshotID             string
	snapshotState          string
	snapshotEncrypted      bool
	snapshotVolumeGiB      int
	snapshotIdentityDigest string
	artifactSnapshotID     string
	artifactIdentityDigest string
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
	case "describe-images":
		rootDevice := f.amiRootDeviceName
		if rootDevice == "" {
			rootDevice = "/dev/xvda"
		}
		rootGiB := f.amiRootVolumeGiB
		if rootGiB == 0 {
			rootGiB = 30
		}
		mappings := []map[string]any{{"DeviceName": rootDevice, "Ebs": map[string]any{"VolumeSize": rootGiB}}}
		for _, device := range f.amiOccupiedDevices {
			if device != rootDevice {
				mappings = append(mappings, map[string]any{"DeviceName": device, "Ebs": map[string]any{"VolumeSize": 10}})
			}
		}
		return json.Marshal(map[string]any{"Images": []any{map[string]any{"ImageId": "ami-gpu", "RootDeviceName": rootDevice, "BlockDeviceMappings": mappings}}})
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
			rootVolumeGiB, rootEncrypted = 200, true
		}
		rootDevice := f.rootDeviceName
		if rootDevice == "" {
			rootDevice = f.amiRootDeviceName
		}
		if rootDevice == "" {
			rootDevice = "/dev/xvda"
		}
		instanceSubnet := f.instanceSubnet
		if instanceSubnet == "" {
			instanceSubnet = "subnet-private"
		}
		tagDevice := f.tagRootDeviceName
		if tagDevice == "" {
			tagDevice = rootDevice
		}
		tagGiB := f.tagRootVolumeGiB
		if tagGiB == 0 {
			tagGiB = rootVolumeGiB
		}
		tagEncrypted := rootEncrypted
		if f.tagRootEncrypted != nil {
			tagEncrypted = *f.tagRootEncrypted
		}
		tags := []map[string]string{{"Key": "infercrane:external-key", "Value": f.externalKey}, {"Key": "infercrane:root-device-name", "Value": tagDevice}, {"Key": "infercrane:root-volume-gib", "Value": fmt.Sprint(tagGiB)}, {"Key": "infercrane:root-volume-encrypted", "Value": fmt.Sprint(tagEncrypted)}}
		if f.artifactSnapshotID != "" {
			tags = append(tags, map[string]string{"Key": "infercrane:artifact-snapshot-id", "Value": f.artifactSnapshotID}, map[string]string{"Key": "infercrane:artifact-identity-digest", "Value": f.artifactIdentityDigest})
		}
		response := map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{"InstanceId": f.instanceID, "ImageId": "ami-gpu", "InstanceType": instanceType, "SubnetId": instanceSubnet, "PrivateIpAddress": "10.0.1.12", "RootDeviceName": rootDevice, "BlockDeviceMappings": []map[string]any{{"DeviceName": rootDevice, "Ebs": map[string]string{"VolumeId": "vol-root"}}}, "IamInstanceProfile": map[string]string{"Arn": "arn:aws:iam::123456789012:instance-profile/infercrane-worker"}, "SecurityGroups": []map[string]string{{"GroupId": "sg-inference"}}, "State": map[string]string{"Name": f.state}, "Tags": tags}}}}}
		return json.Marshal(response)
	case "describe-volumes":
		rootVolumeGiB, rootEncrypted := f.rootVolumeGiB, f.rootEncrypted
		if rootVolumeGiB == 0 {
			rootVolumeGiB, rootEncrypted = 200, true
		}
		return json.Marshal(map[string]any{"Volumes": []any{map[string]any{"VolumeId": "vol-root", "Size": rootVolumeGiB, "Encrypted": rootEncrypted}}})
	case "describe-snapshots":
		state := f.snapshotState
		if state == "" {
			state = "completed"
		}
		volumeGiB := f.snapshotVolumeGiB
		if volumeGiB == 0 {
			volumeGiB = 100
		}
		encrypted := f.snapshotEncrypted
		if f.snapshotState == "" && !f.snapshotEncrypted {
			encrypted = true
		}
		return json.Marshal(map[string]any{"Snapshots": []any{map[string]any{"SnapshotId": f.snapshotID, "State": state, "Encrypted": encrypted, "VolumeSize": volumeGiB, "Tags": []map[string]string{{"Key": "infercrane:artifact-cache", "Value": "true"}, {"Key": "infercrane:model-identity-digest", "Value": f.snapshotIdentityDigest}}}}})
	case "run-instances":
		f.createCalls++
		f.runInstanceArgs = append([]string(nil), args...)
		f.runInstanceHistory = append(f.runInstanceHistory, append([]string(nil), args...))
		subnet := awsArgumentSubnet(args)
		if payload := f.createFailureBySubnet[subnet]; payload != "" {
			return []byte(payload), errors.New("AWS CLI failed")
		}
		if f.createFailurePayload != "" {
			return []byte(f.createFailurePayload), errors.New("AWS CLI failed")
		}
		f.instanceID, f.state = "i-fixture", "running"
		f.instanceSubnet = subnet
		f.externalKey = awsArgumentTagExternalKey(args)
		f.rootVolumeGiB, f.rootEncrypted = awsArgumentRootVolume(args)
		f.rootDeviceName = awsArgumentRootDevice(args)
		f.artifactSnapshotID = awsArgumentTag(args, "infercrane:artifact-snapshot-id")
		f.artifactIdentityDigest = awsArgumentTag(args, "infercrane:artifact-identity-digest")
		if f.failAfterCreate {
			f.failAfterCreate = false
			return []byte("transport lost after create"), errors.New("transport lost")
		}
		return []byte(`{"Instances":[{"InstanceId":"i-fixture"}]}`), nil
	case "terminate-instances":
		f.deleteCalls++
		f.state = "terminated"
		return []byte(`{"TerminatingInstances":[{"InstanceId":"i-fixture"}]}`), nil
	case "get-console-output":
		return json.Marshal(map[string]string{"Output": f.consoleOutput})
	default:
		return nil, fmt.Errorf("unexpected EC2 command %q", args[1])
	}
}

func TestAWSEC2NormalizesActionableFailuresWithoutRawProviderOutput(t *testing.T) {
	tests := []struct {
		name, payload, message string
		kind                   error
	}{
		{"authorization", `UnauthorizedOperation: encoded authorization failure message SECRET`, "verify the assumed role", ErrProviderAuthorization},
		{"quota", `VcpuLimitExceeded: account 123456789012 has a limit`, "compute quota is exhausted", ErrProviderQuota},
		{"capacity", `InsufficientInstanceCapacity: g6e.xlarge unavailable`, "configured capacity boundaries", ErrProviderCapacity},
		{"configuration", `InvalidParameterValue: subnet-private is invalid`, "launch configuration", ErrInvalidReplicaSpec},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeAWSRunner{createFailurePayload: test.payload}
			_, err := testAWSEC2(runner).EnsureReplica(context.Background(), awsReplicaSpec())
			if !errors.Is(err, test.kind) || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error=%v kind=%v", err, test.kind)
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "123456789012") || strings.Contains(err.Error(), "subnet-private") {
				t.Fatalf("raw provider output escaped normalization: %v", err)
			}
		})
	}
}

func awsArgumentTagExternalKey(args []string) string {
	return awsArgumentTag(args, "infercrane:external-key")
}

func awsArgumentTag(args []string, key string) string {
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
				if tag.Key == key {
					return tag.Value
				}
			}
		}
	}
	return ""
}

func awsArgumentSubnet(args []string) string {
	for i, arg := range args {
		if arg != "--network-interfaces" || i+1 >= len(args) {
			continue
		}
		var interfaces []struct {
			SubnetID string `json:"SubnetId"`
		}
		if json.Unmarshal([]byte(args[i+1]), &interfaces) == nil && len(interfaces) == 1 {
			return interfaces[0].SubnetID
		}
	}
	return ""
}

func awsArgumentClientToken(args []string) string {
	for i, arg := range args {
		if arg == "--client-token" && i+1 < len(args) {
			return args[i+1]
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
				SnapshotID string `json:"SnapshotId"`
			}
		}
		if json.Unmarshal([]byte(args[i+1]), &mappings) == nil {
			for _, mapping := range mappings {
				if mapping.Ebs.SnapshotID == "" {
					return mapping.Ebs.VolumeSize, mapping.Ebs.Encrypted
				}
			}
		}
	}
	return 0, false
}

func awsArgumentRootDevice(args []string) string {
	for i, arg := range args {
		if arg != "--block-device-mappings" || i+1 >= len(args) {
			continue
		}
		var mappings []struct {
			DeviceName string
			EBS        struct {
				SnapshotID string `json:"SnapshotId"`
			} `json:"Ebs"`
		}
		if json.Unmarshal([]byte(args[i+1]), &mappings) == nil {
			for _, mapping := range mappings {
				if mapping.EBS.SnapshotID == "" {
					return mapping.DeviceName
				}
			}
		}
	}
	return ""
}

func awsArgumentArtifactVolume(args []string) (snapshotID string, encrypted, deleteOnTermination bool, initializationRate int) {
	for i, arg := range args {
		if arg != "--block-device-mappings" || i+1 >= len(args) {
			continue
		}
		var mappings []struct {
			DeviceName string
			Ebs        struct {
				SnapshotID               string `json:"SnapshotId"`
				Encrypted                bool
				DeleteOnTermination      bool
				VolumeInitializationRate int
			}
		}
		if json.Unmarshal([]byte(args[i+1]), &mappings) != nil {
			return "", false, false, 0
		}
		for _, mapping := range mappings {
			if mapping.DeviceName == "/dev/sdf" {
				return mapping.Ebs.SnapshotID, mapping.Ebs.Encrypted, mapping.Ebs.DeleteOnTermination, mapping.Ebs.VolumeInitializationRate
			}
		}
	}
	return "", false, false, 0
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

func configureAWSArtifactSnapshot(provider *AWSEC2, runner *fakeAWSRunner) {
	identity := modelIdentity(awsReplicaSpec())
	runner.snapshotID = "snap-0123456789abcdef0"
	runner.snapshotIdentityDigest = modelIdentityDigest(identity)
	provider.ArtifactCachePolicy = "required"
	provider.ArtifactSnapshots = map[string]string{identity: runner.snapshotID}
	provider.ArtifactVolumeInitializationRate = 200
}

func TestAWSEC2LaunchesWithVerifiedImmutableArtifactSnapshot(t *testing.T) {
	runner := &fakeAWSRunner{}
	provider := testAWSEC2(runner)
	configureAWSArtifactSnapshot(&provider, runner)
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err != nil {
		t.Fatal(err)
	}
	snapshotID, encrypted, deleteOnTermination, rate := awsArgumentArtifactVolume(runner.runInstanceArgs)
	if snapshotID != runner.snapshotID || !encrypted || !deleteOnTermination || rate != 200 {
		t.Fatalf("artifact mapping snapshot=%q encrypted=%v delete=%v rate=%d", snapshotID, encrypted, deleteOnTermination, rate)
	}
	if awsArgumentTag(runner.runInstanceArgs, "infercrane:artifact-snapshot-id") != runner.snapshotID || awsArgumentTag(runner.runInstanceArgs, "infercrane:artifact-identity-digest") != runner.snapshotIdentityDigest {
		t.Fatalf("immutable cache identity tags missing: %v", runner.runInstanceArgs)
	}
	userData := awsUserData(runner.runInstanceArgs)
	for _, expected := range []string{"blkid -L INFERCRANE_ART", "mount -o ro,nosuid,nodev", "infercrane_stage artifact_cache_hit", "HF_HUB_OFFLINE=1", "/root/.cache/huggingface:ro"} {
		if !strings.Contains(userData, expected) {
			t.Fatalf("artifact cache bootstrap missing %q:\n%s", expected, userData)
		}
	}
	command := exec.Command("sh", "-n")
	command.Stdin = strings.NewReader(userData)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("artifact bootstrap is invalid POSIX shell: %v: %s", err, output)
	}
}

func TestAWSEC2ArtifactSnapshotValidationFailsBeforeLaunch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeAWSRunner)
	}{
		{name: "unencrypted", mutate: func(r *fakeAWSRunner) { r.snapshotState = "completed"; r.snapshotEncrypted = false }},
		{name: "incomplete", mutate: func(r *fakeAWSRunner) { r.snapshotState = "pending"; r.snapshotEncrypted = true }},
		{name: "wrong identity", mutate: func(r *fakeAWSRunner) { r.snapshotIdentityDigest = "sha256:" + strings.Repeat("0", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeAWSRunner{}
			provider := testAWSEC2(runner)
			configureAWSArtifactSnapshot(&provider, runner)
			test.mutate(runner)
			if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || runner.createCalls != 0 {
				t.Fatalf("invalid snapshot launched: creates=%d err=%v", runner.createCalls, err)
			}
		})
	}
}

func TestAWSEC2RequiredArtifactSnapshotMustExistAndBeRuntimeCompatible(t *testing.T) {
	runner := &fakeAWSRunner{}
	provider := testAWSEC2(runner)
	provider.ArtifactCachePolicy = "required"
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || runner.createCalls != 0 {
		t.Fatalf("required cache without mapping launched: creates=%d err=%v", runner.createCalls, err)
	}
	spec := awsReplicaSpec()
	spec.Runtime = "custom-oci"
	spec.Workload = runtimecontract.Workload{Image: provider.ImageDigest, Command: []string{"serve"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "qualified only") || runner.createCalls != 0 {
		t.Fatalf("unqualified runtime accepted required cache: creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2ArtifactSnapshotSurvivesLostCreateResponseAdoption(t *testing.T) {
	runner := &fakeAWSRunner{failAfterCreate: true}
	provider := testAWSEC2(runner)
	configureAWSArtifactSnapshot(&provider, runner)
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil {
		t.Fatal("lost response was not surfaced")
	}
	handle, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err != nil || handle.ResourceID != "i-fixture" || runner.createCalls != 1 {
		t.Fatalf("handle=%#v creates=%d err=%v", handle, runner.createCalls, err)
	}
}

func TestAWSEC2ArtifactAdapterAdoptsOnlyConfiguredVerifiedSnapshot(t *testing.T) {
	runner := &fakeAWSRunner{}
	provider := testAWSEC2(runner)
	configureAWSArtifactSnapshot(&provider, runner)
	request := artifactcache.Request{ArtifactID: "artifact-1", ModelIdentity: modelIdentity(awsReplicaSpec()), Provider: "aws", Region: provider.Region, Location: "aws-ebs://" + runner.snapshotID, IdempotencyKey: "release-42"}
	operation, err := provider.Prefetch(context.Background(), request)
	if err != nil || operation.Status != "succeeded" || operation.ProviderOperationID != runner.snapshotID {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	observation, err := provider.Observe(context.Background(), request)
	if err != nil || observation.State != "present" || observation.Source != "aws-ebs-snapshot" || !strings.Contains(observation.EvidenceJSON, runner.snapshotID) || !observation.ExpiresAt.After(observation.ObservedAt) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	request.Location = "aws-ebs://snap-deadbeef"
	if _, err = provider.Prefetch(context.Background(), request); err == nil {
		t.Fatal("unconfigured snapshot was adopted")
	}
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
	if !strings.Contains(joined, `"AssociatePublicIpAddress":false`) || !strings.Contains(joined, "infercrane:external-key") || !strings.Contains(joined, "infercrane:root-device-name") || !strings.Contains(joined, "--client-token") || !strings.Contains(joined, "--count 1") || !strings.Contains(joined, `"VolumeSize":200`) || !strings.Contains(joined, `"VolumeType":"gp3"`) || !strings.Contains(joined, `"Encrypted":true`) || !strings.Contains(joined, `"DeleteOnTermination":true`) || strings.Contains(joined, "--min-count") || strings.Contains(joined, "--max-count") || strings.Contains(joined, "temporary-secret") {
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
	if !strings.Contains(userData, "infercrane_stage identity_start") || !strings.Contains(userData, "infercrane_stage identity_ready") || !strings.Contains(userData, "docker image inspect '") || !strings.Contains(userData, "infercrane_stage image_cache_hit") || !strings.Contains(userData, "infercrane_stage image_pull_complete") || !strings.Contains(userData, "infercrane_stage runtime_start") || !strings.Contains(userData, "infercrane_stage runtime_container_started") {
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

func TestAWSEC2DiscoversAndOverridesTheActualAMIRootDevice(t *testing.T) {
	runner := &fakeAWSRunner{amiRootDeviceName: "/dev/sda1", amiRootVolumeGiB: 75, amiOccupiedDevices: []string{"/dev/sdf"}}
	provider := testAWSEC2(runner)
	configureAWSArtifactSnapshot(&provider, runner)
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err != nil {
		t.Fatal(err)
	}
	if got := awsArgumentRootDevice(runner.runInstanceArgs); got != "/dev/sda1" {
		t.Fatalf("root device=%q want /dev/sda1; args=%v", got, runner.runInstanceArgs)
	}
	if got := awsArgumentTag(runner.runInstanceArgs, "infercrane:root-device-name"); got != "/dev/sda1" {
		t.Fatalf("root-device adoption tag=%q", got)
	}
	artifactDevice := ""
	for i, arg := range runner.runInstanceArgs {
		if arg != "--block-device-mappings" || i+1 >= len(runner.runInstanceArgs) {
			continue
		}
		var mappings []struct {
			DeviceName string
			EBS        struct {
				SnapshotID string `json:"SnapshotId"`
			} `json:"Ebs"`
		}
		_ = json.Unmarshal([]byte(runner.runInstanceArgs[i+1]), &mappings)
		for _, mapping := range mappings {
			if mapping.EBS.SnapshotID != "" {
				artifactDevice = mapping.DeviceName
			}
		}
	}
	if artifactDevice != "/dev/sdg" {
		t.Fatalf("artifact device=%q want first unoccupied /dev/sdg", artifactDevice)
	}
}

func TestAWSEC2RejectsRootVolumeSmallerThanAMISnapshotBeforeLaunch(t *testing.T) {
	runner := &fakeAWSRunner{amiRootDeviceName: "/dev/sda1", amiRootVolumeGiB: 250}
	provider := testAWSEC2(runner)
	provider.RootVolumeGiB = 200
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || !strings.Contains(err.Error(), "smaller than AMI root snapshot") || runner.createCalls != 0 {
		t.Fatalf("undersized root launched: creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2TriesOrderedCapacityBoundariesOnlyForDefinitiveCapacityFailure(t *testing.T) {
	runner := &fakeAWSRunner{createFailureBySubnet: map[string]string{
		"subnet-private-a": "InsufficientInstanceCapacity: no current stock",
	}}
	provider := testAWSEC2(runner)
	provider.SubnetID = ""
	provider.SubnetIDs = []string{"subnet-private-a", "subnet-private-b"}
	handle, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err != nil || handle.ResourceID != "i-fixture" || runner.createCalls != 2 {
		t.Fatalf("handle=%#v creates=%d err=%v", handle, runner.createCalls, err)
	}
	if len(runner.runInstanceHistory) != 2 || awsArgumentSubnet(runner.runInstanceHistory[0]) != "subnet-private-a" || awsArgumentSubnet(runner.runInstanceHistory[1]) != "subnet-private-b" {
		t.Fatalf("capacity boundaries were not tried in order: %#v", runner.runInstanceHistory)
	}
	firstToken := awsArgumentClientToken(runner.runInstanceHistory[0])
	secondToken := awsArgumentClientToken(runner.runInstanceHistory[1])
	if firstToken == "" || secondToken == "" || firstToken == secondToken {
		t.Fatalf("placement attempts require distinct idempotency tokens: %q %q", firstToken, secondToken)
	}
}

func TestAWSEC2DoesNotTryAnotherBoundaryAfterAmbiguousFailure(t *testing.T) {
	runner := &fakeAWSRunner{createFailureBySubnet: map[string]string{
		"subnet-private-a": "RequestTimeout: the response was not received",
	}}
	provider := testAWSEC2(runner)
	provider.SubnetID = ""
	provider.SubnetIDs = []string{"subnet-private-a", "subnet-private-b"}
	if _, err := provider.EnsureReplica(context.Background(), awsReplicaSpec()); err == nil || runner.createCalls != 1 {
		t.Fatalf("ambiguous launch must stop before another placement: creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2AdoptsOwnedInstanceFromAnyApprovedBoundary(t *testing.T) {
	runner := &fakeAWSRunner{instanceID: "i-existing", externalKey: awsReplicaSpec().ExternalKey, state: "running", instanceSubnet: "subnet-private-b"}
	provider := testAWSEC2(runner)
	provider.SubnetID = ""
	provider.SubnetIDs = []string{"subnet-private-a", "subnet-private-b"}
	handle, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if err != nil || handle.ResourceID != "i-existing" || runner.createCalls != 0 {
		t.Fatalf("handle=%#v creates=%d err=%v", handle, runner.createCalls, err)
	}
}

func TestAWSEC2ReportsAllCapacityBoundariesUnavailable(t *testing.T) {
	runner := &fakeAWSRunner{createFailureBySubnet: map[string]string{
		"subnet-private-a": "InsufficientInstanceCapacity: no current stock",
		"subnet-private-b": "InsufficientInstanceCapacity: no current stock",
	}}
	provider := testAWSEC2(runner)
	provider.SubnetID = ""
	provider.SubnetIDs = []string{"subnet-private-a", "subnet-private-b"}
	_, err := provider.EnsureReplica(context.Background(), awsReplicaSpec())
	if !errors.Is(err, ErrProviderCapacity) || runner.createCalls != 2 || !strings.Contains(err.Error(), "across 2 configured capacity boundaries") {
		t.Fatalf("creates=%d err=%v", runner.createCalls, err)
	}
}

func TestAWSEC2PersistsOnlyStructuredStartupEvidence(t *testing.T) {
	runner := &fakeAWSRunner{
		instanceID: "i-fixture", externalKey: awsReplicaSpec().ExternalKey, state: "running",
		consoleOutput: "authorization=secret-value\n" +
			"infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
			"infercrane_startup stage=image_check at=2026-08-23T10:00:02Z\n" +
			"infercrane_startup stage=image_cache_hit at=2026-08-23T10:00:03Z\n" +
			"infercrane_startup stage=runtime_start at=2026-08-23T10:00:04Z\n",
	}
	observation, err := testAWSEC2(runner).ObserveReplica(context.Background(), ProviderHandle{ResourceID: runner.instanceID}, 8000)
	if err != nil || !strings.Contains(observation.Details, `"current_stage":"runtime_start"`) || !strings.Contains(observation.Details, `"image_cache":"hit"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	if strings.Contains(observation.Details, "secret-value") || strings.Contains(observation.Details, "authorization") {
		t.Fatalf("raw console output leaked into provider details: %s", observation.Details)
	}
}

func TestAWSEC2RequiredImageCacheMissFailsClosed(t *testing.T) {
	runner := &fakeAWSRunner{
		instanceID: "i-fixture", externalKey: awsReplicaSpec().ExternalKey, state: "running",
		consoleOutput: "infercrane_startup stage=identity_start at=2026-08-23T10:00:00Z\n" +
			"infercrane_startup stage=image_check at=2026-08-23T10:00:01Z\n" +
			"infercrane_startup stage=image_cache_miss_required at=2026-08-23T10:00:02Z\n",
	}
	provider := testAWSEC2(runner)
	provider.ImageCachePolicy = "required"
	observation, err := provider.ObserveReplica(context.Background(), ProviderHandle{ResourceID: runner.instanceID}, 8000)
	if err != nil || observation.State != "failed" || observation.Endpoint != "" || !strings.Contains(observation.Details, `"image_cache":"required_miss"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	spec := awsReplicaSpec()
	runner.instanceID = ""
	if _, err = provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	userData := awsUserData(runner.runInstanceArgs)
	if !strings.Contains(userData, "image_cache_miss_required") || strings.Contains(userData, "docker pull") {
		t.Fatalf("required cache policy did not fail before pull:\n%s", userData)
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

func TestAWSEC2RefusesAdoptionWhenRootIntentTagsLieAboutActualStorage(t *testing.T) {
	claimedEncrypted := true
	runner := &fakeAWSRunner{
		instanceID:        "i-stale",
		externalKey:       awsReplicaSpec().ExternalKey,
		state:             "running",
		rootVolumeGiB:     75,
		rootEncrypted:     false,
		tagRootVolumeGiB:  200,
		tagRootEncrypted:  &claimedEncrypted,
		tagRootDeviceName: "/dev/sda1",
		amiRootDeviceName: "/dev/sda1",
		amiRootVolumeGiB:  75,
	}
	_, err := testAWSEC2(runner).EnsureReplica(context.Background(), awsReplicaSpec())
	if err == nil || !strings.Contains(err.Error(), "encrypted root volume") || runner.createCalls != 0 {
		t.Fatalf("lying storage tags were trusted: creates=%d err=%v", runner.createCalls, err)
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
