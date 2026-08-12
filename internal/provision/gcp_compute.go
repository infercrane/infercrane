package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
)

var errGCPResourceNotFound = errors.New("GCP resource not found")

// GCPCompute owns one private Compute Engine VM for one replica intent. The
// resource name is derived from ExternalKey, so an uncertain insert response
// is adopted by observation instead of replayed as a second VM.
type GCPCompute struct {
	Binary                                                                                             string
	Runner                                                                                             CommandRunner
	Project, Zone, Subnet, MachineType, GPUType, ServiceAccount, VMImage, ContainerImage, WorkerSecret string
}

type gcpInstance struct {
	Name, Status      string
	NetworkInterfaces []struct {
		NetworkIP string `json:"networkIP"`
	} `json:"networkInterfaces"`
	Metadata struct{ Items []struct{ Key, Value string } }
}

func (g GCPCompute) Handle(externalKey string) ProviderHandle {
	return ProviderHandle{RequestID: g.requestID(externalKey), ResourceID: g.name(externalKey), ExternalKey: externalKey}
}
func (g GCPCompute) EnsureReplica(ctx context.Context, spec ReplicaSpec) (ProviderHandle, error) {
	if err := g.validate(spec); err != nil {
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
	args := []string{"compute", "instances", "create", handle.ResourceID, "--project", g.Project, "--zone", g.Zone, "--machine-type", g.MachineType, "--accelerator", "type=" + g.GPUType + ",count=1", "--subnet", g.Subnet, "--no-address", "--service-account", g.ServiceAccount, "--scopes", "cloud-platform", "--image", g.VMImage, "--maintenance-policy", "TERMINATE", "--metadata", metadata, "--labels", "infercrane-managed=true", "--format", "json", "--quiet"}
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
	details, _ := json.Marshal(map[string]any{"project": g.Project, "zone": g.Zone, "instance": instance.Name, "network": "private", "identity": "attached-service-account", "cost_state": "unknown"})
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
	if !strings.Contains(g.ContainerImage, "@sha256:") {
		return errors.New("GCP container image must be pinned by sha256 digest")
	}
	if !spec.Workload.Empty() {
		if err := spec.Workload.Validate(); err != nil {
			return err
		}
	}
	return nil
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
		if strings.Contains(message, "was not found") || strings.Contains(message, "could not fetch resource") {
			return nil, errGCPResourceNotFound
		}
		return nil, errors.New("GCP API request failed")
	}
	return output, nil
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
	canonical := strings.Join([]string{g.Project, g.Zone, g.Subnet, g.MachineType, g.GPUType, g.ServiceAccount, g.VMImage, g.startup(spec, port)}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
func (g GCPCompute) startup(spec ReplicaSpec, port int) string {
	image := g.ContainerImage
	args := []string{"--model", spec.Model, "--port", fmt.Sprint(port), "--api-key", "${WORKER_API_KEY}"}
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
	return "#!/bin/sh\nset -eu\nworker_key=$(gcloud secrets versions access latest --secret=" + shellQuote(g.WorkerSecret) + ")\ndocker pull " + shellQuote(image) + "\ndocker run -d --restart=unless-stopped --gpus all -e INFERCRANE_WORKER_API_KEY=\"$worker_key\" -p " + fmt.Sprintf("%d:%d", port, port) + " " + shellQuote(image) + " " + strings.Join(quoted, " ") + "\n"
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
