package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWSEC2 is a narrow BYOC adapter. It delegates AWS authentication and API
// compatibility to AWS CLI v2 while retaining deterministic lifecycle policy
// in InferCrane. It never persists or logs AssumeRole credentials.
type AWSEC2 struct {
	Binary                   string
	Runner                   CommandRunner
	RoleARN, ExternalID      string
	Region, SubnetID         string
	SecurityGroupIDs         []string
	AMIID, InstanceType, GPU string
	InstanceProfileARN       string
	WorkerSecretARN          string
	ImageDigest              string
	CostSource               string
	CostObservedAt           time.Time
	HourlyCostMicrousd       int64
	RootVolumeGiB            int
	ImageCachePolicy         string
}

type awsCredentials struct {
	Credentials struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
	} `json:"Credentials"`
}

type awsDescribe struct {
	Reservations []struct {
		Instances []struct {
			InstanceID       string `json:"InstanceId"`
			ImageID          string `json:"ImageId"`
			InstanceType     string `json:"InstanceType"`
			SubnetID         string `json:"SubnetId"`
			PrivateIPAddress string `json:"PrivateIpAddress"`
			IAMProfile       struct {
				ARN string `json:"Arn"`
			} `json:"IamInstanceProfile"`
			SecurityGroups []struct {
				GroupID string `json:"GroupId"`
			} `json:"SecurityGroups"`
			State struct {
				Name string `json:"Name"`
			} `json:"State"`
			Tags []struct {
				Key, Value string
			} `json:"Tags"`
		} `json:"Instances"`
	} `json:"Reservations"`
}

type awsInstance struct {
	ID, ExternalKey, State, PrivateIP string
	ImageID, InstanceType, SubnetID   string
	InstanceProfileARN                string
	SecurityGroupIDs                  []string
	RootVolumeGiB                     int
	RootVolumeEncrypted               bool
}

// Check performs a read-only role-assumption and identity probe. It validates
// the same credential boundary used by lifecycle calls without creating or
// changing an AWS resource.
func (a AWSEC2) Check(ctx context.Context) error {
	if a.RoleARN == "" || a.Region == "" {
		return errors.New("AWS role ARN and region are required")
	}
	output, err := a.run(ctx, "sts", "get-caller-identity", "--output", "json", "--no-cli-pager")
	if err != nil {
		return err
	}
	var identity struct {
		Account string `json:"Account"`
		ARN     string `json:"Arn"`
	}
	if json.Unmarshal(output, &identity) != nil || identity.Account == "" || identity.ARN == "" {
		return errors.New("AWS identity probe returned invalid output")
	}
	return nil
}

func (a AWSEC2) Handle(externalKey string) ProviderHandle {
	return ProviderHandle{RequestID: a.clientToken(externalKey), ExternalKey: externalKey}
}

func (a AWSEC2) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if !spec.Workload.Empty() {
		spec.Port = spec.Workload.Port
	}
	if err := a.validate(spec); err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	existing, err := a.find(ctx, spec.ExternalKey)
	if err != nil {
		return ProviderHandle{}, err
	}
	if existing.ID != "" {
		if err := a.validateAdoptedInstance(existing); err != nil {
			return ProviderHandle{}, err
		}
		return ProviderHandle{RequestID: a.clientToken(spec.ExternalKey), ResourceID: existing.ID, ExternalKey: spec.ExternalKey}, nil
	}
	port := spec.Port
	if port == 0 {
		port = 8000
	}
	userData := a.userData(spec, port)
	network := []map[string]any{{"DeviceIndex": 0, "SubnetId": a.SubnetID, "Groups": a.SecurityGroupIDs, "AssociatePublicIpAddress": false}}
	networkJSON, _ := json.Marshal(network)
	rootVolumeGiB := a.rootVolumeGiB()
	resourceTags := []map[string]string{{"Key": "infercrane:managed", "Value": "true"}, {"Key": "infercrane:external-key", "Value": spec.ExternalKey}, {"Key": "infercrane:root-volume-gib", "Value": fmt.Sprint(rootVolumeGiB)}, {"Key": "infercrane:root-volume-encrypted", "Value": "true"}, {"Key": "Name", "Value": "infercrane-" + spec.ExternalKey}}
	// Root volumes remain billable if provider-side termination fails to remove
	// them. Give instances and volumes the same durable ownership identity so
	// inventory and guarded cleanup can detect either resource independently.
	tags := []map[string]any{{"ResourceType": "instance", "Tags": resourceTags}, {"ResourceType": "volume", "Tags": resourceTags}}
	tagsJSON, _ := json.Marshal(tags)
	blockDevices := []map[string]any{{"DeviceName": "/dev/xvda", "Ebs": map[string]any{"VolumeSize": rootVolumeGiB, "VolumeType": "gp3", "Encrypted": true, "DeleteOnTermination": true}}}
	blockDevicesJSON, _ := json.Marshal(blockDevices)
	args := []string{"ec2", "run-instances", "--region", a.Region, "--image-id", a.AMIID, "--instance-type", a.InstanceType, "--count", "1", "--client-token", a.clientToken(spec.ExternalKey), "--iam-instance-profile", "Arn=" + a.InstanceProfileARN, "--network-interfaces", string(networkJSON), "--block-device-mappings", string(blockDevicesJSON), "--tag-specifications", string(tagsJSON), "--user-data", userData, "--output", "json", "--no-cli-pager"}
	output, err := a.run(ctx, args...)
	if err != nil {
		return ProviderHandle{}, fmt.Errorf("launch AWS EC2 instance: %w", err)
	}
	var launched struct {
		Instances []struct {
			InstanceID string `json:"InstanceId"`
		} `json:"Instances"`
	}
	if err := json.Unmarshal(output, &launched); err != nil || len(launched.Instances) != 1 || launched.Instances[0].InstanceID == "" {
		return ProviderHandle{}, errors.New("AWS EC2 launch returned an invalid instance identity")
	}
	return ProviderHandle{RequestID: a.clientToken(spec.ExternalKey), ResourceID: launched.Instances[0].InstanceID, ExternalKey: spec.ExternalKey}, nil
}

func (a AWSEC2) ObserveReplica(ctx context.Context, handle ProviderHandle, port int) (Observation, error) {
	if handle.ExternalKey == "" && handle.ResourceID == "" {
		return Observation{}, errors.New("AWS provider handle requires external key or resource ID")
	}
	instance, err := a.findHandle(ctx, handle)
	if err != nil || instance.ID == "" {
		return Observation{}, err
	}
	if port == 0 {
		port = 8000
	}
	state := normalizeAWSState(instance.State)
	endpoint := ""
	if state == "ready" && net.ParseIP(instance.PrivateIP) != nil {
		endpoint = fmt.Sprintf("http://%s:%d", instance.PrivateIP, port)
	}
	details := map[string]any{"instance_id": instance.ID, "region": a.Region, "state": instance.State, "network": "private"}
	if state == "ready" {
		// Console startup evidence is best-effort. Existing AWS roles that have
		// not yet granted GetConsoleOutput must continue to reconcile safely.
		// Never persist the raw console: it may contain runtime or model output.
		if output, consoleErr := a.run(ctx, "ec2", "get-console-output", "--region", a.Region, "--instance-id", instance.ID, "--latest", "--output", "json", "--no-cli-pager"); consoleErr == nil {
			var response struct {
				Output string `json:"Output"`
			}
			if json.Unmarshal(output, &response) == nil {
				if evidence, ok := parseStartupEvidence(response.Output); ok {
					details["startup_evidence"] = evidence
					if evidence.CurrentStage == "image_cache_miss_required" {
						state, endpoint = "failed", ""
					}
				}
			}
		}
	}
	if a.HourlyCostMicrousd > 0 && a.CostSource != "" && !a.CostObservedAt.IsZero() {
		details["hourly_cost_microusd"], details["cost_source"], details["cost_observed_at"] = a.HourlyCostMicrousd, a.CostSource, a.CostObservedAt.UTC()
	} else {
		details["cost_state"] = "unknown"
	}
	encoded, _ := json.Marshal(details)
	return Observation{Exists: state != "absent", State: state, Endpoint: endpoint, Details: string(encoded)}, nil
}

func (a AWSEC2) DeleteReplica(ctx context.Context, handle ProviderHandle) error {
	instance, err := a.findHandle(ctx, handle)
	if err != nil || instance.ID == "" || normalizeAWSState(instance.State) == "absent" {
		return err
	}
	_, err = a.run(ctx, "ec2", "terminate-instances", "--region", a.Region, "--instance-ids", instance.ID, "--output", "json", "--no-cli-pager")
	if err != nil {
		return fmt.Errorf("terminate AWS EC2 instance %s: %w", instance.ID, err)
	}
	return nil
}

func (a AWSEC2) Inventory(ctx context.Context, filter InventoryFilter) ([]Resource, error) {
	instances, err := a.describe(ctx, "Name=tag:infercrane:managed,Values=true")
	if err != nil {
		return nil, err
	}
	resources := make([]Resource, 0, len(instances))
	for _, instance := range instances {
		if filter.Prefix != "" && !strings.HasPrefix(instance.ExternalKey, filter.Prefix) {
			continue
		}
		state := normalizeAWSState(instance.State)
		if state == "absent" {
			continue
		}
		endpoint := ""
		if state == "ready" && net.ParseIP(instance.PrivateIP) != nil {
			endpoint = "http://" + instance.PrivateIP + ":8000"
		}
		resources = append(resources, Resource{ID: instance.ID, ExternalKey: instance.ExternalKey, State: state, Endpoint: endpoint})
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].ExternalKey < resources[j].ExternalKey })
	return resources, nil
}

func (a AWSEC2) validate(spec ReplicaSpec) error {
	if spec.ExternalKey == "" || spec.Model == "" || spec.Cloud != "aws" || spec.Region == "" || spec.Region != a.Region {
		return errors.New("AWS replica requires external key, model, cloud aws, and configured region")
	}
	if a.RoleARN == "" || a.SubnetID == "" || len(a.SecurityGroupIDs) == 0 || a.AMIID == "" || a.InstanceType == "" || a.GPU == "" || a.InstanceProfileARN == "" || a.WorkerSecretARN == "" {
		return errors.New("AWS role, region, subnet, security groups, AMI, instance type, GPU, instance profile, and worker secret ARN are required")
	}
	if spec.GPU != a.GPU {
		return fmt.Errorf("configured AWS instance type %s is qualified for GPU %s, not %s", a.InstanceType, a.GPU, spec.GPU)
	}
	if a.rootVolumeGiB() < 50 || a.rootVolumeGiB() > 16384 {
		return errors.New("AWS root volume must be between 50 and 16384 GiB")
	}
	if !spec.Workload.Empty() {
		if err := spec.Workload.Validate(); err != nil {
			return fmt.Errorf("portable workload: %w", err)
		}
	} else if !strings.Contains(a.ImageDigest, "@sha256:") {
		return errors.New("AWS workload image must be pinned by sha256 digest")
	}
	return nil
}

func (a AWSEC2) findHandle(ctx context.Context, handle ProviderHandle) (awsInstance, error) {
	if handle.ResourceID != "" {
		instances, err := a.describe(ctx, "Name=instance-id,Values="+handle.ResourceID)
		if err != nil || len(instances) == 0 {
			return awsInstance{}, err
		}
		return instances[0], nil
	}
	return a.find(ctx, handle.ExternalKey)
}

func (a AWSEC2) find(ctx context.Context, externalKey string) (awsInstance, error) {
	instances, err := a.describe(ctx, "Name=tag:infercrane:external-key,Values="+externalKey)
	if err != nil {
		return awsInstance{}, err
	}
	active := make([]awsInstance, 0, len(instances))
	for _, instance := range instances {
		if normalizeAWSState(instance.State) != "absent" {
			active = append(active, instance)
		}
	}
	if len(active) > 1 {
		return awsInstance{}, fmt.Errorf("multiple AWS EC2 instances match durable key %q", externalKey)
	}
	if len(active) == 0 {
		return awsInstance{}, nil
	}
	return active[0], nil
}

func (a AWSEC2) validateAdoptedInstance(instance awsInstance) error {
	expectedGroups := append([]string(nil), a.SecurityGroupIDs...)
	actualGroups := append([]string(nil), instance.SecurityGroupIDs...)
	sort.Strings(expectedGroups)
	sort.Strings(actualGroups)
	if instance.ImageID != a.AMIID || instance.InstanceType != a.InstanceType || instance.SubnetID != a.SubnetID || instance.InstanceProfileARN != a.InstanceProfileARN || strings.Join(actualGroups, "\x00") != strings.Join(expectedGroups, "\x00") || instance.RootVolumeGiB != a.rootVolumeGiB() || !instance.RootVolumeEncrypted {
		return fmt.Errorf("AWS EC2 instance %s with durable key %q does not match the configured AMI, instance type, subnet, instance profile, security groups, and encrypted root volume", instance.ID, instance.ExternalKey)
	}
	return nil
}

func (a AWSEC2) describe(ctx context.Context, filter string) ([]awsInstance, error) {
	output, err := a.run(ctx, "ec2", "describe-instances", "--region", a.Region, "--filters", filter, "--output", "json", "--no-cli-pager")
	if err != nil {
		return nil, fmt.Errorf("describe AWS EC2 instances: %w", err)
	}
	var response awsDescribe
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, errors.New("AWS EC2 describe returned invalid JSON")
	}
	var out []awsInstance
	for _, reservation := range response.Reservations {
		for _, instance := range reservation.Instances {
			item := awsInstance{ID: instance.InstanceID, State: instance.State.Name, PrivateIP: instance.PrivateIPAddress, ImageID: instance.ImageID, InstanceType: instance.InstanceType, SubnetID: instance.SubnetID, InstanceProfileARN: instance.IAMProfile.ARN}
			for _, group := range instance.SecurityGroups {
				item.SecurityGroupIDs = append(item.SecurityGroupIDs, group.GroupID)
			}
			for _, tag := range instance.Tags {
				switch tag.Key {
				case "infercrane:external-key":
					item.ExternalKey = tag.Value
				case "infercrane:root-volume-gib":
					item.RootVolumeGiB, _ = strconv.Atoi(tag.Value)
				case "infercrane:root-volume-encrypted":
					item.RootVolumeEncrypted = tag.Value == "true"
				}
			}
			out = append(out, item)
		}
	}
	return out, nil
}

func (a AWSEC2) run(ctx context.Context, args ...string) ([]byte, error) {
	runner := a.Runner
	if runner == nil {
		binary := a.Binary
		if binary == "" {
			binary = "aws"
		}
		if _, err := exec.LookPath(binary); err != nil {
			return nil, errors.New("AWS CLI v2 is required")
		}
		runner = execRunner{binary: binary}
	}
	assumeArgs := []string{"sts", "assume-role", "--role-arn", a.RoleARN, "--role-session-name", "infercrane-control-plane", "--duration-seconds", "3600", "--output", "json", "--no-cli-pager"}
	if a.ExternalID != "" {
		assumeArgs = append(assumeArgs, "--external-id", a.ExternalID)
	}
	credentialOutput, err := runner.Run(ctx, nil, assumeArgs...)
	if err != nil {
		return nil, errors.New("AWS role assumption failed")
	}
	var credentials awsCredentials
	if err := json.Unmarshal(credentialOutput, &credentials); err != nil || credentials.Credentials.AccessKeyID == "" || credentials.Credentials.SecretAccessKey == "" || credentials.Credentials.SessionToken == "" {
		return nil, errors.New("AWS role assumption returned invalid temporary credentials")
	}
	environment := []string{"AWS_ACCESS_KEY_ID=" + credentials.Credentials.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + credentials.Credentials.SecretAccessKey, "AWS_SESSION_TOKEN=" + credentials.Credentials.SessionToken, "AWS_REGION=" + a.Region, "AWS_DEFAULT_REGION=" + a.Region}
	output, err := runner.Run(ctx, environment, args...)
	if err != nil {
		return nil, normalizeAWSAPIError(output)
	}
	return output, nil
}

// normalizeAWSAPIError preserves actionable provider semantics without
// persisting raw CLI output, account identifiers, resource ARNs, credentials,
// or encoded authorization messages.
func normalizeAWSAPIError(output []byte) error {
	message := strings.ToLower(string(output))
	switch {
	case strings.Contains(message, "unauthorizedoperation"), strings.Contains(message, "accessdenied"), strings.Contains(message, "authfailure"):
		return fmt.Errorf("%w: verify the assumed role and resource-scoped EC2 permissions", ErrProviderAuthorization)
	case strings.Contains(message, "vcpulimitexceeded"), strings.Contains(message, "instancelimitexceeded"):
		return fmt.Errorf("%w: EC2 compute quota is exhausted for the requested instance family", ErrProviderQuota)
	case strings.Contains(message, "insufficientinstancecapacity"):
		return fmt.Errorf("%w: the requested EC2 instance type is unavailable in the selected capacity boundary", ErrProviderCapacity)
	case strings.Contains(message, "requestlimitexceeded"), strings.Contains(message, "throttl"):
		return errors.New("AWS API request was rate limited")
	case strings.Contains(message, "idempotentparametermismatch"):
		return errors.New("AWS idempotency token conflicts with changed launch parameters")
	case strings.Contains(message, "invalidparameter"), strings.Contains(message, "invalidamiid"):
		return fmt.Errorf("%w: AWS rejected the EC2 launch configuration", ErrInvalidReplicaSpec)
	default:
		return errors.New("AWS API request failed")
	}
}

func (a AWSEC2) clientToken(externalKey string) string {
	sum := sha256.Sum256([]byte(a.Region + "\x00" + a.SubnetID + "\x00" + externalKey))
	return hex.EncodeToString(sum[:])
}

func (a AWSEC2) rootVolumeGiB() int {
	if a.RootVolumeGiB > 0 {
		return a.RootVolumeGiB
	}
	return 100
}

func (a AWSEC2) userData(spec ReplicaSpec, port int) string {
	if !spec.Workload.Empty() {
		args := append(append([]string(nil), spec.Workload.Command...), spec.RuntimeArgs...)
		quoted := make([]string, len(args))
		for i, arg := range args {
			switch arg {
			case "${WORKER_API_KEY}":
				quoted[i] = `"$worker_key"`
			default:
				arg = strings.ReplaceAll(arg, "${MODEL}", spec.Model)
				arg = strings.ReplaceAll(arg, "${MODEL_REVISION}", spec.ModelRevision)
				arg = strings.ReplaceAll(arg, "${PORT}", fmt.Sprint(port))
				quoted[i] = shellQuote(arg)
			}
		}
		image := spec.Workload.Image
		return "#!/bin/sh\nset -eu\ninfercrane_stage() { printf 'infercrane_startup stage=%s at=%s\\n' \"$1\" \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\" >/dev/console; }\ninfercrane_stage identity_start\nworker_key=$(aws secretsmanager get-secret-value --region " + shellQuote(a.Region) + " --secret-id " + shellQuote(a.WorkerSecretARN) + " --query SecretString --output text)\ninfercrane_stage identity_ready\n" + cachedImageBootstrapWithPolicy(image, a.imageCachePolicy()) + "infercrane_stage runtime_start\ncontainer_id=$(docker run -d --restart=unless-stopped --gpus all -e INFERCRANE_WORKER_API_KEY=\"$worker_key\" -e VLLM_API_KEY=\"$worker_key\" -p " + fmt.Sprintf("%d:%d", port, port) + " " + shellQuote(image) + " " + strings.Join(quoted, " ") + ")\ninfercrane_stage runtime_container_started\ndocker logs --follow \"$container_id\" >/dev/console 2>&1 &\n"
	}
	// The vLLM OpenAI image entrypoint is `vllm serve`. Since v0.22 the model
	// is a positional argument; keep generated startup compatible with the
	// current CLI instead of relying on its deprecated --model alias.
	args := []string{spec.Model, "--port", fmt.Sprint(port)}
	if spec.ModelRevision != "" {
		args = append(args, "--revision", spec.ModelRevision)
	}
	args = append(args, spec.RuntimeArgs...)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return "#!/bin/sh\nset -eu\ninfercrane_stage() { printf 'infercrane_startup stage=%s at=%s\\n' \"$1\" \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\" >/dev/console; }\ninfercrane_stage identity_start\nworker_key=$(aws secretsmanager get-secret-value --region " + shellQuote(a.Region) + " --secret-id " + shellQuote(a.WorkerSecretARN) + " --query SecretString --output text)\ninfercrane_stage identity_ready\n" + cachedImageBootstrapWithPolicy(a.ImageDigest, a.imageCachePolicy()) + "infercrane_stage runtime_start\ncontainer_id=$(docker run -d --restart=unless-stopped --gpus all -e VLLM_API_KEY=\"$worker_key\" -p " + fmt.Sprintf("%d:%d", port, port) + " " + shellQuote(a.ImageDigest) + " " + strings.Join(quoted, " ") + ")\ninfercrane_stage runtime_container_started\ndocker logs --follow \"$container_id\" >/dev/console 2>&1 &\n"
}

func (a AWSEC2) imageCachePolicy() string {
	if a.ImageCachePolicy == "required" {
		return "required"
	}
	return "prefer"
}

func normalizeAWSState(state string) string {
	switch strings.ToLower(state) {
	case "pending":
		return "provisioning"
	case "running":
		return "ready"
	case "stopping", "stopped", "shutting-down":
		return "deleting"
	case "terminated", "":
		return "absent"
	default:
		return "unknown"
	}
}
