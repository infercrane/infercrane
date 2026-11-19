package provision

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secrets"
)

// ModelCredential tells a worker where to read the token for a private or
// gated Hugging Face model. Adapters deliver it natively: a RunPod secret in
// the account that runs the Pod, a Kubernetes secretKeyRef, or a Secrets
// Manager ARN the EC2 instance profile reads. The value reaches the runtime
// only as HF_TOKEN, which huggingface_hub sends to Hugging Face alone.
type ModelCredential struct {
	ReferenceID string
	Resolver    string
	Reference   string `json:",omitempty"`
	// Open returns a sealed value in memory. Only an adapter that stores the
	// value in the provider account's own secret store calls it.
	Open func(context.Context) (string, error) `json:"-"`
}

// ObjectFile is one file of a model or adapter in an object store, pinned by
// its sha256.
type ObjectFile struct {
	Path   string
	Size   int64
	SHA256 string
}

// ObjectSource is model weights or a LoRA adapter in the customer's own S3 or
// GCS bucket, pinned by the digest of its file manifest. The worker downloads
// every file with its own cloud identity and checks its sha256 before the
// server starts.
type ObjectSource struct {
	URI    string
	Digest string
	Files  []ObjectFile
}

// Adapter is one LoRA adapter served on the base model, pinned by a Hugging
// Face commit or an object manifest digest.
type Adapter struct {
	Name       string
	Source     string
	Repository string        `json:",omitempty"`
	Revision   string        `json:",omitempty"`
	Rank       int           `json:",omitempty"`
	Objects    *ObjectSource `json:",omitempty"`
}

// Paths inside a worker. Fetched files live on a volume of their own so they
// never mix with a provider's model cache.
const (
	fetchRoot          = "/infercrane"
	fetchedModelDir    = fetchRoot + "/model"
	fetchedAdaptersDir = fetchRoot + "/adapters"
)

var (
	objectURIPattern   = regexp.MustCompile(`^(s3|gs)://([a-z0-9][a-z0-9._-]{1,61}[a-z0-9])(/[A-Za-z0-9._/=+-]*)?$`)
	objectPathPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	sha256Hex          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	manifestDigest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	adapterNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	hubRepository      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
)

// vLLM 0.22.1 accepts only these LoRA ranks (vllm/config/lora.py MaxLoRARanks).
var vllmLoRARanks = []int{1, 8, 16, 32, 64, 128, 256, 320, 512}

// SplitObjectURI returns the scheme, bucket, and prefix of an object store
// URI, or false when it is not a plain s3:// or gs:// location.
func SplitObjectURI(uri string) (scheme, bucket, prefix string, ok bool) {
	match := objectURIPattern.FindStringSubmatch(uri)
	if match == nil || strings.Contains(uri, "//"+match[2]+"//") || strings.Contains(match[3], "..") {
		return "", "", "", false
	}
	return match[1], match[2], strings.Trim(match[3], "/"), true
}

// ValidObjectPath reports whether a manifest path is a plain relative path.
func ValidObjectPath(path string) bool {
	if len(path) > 512 || !objectPathPattern.MatchString(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Validate checks an object source before anything is provisioned.
func (o ObjectSource) Validate() error {
	if _, _, _, ok := SplitObjectURI(o.URI); !ok {
		return fmt.Errorf("%w: object store location %q is not an s3:// or gs:// bucket path", ErrInvalidReplicaSpec, o.URI)
	}
	if !manifestDigest.MatchString(o.Digest) {
		return fmt.Errorf("%w: object store files must be pinned by a sha256 manifest digest", ErrInvalidReplicaSpec)
	}
	if len(o.Files) == 0 || len(o.Files) > 4096 {
		return fmt.Errorf("%w: an object manifest lists 1 to 4096 files", ErrInvalidReplicaSpec)
	}
	seen := make(map[string]bool, len(o.Files))
	for _, file := range o.Files {
		if !ValidObjectPath(file.Path) || !sha256Hex.MatchString(file.SHA256) || file.Size < 0 || seen[file.Path] {
			return fmt.Errorf("%w: object manifest file %q is invalid", ErrInvalidReplicaSpec, file.Path)
		}
		seen[file.Path] = true
	}
	return nil
}

func (a Adapter) validate() error {
	if !adapterNamePattern.MatchString(a.Name) {
		return fmt.Errorf("%w: adapter name %q must be 1 to 64 lowercase letters, numbers, dots, underscores, or dashes", ErrInvalidReplicaSpec, a.Name)
	}
	switch a.Source {
	case "huggingface":
		if !hubRepository.MatchString(a.Repository) || !commitPattern.MatchString(a.Revision) || a.Objects != nil {
			return fmt.Errorf("%w: Hugging Face adapter %q needs OWNER/NAME pinned to a 40-character commit", ErrInvalidReplicaSpec, a.Name)
		}
	case "s3", "gs":
		if a.Objects == nil || !strings.HasPrefix(a.Objects.URI, a.Source+"://") || a.Objects.Digest != a.Revision {
			return fmt.Errorf("%w: object store adapter %q needs its imported manifest", ErrInvalidReplicaSpec, a.Name)
		}
		if err := a.Objects.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: adapter %q source must be huggingface, s3, or gs", ErrInvalidReplicaSpec, a.Name)
	}
	if a.Rank < 0 || a.Rank > 512 {
		return fmt.Errorf("%w: adapter %q rank must be between 1 and 512", ErrInvalidReplicaSpec, a.Name)
	}
	return nil
}

// ModelInputs reports what a worker must fetch or be given beyond a public
// Hugging Face model.
type ModelInputs struct {
	Credential bool
	Objects    bool
	Adapters   bool
}

func modelInputs(spec ReplicaSpec) ModelInputs {
	inputs := ModelInputs{Credential: spec.ModelCredential != nil, Objects: spec.ModelObjects != nil, Adapters: len(spec.Adapters) > 0}
	for _, adapter := range spec.Adapters {
		if adapter.Objects != nil {
			inputs.Objects = true
		}
	}
	return inputs
}

// validateModelInputs checks the model inputs every adapter shares.
func validateModelInputs(spec ReplicaSpec) error {
	if credential := spec.ModelCredential; credential != nil {
		if credential.ReferenceID == "" {
			return fmt.Errorf("%w: model credential reference is required", ErrInvalidReplicaSpec)
		}
		if secrets.WorkerHeld(credential.Resolver) {
			if err := secrets.ValidateReference(credentialReference(credential)); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
			}
		}
	}
	if spec.ModelObjects != nil {
		if err := spec.ModelObjects.Validate(); err != nil {
			return err
		}
		if !strings.HasPrefix(spec.Model, "s3://") && !strings.HasPrefix(spec.Model, "gs://") || spec.ModelObjects.URI != spec.Model {
			return fmt.Errorf("%w: object store weights must be the replica's model", ErrInvalidReplicaSpec)
		}
	}
	if len(spec.Adapters) == 0 {
		return nil
	}
	// The gateway sends every request to one upstream model name, so one
	// endpoint serves one adapter.
	if len(spec.Adapters) > 1 {
		return fmt.Errorf("%w: one endpoint serves one LoRA adapter; deploy another endpoint for another adapter", ErrInvalidReplicaSpec)
	}
	if spec.Runtime != "vllm" && spec.Runtime != "sglang" {
		return fmt.Errorf("%w: LoRA adapters are rendered for the vllm and sglang runtimes only", ErrInvalidReplicaSpec)
	}
	for _, adapter := range spec.Adapters {
		if err := adapter.validate(); err != nil {
			return err
		}
	}
	for _, arg := range spec.RuntimeArgs {
		if lora, _ := loraFlag(arg); lora {
			return fmt.Errorf("%w: LoRA flags come from model adapters, not runtime args (%s)", ErrInvalidReplicaSpec, arg)
		}
	}
	return nil
}

func credentialReference(credential *ModelCredential) domain.SecretReference {
	return domain.SecretReference{Resolver: credential.Resolver, Reference: credential.Reference}
}

// refuseModelInputs fails closed for an adapter that cannot deliver one of
// the requested inputs, before anything is provisioned.
func refuseModelInputs(adapter string, spec ReplicaSpec, supported ModelInputs, credentialResolvers ...string) error {
	requested := modelInputs(spec)
	if requested.Credential && (!supported.Credential || !containsString(credentialResolvers, spec.ModelCredential.Resolver)) {
		if supported.Credential {
			return fmt.Errorf("%w: %s delivers a model credential from a %s secret, not %s", ErrInvalidReplicaSpec, adapter, strings.Join(credentialResolvers, " or "), spec.ModelCredential.Resolver)
		}
		return fmt.Errorf("%w: %s cannot give a worker a private model credential; use runpod-pods, kubernetes, or aws-ec2", ErrInvalidReplicaSpec, adapter)
	}
	if requested.Objects && !supported.Objects {
		return fmt.Errorf("%w: %s workers have no cloud identity to read an object store; use kubernetes or aws-ec2, or push the files to a private Hugging Face repository", ErrInvalidReplicaSpec, adapter)
	}
	if requested.Adapters && !supported.Adapters {
		return fmt.Errorf("%w: %s does not serve LoRA adapters; use runpod-pods, kubernetes, or aws-ec2", ErrInvalidReplicaSpec, adapter)
	}
	return validateModelInputs(spec)
}

// loraFlag reports whether a runtime argument is a LoRA flag that only model
// adapters may set.
func loraFlag(arg string) (bool, string) {
	name, _, _ := strings.Cut(arg, "=")
	switch name {
	case "--enable-lora", "--no-enable-lora", "--lora-modules", "--max-loras", "--max-lora-rank", "--max-cpu-loras", "--lora-paths", "--max-loras-per-batch", "--max-loaded-loras":
		return true, name
	}
	return false, ""
}

// adapterArgs renders the reviewed LoRA flags of vLLM 0.22.1 and SGLang 0.5.12
// for adapters already fetched under fetchedAdaptersDir.
func adapterArgs(runtime string, adapters []Adapter) []string {
	if len(adapters) == 0 {
		return nil
	}
	rank := 0
	modules := make([]string, 0, len(adapters))
	for _, adapter := range adapters {
		modules = append(modules, adapter.Name+"="+adapterDir(adapter))
		if adapter.Rank > rank {
			rank = adapter.Rank
		}
	}
	count := strconv.Itoa(len(adapters))
	switch runtime {
	case "vllm":
		args := []string{"--enable-lora", "--max-loras", count}
		if rank > 0 {
			args = append(args, "--max-lora-rank", strconv.Itoa(vllmRank(rank)))
		}
		return append(append(args, "--lora-modules"), modules...)
	case "sglang":
		args := []string{"--enable-lora", "--max-loras-per-batch", count}
		if rank > 0 {
			args = append(args, "--max-lora-rank", strconv.Itoa(rank))
		}
		return append(append(args, "--lora-paths"), modules...)
	}
	return nil
}

// vllmRank rounds an adapter rank up to the nearest rank vLLM accepts.
func vllmRank(rank int) int {
	for _, allowed := range vllmLoRARanks {
		if rank <= allowed {
			return allowed
		}
	}
	return vllmLoRARanks[len(vllmLoRARanks)-1]
}

func adapterDir(adapter Adapter) string { return fetchedAdaptersDir + "/" + adapter.Name }

// servedCommand rewrites an engine command for the replica's model inputs:
// object store weights are served from their local copy under the requested
// model name, and LoRA flags are appended. Placeholders are expanded by the
// adapter afterwards.
func servedCommand(command []string, spec ReplicaSpec) []string {
	out := append([]string(nil), command...)
	if spec.ModelObjects != nil {
		out = localModelCommand(out)
	}
	return append(out, adapterArgs(spec.Runtime, spec.Adapters)...)
}

// localModelCommand serves the fetched copy instead of a hub repository: the
// first ${MODEL} becomes the local path, the revision pair is dropped (the
// manifest digest already pinned the files), and the server keeps answering to
// the requested model name.
func localModelCommand(command []string) []string {
	out := make([]string, 0, len(command)+2)
	replacedPath, named := false, false
	for index := 0; index < len(command); index++ {
		arg := command[index]
		if arg == "--revision" && index+1 < len(command) && command[index+1] == "${MODEL_REVISION}" {
			index++
			continue
		}
		if arg == "--served-model-name" {
			named = true
		}
		if arg == "${MODEL}" && !replacedPath && !(index > 0 && command[index-1] == "--served-model-name") {
			arg, replacedPath = fetchedModelDir, true
		}
		out = append(out, arg)
	}
	if !named {
		out = append(out, "--served-model-name", "${MODEL}")
	}
	return out
}

// fetchScript is the POSIX shell a worker runs before its server starts. It
// downloads object store files with the worker's own cloud identity (aws or
// gcloud on PATH) and checks every sha256, then fetches Hugging Face adapters
// at their pinned commit. Any failure stops the replica before it serves.
func fetchScript(spec ReplicaSpec, objects, hub bool) string {
	var script strings.Builder
	script.WriteString("set -eu\numask 022\n")
	if objects {
		if spec.ModelObjects != nil {
			writeObjectFetch(&script, *spec.ModelObjects, fetchedModelDir)
		}
		for _, adapter := range spec.Adapters {
			if adapter.Objects != nil {
				writeObjectFetch(&script, *adapter.Objects, adapterDir(adapter))
			}
		}
	}
	if hub {
		for _, adapter := range spec.Adapters {
			if adapter.Source != "huggingface" {
				continue
			}
			script.WriteString("python3 -c 'import sys; from huggingface_hub import snapshot_download; snapshot_download(repo_id=sys.argv[1], revision=sys.argv[2], local_dir=sys.argv[3])' ")
			script.WriteString(shellQuote(adapter.Repository) + " " + shellQuote(adapter.Revision) + " " + shellQuote(adapterDir(adapter)) + "\n")
		}
	}
	return script.String()
}

func writeObjectFetch(script *strings.Builder, source ObjectSource, dir string) {
	scheme, _, _, _ := SplitObjectURI(source.URI)
	base := strings.TrimRight(source.URI, "/")
	files := append([]ObjectFile(nil), source.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	script.WriteString("mkdir -p " + shellQuote(dir) + "\n")
	for _, file := range files {
		target := dir + "/" + file.Path
		if strings.Contains(file.Path, "/") {
			script.WriteString("mkdir -p " + shellQuote(target[:strings.LastIndex(target, "/")]) + "\n")
		}
		if scheme == "gs" {
			script.WriteString("gcloud storage cp --quiet " + shellQuote(base+"/"+file.Path) + " " + shellQuote(target) + "\n")
		} else {
			script.WriteString("aws s3 cp --only-show-errors " + shellQuote(base+"/"+file.Path) + " " + shellQuote(target) + "\n")
		}
	}
	script.WriteString("(cd " + shellQuote(dir) + " && sha256sum -c --strict --quiet) <<'INFERCRANE_MANIFEST'\n")
	for _, file := range files {
		script.WriteString(file.SHA256 + "  " + file.Path + "\n")
	}
	script.WriteString("INFERCRANE_MANIFEST\n")
}

// hubFetchNeeded reports whether the runtime container must fetch Hugging
// Face adapters before it starts the server.
func hubFetchNeeded(spec ReplicaSpec) bool {
	for _, adapter := range spec.Adapters {
		if adapter.Source == "huggingface" {
			return true
		}
	}
	return false
}

// wrapWithFetch runs a fetch script in the runtime container and then execs
// the server, so the server's own argv is unchanged after the fetch.
func wrapWithFetch(command []string, script string) []string {
	return append([]string{"/bin/sh", "-c", script + "exec \"$@\"\n", "infercrane-serve"}, command...)
}
