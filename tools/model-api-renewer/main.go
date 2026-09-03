// model-api-renewer is a one-shot, operator-only renewal process for the
// directly operated MVP catalog. It is designed for a scheduled Fly Machine:
// it normally exits without supplier traffic and qualifies sequentially only
// when current evidence is close to expiry.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultRenewBefore = 6 * time.Hour
	qualificationTTL   = 24 * time.Hour
	qualificationCap   = 512
	samplesPerMode     = 3
)

type profile struct {
	Name, ProductID, OfferID, TupleKey, Revision string
	SupplierCredentialEnv                        string
	CredentialReferenceEnv                       string
	CommercialTermsRef                           string
	ReleaseTool                                  string
	Publication                                  []publicationStep
}

type publicationStep struct{ ContractType, Filename string }

var renewalProfiles = []profile{
	{
		Name: "deepseek-v4-flash", ProductID: "deepseek-v4-flash", OfferID: "deepseek-direct-v4-flash",
		TupleKey: "deepseek|deepseek-v4-flash|openai|global", Revision: "DeepSeek-V4-Flash-0731",
		SupplierCredentialEnv:  "DEEPSEEK_API_KEY",
		CredentialReferenceEnv: "INFERCRANE_MODEL_API_DEEPSEEK_CREDENTIAL_REFERENCE",
		CommercialTermsRef:     "https://api-docs.deepseek.com/quick_start/pricing",
		ReleaseTool:            "infercrane-model-api-production-release",
		Publication: []publicationStep{
			{"rate", "02-retail-rate.json"}, {"offer", "03-supplier-offer.json"}, {"qualification", "04-qualification.json"},
			{"plan", "05-supply-plan.json"}, {"product", "07-product-available.json"}, {"publication", "06-publication.json"}, {"entitlement", "08-canary-entitlement.json"},
		},
	},
	{
		Name: "glm-5.2", ProductID: "glm-5.2", OfferID: "zai-glm-5-2",
		TupleKey: "zai|glm-5.2|openai|global", Revision: "glm-5.2",
		SupplierCredentialEnv:  "ZAI_API_KEY",
		CredentialReferenceEnv: "INFERCRANE_MODEL_API_ZAI_CREDENTIAL_REFERENCE",
		CommercialTermsRef:     "https://docs.z.ai/guides/llm/glm-5.2",
		ReleaseTool:            "infercrane-model-api-mvp-release",
		Publication:            zaiPublicationSteps(),
	},
	{
		Name: "glm-5.3", ProductID: "glm-5.3", OfferID: "zai-glm-5-3",
		TupleKey: "zai|glm-5.3|openai|global", Revision: "glm-5.3",
		SupplierCredentialEnv:  "ZAI_API_KEY",
		CredentialReferenceEnv: "INFERCRANE_MODEL_API_ZAI_CREDENTIAL_REFERENCE",
		CommercialTermsRef:     "https://docs.z.ai/guides/llm/glm-5.3",
		ReleaseTool:            "infercrane-model-api-mvp-release",
		Publication:            zaiPublicationSteps(),
	},
	{
		Name: "glm-5.3-flash", ProductID: "glm-5.3-flash", OfferID: "zai-glm-5-3-flash",
		TupleKey: "zai|glm-5.3-flash|openai|global", Revision: "glm-5.3-flash",
		SupplierCredentialEnv:  "ZAI_API_KEY",
		CredentialReferenceEnv: "INFERCRANE_MODEL_API_ZAI_CREDENTIAL_REFERENCE",
		CommercialTermsRef:     "https://docs.z.ai/guides/vlm/glm-5.3-flash",
		ReleaseTool:            "infercrane-model-api-mvp-release",
		Publication:            zaiPublicationSteps(),
	},
}

func zaiPublicationSteps() []publicationStep {
	return []publicationStep{
		{"rate", "02-retail-rate.json"}, {"offer", "03-supplier-offer.json"}, {"qualification", "04-qualification.json"},
		{"target-binding", "05-target-binding.json"}, {"plan", "06-supply-plan.json"}, {"product", "08-product-available.json"},
		{"publication", "07-publication.json"}, {"entitlement", "09-canary-entitlement.json"},
	}
}

type config struct {
	ControlURL, APIKey                                string
	OperatorWorkspace, ServingPlan, CustomerWorkspace string
	MachineID, StateDirectory                         string
	RenewBefore                                       time.Duration
	Force                                             bool
}

type catalogResponse struct {
	Model struct {
		EvidenceValidUntil *time.Time `json:"evidence_valid_until"`
		Callable           bool       `json:"callable"`
	} `json:"model"`
}

type commandRunner func(context.Context, string, ...string) error

func main() {
	cfg := config{
		ControlURL:        os.Getenv("INFERCRANE_URL"),
		APIKey:            os.Getenv("INFERCRANE_API_KEY"),
		OperatorWorkspace: os.Getenv("INFERCRANE_MODEL_API_OPERATOR_WORKSPACE"),
		ServingPlan:       os.Getenv("INFERCRANE_MODEL_API_SERVING_PLAN"),
		CustomerWorkspace: os.Getenv("INFERCRANE_MODEL_API_CANARY_WORKSPACE"),
		MachineID:         os.Getenv("FLY_MACHINE_ID"),
		StateDirectory:    os.Getenv("INFERCRANE_MODEL_API_RENEWAL_STATE_DIR"),
		RenewBefore:       defaultRenewBefore,
	}
	flag.BoolVar(&cfg.Force, "force", false, "qualify all profiles even when current evidence is outside the renewal window")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("positional arguments are not accepted"))
	}
	if cfg.StateDirectory == "" {
		cfg.StateDirectory = "/home/app/.local/state/infercrane/model-api-renewal"
	}
	if err := cfg.validate(); err != nil {
		fatal(err)
	}
	for _, item := range renewalProfiles {
		if strings.TrimSpace(os.Getenv(item.SupplierCredentialEnv)) == "" {
			fatal(fmt.Errorf("%s is required", item.SupplierCredentialEnv))
		}
		if value := strings.TrimSpace(os.Getenv(item.CredentialReferenceEnv)); value == "" || strings.ContainsAny(value, "\r\n\x00") {
			fatal(fmt.Errorf("%s is required and must be a single safe value", item.CredentialReferenceEnv))
		}
	}
	for _, binary := range []string{"infercrane", "infercrane-model-api-qualifier", "infercrane-model-api-production-release", "infercrane-model-api-mvp-release"} {
		if _, err := exec.LookPath(binary); err != nil {
			fatal(fmt.Errorf("required operator binary %s is unavailable", binary))
		}
	}
	if err := os.MkdirAll(cfg.StateDirectory, 0o700); err != nil {
		fatal(errors.New("renewal state directory could not be created"))
	}
	if err := os.Chmod(cfg.StateDirectory, 0o700); err != nil {
		fatal(errors.New("renewal state directory could not be protected"))
	}
	lock, err := acquireLock(filepath.Join(cfg.StateDirectory, "renewal.lock"))
	if err != nil {
		fatal(err)
	}
	defer releaseLock(lock)

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	failed := false
	for _, item := range renewalProfiles {
		if err := renewProfile(ctx, cfg, item, time.Now().UTC(), runCommand); err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "renewal failed for %s: %v\n", item.Name, err)
		}
	}
	if failed {
		fatal(errors.New("one or more model API renewals failed closed"))
	}
}

func (c config) validate() error {
	parsed, err := url.Parse(strings.TrimSpace(c.ControlURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("INFERCRANE_URL must be an absolute HTTPS origin")
	}
	if strings.Trim(parsed.Path, "/") != "" {
		return errors.New("INFERCRANE_URL must not include a path")
	}
	for name, value := range map[string]string{
		"INFERCRANE_API_KEY":                      c.APIKey,
		"INFERCRANE_MODEL_API_OPERATOR_WORKSPACE": c.OperatorWorkspace,
		"INFERCRANE_MODEL_API_SERVING_PLAN":       c.ServingPlan,
		"INFERCRANE_MODEL_API_CANARY_WORKSPACE":   c.CustomerWorkspace,
		"FLY_MACHINE_ID":                          c.MachineID,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s is required and must be a single safe value", name)
		}
	}
	if c.RenewBefore <= 0 || c.RenewBefore >= qualificationTTL {
		return errors.New("renewal window must remain positive and shorter than the qualification TTL")
	}
	if !filepath.IsAbs(c.StateDirectory) || filepath.Clean(c.StateDirectory) == "/" {
		return errors.New("renewal state directory must be an absolute non-root path")
	}
	return nil
}

func renewProfile(ctx context.Context, cfg config, item profile, now time.Time, run commandRunner) error {
	credentialReference := strings.TrimSpace(os.Getenv(item.CredentialReferenceEnv))
	if credentialReference == "" || strings.ContainsAny(credentialReference, "\r\n\x00") {
		return fmt.Errorf("%s is required", item.CredentialReferenceEnv)
	}
	current, err := currentCatalogState(ctx, cfg, item.ProductID)
	if err != nil {
		return err
	}
	if !cfg.Force && current.Model.Callable && current.Model.EvidenceValidUntil != nil && current.Model.EvidenceValidUntil.After(now.Add(cfg.RenewBefore)) {
		fmt.Printf("%s: current evidence is outside the renewal window; no supplier traffic sent\n", item.Name)
		return nil
	}

	version := int(now.Unix())
	stamp := now.UTC().Format("20060102T150405Z")
	runDirectory := filepath.Join(cfg.StateDirectory, item.Name, stamp)
	if err = os.MkdirAll(runDirectory, 0o700); err != nil {
		return errors.New("immutable renewal directory could not be created")
	}
	if err = os.Chmod(runDirectory, 0o700); err != nil {
		return errors.New("immutable renewal directory could not be protected")
	}
	rawPath := filepath.Join(runDirectory, "raw.json")
	qualificationPath := filepath.Join(runDirectory, "qualification.json")
	releaseDirectory := filepath.Join(runDirectory, "release")
	evidenceRef := fmt.Sprintf("fly-machine://%s/model-api-renewal/%s/%s/raw.json", cfg.MachineID, item.Name, stamp)
	qualificationID := fmt.Sprintf("%s-q-%s-r%d", item.OfferID, stamp, version)

	qualifierArgs := []string{
		"--profile", item.Name,
		"--confirm-live",
		"--offer-id", item.OfferID,
		"--offer-version", strconv.Itoa(version),
		"--qualification-id", qualificationID,
		"--tuple-key", item.TupleKey,
		"--expected-revision", item.Revision,
		"--samples-per-mode", strconv.Itoa(samplesPerMode),
		"--max-output-tokens", strconv.Itoa(qualificationCap),
		"--request-timeout", "180s",
		"--total-timeout", "12m",
		"--valid-for", qualificationTTL.String(),
		"--evidence-ref", evidenceRef,
		"--evidence-output", rawPath,
		"--qualification-output", qualificationPath,
	}
	fmt.Printf("%s: entering renewal window; running bounded sequential qualification\n", item.Name)
	if err = run(ctx, "infercrane-model-api-qualifier", qualifierArgs...); err != nil {
		return fmt.Errorf("qualification failed: %w", err)
	}
	releaseArgs := []string{
		"--qualification", qualificationPath,
		"--credential-reference", credentialReference,
		"--operator-workspace", cfg.OperatorWorkspace,
		"--serving-plan", cfg.ServingPlan,
		"--customer-workspace", cfg.CustomerWorkspace,
		"--output-directory", releaseDirectory,
		"--release-version", strconv.Itoa(version),
	}
	if item.ReleaseTool == "infercrane-model-api-mvp-release" {
		releaseArgs = append(releaseArgs, "--profile", item.Name, "--commercial-terms-ref", item.CommercialTermsRef)
	}
	if err = run(ctx, item.ReleaseTool, releaseArgs...); err != nil {
		return fmt.Errorf("release generation failed: %w", err)
	}
	for index, step := range item.Publication {
		manifest := filepath.Join(releaseDirectory, step.Filename)
		if err = run(ctx, "infercrane", "model-api", "publish", step.ContractType, "--file", manifest); err != nil {
			return fmt.Errorf("publication failed at contract %d (%s): %w", index+1, step.ContractType, err)
		}
	}
	fmt.Printf("%s: qualified and published immutable release r%d\n", item.Name, version)
	return nil
}

func currentCatalogState(ctx context.Context, cfg config, productID string) (catalogResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.ControlURL, "/")+"/api/v1/model-api-catalog/"+url.PathEscape(productID), nil)
	if err != nil {
		return catalogResponse{}, errors.New("catalog request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return catalogResponse{}, errors.New("catalog could not be reached")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return catalogResponse{}, fmt.Errorf("catalog returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return catalogResponse{}, errors.New("catalog returned an oversized or unreadable response")
	}
	var decoded catalogResponse
	if err = json.Unmarshal(body, &decoded); err != nil {
		return catalogResponse{}, errors.New("catalog returned an invalid response")
	}
	return decoded, nil
}

func acquireLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("renewal lock could not be opened")
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another model API renewal is already running")
	}
	return file, nil
}

func releaseLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func runCommand(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = os.Environ()
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s exited unsuccessfully", name)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "model API renewal:", err)
	os.Exit(1)
}
