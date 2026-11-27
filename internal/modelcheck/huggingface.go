package modelcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultEndpoint is the public Hugging Face Hub.
	DefaultEndpoint = "https://huggingface.co"

	defaultTimeout    = 8 * time.Second
	defaultRefTTL     = 10 * time.Minute
	defaultMaxEntries = 512
	defaultConcurrent = 8
	maxInfoBytes      = 4 << 20
	maxConfigBytes    = 2 << 20
	maxRedirects      = 3
	userAgent         = "infercrane-model-check"
)

// Error codes are stable API values. Messages are safe to show and never
// contain upstream bodies or credentials.
const (
	CodeInvalidModel     = "invalid_model"
	CodeModelNotFound    = "model_not_found"
	CodeRevisionNotFound = "revision_not_found"
	CodeTokenRejected    = "hugging_face_token_rejected"
	CodeUnavailable      = "hugging_face_unavailable"
)

// Error reports why a model could not be read.
type Error struct {
	Code    string
	Message string
	Timeout bool
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Config read states.
const (
	ConfigRead          = "read"
	ConfigTokenRequired = "token_required"
	ConfigNotFound      = "not_found"
	ConfigInvalid       = "invalid"
)

// Model is what Hugging Face reports for one repository at one commit.
// Empty strings and nil pointers mean Hugging Face did not report the value.
type Model struct {
	Repository        string
	RequestedRevision string
	Commit            string
	PipelineTag       string
	LibraryName       string
	License           string
	Gated             string
	Private           bool
	Disabled          bool
	Architectures     []string
	ModelType         string
	Files             Files
	Weights           *Weights
	Config            *Config
	ConfigState       string
	// TokenizerDigest is the sha256 of the files a server reads text with
	// (tokenizerDigest), or empty when Hugging Face listed none or did not
	// report a hash for one of them.
	TokenizerDigest string
	TokenUsed       bool
	ReadAt          time.Time
	Cached          bool
}

// Files says which weight formats the repository holds at the commit.
type Files struct {
	Listed      bool
	Config      bool
	Safetensors bool
	GGUF        bool
	PyTorch     bool
	// Tokenizer is true when a tokenizer file the runtimes load is listed.
	Tokenizer bool
}

// TokenizerFile reports whether a repository file is one of the tokenizer
// files vLLM and SGLang load through transformers or mistral_common.
func TokenizerFile(name string) bool {
	name = strings.ToLower(name)
	switch name {
	case "tokenizer.json", "tokenizer.model", "tokenizer_config.json", "vocab.json", "tekken.json":
		return true
	}
	return strings.HasSuffix(name, ".tiktoken")
}

// Weights is Hugging Face's safetensors summary: stored parameter counts by
// tensor dtype. Packed quantized weights count as their storage dtype.
type Weights struct {
	Parameters int64
	ByDType    map[string]int64
}

// Request names one model to read. A token is used only for this read: it is
// never cached, logged, or returned. Scope keys the cache for reads made with
// a token, so one tenant's token never serves another tenant's request.
type Request struct {
	Repository string
	Revision   string
	Token      string
	Scope      string
}

// ResolverOptions configures a Resolver. Zero values select the defaults.
type ResolverOptions struct {
	Endpoint   string
	Client     *http.Client
	Timeout    time.Duration
	RefTTL     time.Duration
	MaxEntries int
	Concurrent int
	Now        func() time.Time
}

// Resolver reads model metadata from the Hub with a deadline, a bound on
// concurrent upstream requests, and a cache keyed by immutable commit.
type Resolver struct {
	endpoint   string
	host       string
	client     *http.Client
	timeout    time.Duration
	refTTL     time.Duration
	maxEntries int
	now        func() time.Time
	slots      chan struct{}

	mu      sync.Mutex
	commits map[string]Model
	order   []string
	refs    map[string]refEntry
}

type refEntry struct {
	commit  string
	expires time.Time
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
)

// ValidRepository reports whether value is an exact owner/name repository.
func ValidRepository(value string) bool {
	return repositoryPattern.MatchString(value) && !strings.Contains(value, "--") && !strings.Contains(value, "..")
}

// ValidRevision reports whether value is a branch, tag, or commit name the
// resolver will send to Hugging Face.
func ValidRevision(value string) bool {
	return revisionPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.HasSuffix(value, "/")
}

// New returns a Resolver. The endpoint must be an absolute http(s) URL.
func New(options ResolverOptions) (*Resolver, error) {
	endpoint := strings.TrimRight(options.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("hugging face endpoint must be an absolute http(s) URL without a query, got %q", options.Endpoint)
	}
	client := &http.Client{}
	if options.Client != nil {
		copied := *options.Client
		client = &copied
	}
	host := parsed.Host
	// A renamed repository answers with a redirect on the same host. Any
	// other redirect would send the request, and a token, somewhere else.
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects || request.URL.Host != host || request.URL.Scheme != parsed.Scheme {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resolver := &Resolver{
		endpoint: endpoint, host: host, client: client,
		timeout: options.Timeout, refTTL: options.RefTTL, maxEntries: options.MaxEntries, now: options.Now,
		commits: map[string]Model{}, refs: map[string]refEntry{},
	}
	if resolver.timeout <= 0 {
		resolver.timeout = defaultTimeout
	}
	if resolver.refTTL <= 0 {
		resolver.refTTL = defaultRefTTL
	}
	if resolver.maxEntries <= 0 {
		resolver.maxEntries = defaultMaxEntries
	}
	if resolver.now == nil {
		resolver.now = time.Now
	}
	concurrent := options.Concurrent
	if concurrent <= 0 {
		concurrent = defaultConcurrent
	}
	resolver.slots = make(chan struct{}, concurrent)
	return resolver, nil
}

// Resolve pins the request's revision to a commit and reads the model's
// metadata, config.json, and safetensors summary at that commit.
func (r *Resolver) Resolve(ctx context.Context, request Request) (Model, error) {
	repository := strings.TrimSpace(request.Repository)
	revision := strings.TrimSpace(request.Revision)
	if revision == "" {
		revision = "main"
	}
	if !ValidRepository(repository) {
		return Model{}, &Error{Code: CodeInvalidModel, Message: "model must be an exact Hugging Face owner/name repository"}
	}
	if !ValidRevision(revision) {
		return Model{}, &Error{Code: CodeInvalidModel, Message: "revision must be a branch, tag, or commit name"}
	}
	scope := "public"
	if request.Token != "" {
		if strings.TrimSpace(request.Scope) == "" {
			return Model{}, errors.New("a token read needs a cache scope")
		}
		scope = "token\x00" + request.Scope
	}
	refKey := scope + "\x00" + strings.ToLower(repository) + "\x00" + revision
	commit := ""
	if gitCommit.MatchString(revision) {
		commit = revision
	} else {
		r.mu.Lock()
		if entry, ok := r.refs[refKey]; ok && r.now().Before(entry.expires) {
			commit = entry.commit
		}
		r.mu.Unlock()
	}
	if commit != "" {
		if model, ok := r.cached(scope, repository, commit); ok {
			model.RequestedRevision = revision
			return model, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	model, err := r.read(ctx, repository, revision, request.Token)
	if err != nil {
		return Model{}, err
	}
	model.RequestedRevision = revision
	r.store(scope, refKey, model)
	return model, nil
}

func (r *Resolver) cached(scope, repository, commit string) (Model, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	model, ok := r.commits[scope+"\x00"+strings.ToLower(repository)+"\x00"+commit]
	if ok {
		model.Cached = true
		model.Architectures = append([]string(nil), model.Architectures...)
	}
	return model, ok
}

func (r *Resolver) store(scope, refKey string, model Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs[refKey] = refEntry{commit: model.Commit, expires: r.now().Add(r.refTTL)}
	key := scope + "\x00" + strings.ToLower(model.Repository) + "\x00" + model.Commit
	if _, exists := r.commits[key]; !exists {
		r.order = append(r.order, key)
	}
	r.commits[key] = model
	for len(r.order) > r.maxEntries {
		delete(r.commits, r.order[0])
		r.order = r.order[1:]
	}
	for key, entry := range r.refs {
		if !r.now().Before(entry.expires) || len(r.refs) > r.maxEntries {
			delete(r.refs, key)
		}
	}
}

var infoFields = []string{"sha", "pipeline_tag", "library_name", "cardData", "tags", "gated", "private", "disabled", "config", "safetensors", "siblings"}

type infoResponse struct {
	ID          string          `json:"id"`
	SHA         string          `json:"sha"`
	PipelineTag string          `json:"pipeline_tag"`
	LibraryName string          `json:"library_name"`
	CardData    json.RawMessage `json:"cardData"`
	Tags        []string        `json:"tags"`
	Gated       json.RawMessage `json:"gated"`
	Private     bool            `json:"private"`
	Disabled    bool            `json:"disabled"`
	Config      struct {
		Architectures []string `json:"architectures"`
		ModelType     string   `json:"model_type"`
	} `json:"config"`
	Safetensors *struct {
		Parameters map[string]int64 `json:"parameters"`
		Total      int64            `json:"total"`
	} `json:"safetensors"`
	Siblings []sibling `json:"siblings"`
}

// sibling is one repository file; with blobs=true Hugging Face adds the git
// blob ID and, for a file in LFS, its content sha256.
type sibling struct {
	Name   string `json:"rfilename"`
	BlobID string `json:"blobId"`
	LFS    *struct {
		SHA256 string `json:"sha256"`
	} `json:"lfs"`
}

// tokenizerDigestFile reports whether a file changes how a server reads
// text: the tokenizer files the runtimes load, the BPE merges, the chat
// template, and the special and added tokens.
func tokenizerDigestFile(name string) bool {
	switch strings.ToLower(name) {
	case "merges.txt", "chat_template.jinja", "chat_template.json", "special_tokens_map.json", "added_tokens.json":
		return true
	}
	return TokenizerFile(name)
}

// tokenizerDigest is "sha256:" and the sha256 of sorted "path  hash" lines,
// one for each file that changes how a server reads text, where the hash is
// the file's LFS sha256, or "git:" and its git blob ID. Two repositories
// whose digests are equal list the same such files with the same content,
// so their servers read text alike; a git blob and a content sha256 never
// compare equal, so unlike hashes read as different, never as the same.
// It is empty when no such file is listed or one has no hash.
func tokenizerDigest(siblings []sibling) string {
	var lines []string
	for _, file := range siblings {
		if !tokenizerDigestFile(file.Name) {
			continue
		}
		hash := ""
		switch {
		case file.LFS != nil && file.LFS.SHA256 != "":
			hash = file.LFS.SHA256
		case file.BlobID != "":
			hash = "git:" + file.BlobID
		default:
			return ""
		}
		lines = append(lines, file.Name+"  "+hash)
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *Resolver) read(ctx context.Context, repository, revision, token string) (Model, error) {
	query := url.Values{}
	for _, field := range infoFields {
		query.Add("expand[]", field)
	}
	// Each sibling's blob ID and LFS sha256, for the tokenizer digest.
	query.Set("blobs", "true")
	address := r.endpoint + "/api/models/" + repositoryPath(repository) + "/revision/" + url.PathEscape(revision) + "?" + query.Encode()
	status, header, body, err := r.get(ctx, address, token, maxInfoBytes)
	if err != nil {
		return Model{}, err
	}
	switch {
	case status == http.StatusOK:
	case header.Get("X-Error-Code") == "RevisionNotFound":
		return Model{}, &Error{Code: CodeRevisionNotFound, Message: "Hugging Face has no revision " + revision + " for " + repository}
	case token != "" && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		return Model{}, &Error{Code: CodeTokenRejected, Message: "Hugging Face did not accept the saved token for " + repository}
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		// Hugging Face answers 401 for a missing repository and for a
		// private one alike, so the two cannot be told apart without a token.
		message := "Hugging Face did not find " + repository + ", or it is private. Check the name, or add a Hugging Face token."
		if token != "" {
			message = "Hugging Face did not find " + repository + " with the saved token."
		}
		return Model{}, &Error{Code: CodeModelNotFound, Message: message}
	default:
		return Model{}, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("Hugging Face answered with status %d", status)}
	}
	var info infoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		return Model{}, &Error{Code: CodeUnavailable, Message: "Hugging Face returned model metadata InferCrane could not read"}
	}
	if !gitCommit.MatchString(info.SHA) {
		return Model{}, &Error{Code: CodeUnavailable, Message: "Hugging Face did not report an immutable commit for this revision"}
	}
	model := Model{
		Repository: repository, Commit: info.SHA, PipelineTag: info.PipelineTag, LibraryName: info.LibraryName,
		License: license(info.CardData, info.Tags), Gated: gated(info.Gated), Private: info.Private, Disabled: info.Disabled,
		Architectures: append([]string(nil), info.Config.Architectures...), ModelType: info.Config.ModelType,
		TokenUsed: token != "", ReadAt: r.now().UTC(),
	}
	if ValidRepository(info.ID) {
		model.Repository = info.ID
	}
	if info.Safetensors != nil && len(info.Safetensors.Parameters) > 0 {
		weights := &Weights{ByDType: map[string]int64{}}
		for dtype, count := range info.Safetensors.Parameters {
			if count > 0 {
				weights.ByDType[strings.ToUpper(dtype)] += count
				weights.Parameters += count
			}
		}
		if weights.Parameters > 0 {
			model.Weights = weights
		}
	}
	model.Files.Listed = info.Siblings != nil
	model.TokenizerDigest = tokenizerDigest(info.Siblings)
	for _, sibling := range info.Siblings {
		name := strings.ToLower(sibling.Name)
		switch {
		case name == "config.json":
			model.Files.Config = true
		case TokenizerFile(name):
			model.Files.Tokenizer = true
		case strings.HasSuffix(name, ".safetensors"):
			model.Files.Safetensors = true
		case strings.HasSuffix(name, ".gguf"):
			model.Files.GGUF = true
		case strings.HasSuffix(name, ".bin") || strings.HasSuffix(name, ".pt") || strings.HasSuffix(name, ".pth"):
			model.Files.PyTorch = true
		}
	}
	if model.Files.Listed && !model.Files.Config {
		model.ConfigState = ConfigNotFound
		return model, nil
	}
	config, state, err := r.readConfig(ctx, model.Repository, model.Commit, token)
	if err != nil {
		return Model{}, err
	}
	model.Config, model.ConfigState = config, state
	if config != nil && len(model.Architectures) == 0 {
		model.Architectures = append([]string(nil), config.Architectures...)
	}
	if config != nil && model.ModelType == "" {
		model.ModelType = config.ModelType
	}
	return model, nil
}

func (r *Resolver) readConfig(ctx context.Context, repository, commit, token string) (*Config, string, error) {
	address := r.endpoint + "/" + repositoryPath(repository) + "/raw/" + commit + "/config.json"
	status, _, body, err := r.get(ctx, address, token, maxConfigBytes)
	if err != nil {
		return nil, "", err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ConfigTokenRequired, nil
	case http.StatusNotFound:
		return nil, ConfigNotFound, nil
	default:
		return nil, "", &Error{Code: CodeUnavailable, Message: fmt.Sprintf("Hugging Face answered with status %d for config.json", status)}
	}
	config, err := ParseConfig(body)
	if err != nil {
		return nil, ConfigInvalid, nil
	}
	return &config, ConfigRead, nil
}

func (r *Resolver) get(ctx context.Context, address, token string, limit int64) (int, http.Header, []byte, error) {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return 0, nil, nil, unavailable(ctx.Err())
	}
	defer func() { <-r.slots }()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, nil, nil, &Error{Code: CodeInvalidModel, Message: "model reference could not form a Hugging Face request"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return 0, nil, nil, unavailable(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return 0, nil, nil, unavailable(err)
	}
	if int64(len(body)) > limit {
		return 0, nil, nil, &Error{Code: CodeUnavailable, Message: "Hugging Face returned more metadata than InferCrane reads"}
	}
	return response.StatusCode, response.Header, body, nil
}

func unavailable(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: CodeUnavailable, Message: "Hugging Face did not answer in time", Timeout: true}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &Error{Code: CodeUnavailable, Message: "Hugging Face could not be reached"}
}

func repositoryPath(repository string) string {
	owner, name, _ := strings.Cut(repository, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func gated(raw json.RawMessage) string {
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		if flag {
			return "auto"
		}
		return ""
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		return strings.ToLower(strings.TrimSpace(mode))
	}
	return ""
}

// license prefers the model card's declared license and falls back to the
// Hub's license: tag. A card that names a custom license reports its name.
func license(cardData json.RawMessage, tags []string) string {
	var card struct {
		License     json.RawMessage `json:"license"`
		LicenseName string          `json:"license_name"`
	}
	if len(cardData) > 0 && json.Unmarshal(cardData, &card) == nil {
		var single string
		var many []string
		switch {
		case json.Unmarshal(card.License, &single) == nil && strings.TrimSpace(single) != "":
			value := strings.TrimSpace(single)
			if value == "other" && strings.TrimSpace(card.LicenseName) != "" {
				value = strings.TrimSpace(card.LicenseName)
			}
			return value
		case json.Unmarshal(card.License, &many) == nil && len(many) > 0:
			sort.Strings(many)
			return strings.Join(many, ", ")
		}
	}
	for _, tag := range tags {
		if value, found := strings.CutPrefix(tag, "license:"); found && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// ConfigDigest is the sha256 of a config.json as served.
func ConfigDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// AdapterRead is a LoRA adapter repository pinned to a commit and the bytes of
// its adapter_config.json, when Hugging Face returned them.
type AdapterRead struct {
	Model       Model
	Config      []byte
	ConfigState string
	Weights     bool
}

// ResolveAdapter pins an adapter repository to a commit and reads its
// adapter_config.json with the same bounds, cache scope, and token rules as a
// model read. The token goes only to the Hub host.
func (r *Resolver) ResolveAdapter(ctx context.Context, request Request) (AdapterRead, error) {
	model, err := r.Resolve(ctx, request)
	if err != nil {
		return AdapterRead{}, err
	}
	read := AdapterRead{Model: model, ConfigState: ConfigNotFound, Weights: model.Files.Safetensors}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	status, _, body, err := r.get(ctx, r.endpoint+"/"+repositoryPath(model.Repository)+"/raw/"+model.Commit+"/adapter_config.json", request.Token, maxConfigBytes)
	if err != nil {
		return AdapterRead{}, err
	}
	switch status {
	case http.StatusOK:
		read.Config, read.ConfigState = body, ConfigRead
	case http.StatusUnauthorized, http.StatusForbidden:
		read.ConfigState = ConfigTokenRequired
	case http.StatusNotFound:
	default:
		return AdapterRead{}, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("Hugging Face answered with status %d for adapter_config.json", status)}
	}
	return read, nil
}
