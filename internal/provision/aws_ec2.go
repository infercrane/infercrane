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

	"github.com/infercrane/infercrane/internal/artifactcache"
)

// AWSEC2 is a narrow BYOC adapter. It delegates AWS authentication and API
// compatibility to AWS CLI v2 while retaining deterministic lifecycle policy
// in InferCrane. It never persists or logs AssumeRole credentials.
type AWSEC2 struct {
	Binary                           string
	Runner                           CommandRunner
	RoleARN, ExternalID              string
	Region, SubnetID                 string
	SubnetIDs                        []string
	SecurityGroupIDs                 []string
	AMIID, InstanceType, GPU         string
	GPUCount                         int
	InstanceProfileARN               string
	WorkerSecretARN                  string
	ImageDigest                      string
	CostSource                       string
	CostObservedAt                   time.Time
	HourlyCostMicrousd               int64
	RootVolumeGiB                    int
	ImageCachePolicy                 string
	ArtifactCachePolicy              string
	ArtifactSnapshots                map[string]string
	ArtifactVolumeInitializationRate int
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
			RootDeviceName   string `json:"RootDeviceName"`
			BlockDevices     []struct {
				DeviceName string `json:"DeviceName"`
				EBS        struct {
					VolumeID string `json:"VolumeId"`
				} `json:"Ebs"`
			} `json:"BlockDeviceMappings"`
			IAMProfile struct {
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
	RootDeviceName                    string
	RootVolumeGiB                     int
	RootVolumeEncrypted               bool
	RootDeviceNameIntent              string
	RootVolumeGiBIntent               int
	RootVolumeEncryptedIntent         bool
	ArtifactSnapshotID                string
	ArtifactIdentityDigest            string
}

type awsImage struct {
	ImageID         string
	RootDeviceName  string
	RootVolumeGiB   int
	OccupiedDevices map[string]struct{}
}

type awsSnapshot struct {
	SnapshotID, State string
	Encrypted         bool
	VolumeSize        int
	Tags              map[string]string
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
	if a.AMIID != "" {
		if _, err = a.resolveImage(ctx); err != nil {
			return err
		}
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
	image, err := a.resolveImage(ctx)
	if err != nil {
		return ProviderHandle{}, err
	}
	existing, err := a.find(ctx, spec.ExternalKey)
	if err != nil {
		return ProviderHandle{}, err
	}
	if existing.ID != "" {
		if err := a.validateAdoptedInstance(existing, spec, image); err != nil {
			return ProviderHandle{}, err
		}
		return ProviderHandle{RequestID: a.clientToken(spec.ExternalKey), ResourceID: existing.ID, ExternalKey: spec.ExternalKey}, nil
	}
	port := spec.Port
	if port == 0 {
		port = 8000
	}
	artifactSnapshot, err := a.resolveArtifactSnapshot(ctx, spec)
	if err != nil {
		return ProviderHandle{}, err
	}
	userData := a.userData(spec, port, artifactSnapshot.SnapshotID)
	rootVolumeGiB := a.rootVolumeGiB()
	instanceTags := []map[string]string{{"Key": "infercrane:managed", "Value": "true"}, {"Key": "infercrane:external-key", "Value": spec.ExternalKey}, {"Key": "infercrane:root-device-name", "Value": image.RootDeviceName}, {"Key": "infercrane:root-volume-gib", "Value": fmt.Sprint(rootVolumeGiB)}, {"Key": "infercrane:root-volume-encrypted", "Value": "true"}, {"Key": "Name", "Value": "infercrane-" + spec.ExternalKey}}
	volumeTags := []map[string]string{{"Key": "infercrane:managed", "Value": "true"}, {"Key": "infercrane:external-key", "Value": spec.ExternalKey}, {"Key": "Name", "Value": "infercrane-" + spec.ExternalKey}}
	if artifactSnapshot.SnapshotID != "" {
		cacheTags := []map[string]string{
			{"Key": "infercrane:artifact-snapshot-id", "Value": artifactSnapshot.SnapshotID},
			{"Key": "infercrane:artifact-identity-digest", "Value": modelIdentityDigest(modelIdentity(spec))},
		}
		instanceTags = append(instanceTags, cacheTags...)
		volumeTags = append(volumeTags, cacheTags...)
	}
	// Root volumes remain billable if provider-side termination fails to remove
	// them. Give instances and volumes the same durable ownership identity so
	// inventory and guarded cleanup can detect either resource independently.
	tags := []map[string]any{{"ResourceType": "instance", "Tags": instanceTags}, {"ResourceType": "volume", "Tags": volumeTags}}
	tagsJSON, _ := json.Marshal(tags)
	blockDevices := []map[string]any{{"DeviceName": image.RootDeviceName, "Ebs": map[string]any{"VolumeSize": rootVolumeGiB, "VolumeType": "gp3", "Encrypted": true, "DeleteOnTermination": true}}}
	if artifactSnapshot.SnapshotID != "" {
		artifactDevice, deviceErr := artifactDeviceName(image)
		if deviceErr != nil {
			return ProviderHandle{}, deviceErr
		}
		ebs := map[string]any{"SnapshotId": artifactSnapshot.SnapshotID, "VolumeType": "gp3", "Encrypted": true, "DeleteOnTermination": true}
		if a.ArtifactVolumeInitializationRate > 0 {
			ebs["VolumeInitializationRate"] = a.ArtifactVolumeInitializationRate
		}
		blockDevices = append(blockDevices, map[string]any{"DeviceName": artifactDevice, "Ebs": ebs})
	}
	blockDevicesJSON, _ := json.Marshal(blockDevices)
	var capacityErrors []string
	for _, subnetID := range a.subnets() {
		network := []map[string]any{{"DeviceIndex": 0, "SubnetId": subnetID, "Groups": a.SecurityGroupIDs, "AssociatePublicIpAddress": false}}
		networkJSON, _ := json.Marshal(network)
		args := []string{"ec2", "run-instances", "--region", a.Region, "--image-id", a.AMIID, "--instance-type", a.InstanceType, "--count", "1", "--client-token", a.placementClientToken(spec.ExternalKey, subnetID), "--iam-instance-profile", "Arn=" + a.InstanceProfileARN, "--network-interfaces", string(networkJSON), "--block-device-mappings", string(blockDevicesJSON), "--tag-specifications", string(tagsJSON), "--user-data", userData, "--output", "json", "--no-cli-pager"}
		output, launchErr := a.run(ctx, args...)
		if launchErr != nil {
			if errors.Is(launchErr, ErrProviderCapacity) {
				capacityErrors = append(capacityErrors, subnetID)
				continue
			}
			// An authorization, timeout, or unknown response may have happened
			// after AWS accepted the side effect. Never try another placement;
			// the durable retry first adopts by ownership tag.
			return ProviderHandle{}, fmt.Errorf("launch AWS EC2 instance: %w", launchErr)
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
	return ProviderHandle{}, fmt.Errorf("launch AWS EC2 instance: %w: unavailable across %d configured capacity boundaries", ErrProviderCapacity, len(capacityErrors))
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
					if evidence.CurrentStage == "image_cache_miss_required" || evidence.CurrentStage == "artifact_cache_mount_failed" {
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
	if a.RoleARN == "" || len(a.subnets()) == 0 || len(a.SecurityGroupIDs) == 0 || a.AMIID == "" || a.InstanceType == "" || a.GPU == "" || a.InstanceProfileARN == "" || a.WorkerSecretARN == "" {
		return errors.New("AWS role, region, subnet, security groups, AMI, instance type, GPU, instance profile, and worker secret ARN are required")
	}
	if spec.GPU != a.GPU {
		return fmt.Errorf("configured AWS instance type %s is qualified for GPU %s, not %s", a.InstanceType, a.GPU, spec.GPU)
	}
	configuredGPUCount := a.GPUCount
	if configuredGPUCount == 0 {
		configuredGPUCount = 1
	}
	requestedGPUCount := spec.GPUCount
	if requestedGPUCount == 0 {
		requestedGPUCount = 1
	}
	if configuredGPUCount < 1 || configuredGPUCount > 1024 {
		return errors.New("configured AWS GPU count must be between 1 and 1024")
	}
	if requestedGPUCount != configuredGPUCount {
		return fmt.Errorf("configured AWS instance type %s is qualified for %d GPUs, not %d", a.InstanceType, configuredGPUCount, requestedGPUCount)
	}
	if a.rootVolumeGiB() < 50 || a.rootVolumeGiB() > 16384 {
		return errors.New("AWS root volume must be between 50 and 16384 GiB")
	}
	if policy := a.artifactCachePolicy(); policy != "disabled" && policy != "prefer" && policy != "required" {
		return errors.New("AWS artifact cache policy must be disabled, prefer, or required")
	}
	if rate := a.ArtifactVolumeInitializationRate; rate != 0 && (rate < 100 || rate > 300) {
		return errors.New("AWS artifact volume initialization rate must be 0 or between 100 and 300 MiB/s")
	}
	if a.artifactCachePolicy() == "required" && !artifactCacheRuntimeSupported(spec) {
		return errors.New("AWS artifact cache is qualified only for vLLM and SGLang workloads")
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

func (a AWSEC2) resolveArtifactSnapshot(ctx context.Context, spec ReplicaSpec) (awsSnapshot, error) {
	policy := a.artifactCachePolicy()
	if policy == "disabled" || !artifactCacheRuntimeSupported(spec) {
		return awsSnapshot{}, nil
	}
	identity := modelIdentity(spec)
	snapshotID := a.ArtifactSnapshots[identity]
	if snapshotID == "" {
		if policy == "required" {
			return awsSnapshot{}, fmt.Errorf("AWS artifact cache requires a verified snapshot for immutable model %q", identity)
		}
		return awsSnapshot{}, nil
	}
	return a.verifyArtifactSnapshot(ctx, identity, snapshotID)
}

func (a AWSEC2) verifyArtifactSnapshot(ctx context.Context, identity, snapshotID string) (awsSnapshot, error) {
	output, err := a.run(ctx, "ec2", "describe-snapshots", "--region", a.Region, "--snapshot-ids", snapshotID, "--output", "json", "--no-cli-pager")
	if err != nil {
		return awsSnapshot{}, fmt.Errorf("verify AWS artifact snapshot: %w", err)
	}
	var response struct {
		Snapshots []struct {
			SnapshotID string `json:"SnapshotId"`
			State      string `json:"State"`
			Encrypted  bool   `json:"Encrypted"`
			VolumeSize int    `json:"VolumeSize"`
			Tags       []struct {
				Key, Value string
			} `json:"Tags"`
		} `json:"Snapshots"`
	}
	if json.Unmarshal(output, &response) != nil || len(response.Snapshots) != 1 || response.Snapshots[0].SnapshotID != snapshotID {
		return awsSnapshot{}, errors.New("AWS artifact snapshot verification returned invalid output")
	}
	raw := response.Snapshots[0]
	result := awsSnapshot{SnapshotID: raw.SnapshotID, State: raw.State, Encrypted: raw.Encrypted, VolumeSize: raw.VolumeSize, Tags: map[string]string{}}
	for _, tag := range raw.Tags {
		result.Tags[tag.Key] = tag.Value
	}
	expectedDigest := modelIdentityDigest(identity)
	if result.State != "completed" || !result.Encrypted || result.VolumeSize < 1 || result.Tags["infercrane:artifact-cache"] != "true" || result.Tags["infercrane:model-identity-digest"] != expectedDigest {
		return awsSnapshot{}, fmt.Errorf("AWS artifact snapshot %s is not completed, encrypted, and tagged for immutable model identity digest %s", snapshotID, expectedDigest)
	}
	return result, nil
}

// resolveImage discovers the AMI root device before RunInstances. AWS AMIs do
// not share one universal root-device name: treating /dev/xvda as a constant
// can attach a second empty volume while leaving the real image root small or
// unencrypted. The image lookup is read-only and the returned root mapping is
// used for both launch and adoption validation.
func (a AWSEC2) resolveImage(ctx context.Context) (awsImage, error) {
	output, err := a.run(ctx, "ec2", "describe-images", "--region", a.Region, "--image-ids", a.AMIID, "--output", "json", "--no-cli-pager")
	if err != nil {
		return awsImage{}, fmt.Errorf("inspect AWS AMI root device: %w", err)
	}
	var response struct {
		Images []struct {
			ImageID             string `json:"ImageId"`
			RootDeviceName      string `json:"RootDeviceName"`
			BlockDeviceMappings []struct {
				DeviceName string `json:"DeviceName"`
				EBS        *struct {
					VolumeSize int `json:"VolumeSize"`
				} `json:"Ebs"`
			} `json:"BlockDeviceMappings"`
		} `json:"Images"`
	}
	if json.Unmarshal(output, &response) != nil || len(response.Images) != 1 || response.Images[0].ImageID != a.AMIID {
		return awsImage{}, errors.New("AWS AMI inspection returned invalid output")
	}
	raw := response.Images[0]
	if !strings.HasPrefix(raw.RootDeviceName, "/dev/") {
		return awsImage{}, errors.New("AWS AMI does not declare a valid root device")
	}
	result := awsImage{ImageID: raw.ImageID, RootDeviceName: raw.RootDeviceName, OccupiedDevices: map[string]struct{}{}}
	for _, mapping := range raw.BlockDeviceMappings {
		if mapping.DeviceName != "" {
			result.OccupiedDevices[mapping.DeviceName] = struct{}{}
		}
		if mapping.DeviceName == raw.RootDeviceName && mapping.EBS != nil {
			result.RootVolumeGiB = mapping.EBS.VolumeSize
		}
	}
	if result.RootVolumeGiB < 1 {
		return awsImage{}, errors.New("AWS AMI root device is not backed by a valid EBS volume")
	}
	if a.rootVolumeGiB() < result.RootVolumeGiB {
		return awsImage{}, fmt.Errorf("AWS root volume %d GiB is smaller than AMI root snapshot %d GiB", a.rootVolumeGiB(), result.RootVolumeGiB)
	}
	return result, nil
}

func artifactDeviceName(image awsImage) (string, error) {
	for _, candidate := range []string{"/dev/sdf", "/dev/sdg", "/dev/sdh", "/dev/sdi", "/dev/sdj", "/dev/sdk", "/dev/sdl", "/dev/sdm", "/dev/sdn", "/dev/sdo", "/dev/sdp"} {
		if _, occupied := image.OccupiedDevices[candidate]; !occupied && candidate != image.RootDeviceName {
			return candidate, nil
		}
	}
	return "", errors.New("AWS AMI leaves no supported device name for the artifact-cache volume")
}

// Prefetch adopts a customer-prepared immutable EBS snapshot after validating
// its closed identity contract. InferCrane does not copy model bytes or enable
// billed snapshot acceleration implicitly.
func (a AWSEC2) Prefetch(ctx context.Context, request artifactcache.Request) (artifactcache.Operation, error) {
	if err := request.Validate(); err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_prefetch_request", err)
	}
	if a.artifactCachePolicy() == "disabled" {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_cache_disabled", errors.New("AWS artifact cache adapter is disabled"))
	}
	if request.Provider != "aws" || request.Region != a.Region {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_provider_boundary", errors.New("AWS artifact prefetch provider and region must match the configured adapter"))
	}
	snapshotID, err := awsSnapshotLocation(request.Location)
	if err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_cache_location", err)
	}
	if configured := a.ArtifactSnapshots[request.ModelIdentity]; configured == "" || configured != snapshotID {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_cache_not_configured", errors.New("AWS artifact snapshot location is not configured for this immutable model identity"))
	}
	if _, err = a.verifyArtifactSnapshot(ctx, request.ModelIdentity, snapshotID); err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_snapshot_verification_failed", err)
	}
	return artifactcache.Operation{ProviderOperationID: snapshotID, Status: "succeeded"}, nil
}

// Observe returns bounded, expiring evidence for the same verified snapshot.
func (a AWSEC2) Observe(ctx context.Context, request artifactcache.Request) (artifactcache.Observation, error) {
	if err := request.Validate(); err != nil {
		return artifactcache.Observation{}, err
	}
	if a.artifactCachePolicy() == "disabled" || request.Provider != "aws" || request.Region != a.Region {
		return artifactcache.Observation{}, errors.New("AWS artifact cache adapter, provider, and region must match the configured boundary")
	}
	snapshotID, err := awsSnapshotLocation(request.Location)
	if err != nil {
		return artifactcache.Observation{}, err
	}
	if configured := a.ArtifactSnapshots[request.ModelIdentity]; configured == "" || configured != snapshotID {
		return artifactcache.Observation{}, errors.New("AWS artifact snapshot location is not configured for this immutable model identity")
	}
	snapshot, err := a.verifyArtifactSnapshot(ctx, request.ModelIdentity, snapshotID)
	if err != nil {
		return artifactcache.Observation{}, err
	}
	observed := time.Now().UTC()
	evidence, _ := json.Marshal(map[string]any{"snapshot_id": snapshot.SnapshotID, "encrypted": snapshot.Encrypted, "volume_size_gib": snapshot.VolumeSize, "model_identity_digest": modelIdentityDigest(request.ModelIdentity)})
	return artifactcache.Observation{State: "present", Source: "aws-ebs-snapshot", EvidenceJSON: string(evidence), ObservedAt: observed, ExpiresAt: observed.Add(10 * time.Minute)}, nil
}

func awsSnapshotLocation(location string) (string, error) {
	const prefix = "aws-ebs://"
	if !strings.HasPrefix(location, prefix) {
		return "", errors.New("AWS artifact cache location must use aws-ebs://snap-ID")
	}
	snapshotID := strings.TrimPrefix(location, prefix)
	if !validAWSSnapshotID(snapshotID) {
		return "", errors.New("AWS artifact cache location contains an invalid snapshot ID")
	}
	return snapshotID, nil
}

func validAWSSnapshotID(value string) bool {
	if !strings.HasPrefix(value, "snap-") || len(value) < len("snap-")+8 {
		return false
	}
	for _, char := range value[len("snap-"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func artifactCacheRuntimeSupported(spec ReplicaSpec) bool {
	runtimeName := spec.Runtime
	if runtimeName == "" && spec.Workload.Empty() {
		runtimeName = "vllm"
	}
	return runtimeName == "vllm" || runtimeName == "sglang"
}

func modelIdentity(spec ReplicaSpec) string {
	if spec.ModelRevision == "" {
		return spec.Model
	}
	return spec.Model + "@" + spec.ModelRevision
}

func modelIdentityDigest(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validArtifactModelIdentity(identity string) bool {
	separator := strings.LastIndex(identity, "@")
	if separator <= 0 || strings.TrimSpace(identity) != identity {
		return false
	}
	revision := identity[separator+1:]
	if len(revision) != 40 {
		return false
	}
	for _, char := range revision {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
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

func (a AWSEC2) validateAdoptedInstance(instance awsInstance, spec ReplicaSpec, image awsImage) error {
	expectedGroups := append([]string(nil), a.SecurityGroupIDs...)
	actualGroups := append([]string(nil), instance.SecurityGroupIDs...)
	sort.Strings(expectedGroups)
	sort.Strings(actualGroups)
	if instance.ImageID != a.AMIID || instance.InstanceType != a.InstanceType || !containsString(a.subnets(), instance.SubnetID) || instance.InstanceProfileARN != a.InstanceProfileARN || strings.Join(actualGroups, "\x00") != strings.Join(expectedGroups, "\x00") || instance.RootDeviceName != image.RootDeviceName || instance.RootDeviceNameIntent != image.RootDeviceName || instance.RootVolumeGiB != a.rootVolumeGiB() || instance.RootVolumeGiBIntent != a.rootVolumeGiB() || !instance.RootVolumeEncrypted || !instance.RootVolumeEncryptedIntent {
		return fmt.Errorf("AWS EC2 instance %s with durable key %q does not match the configured AMI, instance type, approved subnets, instance profile, security groups, and encrypted root volume", instance.ID, instance.ExternalKey)
	}
	configuredSnapshot := ""
	if artifactCacheRuntimeSupported(spec) && a.artifactCachePolicy() != "disabled" {
		configuredSnapshot = a.ArtifactSnapshots[modelIdentity(spec)]
	}
	if configuredSnapshot != instance.ArtifactSnapshotID || configuredSnapshot != "" && instance.ArtifactIdentityDigest != modelIdentityDigest(modelIdentity(spec)) {
		return fmt.Errorf("AWS EC2 instance %s with durable key %q does not match the configured immutable artifact snapshot", instance.ID, instance.ExternalKey)
	}
	if a.artifactCachePolicy() == "required" && configuredSnapshot == "" {
		return fmt.Errorf("AWS artifact cache requires a verified snapshot for immutable model %q", modelIdentity(spec))
	}
	return nil
}

func (a AWSEC2) subnets() []string {
	values := append([]string(nil), a.SubnetIDs...)
	if len(values) == 0 && a.SubnetID != "" {
		values = append(values, a.SubnetID)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (a AWSEC2) placementClientToken(externalKey, subnetID string) string {
	digest := sha256.Sum256([]byte(a.Region + "\x00" + externalKey + "\x00" + subnetID + "\x00" + a.InstanceType))
	return "infercrane-" + hex.EncodeToString(digest[:16])
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
			item := awsInstance{ID: instance.InstanceID, State: instance.State.Name, PrivateIP: instance.PrivateIPAddress, ImageID: instance.ImageID, InstanceType: instance.InstanceType, SubnetID: instance.SubnetID, InstanceProfileARN: instance.IAMProfile.ARN, RootDeviceName: instance.RootDeviceName}
			rootVolumeID := ""
			for _, device := range instance.BlockDevices {
				if device.DeviceName == instance.RootDeviceName {
					rootVolumeID = device.EBS.VolumeID
					break
				}
			}
			for _, group := range instance.SecurityGroups {
				item.SecurityGroupIDs = append(item.SecurityGroupIDs, group.GroupID)
			}
			for _, tag := range instance.Tags {
				switch tag.Key {
				case "infercrane:external-key":
					item.ExternalKey = tag.Value
				case "infercrane:root-device-name":
					item.RootDeviceNameIntent = tag.Value
				case "infercrane:root-volume-gib":
					item.RootVolumeGiBIntent, _ = strconv.Atoi(tag.Value)
				case "infercrane:root-volume-encrypted":
					item.RootVolumeEncryptedIntent = tag.Value == "true"
				case "infercrane:artifact-snapshot-id":
					item.ArtifactSnapshotID = tag.Value
				case "infercrane:artifact-identity-digest":
					item.ArtifactIdentityDigest = tag.Value
				}
			}
			if normalizeAWSState(instance.State.Name) != "absent" {
				if rootVolumeID == "" {
					return nil, fmt.Errorf("AWS EC2 instance %s has no EBS mapping for declared root device", instance.InstanceID)
				}
				volume, volumeErr := a.describeVolume(ctx, rootVolumeID)
				if volumeErr != nil {
					return nil, volumeErr
				}
				item.RootVolumeGiB = volume.Size
				item.RootVolumeEncrypted = volume.Encrypted
			}
			out = append(out, item)
		}
	}
	return out, nil
}

func (a AWSEC2) describeVolume(ctx context.Context, volumeID string) (struct {
	Size      int
	Encrypted bool
}, error) {
	var result struct {
		Size      int
		Encrypted bool
	}
	output, err := a.run(ctx, "ec2", "describe-volumes", "--region", a.Region, "--volume-ids", volumeID, "--output", "json", "--no-cli-pager")
	if err != nil {
		return result, fmt.Errorf("inspect AWS root volume: %w", err)
	}
	var response struct {
		Volumes []struct {
			VolumeID  string `json:"VolumeId"`
			Size      int    `json:"Size"`
			Encrypted bool   `json:"Encrypted"`
		} `json:"Volumes"`
	}
	if json.Unmarshal(output, &response) != nil || len(response.Volumes) != 1 || response.Volumes[0].VolumeID != volumeID || response.Volumes[0].Size < 1 {
		return result, errors.New("AWS root volume inspection returned invalid output")
	}
	result.Size = response.Volumes[0].Size
	result.Encrypted = response.Volumes[0].Encrypted
	return result, nil
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
	return 200
}

func (a AWSEC2) userData(spec ReplicaSpec, port int, artifactSnapshotID string) string {
	artifactBootstrap, artifactContainerArgs := awsArtifactCacheBootstrap(artifactSnapshotID)
	gpuCount := spec.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	gpuIdentity := "infercrane_stage accelerator_check\nactual_gpu_count=$(nvidia-smi --list-gpus | wc -l | tr -d ' ')\n[ \"$actual_gpu_count\" -eq " + fmt.Sprint(gpuCount) + " ] || { echo 'allocated GPU count does not match immutable revision intent' >&2; exit 1; }\ninfercrane_stage accelerator_ready\n"
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
		return "#!/bin/sh\nset -eu\ninfercrane_stage() { printf 'infercrane_startup stage=%s at=%s\\n' \"$1\" \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\" >/dev/console; }\ninfercrane_stage identity_start\nworker_key=$(aws secretsmanager get-secret-value --region " + shellQuote(a.Region) + " --secret-id " + shellQuote(a.WorkerSecretARN) + " --query SecretString --output text)\ninfercrane_stage identity_ready\n" + gpuIdentity + cachedImageBootstrapWithPolicy(image, a.imageCachePolicy()) + artifactBootstrap + "infercrane_stage runtime_start\ncontainer_id=$(docker run -d --restart=unless-stopped --gpus all " + artifactContainerArgs + "-e INFERCRANE_WORKER_API_KEY=\"$worker_key\" -e VLLM_API_KEY=\"$worker_key\" -p " + fmt.Sprintf("%d:%d", port, port) + " --entrypoint " + quoted[0] + " " + shellQuote(image) + " " + strings.Join(quoted[1:], " ") + ")\ninfercrane_stage runtime_container_started\ndocker logs --follow \"$container_id\" >/dev/console 2>&1 &\n"
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
	return "#!/bin/sh\nset -eu\ninfercrane_stage() { printf 'infercrane_startup stage=%s at=%s\\n' \"$1\" \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\" >/dev/console; }\ninfercrane_stage identity_start\nworker_key=$(aws secretsmanager get-secret-value --region " + shellQuote(a.Region) + " --secret-id " + shellQuote(a.WorkerSecretARN) + " --query SecretString --output text)\ninfercrane_stage identity_ready\n" + gpuIdentity + cachedImageBootstrapWithPolicy(a.ImageDigest, a.imageCachePolicy()) + artifactBootstrap + "infercrane_stage runtime_start\ncontainer_id=$(docker run -d --restart=unless-stopped --gpus all " + artifactContainerArgs + "-e VLLM_API_KEY=\"$worker_key\" -p " + fmt.Sprintf("%d:%d", port, port) + " " + shellQuote(a.ImageDigest) + " " + strings.Join(quoted, " ") + ")\ninfercrane_stage runtime_container_started\ndocker logs --follow \"$container_id\" >/dev/console 2>&1 &\n"
}

func awsArtifactCacheBootstrap(snapshotID string) (string, string) {
	if snapshotID == "" {
		return "infercrane_stage artifact_check\ninfercrane_stage artifact_cache_unconfigured\n", ""
	}
	bootstrap := "infercrane_stage artifact_check\n" +
		"artifact_device=''\n" +
		"attempt=0\n" +
		"while [ \"$attempt\" -lt 60 ]; do\n" +
		"  artifact_device=$(blkid -L INFERCRANE_ART 2>/dev/null || true)\n" +
		"  [ -n \"$artifact_device\" ] && break\n" +
		"  attempt=$((attempt + 1))\n" +
		"  sleep 5\n" +
		"done\n" +
		"if [ -z \"$artifact_device\" ]; then infercrane_stage artifact_cache_mount_failed; exit 1; fi\n" +
		"mkdir -p /var/lib/infercrane/huggingface\n" +
		"mount -o ro,nosuid,nodev \"$artifact_device\" /var/lib/infercrane/huggingface || { infercrane_stage artifact_cache_mount_failed; exit 1; }\n" +
		"infercrane_stage artifact_cache_hit\n"
	containerArgs := "-e HF_HOME=/root/.cache/huggingface -e HF_HUB_OFFLINE=1 -e TRANSFORMERS_OFFLINE=1 -v /var/lib/infercrane/huggingface:/root/.cache/huggingface:ro "
	return bootstrap, containerArgs
}

func (a AWSEC2) imageCachePolicy() string {
	if a.ImageCachePolicy == "required" {
		return "required"
	}
	return "prefer"
}

func (a AWSEC2) artifactCachePolicy() string {
	if a.ArtifactCachePolicy == "disabled" || a.ArtifactCachePolicy == "required" {
		return a.ArtifactCachePolicy
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
