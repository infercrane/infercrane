package main

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/spf13/cobra"
)

// Shell completion suggests real names and IDs from the control plane:
// deployments, endpoints, optimization runs, models, GPUs, and regions. It
// only reads, it is bounded by a short timeout, and it is silent: an offline
// or signed-out CLI completes nothing rather than printing an error.

const completionTimeout = 1500 * time.Millisecond

// completionSource reads one control-plane collection for completion.
type completionSource struct {
	ctx context.Context
	cfg config.Config
	ok  bool
}

func newCompletionSource(cmd *cobra.Command) (completionSource, context.CancelFunc) {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, completionTimeout)
	cfg, err := config.LoadClient()
	return completionSource{ctx: ctx, cfg: cfg, ok: err == nil && cfg.APIKey != ""}, cancel
}

func (s completionSource) get(path string, out any) bool {
	if !s.ok {
		return false
	}
	return controlJSONWithTimeout(s.ctx, s.cfg, http.MethodGet, path, "", nil, out, completionTimeout) == nil
}

func (s completionSource) deployments(prefix string) []string {
	var response struct {
		Data []deploymentSummary `json:"data"`
	}
	if !s.get("/api/v1/deployments", &response) {
		return nil
	}
	values := make([]string, 0, len(response.Data))
	for _, deployment := range response.Data {
		values = append(values, completionValue(deployment.Name, emptyAs(deployment.ObservedState, "unknown"), deployment.Model))
	}
	return matching(values, prefix)
}

func (s completionSource) endpoints(prefix string) []string {
	var response struct {
		Data []endpointView `json:"data"`
	}
	if !s.get("/api/v1/endpoints", &response) {
		return nil
	}
	values := make([]string, 0, len(response.Data))
	for _, endpoint := range response.Data {
		values = append(values, completionValue(endpoint.Name, emptyAs(endpoint.ObservedState, "unknown")))
	}
	return matching(values, prefix)
}

// campaigns completes optimization run IDs, described by model and state.
func (s completionSource) campaigns(prefix string) []string {
	var response struct {
		Data []optimizationCampaignView `json:"data"`
	}
	if !s.get("/api/v1/optimization/campaigns?limit=50", &response) {
		return nil
	}
	values := make([]string, 0, len(response.Data))
	for _, campaign := range response.Data {
		values = append(values, completionValue(campaign.ID, emptyAs(campaign.ModelIdentity, "unknown model"), strings.ReplaceAll(runStateWord(emptyAs(campaign.State, "unknown")), "_", " ")))
	}
	return matching(values, prefix)
}

// candidates completes the setup IDs of one run, for --candidate.
func (s completionSource) candidates(campaign, prefix string) []string {
	var response struct {
		Campaign optimizationCampaignView `json:"campaign"`
	}
	if campaign == "" || !s.get("/api/v1/optimization/campaigns/"+url.PathEscape(campaign), &response) {
		return nil
	}
	values := make([]string, 0, len(response.Campaign.Candidates))
	for _, candidate := range response.Campaign.Candidates {
		values = append(values, completionValue(candidate.ID, strings.ReplaceAll(setupStateWord(emptyAs(candidate.State, "unknown"), candidate.Control != nil && *candidate.Control), "_", " ")))
	}
	return matching(values, prefix)
}

func (s completionSource) models(prefix string) []string {
	var response struct {
		Data []struct {
			Model       string `json:"model"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if !s.get("/api/v1/catalog/models", &response) {
		return nil
	}
	seen := map[string]bool{}
	values := make([]string, 0, len(response.Data))
	for _, entry := range response.Data {
		if entry.Model == "" || seen[entry.Model] {
			continue
		}
		seen[entry.Model] = true
		values = append(values, completionValue(entry.Model, entry.DisplayName))
	}
	return matching(values, prefix)
}

// offers completes one field of the sourced GPU price catalog: provider, gpu,
// or region, narrowed by the provider and GPU already on the command line.
// The description lists where the value is offered; it says nothing about
// stock or price.
func (s completionSource) offers(field, provider, gpu, prefix string) []string {
	query := url.Values{"limit": {"5000"}}
	if provider != "" && field != "provider" {
		query.Set("provider", provider)
	}
	if gpu != "" && field == "region" {
		query.Set("gpu", gpu)
	}
	var response struct {
		Data []struct {
			Provider string `json:"provider"`
			Region   string `json:"region"`
			GPU      string `json:"gpu"`
		} `json:"data"`
	}
	if !s.get("/api/v1/catalog/gpu-prices?"+query.Encode(), &response) {
		return nil
	}
	where := map[string]map[string]bool{}
	for _, row := range response.Data {
		value, place := row.GPU, row.Provider
		switch field {
		case "provider":
			value, place = row.Provider, ""
		case "region":
			value = row.Region
		}
		if value == "" {
			continue
		}
		if where[value] == nil {
			where[value] = map[string]bool{}
		}
		if place != "" {
			where[value][place] = true
		}
	}
	values := make([]string, 0, len(where))
	for value, places := range where {
		names := make([]string, 0, len(places))
		for place := range places {
			names = append(names, place)
		}
		sort.Strings(names)
		values = append(values, completionValue(value, strings.Join(names, ", ")))
	}
	return matching(values, prefix)
}

// completionValue is "value<TAB>description", which zsh and fish show beside
// the value. Empty description parts are left out.
func completionValue(value string, description ...string) string {
	parts := make([]string, 0, len(description))
	for _, part := range description {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return value
	}
	return value + "\t" + strings.Join(parts, " · ")
}

func matching(values []string, prefix string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		name, _, _ := strings.Cut(value, "\t")
		if strings.HasPrefix(name, prefix) {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func staticValues(prefix string, values ...string) []string {
	return matching(values, prefix)
}

// flagValue returns the value of --name already typed in args.
func flagValue(args []string, name string) string {
	for index, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--"+name+"="); ok {
			return value
		}
		if arg == "--"+name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

// pendingFlag reports the flag whose value is being typed: "--gpu <TAB>" or
// "--gpu=L<TAB>". Boolean flags take no value.
func pendingFlag(cmd *cobra.Command, args []string, toComplete string) (name, prefix, valuePrefix string, ok bool) {
	if flag, value, found := strings.Cut(toComplete, "="); found && strings.HasPrefix(flag, "--") {
		return strings.TrimPrefix(flag, "--"), flag + "=", value, true
	}
	if len(args) == 0 || strings.HasPrefix(toComplete, "-") {
		return "", "", "", false
	}
	last := args[len(args)-1]
	if !strings.HasPrefix(last, "--") || strings.Contains(last, "=") {
		return "", "", "", false
	}
	name = strings.TrimPrefix(last, "--")
	if flag := cmd.Flags().Lookup(name); flag == nil || flag.Value.Type() == "bool" {
		return "", "", "", false
	}
	return name, "", toComplete, true
}

// positional drops flags and their values, leaving the typed arguments.
func positional(cmd *cobra.Command, args []string) []string {
	var out []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			if name := strings.TrimLeft(arg, "-"); !strings.Contains(name, "=") {
				if flag := cmd.Flags().Lookup(name); flag != nil && flag.Value.Type() != "bool" {
					index++
				}
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

// completeFlagValue completes the value of a flag. It returns false when the
// flag has no known values, so the shell falls back to files.
func completeFlagValue(s completionSource, command string, args []string, name, prefix string) ([]string, bool) {
	provider := flagValue(args, "provider")
	if provider == "" {
		provider = flagValue(args, "cloud")
	}
	switch name {
	case "output":
		return staticValues(prefix, "human", "json"), true
	case "objective":
		if command == "autopilot" {
			return nil, false
		}
		return staticValues(prefix, "interactive", "latency", "throughput", "cost-efficiency"), true
	case "compute":
		return staticValues(prefix, "elastic", "serverless"), true
	case "strategy":
		return staticValues(prefix, "round-robin", "consistent-hash", "power-of-two", "cache-aware"), true
	case "gpu":
		return s.offers("gpu", provider, "", prefix), true
	case "region":
		return s.offers("region", provider, flagValue(args, "gpu"), prefix), true
	case "provider", "cloud":
		if command == "artifact" || command == "sandbox" || command == "training" {
			return nil, false
		}
		return s.offers("provider", "", "", prefix), true
	case "target-deployment":
		return s.deployments(prefix), true
	case "deployment":
		if command == "endpoint" {
			return s.deployments(prefix), true
		}
	case "candidate":
		if command == "optimize" {
			return s.candidates(campaignArgument(args), prefix), true
		}
	}
	return nil, false
}

// campaignArgument is the run ID in "optimize ACTION RUN".
func campaignArgument(args []string) string {
	if len(args) >= 2 && !strings.HasPrefix(args[1], "-") {
		return args[1]
	}
	return ""
}

var optimizeActions = []string{"propose", "create", "list", "inspect", "results", "watch", "report", "approve", "activate", "cancel", "doctor", "workload-profile", "evidence", "kernel-run", "kernel-plan"}

var optimizeCampaignActions = map[string]bool{"inspect": true, "results": true, "watch": true, "report": true, "approve": true, "activate": true, "cancel": true}

// completePositional completes the next positional argument of command.
func completePositional(s completionSource, command string, typed []string, prefix string) ([]string, cobra.ShellCompDirective) {
	none := cobra.ShellCompDirectiveNoFileComp
	switch command {
	case "optimize":
		switch {
		case len(typed) == 0:
			return staticValues(prefix, optimizeActions...), none
		case len(typed) == 1 && optimizeCampaignActions[typed[0]]:
			return s.campaigns(prefix), none
		case len(typed) == 1 && (typed[0] == "propose" || typed[0] == "create"):
			return s.models(prefix), none
		}
		return nil, cobra.ShellCompDirectiveDefault
	case "deploy", "plan", "apply":
		if len(typed) == 0 {
			// A model or a spec file: offer catalog models, and files when none match.
			return s.models(prefix), cobra.ShellCompDirectiveDefault
		}
		return nil, cobra.ShellCompDirectiveDefault
	case "open":
		switch {
		case len(typed) == 0:
			return staticValues(prefix, "endpoint", "deployment", "run", "model", "operation"), none
		case len(typed) == 1 && typed[0] == "endpoint":
			return s.endpoints(prefix), none
		case len(typed) == 1 && typed[0] == "deployment":
			return s.deployments(prefix), none
		case len(typed) == 1 && typed[0] == "run":
			return s.campaigns(prefix), none
		case len(typed) == 1 && typed[0] == "model":
			return s.models(prefix), none
		}
		return nil, none
	case "endpoint":
		if len(typed) == 0 {
			return staticValues(prefix, "list", "inspect", "create", "bind", "plan", "guard", "stage", "promote", "delete"), none
		}
		if len(typed) == 1 && typed[0] != "list" && typed[0] != "create" {
			return s.endpoints(prefix), none
		}
		return nil, none
	case "request", "status", "observe", "logs", "events", "inspect", "explain", "benchmark", "passport", "recommend", "slo", "delete", "route", "rollout":
		if len(typed) == 0 {
			return s.deployments(prefix), none
		}
		return nil, none
	}
	return nil, cobra.ShellCompDirectiveDefault
}
