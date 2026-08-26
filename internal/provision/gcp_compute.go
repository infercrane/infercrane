package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/artifactcache"
)

var errGCPResourceNotFound = errors.New("GCP resource not found")

// GCPCompute owns one private Compute Engine VM for one replica intent. The
// resource name is derived from ExternalKey, so an uncertain insert response
// is adopted by observation instead of replayed as a second VM.
type GCPCompute struct {
	Binary                                                                                             string
	Runner                                                                                             CommandRunner
	Project, Zone, Subnet, MachineType, GPUType, ServiceAccount, VMImage, ContainerImage, WorkerSecret string
	BootDiskGiB                                                                                        int
	ArtifactCachePolicy                                                                                string
	ArtifactDisks                                                                                      map[string]string
}

type gcpInstance struct {
	Name, Status      string
	NetworkInterfaces []struct {
		NetworkIP string `json:"networkIP"`
	} `json:"networkInterfaces"`
	Metadata struct{ Items []struct{ Key, Value string } }
}

type gcpDisk struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	SizeGB      string `json:"sizeGb"`
	Zone        string `json:"zone"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

// Check performs a read-only identity and Compute API probe. It deliberately
// does not create capacity: doctor must be safe to run in production.
func (g GCPCompute) Check(ctx context.Context) error {
	if g.Project == "" || g.Zone == "" || g.Subnet == "" || g.MachineType == "" || g.GPUType == "" || g.ServiceAccount == "" || g.VMImage == "" || g.ContainerImage == "" || g.WorkerSecret == "" {
		return errors.New("GCP project, zone, subnet, machine type, GPU, service account, VM image, container digest, and worker secret are required")
	}
	if !strings.Contains(g.ContainerImage, "@sha256:") {
		return errors.New("GCP container image must be pinned by sha256 digest")
	}
	if g.BootDiskGiB < 50 || g.BootDiskGiB > 65536 {
		return errors.New("GCP boot disk must be between 50 and 65536 GiB")
	}
	if err := g.validateArtifactConfig(); err != nil {
		return err
	}
	region, ok := gcpRegionFromZone(g.Zone)
	if !ok {
		return errors.New("GCP zone must end in a single-letter zone suffix")
	}
	imageProject, imageName, ok := gcpImageIdentity(g.VMImage)
	if !ok {
		return errors.New("GCP VM image must use projects/PROJECT/global/images/IMAGE immutable identity")
	}
	if _, err := g.run(ctx, "auth", "print-access-token", "--quiet"); err != nil {
		return fmt.Errorf("authenticate GCP control identity: %w", err)
	}
	checks := [][]string{
		{"compute", "zones", "describe", g.Zone, "--project", g.Project, "--format", "value(name)"},
		{"compute", "machine-types", "describe", g.MachineType, "--zone", g.Zone, "--project", g.Project, "--format", "value(name)"},
		{"compute", "accelerator-types", "describe", g.GPUType, "--zone", g.Zone, "--project", g.Project, "--format", "value(name)"},
		{"iam", "service-accounts", "describe", g.ServiceAccount, "--project", g.Project, "--format", "value(email)"},
		{"secrets", "describe", g.WorkerSecret, "--project", g.Project, "--format", "value(name)"},
	}
	privateGoogleAccess, err := g.run(ctx, "compute", "networks", "subnets", "describe", g.Subnet, "--region", region, "--project", g.Project, "--format", "value(privateIpGoogleAccess)")
	if err != nil {
		return fmt.Errorf("verify GCP dependency compute networks: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(privateGoogleAccess)), "true") {
		return errors.New("GCP subnet must enable Private Google Access because inference workers have no public IP")
	}
	checks = append(checks, []string{"compute", "images", "describe", imageName, "--project", imageProject, "--format", "value(name,status)"})
	for _, args := range checks {
		if _, err := g.run(ctx, args...); err != nil {
			return fmt.Errorf("verify GCP dependency %s: %w", strings.Join(args[:2], " "), err)
		}
	}
	identities := make([]string, 0, len(g.ArtifactDisks))
	for identity := range g.ArtifactDisks {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	for _, identity := range identities {
		if _, err := g.verifyArtifactDisk(ctx, identity, g.ArtifactDisks[identity]); err != nil {
			return err
		}
	}
	return nil
}

func (g GCPCompute) Handle(externalKey string) ProviderHandle {
	return ProviderHandle{RequestID: g.requestID(externalKey), ResourceID: g.name(externalKey), ExternalKey: externalKey}
}
func (g GCPCompute) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if err := g.validate(spec); err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	artifactDisk, err := g.resolveArtifactDisk(ctx, spec)
	if err != nil {
		return ProviderHandle{}, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	handle := g.Handle(spec.ExternalKey)
	existing, err := g.describe(ctx, handle.ResourceID)
	if err != nil {
		return ProviderHandle{}, err
	}
	if existing.Name != "" {
		if metadataValue(existing, "infercrane-external-key") != spec.ExternalKey || metadataValue(existing, "infercrane-intent-digest") != g.intentDigest(spec) {
			return ProviderHandle{}, fmt.Errorf("GCP Compute instance %s does not match immutable intent", existing.Name)
		}
		return handle, nil
	}
	port := spec.Port
	if !spec.Workload.Empty() {
		port = spec.Workload.Port
	}
	if port == 0 {
		port = 8000
	}
	startup := g.startup(spec, port)
	metadata := "^|||^infercrane-external-key=" + spec.ExternalKey + "|||infercrane-intent-digest=" + g.intentDigest(spec) + "|||startup-script=" + startup
	gpuCount := spec.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	args := []string{"compute", "instances", "create", handle.ResourceID, "--project", g.Project, "--zone", g.Zone, "--machine-type", g.MachineType, "--accelerator", fmt.Sprintf("type=%s,count=%d", g.GPUType, gpuCount), "--subnet", g.Subnet, "--no-address", "--service-account", g.ServiceAccount, "--scopes", "cloud-platform", "--image", g.VMImage, "--boot-disk-size", fmt.Sprintf("%dGB", g.BootDiskGiB), "--boot-disk-type", "pd-balanced", "--boot-disk-auto-delete", "--maintenance-policy", "TERMINATE", "--metadata", metadata, "--labels", "infercrane-managed=true", "--format", "json", "--quiet"}
	if artifactDisk.Name != "" {
		args = append(args, "--disk", "name="+artifactDisk.Name+",device-name=infercrane-model-cache,mode=ro,auto-delete=no")
	}
	output, err := g.run(ctx, args...)
	if err != nil {
		return ProviderHandle{}, fmt.Errorf("create GCP Compute instance: %w", err)
	}
	var created []gcpInstance
	if json.Unmarshal(output, &created) != nil || len(created) != 1 || created[0].Name != handle.ResourceID {
		return ProviderHandle{}, errors.New("GCP Compute create returned invalid identity")
	}
	return handle, nil
}
func (g GCPCompute) ObserveReplica(ctx context.Context, handle ProviderHandle, port int) (Observation, error) {
	if handle.ResourceID == "" {
		handle = g.Handle(handle.ExternalKey)
	}
	instance, err := g.describe(ctx, handle.ResourceID)
	if err != nil {
		return Observation{}, err
	}
	if instance.Name == "" {
		return Observation{State: "absent"}, nil
	}
	state := normalizeGCPState(instance.Status)
	endpoint := ""
	ip := gcpPrivateIP(instance)
	if state == "ready" && net.ParseIP(ip) != nil {
		if port == 0 {
			port = 8000
		}
		endpoint = fmt.Sprintf("http://%s:%d", ip, port)
	}
	detailFields := map[string]any{"project": g.Project, "zone": g.Zone, "instance": instance.Name, "network": "private", "identity": "attached-service-account", "cost_state": "unknown"}
	if state == "ready" {
		// Serial console evidence is optional and secret-safe for the same reason
		// as EC2 console evidence: retain only the closed InferCrane marker grammar.
		if serial, serialErr := g.run(ctx, "compute", "instances", "get-serial-port-output", instance.Name, "--project", g.Project, "--zone", g.Zone, "--port", "1", "--start", "0"); serialErr == nil {
			if evidence, ok := parseStartupEvidence(string(serial)); ok {
				detailFields["startup_evidence"] = evidence
			}
		}
	}
	details, _ := json.Marshal(detailFields)
	return Observation{Exists: true, State: state, Endpoint: endpoint, Details: string(details)}, nil
}
func (g GCPCompute) DeleteReplica(ctx context.Context, handle ProviderHandle) error {
	if handle.ResourceID == "" {
		handle = g.Handle(handle.ExternalKey)
	}
	instance, err := g.describe(ctx, handle.ResourceID)
	if err != nil || instance.Name == "" {
		return err
	}
	_, err = g.run(ctx, "compute", "instances", "delete", handle.ResourceID, "--project", g.Project, "--zone", g.Zone, "--quiet")
	if err != nil {
		return fmt.Errorf("delete GCP Compute instance: %w", err)
	}
	return nil
}
func (g GCPCompute) Inventory(ctx context.Context, filter InventoryFilter) ([]Resource, error) {
	output, err := g.run(ctx, "compute", "instances", "list", "--project", g.Project, "--filter", "labels.infercrane-managed=true", "--format", "json")
	if err != nil {
		return nil, err
	}
	var instances []gcpInstance
	if json.Unmarshal(output, &instances) != nil {
		return nil, errors.New("GCP Compute list returned invalid JSON")
	}
	out := make([]Resource, 0, len(instances))
	for _, instance := range instances {
		external := metadataValue(instance, "infercrane-external-key")
		if external == "" || (filter.Prefix != "" && !strings.HasPrefix(external, filter.Prefix)) {
			continue
		}
		state := normalizeGCPState(instance.Status)
		if state == "absent" {
			continue
		}
		endpoint := ""
		ip := gcpPrivateIP(instance)
		if state == "ready" && net.ParseIP(ip) != nil {
			endpoint = "http://" + ip + ":8000"
		}
		out = append(out, Resource{ID: instance.Name, ExternalKey: external, State: state, Endpoint: endpoint})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalKey < out[j].ExternalKey })
	return out, nil
}
func (g GCPCompute) validate(spec ReplicaSpec) error {
	if spec.ExternalKey == "" || spec.Model == "" || spec.Cloud != "gcp" || spec.Region == "" || !strings.HasPrefix(g.Zone, spec.Region+"-") {
		return errors.New("GCP replica requires external key, model, cloud gcp, and configured region/zone")
	}
	if g.Project == "" || g.Subnet == "" || g.MachineType == "" || g.GPUType == "" || g.ServiceAccount == "" || g.VMImage == "" || g.ContainerImage == "" || g.WorkerSecret == "" {
		return errors.New("GCP project, zone, subnet, machine type, GPU, service account, VM image, container digest, and worker secret are required")
	}
	if spec.GPU != g.GPUType {
		return fmt.Errorf("configured machine is qualified for GPU %s, not %s", g.GPUType, spec.GPU)
	}
	if spec.GPUCount < 0 || spec.GPUCount > 1024 {
		return errors.New("GCP GPU count must be between 1 and 1024")
	}
	if !strings.Contains(g.ContainerImage, "@sha256:") {
		return errors.New("GCP container image must be pinned by sha256 digest")
	}
	if g.BootDiskGiB < 50 || g.BootDiskGiB > 65536 {
		return errors.New("GCP boot disk must be between 50 and 65536 GiB")
	}
	if err := g.validateArtifactConfig(); err != nil {
		return err
	}
	if g.artifactCachePolicy() == "required" && !artifactCacheRuntimeSupported(spec) {
		return errors.New("GCP artifact cache is qualified only for vLLM and SGLang workloads")
	}
	if !spec.Workload.Empty() {
		if err := spec.Workload.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (g GCPCompute) validateArtifactConfig() error {
	policy := g.ArtifactCachePolicy
	if policy == "" {
		policy = "prefer"
	}
	if policy != "disabled" && policy != "prefer" && policy != "required" {
		return errors.New("GCP artifact cache policy must be disabled, prefer, or required")
	}
	if policy == "required" && len(g.ArtifactDisks) == 0 {
		return errors.New("required GCP artifact caching needs an immutable model-to-disk mapping")
	}
	if policy == "disabled" && len(g.ArtifactDisks) > 0 {
		return errors.New("disabled GCP artifact caching cannot configure persistent disks")
	}
	for identity, diskName := range g.ArtifactDisks {
		if !validArtifactModelIdentity(identity) || !validGCPResourceName(diskName) {
			return errors.New("GCP artifact disk mappings require immutable model identities and valid zonal disk names")
		}
	}
	return nil
}

func (g GCPCompute) artifactCachePolicy() string {
	if g.ArtifactCachePolicy == "disabled" || g.ArtifactCachePolicy == "required" {
		return g.ArtifactCachePolicy
	}
	return "prefer"
}

func (g GCPCompute) resolveArtifactDisk(ctx context.Context, spec ReplicaSpec) (gcpDisk, error) {
	if g.artifactCachePolicy() == "disabled" || !artifactCacheRuntimeSupported(spec) {
		return gcpDisk{}, nil
	}
	identity := modelIdentity(spec)
	diskName := g.ArtifactDisks[identity]
	if diskName == "" {
		if g.artifactCachePolicy() == "required" {
			return gcpDisk{}, fmt.Errorf("GCP artifact cache requires a verified persistent disk for immutable model %q", identity)
		}
		return gcpDisk{}, nil
	}
	return g.verifyArtifactDisk(ctx, identity, diskName)
}

func (g GCPCompute) verifyArtifactDisk(ctx context.Context, identity, diskName string) (gcpDisk, error) {
	if !validGCPResourceName(diskName) {
		return gcpDisk{}, errors.New("GCP artifact cache disk has an invalid name")
	}
	output, err := g.run(ctx, "compute", "disks", "describe", diskName, "--project", g.Project, "--zone", g.Zone, "--format", "json")
	if err != nil {
		return gcpDisk{}, fmt.Errorf("verify GCP artifact disk: %w", err)
	}
	var disk gcpDisk
	if json.Unmarshal(output, &disk) != nil || disk.Name != diskName {
		return gcpDisk{}, errors.New("GCP artifact disk verification returned invalid output")
	}
	expected := "infercrane-model-identity-digest=" + modelIdentityDigest(identity)
	if disk.Status != "READY" || disk.SizeGB == "" || !strings.HasSuffix(disk.Zone, "/zones/"+g.Zone) || disk.Description != expected {
		return gcpDisk{}, fmt.Errorf("GCP artifact disk %s is not ready, zonal, and bound to immutable model identity digest %s", diskName, modelIdentityDigest(identity))
	}
	return disk, nil
}

func validGCPResourceName(value string) bool {
	if value == "" || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

// Prefetch adopts a customer-prepared immutable zonal Persistent Disk. It
// never creates billable storage or copies model bytes implicitly.
func (g GCPCompute) Prefetch(ctx context.Context, request artifactcache.Request) (artifactcache.Operation, error) {
	if err := request.Validate(); err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_prefetch_request", err)
	}
	if g.artifactCachePolicy() == "disabled" {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_cache_disabled", errors.New("GCP artifact cache adapter is disabled"))
	}
	if request.Provider != "gcp" || request.Region != g.Zone {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_provider_boundary", errors.New("GCP artifact prefetch zone does not match the configured adapter"))
	}
	diskName, err := gcpDiskLocation(request.Location)
	if err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("invalid_cache_location", err)
	}
	if configured := g.ArtifactDisks[request.ModelIdentity]; configured == "" || configured != diskName {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_cache_not_configured", errors.New("GCP artifact disk is not configured for this immutable model identity"))
	}
	if _, err = g.verifyArtifactDisk(ctx, request.ModelIdentity, diskName); err != nil {
		return artifactcache.Operation{}, artifactcache.Definitive("artifact_disk_verification_failed", err)
	}
	return artifactcache.Operation{ProviderOperationID: diskName, Status: "succeeded"}, nil
}

// Observe emits short-lived evidence for the same verified disk boundary.
func (g GCPCompute) Observe(ctx context.Context, request artifactcache.Request) (artifactcache.Observation, error) {
	if err := request.Validate(); err != nil {
		return artifactcache.Observation{}, err
	}
	diskName, err := gcpDiskLocation(request.Location)
	if err != nil || g.artifactCachePolicy() == "disabled" || request.Provider != "gcp" || request.Region != g.Zone || g.ArtifactDisks[request.ModelIdentity] != diskName {
		return artifactcache.Observation{}, errors.New("GCP artifact cache request does not match the configured boundary")
	}
	disk, err := g.verifyArtifactDisk(ctx, request.ModelIdentity, diskName)
	if err != nil {
		return artifactcache.Observation{}, err
	}
	observed := time.Now().UTC()
	evidence, _ := json.Marshal(map[string]any{"disk": disk.Name, "zone": g.Zone, "size_gib": disk.SizeGB, "type": disk.Type, "model_identity_digest": modelIdentityDigest(request.ModelIdentity), "attachment": "read-only"})
	return artifactcache.Observation{State: "present", Source: "gcp-persistent-disk", EvidenceJSON: string(evidence), ObservedAt: observed, ExpiresAt: observed.Add(10 * time.Minute)}, nil
}

func gcpDiskLocation(location string) (string, error) {
	const prefix = "gcp-pd://"
	if !strings.HasPrefix(location, prefix) {
		return "", errors.New("GCP artifact cache location must use gcp-pd://DISK")
	}
	diskName := strings.TrimPrefix(location, prefix)
	if !validGCPResourceName(diskName) {
		return "", errors.New("GCP artifact cache location contains an invalid disk name")
	}
	return diskName, nil
}

func (g GCPCompute) describe(ctx context.Context, name string) (gcpInstance, error) {
	output, err := g.run(ctx, "compute", "instances", "describe", name, "--project", g.Project, "--zone", g.Zone, "--format", "json")
	if err != nil {
		if errors.Is(err, errGCPResourceNotFound) {
			return gcpInstance{}, nil
		}
		return gcpInstance{}, err
	}
	var instance gcpInstance
	if json.Unmarshal(output, &instance) != nil {
		return instance, errors.New("GCP Compute describe returned invalid JSON")
	}
	return instance, nil
}
func (g GCPCompute) run(ctx context.Context, args ...string) ([]byte, error) {
	runner := g.Runner
	if runner == nil {
		binary := g.Binary
		if binary == "" {
			binary = "gcloud"
		}
		runner = execRunner{binary: binary}
	}
	output, err := runner.Run(ctx, nil, args...)
	if err != nil {
		message := strings.ToLower(string(output) + " " + err.Error())
		if gcpNotFoundProbe(args) && (strings.Contains(message, "was not found") || strings.Contains(message, "could not fetch resource") || strings.Contains(message, "notfound")) {
			return nil, errGCPResourceNotFound
		}
		return nil, normalizeGCPAPIError(output)
	}
	return output, nil
}

func gcpNotFoundProbe(args []string) bool {
	return len(args) >= 3 && args[0] == "compute" && ((args[1] == "instances" && (args[2] == "describe" || args[2] == "delete")) || (args[1] == "disks" && args[2] == "describe"))
}

// normalizeGCPAPIError retains only the provider error class and bounded
// remediation. Raw gcloud output can contain project, principal, resource, or
// policy details and must not be persisted in operations or public evidence.
func normalizeGCPAPIError(output []byte) error {
	message := strings.ToLower(string(output))
	switch {
	case strings.Contains(message, "permission_denied"), strings.Contains(message, "permission denied"), strings.Contains(message, "forbidden"), strings.Contains(message, "required permission"), strings.Contains(message, "unauthenticated"):
		return fmt.Errorf("%w: verify the control identity and resource-scoped Compute permissions", ErrProviderAuthorization)
	case strings.Contains(message, "quota_exceeded"), strings.Contains(message, "quota exceeded"), strings.Contains(message, "quota '"), strings.Contains(message, "exceeds quota"):
		return fmt.Errorf("%w: GCP compute or accelerator quota is exhausted for the requested placement", ErrProviderQuota)
	case strings.Contains(message, "zone_resource_pool_exhausted"), strings.Contains(message, "resource pool exhausted"), strings.Contains(message, "does not have enough resources"), strings.Contains(message, "stockout"):
		return fmt.Errorf("%w: the requested GCP machine or accelerator is unavailable in the selected zone", ErrProviderCapacity)
	case strings.Contains(message, "rate limit"), strings.Contains(message, "too many requests"), strings.Contains(message, "http 429"), strings.Contains(message, "resource_exhausted"):
		return errors.New("GCP API request was rate limited")
	case strings.Contains(message, "invalid_argument"), strings.Contains(message, "invalid value"), strings.Contains(message, "invalid resource usage"):
		return fmt.Errorf("%w: GCP rejected the Compute launch configuration", ErrInvalidReplicaSpec)
	default:
		return errors.New("GCP API request failed")
	}
}
func (g GCPCompute) name(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "infercrane-" + hex.EncodeToString(sum[:10])
}
func (g GCPCompute) requestID(key string) string {
	sum := sha256.Sum256([]byte("gcp-compute\x00" + key))
	return hex.EncodeToString(sum[:16])
}
func (g GCPCompute) intentDigest(spec ReplicaSpec) string {
	port := spec.Port
	if !spec.Workload.Empty() {
		port = spec.Workload.Port
	}
	if port == 0 {
		port = 8000
	}
	canonical := strings.Join([]string{g.Project, g.Zone, g.Subnet, g.MachineType, g.GPUType, g.ServiceAccount, g.VMImage, fmt.Sprint(g.BootDiskGiB), g.startup(spec, port)}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
func (g GCPCompute) startup(spec ReplicaSpec, port int) string {
	image := g.ContainerImage
	// The vLLM OpenAI image entrypoint is `vllm serve`; the model is positional
	// in current releases.
	args := []string{spec.Model, "--port", fmt.Sprint(port)}
	if !spec.Workload.Empty() {
		image = spec.Workload.Image
		args = append(append([]string(nil), spec.Workload.Command...), spec.RuntimeArgs...)
	} else {
		if spec.ModelRevision != "" {
			args = append(args, "--revision", spec.ModelRevision)
		}
		args = append(args, spec.RuntimeArgs...)
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg == "${WORKER_API_KEY}" {
			quoted[i] = `"$worker_key"`
			continue
		}
		arg = strings.ReplaceAll(arg, "${MODEL}", spec.Model)
		arg = strings.ReplaceAll(arg, "${MODEL_REVISION}", spec.ModelRevision)
		arg = strings.ReplaceAll(arg, "${PORT}", fmt.Sprint(port))
		quoted[i] = shellQuote(arg)
	}
	entrypoint, arguments := "", " "+strings.Join(quoted, " ")
	if !spec.Workload.Empty() {
		entrypoint, arguments = workloadEntrypoint(quoted), workloadArguments(quoted)
	}
	gpuCount := spec.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	secretURL := "https://secretmanager.googleapis.com/v1/projects/" + url.PathEscape(g.Project) + "/secrets/" + url.PathEscape(g.WorkerSecret) + "/versions/latest:access"
	artifactBootstrap, artifactContainerArgs := gcpArtifactCacheBootstrap(g.ArtifactDisks[modelIdentity(spec)])
	return "#!/bin/sh\nset -eu\n" +
		"infercrane_stage() { printf 'infercrane_startup stage=%s at=%s\\n' \"$1\" \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\" >/dev/console; }\n" +
		"infercrane_stage identity_start\n" +
		"token_json=$(curl -fsS -H 'Metadata-Flavor: Google' 'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token')\n" +
		"access_token=$(printf '%s' \"$token_json\" | sed -n 's/.*\"access_token\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p')\n" +
		"[ -n \"$access_token\" ] || { echo 'workload identity token unavailable' >&2; exit 1; }\n" +
		"secret_json=$(curl -fsS -H \"Authorization: Bearer $access_token\" " + shellQuote(secretURL) + ")\n" +
		"secret_data=$(printf '%s' \"$secret_json\" | sed -n 's/.*\"data\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p')\n" +
		"[ -n \"$secret_data\" ] || { echo 'worker secret payload unavailable' >&2; exit 1; }\n" +
		"worker_key=$(printf '%s' \"$secret_data\" | base64 -d)\n" +
		"unset token_json access_token secret_json secret_data\n" +
		"infercrane_stage identity_ready\n" +
		"infercrane_stage gpu_driver_start\n" +
		"gpu_run_args='--gpus all'\n" +
		"if command -v cos-extensions >/dev/null 2>&1; then\n" +
		"  systemctl is-active --quiet docker.socket || systemctl start docker.socket\n" +
		"  systemctl is-active --quiet gcr-online.target || systemctl start gcr-online.target\n" +
		"  cos-extensions install gpu\n" +
		"  [ -e /dev/nvidia0 ] && [ -e /dev/nvidia-uvm ] && [ -e /dev/nvidiactl ]\n" +
		"  gpu_run_args='--volume /var/lib/nvidia/lib64:/usr/local/nvidia/lib64:ro --volume /var/lib/nvidia/bin:/usr/local/nvidia/bin:ro --device /dev/nvidia-uvm:/dev/nvidia-uvm --device /dev/nvidiactl:/dev/nvidiactl --env LD_LIBRARY_PATH=/usr/local/nvidia/lib64'\n" +
		"  device_index=0; while [ \"$device_index\" -lt " + fmt.Sprint(gpuCount) + " ]; do [ -e \"/dev/nvidia${device_index}\" ] || { echo 'allocated GPU count does not match immutable revision intent' >&2; exit 1; }; gpu_run_args=\"$gpu_run_args --device /dev/nvidia${device_index}:/dev/nvidia${device_index}\"; device_index=$((device_index + 1)); done\n" +
		"else\n" +
		"  actual_gpu_count=$(nvidia-smi --list-gpus | wc -l | tr -d ' ')\n" +
		"  [ \"$actual_gpu_count\" -eq " + fmt.Sprint(gpuCount) + " ] || { echo 'allocated GPU count does not match immutable revision intent' >&2; exit 1; }\n" +
		"fi\n" +
		"infercrane_stage gpu_driver_ready\n" +
		cachedImageBootstrap(image) + artifactBootstrap +
		"infercrane_stage runtime_start\n" +
		"docker run -d --restart=unless-stopped $gpu_run_args " + artifactContainerArgs + "-e INFERCRANE_WORKER_API_KEY=\"$worker_key\" -e VLLM_API_KEY=\"$worker_key\" -p " + fmt.Sprintf("%d:%d", port, port) + entrypoint + " " + shellQuote(image) + arguments + "\n" +
		"infercrane_stage runtime_container_started\n"
}

func workloadEntrypoint(quoted []string) string {
	if len(quoted) == 0 {
		return ""
	}
	return " --entrypoint " + quoted[0]
}

func workloadArguments(quoted []string) string {
	if len(quoted) < 2 {
		return ""
	}
	return " " + strings.Join(quoted[1:], " ")
}

func gcpArtifactCacheBootstrap(diskName string) (string, string) {
	if diskName == "" {
		return "infercrane_stage artifact_check\ninfercrane_stage artifact_cache_unconfigured\n", ""
	}
	bootstrap := "infercrane_stage artifact_check\n" +
		"artifact_device=/dev/disk/by-id/google-infercrane-model-cache\n" +
		"attempt=0\n" +
		"while [ ! -b \"$artifact_device\" ] && [ \"$attempt\" -lt 60 ]; do attempt=$((attempt + 1)); sleep 5; done\n" +
		"[ -b \"$artifact_device\" ] || { infercrane_stage artifact_cache_mount_failed; exit 1; }\n" +
		"mkdir -p /var/lib/infercrane/huggingface\n" +
		"mount -o ro,nosuid,nodev \"$artifact_device\" /var/lib/infercrane/huggingface || { infercrane_stage artifact_cache_mount_failed; exit 1; }\n" +
		"infercrane_stage artifact_cache_hit\n"
	containerArgs := "-e HF_HOME=/root/.cache/huggingface -e HF_HUB_OFFLINE=1 -e TRANSFORMERS_OFFLINE=1 -v /var/lib/infercrane/huggingface:/root/.cache/huggingface:ro "
	return bootstrap, containerArgs
}
func metadataValue(instance gcpInstance, key string) string {
	for _, item := range instance.Metadata.Items {
		if item.Key == key {
			return item.Value
		}
	}
	return ""
}
func gcpPrivateIP(instance gcpInstance) string {
	if len(instance.NetworkInterfaces) == 0 {
		return ""
	}
	return instance.NetworkInterfaces[0].NetworkIP
}
func normalizeGCPState(state string) string {
	switch strings.ToUpper(state) {
	case "PROVISIONING", "STAGING":
		return "provisioning"
	case "RUNNING":
		return "ready"
	case "STOPPING", "SUSPENDING", "REPAIRING":
		return "deleting"
	case "TERMINATED", "":
		return "absent"
	default:
		return "unknown"
	}
}

func gcpRegionFromZone(zone string) (string, bool) {
	last := strings.LastIndexByte(zone, '-')
	if last < 1 || last != len(zone)-2 || zone[last+1] < 'a' || zone[last+1] > 'z' {
		return "", false
	}
	return zone[:last], true
}

func gcpImageIdentity(value string) (string, string, bool) {
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[0] != "projects" || parts[1] == "" || parts[2] != "global" || parts[3] != "images" || parts[4] == "" || parts[4] == "family" {
		return "", "", false
	}
	return parts[1], parts[4], true
}
