// Package modelimport pins model weights or a LoRA adapter in a customer's own
// S3 or GCS bucket by the digest of a file manifest, and says what the
// manifest and the configuration bytes it came with prove. InferCrane never
// reads the bucket: a worker downloads exactly the listed files with its own
// cloud identity and checks every sha256 before its server starts.
package modelimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelcheck"
)

// Kinds of import.
const (
	KindWeights = "weights"
	KindAdapter = "adapter"
)

// Bounds keep one manifest and its configuration small.
const (
	MaxFiles       = 4096
	MaxConfigBytes = 256 << 10
)

var (
	uriPattern  = regexp.MustCompile(`^(s3|gs)://([a-z0-9][a-z0-9._-]{1,61}[a-z0-9])(/[A-Za-z0-9._/=+-]*)?$`)
	pathPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestValue = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Request is one import: a location, its manifest, and optionally the bytes of
// config.json (weights) or adapter_config.json (adapter), so the import can
// check them against the manifest without reading the bucket.
type Request struct {
	Kind          string             `json:"kind"`
	URI           string             `json:"uri"`
	Files         []domain.ModelFile `json:"files"`
	Config        []byte             `json:"-"`
	AdapterConfig []byte             `json:"-"`
	BaseModel     string             `json:"-"`
}

// Source returns s3 or gs for a valid location.
func Source(uri string) (string, bool) {
	match := uriPattern.FindStringSubmatch(uri)
	if match == nil || strings.Contains(uri, "..") || strings.HasSuffix(uri, "//") {
		return "", false
	}
	return match[1], true
}

// NormalizeURI trims a trailing slash so one location has one spelling.
func NormalizeURI(uri string) string { return strings.TrimRight(strings.TrimSpace(uri), "/") }

// ValidDigest reports whether value is a sha256 manifest digest.
func ValidDigest(value string) bool { return digestValue.MatchString(value) }

func validPath(path string) bool {
	if len(path) > 512 || !pathPattern.MatchString(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Validate checks the manifest shape: plain relative paths listed once, sizes,
// and lowercase sha256 values.
func Validate(files []domain.ModelFile) error {
	if len(files) == 0 || len(files) > MaxFiles {
		return fmt.Errorf("a manifest lists 1 to %d files", MaxFiles)
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if !validPath(file.Path) {
			return fmt.Errorf("manifest path %q must be a relative path of letters, numbers, dots, underscores, dashes, and slashes", file.Path)
		}
		if seen[file.Path] {
			return fmt.Errorf("manifest lists %q twice", file.Path)
		}
		if file.Size < 0 || !sha256Hex.MatchString(file.SHA256) {
			return fmt.Errorf("manifest file %q needs a size and a lowercase sha256", file.Path)
		}
		seen[file.Path] = true
	}
	return nil
}

// Digest is the manifest's identity: sha256 over one "sha256  size  path"
// line per file, sorted by path. The same files give the same digest whatever
// order they were listed in.
func Digest(files []domain.ModelFile) string {
	sorted := append([]domain.ModelFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	hash := sha256.New()
	for _, file := range sorted {
		_, _ = io.WriteString(hash, file.SHA256+"  "+strconv.FormatInt(file.Size, 10)+"  "+file.Path+"\n")
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// Build validates a request and returns the import with its checks. A failed
// check means the files cannot serve; Build still returns the checks so the
// caller can show them.
func Build(request Request) (domain.ModelImport, *modelcheck.Report, error) {
	request.URI = NormalizeURI(request.URI)
	source, ok := Source(request.URI)
	if !ok {
		return domain.ModelImport{}, nil, errors.New("uri must be an s3:// or gs:// bucket path")
	}
	if request.Kind != KindWeights && request.Kind != KindAdapter {
		return domain.ModelImport{}, nil, errors.New("kind must be weights or adapter")
	}
	if err := Validate(request.Files); err != nil {
		return domain.ModelImport{}, nil, err
	}
	if len(request.Config) > MaxConfigBytes || len(request.AdapterConfig) > MaxConfigBytes {
		return domain.ModelImport{}, nil, fmt.Errorf("configuration files are limited to %d KiB", MaxConfigBytes>>10)
	}
	item := domain.ModelImport{Kind: request.Kind, Source: source, URI: request.URI, ManifestDigest: Digest(request.Files), Files: request.Files}
	byPath := make(map[string]domain.ModelFile, len(request.Files))
	for _, file := range request.Files {
		byPath[file.Path] = file
		item.Checks.TotalBytes += file.Size
	}
	checks := &item.Checks
	checks.Verified = append(checks.Verified, fmt.Sprintf("Manifest of %d files, %s, pinned as %s", len(request.Files), modelcheck.Bytes(checks.TotalBytes), item.ManifestDigest))
	checks.Unknown = append(checks.Unknown, "Whether the bucket holds these exact bytes: each worker checks every file's sha256 before its server starts")
	var report *modelcheck.Report
	if request.Kind == KindWeights {
		report = checkWeights(request, byPath, checks)
	} else {
		checkAdapter(request, byPath, checks)
	}
	return item, report, nil
}

// configMatches reports whether supplied bytes are the manifest's file.
func configMatches(content []byte, file domain.ModelFile) bool {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]) == file.SHA256 && int64(len(content)) == file.Size
}

func checkWeights(request Request, byPath map[string]domain.ModelFile, checks *domain.ModelImportCheck) *modelcheck.Report {
	model := modelcheck.Model{Repository: request.URI, Commit: Digest(request.Files), Files: modelcheck.Files{Listed: true}, ConfigState: modelcheck.ConfigNotFound}
	for path := range byPath {
		name := strings.ToLower(path)
		switch {
		case name == "config.json":
			model.Files.Config = true
		case modelcheck.TokenizerFile(name) && !strings.Contains(name, "/"):
			model.Files.Tokenizer = true
			checks.TokenizerFiles = append(checks.TokenizerFiles, path)
		case strings.HasSuffix(name, ".safetensors"):
			model.Files.Safetensors = true
		case strings.HasSuffix(name, ".gguf"):
			model.Files.GGUF = true
		case strings.HasSuffix(name, ".bin") || strings.HasSuffix(name, ".pt") || strings.HasSuffix(name, ".pth"):
			model.Files.PyTorch = true
		}
	}
	sort.Strings(checks.TokenizerFiles)
	configFile, hasConfig := byPath["config.json"]
	switch {
	case !hasConfig:
		checks.Failed = append(checks.Failed, "No config.json at the root of the location")
	case len(request.Config) == 0:
		checks.Unknown = append(checks.Unknown, "config.json contents were not provided, so the architecture is unknown")
	case !configMatches(request.Config, configFile):
		checks.Failed = append(checks.Failed, "The config.json provided does not match the manifest's sha256 and size")
	default:
		sum := sha256.Sum256(request.Config)
		checks.ConfigDigest = "sha256:" + hex.EncodeToString(sum[:])
		config, err := modelcheck.ParseConfig(request.Config)
		if err != nil {
			model.ConfigState = modelcheck.ConfigInvalid
			checks.Failed = append(checks.Failed, "config.json is not valid JSON")
			break
		}
		model.Config, model.ConfigState = &config, modelcheck.ConfigRead
		model.Architectures, model.ModelType = config.Architectures, config.ModelType
		if len(config.Architectures) > 0 {
			checks.Architecture = config.Architectures[0]
		}
		checks.ModelType = config.ModelType
		checks.Verified = append(checks.Verified, "config.json matches the manifest and was read")
	}
	if len(checks.TokenizerFiles) > 0 {
		checks.Verified = append(checks.Verified, "Tokenizer files listed: "+strings.Join(checks.TokenizerFiles, ", "))
	} else {
		checks.Failed = append(checks.Failed, "No tokenizer file (tokenizer.json, tokenizer.model, tokenizer_config.json, vocab.json, or tiktoken) at the root")
	}
	if model.Files.Safetensors {
		checks.Verified = append(checks.Verified, "Weights are safetensors files")
	} else {
		checks.Failed = append(checks.Failed, "No .safetensors weights; export the model with safetensors")
	}
	registries, err := modelcheck.Registries()
	if err != nil {
		return nil
	}
	report := modelcheck.Evaluate(model, modelcheck.EvaluateOptions{Registries: registries})
	report.Source, report.SourceURL = sourceName(request.URI), request.URI
	// The bucket is the customer's own: nothing about it is public.
	for index, check := range report.Checks {
		if check.Name == "access" {
			report.Checks[index] = modelcheck.Check{Name: "access", State: modelcheck.Info, Label: "Your bucket", Detail: "Workers read it with their own cloud identity; InferCrane never reads the bucket."}
		}
	}
	report.Access = modelcheck.Access{State: "customer_bucket"}
	return &report
}

func sourceName(uri string) string {
	if strings.HasPrefix(uri, "gs://") {
		return "gcs"
	}
	return "s3"
}

func checkAdapter(request Request, byPath map[string]domain.ModelFile, checks *domain.ModelImportCheck) {
	configFile, hasConfig := byPath["adapter_config.json"]
	weights := false
	for path := range byPath {
		if path == "adapter_model.safetensors" || path == "adapter_model.bin" {
			weights = true
		}
	}
	if !weights {
		checks.Failed = append(checks.Failed, "No adapter_model.safetensors at the root of the location")
	} else {
		checks.Verified = append(checks.Verified, "Adapter weights listed")
	}
	switch {
	case !hasConfig:
		checks.Failed = append(checks.Failed, "No adapter_config.json at the root of the location")
		return
	case len(request.AdapterConfig) == 0:
		checks.Unknown = append(checks.Unknown, "adapter_config.json contents were not provided, so the rank and base model are unknown")
		return
	case !configMatches(request.AdapterConfig, configFile):
		checks.Failed = append(checks.Failed, "The adapter_config.json provided does not match the manifest's sha256 and size")
		return
	}
	sum := sha256.Sum256(request.AdapterConfig)
	checks.ConfigDigest = "sha256:" + hex.EncodeToString(sum[:])
	adapter, err := ParseAdapterConfig(request.AdapterConfig)
	if err != nil {
		checks.Failed = append(checks.Failed, err.Error())
		return
	}
	checks.AdapterRank, checks.AdapterBase = adapter.Rank, adapter.BaseModel
	checks.Verified = append(checks.Verified, fmt.Sprintf("adapter_config.json matches the manifest: LoRA rank %d", adapter.Rank))
	checks.Verified, checks.Unknown, checks.Failed = baseModelCheck(adapter.BaseModel, request.BaseModel, checks.Verified, checks.Unknown, checks.Failed)
}

// AdapterConfig is what a PEFT adapter_config.json says about serving.
type AdapterConfig struct {
	Rank          int      `json:"rank"`
	BaseModel     string   `json:"base_model"`
	TargetModules []string `json:"target_modules,omitempty"`
}

// ParseAdapterConfig reads a PEFT LoRA adapter_config.json.
func ParseAdapterConfig(raw []byte) (AdapterConfig, error) {
	var config struct {
		PEFTType      string          `json:"peft_type"`
		Rank          int             `json:"r"`
		BaseModel     string          `json:"base_model_name_or_path"`
		TargetModules json.RawMessage `json:"target_modules"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return AdapterConfig{}, errors.New("adapter_config.json is not valid JSON")
	}
	if config.PEFTType != "" && !strings.EqualFold(config.PEFTType, "LORA") {
		return AdapterConfig{}, fmt.Errorf("adapter_config.json declares %s, not a LoRA adapter", config.PEFTType)
	}
	if config.Rank < 1 || config.Rank > 512 {
		return AdapterConfig{}, errors.New("adapter_config.json needs a LoRA rank r between 1 and 512")
	}
	out := AdapterConfig{Rank: config.Rank, BaseModel: strings.TrimSpace(config.BaseModel)}
	var modules []string
	if json.Unmarshal(config.TargetModules, &modules) == nil {
		out.TargetModules = modules
	}
	return out, nil
}

// baseModelCheck compares the adapter's declared base model with the model it
// will be served on. A local training path cannot be compared and stays
// unknown.
func baseModelCheck(declared, requested string, verified, unknown, failed []string) ([]string, []string, []string) {
	switch {
	case requested == "":
		return verified, unknown, failed
	case declared == "":
		return verified, append(unknown, "adapter_config.json names no base model"), failed
	case strings.EqualFold(declared, requested):
		return append(verified, "Trained on "+requested), unknown, failed
	case strings.HasPrefix(declared, "/") || strings.HasPrefix(declared, "."):
		return verified, append(unknown, "adapter_config.json names a local training path ("+declared+"), so its base model cannot be compared with "+requested), failed
	}
	return verified, append(unknown, "adapter_config.json names "+declared+" as its base model, not "+requested), failed
}

// BaseModelCheck is baseModelCheck for Hugging Face adapters.
func BaseModelCheck(declared, requested string) domain.ModelImportCheck {
	var check domain.ModelImportCheck
	check.Verified, check.Unknown, check.Failed = baseModelCheck(declared, requested, nil, nil, nil)
	return check
}

// FromDirectory hashes every regular file under dir into a manifest, skipping
// hidden files and directories. It never follows a symbolic link out of dir.
func FromDirectory(dir string) ([]domain.ModelFile, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var files []domain.ModelFile
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if path != root && strings.HasPrefix(name, ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if !validPath(relative) {
			return fmt.Errorf("file %q has a name the manifest cannot carry", relative)
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer file.Close()
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		if copyErr != nil {
			return copyErr
		}
		files = append(files, domain.ModelFile{Path: relative, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))})
		if len(files) > MaxFiles {
			return fmt.Errorf("more than %d files", MaxFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

var (
	adapterName   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	hubRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	hubCommit     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// AdapterSource is huggingface, s3, or gs for an adapter's repository.
func AdapterSource(adapter domain.ModelAdapter) string {
	switch {
	case strings.HasPrefix(adapter.Repository, "s3://"):
		return "s3"
	case strings.HasPrefix(adapter.Repository, "gs://"):
		return "gs"
	}
	return "huggingface"
}

// IsObjectModel reports whether a deployment model names an object store
// location instead of a Hugging Face repository.
func IsObjectModel(model string) bool {
	return strings.HasPrefix(model, "s3://") || strings.HasPrefix(model, "gs://")
}

// ValidateDeployment checks a deployment's model sources before anything is
// stored: object store weights pinned to a manifest digest, and at most one
// LoRA adapter pinned to a commit or digest, rendered only for vLLM and SGLang.
func ValidateDeployment(model, revision, runtime, computeMode string, adapters []domain.ModelAdapter, runtimeArgs []string) error {
	if IsObjectModel(model) {
		if _, ok := Source(model); !ok || NormalizeURI(model) != model {
			return errors.New("model must be an s3:// or gs:// bucket path without a trailing slash")
		}
		if !ValidDigest(revision) {
			return errors.New("object store weights need model_revision set to the sha256 manifest digest from their import")
		}
		if computeMode == "serverless" {
			return errors.New("object store weights need elastic compute")
		}
	}
	if len(adapters) == 0 {
		return nil
	}
	if len(adapters) > 1 {
		return errors.New("one endpoint serves one LoRA adapter; deploy another endpoint for another adapter")
	}
	if runtime != "" && runtime != "vllm" && runtime != "sglang" {
		return errors.New("LoRA adapters are served by the vllm and sglang runtimes")
	}
	if computeMode == "serverless" {
		return errors.New("LoRA adapters need elastic compute")
	}
	for _, arg := range runtimeArgs {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--enable-lora", "--no-enable-lora", "--lora-modules", "--max-loras", "--max-lora-rank", "--max-cpu-loras", "--lora-paths", "--max-loras-per-batch", "--max-loaded-loras":
			return fmt.Errorf("%s comes from model_adapters, not runtime_args", name)
		}
	}
	for _, adapter := range adapters {
		if !adapterName.MatchString(adapter.Name) {
			return fmt.Errorf("adapter name %q must be 1 to 64 lowercase letters, numbers, dots, underscores, or dashes", adapter.Name)
		}
		if adapter.Rank < 0 || adapter.Rank > 512 {
			return fmt.Errorf("adapter %q rank must be between 1 and 512", adapter.Name)
		}
		switch AdapterSource(adapter) {
		case "huggingface":
			if !hubRepository.MatchString(adapter.Repository) || !hubCommit.MatchString(adapter.Revision) {
				return fmt.Errorf("adapter %q needs a Hugging Face OWNER/NAME and its 40-character commit as revision", adapter.Name)
			}
		default:
			if _, ok := Source(adapter.Repository); !ok || NormalizeURI(adapter.Repository) != adapter.Repository || !ValidDigest(adapter.Revision) {
				return fmt.Errorf("adapter %q needs an s3:// or gs:// location and the sha256 manifest digest from its import as revision", adapter.Name)
			}
		}
	}
	return nil
}
