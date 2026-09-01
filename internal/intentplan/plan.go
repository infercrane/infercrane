// Package intentplan turns a small, human-oriented deployment request into a
// deterministic, side-effect-free starting configuration. It uses only reviewed
// recipes and registered compatibility evidence. It never interprets a price as
// capacity, or a configuration as a performance claim.
package intentplan

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/provideridentity"
)

const SchemaVersion = "infercrane.intent-plan/v1"

var ErrInvalidRequest = errors.New("invalid intent planning request")

type Request struct {
	Intent      string `json:"intent"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Region      string `json:"region,omitempty"`
	GPU         string `json:"gpu,omitempty"`
	Objective   string `json:"objective,omitempty"`
	ComputeMode string `json:"compute_mode,omitempty"`
}

type Provider struct {
	ID, Label, State, Reason string
}

type Planner struct {
	Recipes       []curatedrecipe.Entry
	Compatibility []integration.RuntimeCompatibility
	Providers     []Provider
	Prices        map[pricing.Request]pricing.Estimate
	Now           func() time.Time
}

type Interpretation struct {
	Action      string `json:"action"`
	Objective   string `json:"objective"`
	ComputeMode string `json:"compute_mode,omitempty"`
}

type Model struct {
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name"`
	Repository      string   `json:"repository"`
	Revision        string   `json:"revision"`
	Runtime         string   `json:"runtime"`
	Tasks           []string `json:"tasks"`
	Gated           bool     `json:"gated"`
	EvidenceClass   string   `json:"evidence_class"`
	EvidenceSummary string   `json:"evidence_summary"`
}

type Option struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	State  string `json:"state,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type Choice struct {
	Field    string   `json:"field"`
	Label    string   `json:"label"`
	Value    any      `json:"value,omitempty"`
	Editable bool     `json:"editable"`
	Required bool     `json:"required"`
	Options  []Option `json:"options,omitempty"`
}

type MissingChoice struct {
	Field       string   `json:"field"`
	Prompt      string   `json:"prompt"`
	Reason      string   `json:"reason"`
	Options     []Option `json:"options,omitempty"`
	Remediation string   `json:"remediation"`
}

type Configuration struct {
	Model           string   `json:"model"`
	ModelRevision   string   `json:"model_revision"`
	Runtime         string   `json:"runtime"`
	RuntimeVersion  string   `json:"runtime_version,omitempty"`
	RuntimeArgs     []string `json:"runtime_args"`
	Profile         string   `json:"profile"`
	ComputeMode     string   `json:"compute_mode"`
	Provider        string   `json:"provider,omitempty"`
	ProviderAdapter string   `json:"provider_adapter,omitempty"`
	Region          string   `json:"region,omitempty"`
	GPU             string   `json:"gpu"`
	GPUCount        int      `json:"gpu_count"`
	MinReplicas     int      `json:"min_replicas"`
	MaxReplicas     int      `json:"max_replicas"`
	Routing         string   `json:"routing"`
}

type PriceEvidence struct {
	State                string     `json:"state"`
	Currency             string     `json:"currency,omitempty"`
	HourlyUSDPerReplica  *float64   `json:"hourly_usd_per_replica,omitempty"`
	CostScope            string     `json:"cost_scope,omitempty"`
	Authority            string     `json:"price_authority,omitempty"`
	Source               string     `json:"source,omitempty"`
	ObservedAt           *time.Time `json:"observed_at,omitempty"`
	ValidUntil           *time.Time `json:"valid_until,omitempty"`
	DeploymentComparable bool       `json:"deployment_comparable"`
	Reason               string     `json:"reason"`
}

type Evidence struct {
	Configuration string        `json:"configuration"`
	Performance   string        `json:"performance"`
	Capacity      string        `json:"capacity"`
	Price         PriceEvidence `json:"price"`
}

type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	State string `json:"state"`
}

type Edge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
}

type Architecture struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

type Plan struct {
	SchemaVersion  string          `json:"schema_version"`
	Status         string          `json:"status"`
	Mutation       string          `json:"mutation"`
	Interpretation Interpretation  `json:"interpretation"`
	Model          *Model          `json:"model,omitempty"`
	Configuration  *Configuration  `json:"configuration,omitempty"`
	Choices        []Choice        `json:"choices"`
	Missing        []MissingChoice `json:"missing_choices"`
	Architecture   Architecture    `json:"architecture"`
	Evidence       Evidence        `json:"evidence"`
	Warnings       []string        `json:"warnings"`
}

func (p Planner) Plan(request Request) (Plan, error) {
	request = normalizeRequest(request)
	if err := validateRequest(request); err != nil {
		return Plan{}, err
	}
	result := Plan{
		SchemaVersion:  SchemaVersion,
		Status:         "needs_input",
		Mutation:       "none",
		Interpretation: Interpretation{Action: inferAction(request.Intent), Objective: inferObjective(request), ComputeMode: inferComputeMode(request)},
		Choices:        make([]Choice, 0, 8),
		Missing:        make([]MissingChoice, 0, 4),
		Warnings:       []string{"Recommended configuration is a reviewed starting point, not a performance, capacity, or final-cost claim."},
		Evidence: Evidence{
			Configuration: "reviewed model recipe and registered runtime/provider compatibility",
			Performance:   "unmeasured",
			Capacity:      "unknown until a bounded provider probe or accepted launch",
			Price:         unavailablePrice("no exact current catalog observation selected"),
		},
	}
	recipes := append([]curatedrecipe.Entry(nil), p.Recipes...)
	if len(recipes) == 0 {
		recipes = curatedrecipe.All()
	}
	sort.Slice(recipes, func(i, j int) bool { return recipes[i].Name < recipes[j].Name })
	entry, matched, suggestions := resolveModel(recipes, request.Model, request.Intent)
	if !matched {
		result.Missing = append(result.Missing, MissingChoice{
			Field: "model", Prompt: "Which reviewed model do you want to deploy?",
			Reason:  "The request did not identify exactly one reviewed model; InferCrane will not silently substitute a model.",
			Options: suggestions, Remediation: "Choose one reviewed model or provide its exact catalog name or Hugging Face repository.",
		})
		result.Choices = append(result.Choices, Choice{Field: "model", Label: "Model", Editable: true, Required: true, Options: suggestions})
		result.Architecture = baseArchitecture(nil)
		return result, nil
	}
	result.Model = &Model{Name: entry.Name, DisplayName: entry.DisplayName, Repository: entry.Model, Revision: entry.Revision, Runtime: entry.Runtime, Tasks: append([]string(nil), entry.Tasks...), Gated: entry.Gated, EvidenceClass: entry.EvidenceClass, EvidenceSummary: entry.EvidenceSummary}

	profile, profileOK := chooseProfile(entry.Profiles, result.Interpretation.Objective, result.Interpretation.ComputeMode, request.GPU)
	if !profileOK {
		options := profileOptions(entry.Profiles)
		result.Missing = append(result.Missing, MissingChoice{Field: "compute_mode", Prompt: "Choose a reviewed compute profile.", Reason: "No reviewed profile matches the requested compute mode and GPU.", Options: options, Remediation: "Select one of the reviewed profiles or request qualification for the desired mode/GPU tuple."})
		result.Choices = append(result.Choices, Choice{Field: "profile", Label: "Configuration profile", Editable: true, Required: true, Options: options})
		result.Architecture = baseArchitecture(result.Model)
		return result, nil
	}

	gpu := request.GPU
	if gpu == "" {
		gpu = profile.GPUHint
	}
	gpuCount := max(1, profile.GPUCount)
	provider, providerOptions, providerReady := p.chooseProvider(request.Provider, request.Intent, request.Region, gpu, gpuCount, profile)
	if provider.ID == "" {
		result.Missing = append(result.Missing, MissingChoice{Field: "provider", Prompt: "Where should InferCrane deploy this model?", Reason: "No exact comparable quote, reviewed profile hint, or single ready compatible connection selected a provider.", Options: providerOptions, Remediation: "Choose a compatible provider and configure its compute connection before deployment."})
	} else if !providerReady {
		result.Missing = append(result.Missing, MissingChoice{Field: "provider_connection", Prompt: "Connect " + providerLabel(provider) + " before deployment.", Reason: provider.Reason, Options: providerOptions, Remediation: "Configure and verify the provider connection; catalog pricing alone does not authorize or prove a launch."})
	}

	region := request.Region
	if (provider.ID == "aws" || provider.ID == "gcp") && region == "" {
		result.Missing = append(result.Missing, MissingChoice{Field: "region", Prompt: "Which " + providerLabel(provider) + " region should be used?", Reason: "This provider requires an explicit region; price and capacity vary by region.", Remediation: "Choose a configured region, then request a capacity probe before launching."})
	}
	adapter, compatibilityState, compatibilityEvidence := chooseCompatibility(p.Compatibility, provider.ID, profile.Runtime, profile.ComputeMode, profile.ProviderAdapterHint)
	if provider.ID != "" && adapter == "" {
		result.Missing = append(result.Missing, MissingChoice{Field: "provider", Prompt: "Choose a compatible provider.", Reason: "No registered compatibility evidence matches this runtime, provider, and compute mode.", Options: providerOptions, Remediation: "Choose a listed compatible provider or qualify and register the missing tuple."})
	}

	configuration := Configuration{Model: entry.Model, ModelRevision: entry.Revision, Runtime: profile.Runtime, RuntimeVersion: profile.RuntimeVersion, RuntimeArgs: append([]string(nil), profile.RuntimeArgs...), Profile: profile.Name, ComputeMode: profile.ComputeMode, Provider: provider.ID, ProviderAdapter: adapter, Region: region, GPU: gpu, GPUCount: gpuCount, MinReplicas: profile.MinReplicas, MaxReplicas: profile.MaxReplicas, Routing: "round-robin"}
	result.Configuration = &configuration
	result.Choices = append(result.Choices,
		Choice{Field: "model", Label: "Model", Value: entry.Model, Editable: true, Required: true, Options: []Option{{Value: entry.Name, Label: entry.DisplayName, State: "reviewed"}}},
		Choice{Field: "profile", Label: "Configuration profile", Value: profile.Name, Editable: true, Required: true, Options: profileOptions(entry.Profiles)},
		Choice{Field: "provider", Label: "Provider", Value: provider.ID, Editable: true, Required: true, Options: providerOptions},
		Choice{Field: "region", Label: "Region", Value: region, Editable: true, Required: provider.ID == "aws" || provider.ID == "gcp"},
		Choice{Field: "gpu", Label: "GPU", Value: gpu, Editable: true, Required: true, Options: gpuOptions(profile)},
		Choice{Field: "min_replicas", Label: "Minimum replicas", Value: configuration.MinReplicas, Editable: true, Required: true},
		Choice{Field: "max_replicas", Label: "Maximum replicas", Value: configuration.MaxReplicas, Editable: true, Required: true},
	)
	if compatibilityState != "" {
		result.Evidence.Configuration = "reviewed model recipe; runtime/provider compatibility=" + compatibilityState
		if compatibilityEvidence != "" {
			result.Evidence.Configuration += " (" + compatibilityEvidence + ")"
		}
	}
	result.Evidence.Price = p.priceEvidence(configuration)
	result.Architecture = architecture(configuration, result.Model, providerReady)
	if entry.Gated {
		result.Warnings = append(result.Warnings, "Model access is gated; verify license acceptance and provider-side artifact credentials before deployment.")
	}
	result.Warnings = append(result.Warnings, profile.Limitations...)
	if len(result.Missing) == 0 {
		result.Status = "ready"
	}
	return result, nil
}

func normalizeRequest(request Request) Request {
	request.Intent = strings.TrimSpace(request.Intent)
	request.Model = strings.TrimSpace(request.Model)
	request.Provider = canonicalProvider(request.Provider)
	request.Region = strings.TrimSpace(request.Region)
	request.GPU = strings.TrimSpace(request.GPU)
	request.Objective = strings.ToLower(strings.TrimSpace(request.Objective))
	request.ComputeMode = strings.ToLower(strings.TrimSpace(request.ComputeMode))
	return request
}

func validateRequest(request Request) error {
	if request.Intent == "" && request.Model == "" {
		return errors.Join(ErrInvalidRequest, errors.New("intent or model is required"))
	}
	if len(request.Intent) > 2048 || len(request.Model) > 256 || len(request.Provider) > 64 || len(request.Region) > 128 || len(request.GPU) > 128 || len(request.Objective) > 64 || len(request.ComputeMode) > 64 {
		return errors.Join(ErrInvalidRequest, errors.New("intent planning fields exceed their bounded size"))
	}
	if request.Objective != "" && request.Objective != "interactive" && request.Objective != "latency" && request.Objective != "throughput" && request.Objective != "cost-efficiency" {
		return errors.Join(ErrInvalidRequest, errors.New("objective must be interactive, latency, throughput, or cost-efficiency"))
	}
	if request.ComputeMode != "" && request.ComputeMode != "elastic" && request.ComputeMode != "serverless" {
		return errors.Join(ErrInvalidRequest, errors.New("compute_mode must be elastic or serverless"))
	}
	return nil
}

func inferAction(intent string) string {
	words := normalizedWords(intent)
	if hasWord(words, "optimize") || hasWord(words, "optimise") || hasWord(words, "tune") {
		return "optimize"
	}
	return "deploy"
}

func inferObjective(request Request) string {
	if request.Objective != "" {
		return request.Objective
	}
	words := normalizedWords(request.Intent)
	switch {
	case hasWord(words, "throughput") || hasWord(words, "batch"):
		return "throughput"
	case hasWord(words, "cheap") || hasWord(words, "cheapest") || hasWord(words, "cost") || hasWord(words, "budget"):
		return "cost-efficiency"
	case hasWord(words, "latency") || hasWord(words, "fast") || hasWord(words, "interactive"):
		return "latency"
	default:
		return "interactive"
	}
}

func inferComputeMode(request Request) string {
	if request.ComputeMode != "" {
		return request.ComputeMode
	}
	words := normalizedWords(request.Intent)
	if hasWord(words, "serverless") {
		return "serverless"
	}
	return "elastic"
}

func resolveModel(recipes []curatedrecipe.Entry, explicit, intent string) (curatedrecipe.Entry, bool, []Option) {
	query := explicit
	if query == "" {
		query = intent
	}
	normalizedQuery := normalizeIdentity(query)
	type match struct {
		entry curatedrecipe.Entry
		score int
	}
	matches := make([]match, 0, 2)
	for _, entry := range recipes {
		score := 0
		for weight, identity := range map[int]string{4: entry.Model, 3: entry.Name, 2: entry.DisplayName} {
			identity = normalizeIdentity(identity)
			if explicit != "" && normalizedQuery == identity {
				score = max(score, weight+10)
			} else if explicit == "" && identity != "" && strings.Contains(normalizedQuery, identity) {
				score = max(score, weight)
			}
		}
		if score > 0 {
			matches = append(matches, match{entry: entry, score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].entry.Name < matches[j].entry.Name
	})
	if len(matches) == 1 || len(matches) > 1 && matches[0].score > matches[1].score {
		return matches[0].entry, true, nil
	}
	return curatedrecipe.Entry{}, false, modelSuggestions(recipes, query)
}

func modelSuggestions(recipes []curatedrecipe.Entry, query string) []Option {
	words := normalizedWords(query)
	type scored struct {
		entry curatedrecipe.Entry
		score int
	}
	items := make([]scored, 0, len(recipes))
	for _, entry := range recipes {
		haystack := normalizedWords(entry.Name + " " + entry.DisplayName + " " + entry.Publisher + " " + entry.Model + " " + strings.Join(entry.Tasks, " "))
		score := 0
		for word := range words {
			if len(word) >= 3 && hasWord(haystack, word) {
				score++
			}
		}
		items = append(items, scored{entry: entry, score: score})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].entry.Name < items[j].entry.Name
	})
	options := make([]Option, 0, min(8, len(items)))
	for _, item := range items {
		if len(options) == 8 {
			break
		}
		options = append(options, Option{Value: item.entry.Name, Label: item.entry.DisplayName, State: "reviewed"})
	}
	return options
}

func chooseProfile(profiles []curatedrecipe.ServingProfile, objective, mode, gpu string) (curatedrecipe.ServingProfile, bool) {
	items := append([]curatedrecipe.ServingProfile(nil), profiles...)
	sort.Slice(items, func(i, j int) bool {
		a, b := profileRank(items[i], objective), profileRank(items[j], objective)
		if a != b {
			return a < b
		}
		return items[i].Name < items[j].Name
	})
	for _, profile := range items {
		if profile.ComputeMode != mode || gpu != "" && !profileAcceptsGPU(profile, gpu) {
			continue
		}
		return profile, true
	}
	return curatedrecipe.ServingProfile{}, false
}

func profileRank(profile curatedrecipe.ServingProfile, objective string) int {
	preferred := "balanced"
	if objective == "latency" || objective == "interactive" {
		preferred = "interactive"
	} else if objective == "throughput" || objective == "cost-efficiency" {
		preferred = "throughput"
	}
	if strings.Contains(profile.Name, preferred) {
		return 0
	}
	if strings.Contains(profile.Name, "balanced") || strings.Contains(profile.Name, "elastic") {
		return 1
	}
	return 2
}

func profileAcceptsGPU(profile curatedrecipe.ServingProfile, gpu string) bool {
	if len(profile.CompatibleGPUs) == 0 {
		return strings.EqualFold(profile.GPUHint, gpu)
	}
	for _, candidate := range profile.CompatibleGPUs {
		if strings.EqualFold(candidate, gpu) {
			return true
		}
	}
	return false
}

func (p Planner) chooseProvider(explicit, intent, region, gpu string, gpuCount int, profile curatedrecipe.ServingProfile) (Provider, []Option, bool) {
	requested := explicit
	if requested == "" {
		requested = providerFromIntent(intent)
	}
	providers := append([]Provider(nil), p.Providers...)
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	compatibleClouds := map[string]bool{}
	for _, row := range p.Compatibility {
		if row.Runtime == profile.Runtime && string(row.Mode) == profile.ComputeMode && row.State != integration.QualificationDeferred && (profile.ProviderAdapterHint == "" || row.Adapter == profile.ProviderAdapterHint) {
			compatibleClouds[row.Cloud] = true
		}
	}
	options := make([]Option, 0, len(providers))
	var selected Provider
	readyCompatible := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		if !compatibleClouds[provider.ID] {
			continue
		}
		state := provider.State
		if state == "" {
			state = "connection-required"
		}
		options = append(options, Option{Value: provider.ID, Label: providerLabel(provider), State: state, Reason: provider.Reason})
		if requested == provider.ID {
			selected = provider
		}
		if provider.State == "ready" {
			readyCompatible = append(readyCompatible, provider)
		}
	}
	if selected.ID != "" {
		return selected, options, selected.State == "ready"
	}
	if requested != "" {
		return Provider{}, options, false
	}
	if provider, ok := p.cheapestComparableProvider(providers, compatibleClouds, region, gpu, gpuCount); ok {
		return provider, options, provider.State == "ready"
	}
	if profile.CloudHint != "" {
		for _, provider := range readyCompatible {
			if provider.ID == profile.CloudHint {
				return provider, options, true
			}
		}
	}
	if len(readyCompatible) == 1 {
		return readyCompatible[0], options, true
	}
	return Provider{}, options, false
}

// cheapestComparableProvider ranks price only after configuration compatibility
// has been established. Connection readiness deliberately does not participate:
// an unconnected provider may be the recommendation, but cannot make the plan
// ready or authorize a launch.
func (p Planner) cheapestComparableProvider(providers []Provider, compatibleClouds map[string]bool, region, gpu string, gpuCount int) (Provider, bool) {
	var selected Provider
	var selectedPrice pricing.Estimate
	found := false
	for _, provider := range providers {
		if !compatibleClouds[provider.ID] {
			continue
		}
		estimate, ok := p.selectPrice(provider.ID, region, gpu, gpuCount, true)
		if !ok {
			continue
		}
		if !found || estimate.Hourly < selectedPrice.Hourly || estimate.Hourly == selectedPrice.Hourly && provider.ID < selected.ID {
			selected, selectedPrice, found = provider, estimate, true
		}
	}
	return selected, found
}

func chooseCompatibility(rows []integration.RuntimeCompatibility, cloud, runtime, mode, hint string) (string, string, string) {
	items := append([]integration.RuntimeCompatibility(nil), rows...)
	sort.Slice(items, func(i, j int) bool {
		if qualificationRank(items[i].State) != qualificationRank(items[j].State) {
			return qualificationRank(items[i].State) < qualificationRank(items[j].State)
		}
		return items[i].Adapter < items[j].Adapter
	})
	for _, row := range items {
		if row.Cloud != cloud || row.Runtime != runtime || string(row.Mode) != mode || row.State == integration.QualificationDeferred {
			continue
		}
		if hint != "" && row.Adapter != hint {
			continue
		}
		return row.Adapter, string(row.State), row.Evidence
	}
	return "", "", ""
}

func qualificationRank(state integration.QualificationState) int {
	switch state {
	case integration.QualificationReal:
		return 0
	case integration.QualificationLocal:
		return 1
	case integration.QualificationSimulated:
		return 2
	default:
		return 3
	}
}

func (p Planner) priceEvidence(configuration Configuration) PriceEvidence {
	selected, found := p.selectPrice(configuration.Provider, configuration.Region, configuration.GPU, configuration.GPUCount, true)
	if !found {
		selected, found = p.selectPrice(configuration.Provider, configuration.Region, configuration.GPU, configuration.GPUCount, false)
	}
	if !found {
		return unavailablePrice("no exact current price observation for one replica of this provider/region/GPU tuple")
	}
	hourly, observed, valid := selected.Hourly, selected.ObservedAt.UTC(), selected.ObservedAt.Add(selected.StaleAfter).UTC()
	reason := "current sourced catalog observation; stock, quota, discounts, and final invoice remain separate"
	if !selected.DeploymentComparable() {
		reason = "current billing component or non-authoritative observation; it must not rank against a complete deployment total"
	}
	return PriceEvidence{State: "current", Currency: selected.Currency, HourlyUSDPerReplica: &hourly, CostScope: string(selected.CostScope), Authority: string(selected.Authority), Source: selected.Source, ObservedAt: &observed, ValidUntil: &valid, DeploymentComparable: selected.DeploymentComparable(), Reason: reason}
}

func (p Planner) selectPrice(provider, region, gpu string, gpuCount int, comparableOnly bool) (pricing.Estimate, bool) {
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	var selected pricing.Estimate
	selectedKey := ""
	found := false
	for request, estimate := range p.Prices {
		if request.Cloud != provider || request.GPUCount != gpuCount || request.Replicas != 1 || !provideridentity.MatchesGPU(provider, request.GPU, gpu) || estimate.Stale(now) || estimate.Hourly < 0 || math.IsNaN(estimate.Hourly) || math.IsInf(estimate.Hourly, 0) {
			continue
		}
		if region != "" && !strings.EqualFold(request.Region, region) {
			continue
		}
		if region == "" && request.Region != "global" {
			continue
		}
		if comparableOnly && (!estimate.DeploymentComparable() || !strings.EqualFold(estimate.Currency, "USD")) {
			continue
		}
		candidateKey := request.Region + "\x00" + request.GPU + "\x00" + estimate.Source
		if !found || estimate.Hourly < selected.Hourly || estimate.Hourly == selected.Hourly && (estimate.ObservedAt.After(selected.ObservedAt) || estimate.ObservedAt.Equal(selected.ObservedAt) && candidateKey < selectedKey) {
			selected, selectedKey, found = estimate, candidateKey, true
		}
	}
	return selected, found
}

func unavailablePrice(reason string) PriceEvidence {
	return PriceEvidence{State: "unavailable", DeploymentComparable: false, Reason: reason}
}

func baseArchitecture(model *Model) Architecture {
	nodes := []Node{{ID: "application", Kind: "client", Label: "Your application", State: "planned"}, {ID: "endpoint", Kind: "gateway", Label: "Stable OpenAI-compatible endpoint", State: "planned"}}
	edges := []Edge{{From: "application", To: "endpoint", Label: "inference request"}}
	if model != nil {
		nodes = append(nodes, Node{ID: "model", Kind: "artifact", Label: model.Repository + "@" + model.Revision, State: "reviewed"})
	}
	return Architecture{Nodes: nodes, Edges: edges}
}

func architecture(configuration Configuration, model *Model, providerReady bool) Architecture {
	result := baseArchitecture(model)
	providerState := "connection-required"
	if providerReady {
		providerState = "configured"
	}
	providerLabel := configuration.Provider
	if providerLabel == "" {
		providerLabel = "Choose provider"
	}
	result.Nodes = append(result.Nodes,
		Node{ID: "runtime", Kind: "runtime", Label: configuration.Runtime + " / " + configuration.Profile, State: "reviewed"},
		Node{ID: "compute", Kind: "compute", Label: providerLabel + " / " + configuration.GPU, State: providerState},
	)
	result.Edges = append(result.Edges,
		Edge{From: "endpoint", To: "runtime", Label: configuration.Routing},
		Edge{From: "compute", To: "runtime", Label: "hosts"},
		Edge{From: "runtime", To: "model", Label: "loads immutable artifact"},
	)
	return result
}

func profileOptions(profiles []curatedrecipe.ServingProfile) []Option {
	items := append([]curatedrecipe.ServingProfile(nil), profiles...)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	options := make([]Option, 0, min(8, len(items)))
	for _, profile := range items {
		if len(options) == 8 {
			break
		}
		options = append(options, Option{Value: profile.Name, Label: profile.DisplayName, State: profile.EvidenceClass, Reason: profile.Description})
	}
	return options
}

func gpuOptions(profile curatedrecipe.ServingProfile) []Option {
	values := append([]string(nil), profile.CompatibleGPUs...)
	if len(values) == 0 && profile.GPUHint != "" {
		values = []string{profile.GPUHint}
	}
	sort.Strings(values)
	options := make([]Option, 0, len(values))
	for _, value := range values {
		options = append(options, Option{Value: value, Label: value, State: "configuration-reviewed"})
	}
	return options
}

func canonicalProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "amazon", "amazon web services", "aws-ec2":
		return "aws"
	case "google", "google cloud", "gcp-compute":
		return "gcp"
	case "runpod pods", "runpod-pods":
		return "runpod"
	case "k8s":
		return "kubernetes"
	default:
		return value
	}
}

func providerFromIntent(intent string) string {
	words := normalizedWords(intent)
	candidates := make([]string, 0, 2)
	for _, candidate := range []string{"aws", "gcp", "runpod", "kubernetes"} {
		if hasWord(words, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if hasWord(words, "google") && hasWord(words, "cloud") {
		candidates = append(candidates, "gcp")
	}
	sort.Strings(candidates)
	candidates = unique(candidates)
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func providerLabel(provider Provider) string {
	if provider.Label != "" {
		return provider.Label
	}
	if provider.ID == "" {
		return "provider"
	}
	return provider.ID
}

func normalizeIdentity(value string) string {
	var out strings.Builder
	for _, char := range strings.ToLower(value) {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			out.WriteRune(char)
		}
	}
	return out.String()
}

func normalizedWords(value string) map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(value), func(char rune) bool { return !unicode.IsLetter(char) && !unicode.IsDigit(char) }) {
		if word != "" {
			words[word] = true
		}
	}
	return words
}

func hasWord(words map[string]bool, value string) bool { return words[value] }

func unique(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
