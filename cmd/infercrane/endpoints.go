package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

type endpointView struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	LogicalModelID         string `json:"logical_model_id"`
	EnvironmentID          string `json:"environment_id"`
	DesiredState           string `json:"desired_state"`
	ObservedState          string `json:"observed_state"`
	ActiveServingPlanID    string `json:"active_serving_plan_id"`
	CandidateServingPlanID string `json:"candidate_serving_plan_id"`
}

func environmentCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("environment requires list, create, or promote")
	}
	if args[0] == "promote" {
		return environmentPromoteCommand(ctx, cfg, args[1:])
	}
	fs := flag.NewFlagSet("environment "+args[0], flag.ContinueOnError)
	policy := fs.String("policy", "{}", "bounded environment policy JSON")
	output := fs.String("output", "human", "human or json")
	resource := ""
	flagArgs := args[1:]
	if args[0] == "create" && len(args) > 1 {
		resource, flagArgs = args[1], args[2:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if args[0] == "list" {
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/environments", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Data)
		}
		return printNamedResources("ENVIRONMENT", response.Data)
	}
	if args[0] != "create" || resource == "" || fs.NArg() != 0 {
		return errors.New("usage: infercrane environment create NAME [--policy JSON]")
	}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/environments", "", map[string]any{"name": resource, "policy": jsonValue(*policy)}, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("environment %s created\n", resource)
	return nil
}

func environmentPromoteCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane environment promote SOURCE_ENDPOINT --to DESTINATION_ENDPOINT [--yes]")
	}
	source := args[0]
	fs := flag.NewFlagSet("environment promote", flag.ContinueOnError)
	destination := fs.String("to", "", "destination endpoint")
	confirm := fs.Bool("yes", false, "stage the destination candidate")
	idempotencyKey := fs.String("idempotency-key", "", "stable safe-retry key")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if fs.NArg() != 0 || *destination == "" || source == *destination {
		return errors.New("promotion requires distinct source and destination endpoints")
	}
	var sourceView, destinationView map[string]any
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(source), "", nil, &sourceView); err != nil {
		return fmt.Errorf("inspect source endpoint: %w", err)
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(*destination), "", nil, &destinationView); err != nil {
		return fmt.Errorf("inspect destination endpoint: %w", err)
	}
	sourceModel := nestedString(sourceView, "logical_model", "name")
	destinationModel := nestedString(destinationView, "logical_model", "name")
	sourceEnvironment := nestedString(sourceView, "environment", "name")
	destinationEnvironment := nestedString(destinationView, "environment", "name")
	sourcePlan := nestedString(sourceView, "active_plan", "id")
	destinationPlan := nestedString(destinationView, "active_plan", "id")
	if sourceModel == "" || sourceModel != destinationModel {
		return errors.New("source and destination must use the same logical model")
	}
	plan := map[string]any{"source_endpoint": source, "source_environment": sourceEnvironment, "source_plan": sourcePlan, "destination_endpoint": *destination, "destination_environment": destinationEnvironment, "destination_active_plan": destinationPlan, "action": "atomically stage source plan as destination candidate", "activation": "requires destination Release Guard PASS"}
	if !*confirm {
		if *output == "json" {
			return printJSON(map[string]any{"plan": plan, "mutation": false})
		}
		fmt.Printf("Environment promotion plan\n\nLogical model  %s\nSource         %s (%s) · plan %s\nDestination    %s (%s) · active %s\n\n+ clone source bindings into destination\n+ stage immutable destination candidate\n= keep destination active plan serving\n= require destination Release Guard PASS before activation\n\nNo mutation performed. Apply with:\n  infercrane environment promote %s --to %s --yes\n", sourceModel, source, sourceEnvironment, shortValue(sourcePlan), *destination, destinationEnvironment, shortValue(destinationPlan), source, *destination)
		return nil
	}
	if *idempotencyKey == "" {
		*idempotencyKey = "environment-promotion-" + source + "-to-" + *destination + "-" + sourcePlan
	}
	var response map[string]any
	body := map[string]string{"source_endpoint": source, "destination_endpoint": *destination, "idempotency_key": *idempotencyKey}
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/environment-promotions", *idempotencyKey, body, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	promotion, _ := response["promotion"].(map[string]any)
	fmt.Printf("Environment candidate staged\nLogical model  %s\nSource         %s (%s)\nDestination    %s (%s)\nCandidate      %s\n\nProduction traffic did not change. Next:\n  infercrane endpoint guard %s --evaluate\n  infercrane endpoint promote %s %v\n", sourceModel, source, sourceEnvironment, *destination, destinationEnvironment, shortValue(fmt.Sprint(promotion["destination_plan_id"])), *destination, *destination, promotion["destination_plan_id"])
	return nil
}

func nestedString(value map[string]any, object, key string) string {
	nested, _ := value[object].(map[string]any)
	if nested == nil || nested[key] == nil {
		return ""
	}
	return fmt.Sprint(nested[key])
}

func logicalModelCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("logical-model requires list or create")
	}
	fs := flag.NewFlagSet("logical-model "+args[0], flag.ContinueOnError)
	description := fs.String("description", "", "logical model description")
	output := fs.String("output", "human", "human or json")
	resource := ""
	flagArgs := args[1:]
	if args[0] == "create" && len(args) > 1 {
		resource, flagArgs = args[1], args[2:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if args[0] == "list" {
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/logical-models", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Data)
		}
		return printNamedResources("LOGICAL MODEL", response.Data)
	}
	if args[0] != "create" || resource == "" || fs.NArg() != 0 {
		return errors.New("usage: infercrane logical-model create NAME [--description TEXT]")
	}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/logical-models", "", map[string]string{"name": resource, "description": *description}, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("logical model %s created\n", resource)
	return nil
}

func endpointCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("endpoint requires list, inspect, create, bind, plan, stage, or promote")
	}
	action := args[0]
	fs := flag.NewFlagSet("endpoint "+action, flag.ContinueOnError)
	model := fs.String("model", "", "logical model name")
	environment := fs.String("environment", "production", "environment name")
	bindingName := fs.String("name", "primary", "binding name")
	deployment := fs.String("deployment", "", "deployment name")
	target := fs.String("target", "", "external target name")
	connection := fs.String("connection", "", "configured external provider connection")
	ownership := fs.String("ownership", "lifecycle-managed", "observe-only, traffic-managed, or lifecycle-managed")
	externalAdapter := fs.String("external-adapter", "", "openrouter or openai-compatible-external")
	secretReference := fs.String("secret-reference", "", "secret reference ID for an authenticated external API")
	requestLimit := fs.Int64("request-limit", 0, "hard external request reservation limit")
	costLimit := fs.String("cost-limit-usd", "", "hard external USD reservation budget")
	maxRequestCost := fs.String("max-request-cost-usd", "", "worst-case external USD reserved per request")
	acknowledgeExternal := fs.Bool("acknowledge-external-data", false, "acknowledge prompts and outputs leave controlled infrastructure")
	enableExternal := fs.Bool("enable-external", false, "enable authenticated external binding after validation")
	policy := fs.String("policy", "manual", "manual, primary-fallback, or weighted")
	bindings := fs.String("bindings", "", "ordered binding names, optionally NAME:WEIGHT")
	evaluateGuard := fs.Bool("evaluate", false, "evaluate active and candidate plans")
	guardWindow := fs.Duration("window", time.Hour, "persisted telemetry window")
	disableGuard := fs.Bool("disable", false, "disable endpoint Release Guard policy")
	confirmDelete := fs.Bool("yes", false, "confirm endpoint deletion")
	minimumRequests := fs.Int("minimum-requests", 0, "minimum requests required for each plan")
	maxTTFTRegression := fs.Float64("max-ttft-regression", -1, "maximum candidate TTFT regression percent")
	output := fs.String("output", "human", "human or json")
	resource, planID := "", ""
	flagArgs := args[1:]
	switch action {
	case "inspect", "create", "bind", "plan", "guard", "delete":
		if len(args) > 1 {
			resource, flagArgs = args[1], args[2:]
		}
	case "stage", "promote":
		if len(args) > 2 {
			resource, planID, flagArgs = args[1], args[2], args[3:]
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	switch action {
	case "list":
		var response struct {
			Data []endpointView `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints", "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response.Data)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ENDPOINT\tSTATE\tACTIVE PLAN\tCANDIDATE")
		for _, item := range response.Data {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Name, item.ObservedState, shortValue(item.ActiveServingPlanID), shortValue(item.CandidateServingPlanID))
		}
		return w.Flush()
	case "inspect":
		if resource == "" || fs.NArg() != 0 {
			return errors.New("usage: infercrane endpoint inspect NAME")
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+resource, "", nil, &response); err != nil {
			return err
		}
		return printJSON(response)
	case "create":
		if resource == "" || fs.NArg() != 0 || *model == "" {
			return errors.New("usage: infercrane endpoint create NAME --model LOGICAL_MODEL [--environment production]")
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/endpoints", "", map[string]string{"name": resource, "logical_model": *model, "environment": *environment}, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("endpoint %s created · add a binding, then create its first plan\n", resource)
		return nil
	case "bind":
		sources := 0
		for _, source := range []string{*deployment, *target, *connection} {
			if source != "" {
				sources++
			}
		}
		if resource == "" || fs.NArg() != 0 || sources != 1 {
			return errors.New("usage: infercrane endpoint bind ENDPOINT --name NAME (--deployment NAME | --target NAME | --connection NAME) [policy flags]")
		}
		kind := "deployment"
		if *target != "" || *connection != "" {
			kind = "external"
		}
		config := map[string]any{}
		if *connection != "" || *externalAdapter != "" || *secretReference != "" || *enableExternal {
			if kind != "external" || *requestLimit < 1 || *costLimit == "" || *maxRequestCost == "" || (*connection == "" && (*externalAdapter == "" || *secretReference == "")) {
				return errors.New("authenticated external binding requires --connection (or --target with adapter and secret reference), request and cost limits")
			}
			if *enableExternal && !*acknowledgeExternal {
				return errors.New("--enable-external requires --acknowledge-external-data")
			}
			costMicrousd, err := parseMicrousd(*costLimit)
			if err != nil {
				return fmt.Errorf("--cost-limit-usd: %w", err)
			}
			maxMicrousd, err := parseMicrousd(*maxRequestCost)
			if err != nil {
				return fmt.Errorf("--max-request-cost-usd: %w", err)
			}
			config = map[string]any{"enabled": *enableExternal, "privacy_acknowledged": *acknowledgeExternal, "request_limit": *requestLimit, "cost_limit_microusd": costMicrousd, "max_request_cost_microusd": maxMicrousd}
			if *connection == "" {
				config["adapter"] = *externalAdapter
				config["secret_reference_id"] = *secretReference
			}
		}
		if *connection != "" {
			*ownership = "traffic-managed"
		}
		request := map[string]any{"name": *bindingName, "kind": kind, "ownership_mode": *ownership, "deployment": *deployment, "target": *target, "provider_connection": *connection, "config": config}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/endpoints/"+resource+"/bindings", "", request, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("binding %s added to %s\n", *bindingName, resource)
		return nil
	case "plan":
		if resource == "" || fs.NArg() != 0 || *bindings == "" {
			return errors.New("usage: infercrane endpoint plan ENDPOINT --policy POLICY --bindings NAME[:WEIGHT],...")
		}
		parsed, err := parsePlanBindings(*bindings)
		if err != nil {
			return err
		}
		var response map[string]any
		if err = controlJSON(ctx, cfg, http.MethodPost, "/api/v1/endpoints/"+resource+"/plans", "", map[string]any{"routing_policy": *policy, "bindings": parsed}, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("serving plan created for %s · %v\n", resource, response["slot"])
		return nil
	case "stage", "promote":
		if resource == "" || planID == "" || fs.NArg() != 0 {
			return fmt.Errorf("usage: infercrane endpoint %s ENDPOINT PLAN_ID", action)
		}
		slot := "candidate"
		if action == "promote" {
			slot = "active"
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodPut, "/api/v1/endpoints/"+resource+"/plans/"+planID+"/"+slot, "", map[string]any{}, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("endpoint %s %s plan %s\n", resource, action, shortValue(planID))
		return nil
	case "guard":
		if resource == "" || fs.NArg() != 0 || *guardWindow <= 0 {
			return errors.New("usage: infercrane endpoint guard ENDPOINT [--evaluate] [--minimum-requests N --max-ttft-regression PERCENT]")
		}
		policyPath := "/api/v1/endpoints/" + resource + "/release-guard/policy"
		var policyResponse struct {
			Policy map[string]any `json:"policy"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, policyPath, "", nil, &policyResponse); err != nil {
			return err
		}
		policyChanged := *disableGuard || *minimumRequests > 0 || *maxTTFTRegression >= 0
		if policyChanged {
			if *disableGuard {
				policyResponse.Policy["enabled"] = false
			}
			if *minimumRequests > 0 {
				policyResponse.Policy["minimum_requests"] = *minimumRequests
			}
			if *maxTTFTRegression >= 0 {
				policyResponse.Policy["max_ttft_regression_percent"] = *maxTTFTRegression
			}
			if err := controlJSON(ctx, cfg, http.MethodPut, policyPath, "", policyResponse.Policy, &policyResponse); err != nil {
				return err
			}
		}
		if *evaluateGuard {
			var response map[string]any
			if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/endpoints/"+resource+"/release-guard/evaluate", "", map[string]any{"window_seconds": int(guardWindow.Seconds())}, &response); err != nil {
				return err
			}
			if *output == "json" {
				return printJSON(response)
			}
			evaluation, _ := response["evaluation"].(map[string]any)
			fmt.Printf("Release Guard  %v\nEndpoint       %s\nActive plan    %s\nCandidate      %s\n", evaluation["decision"], resource, shortValue(fmt.Sprint(evaluation["active_serving_plan_id"])), shortValue(fmt.Sprint(evaluation["candidate_serving_plan_id"])))
			return nil
		}
		var history struct {
			Data []map[string]any `json:"data"`
		}
		if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+resource+"/release-guard/evaluations", "", nil, &history); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(map[string]any{"policy": policyResponse.Policy, "evaluations": history.Data})
		}
		decision := "no evaluation"
		if len(history.Data) > 0 {
			decision = fmt.Sprint(history.Data[0]["decision"])
		}
		fmt.Printf("Endpoint       %s\nRelease Guard  %s\nMinimum data   %v requests per plan\n", resource, decision, policyResponse.Policy["minimum_requests"])
		return nil
	case "delete":
		if resource == "" || fs.NArg() != 0 || !*confirmDelete {
			return errors.New("usage: infercrane endpoint delete ENDPOINT --yes")
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodDelete, "/api/v1/endpoints/"+resource, "", nil, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("endpoint %s deleted · bound deployments were not deleted\n", resource)
		return nil
	default:
		return fmt.Errorf("unknown endpoint action %q", action)
	}
}

func parsePlanBindings(value string) ([]map[string]any, error) {
	parts := strings.Split(value, ",")
	out := make([]map[string]any, 0, len(parts))
	for priority, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if fields[0] == "" || len(fields) > 2 {
			return nil, errors.New("bindings must use NAME or NAME:WEIGHT")
		}
		weight := 100
		if len(fields) == 2 {
			parsed, err := strconv.Atoi(fields[1])
			if err != nil || parsed < 1 || parsed > 10000 {
				return nil, fmt.Errorf("invalid binding weight %q", fields[1])
			}
			weight = parsed
		}
		out = append(out, map[string]any{"name": fields[0], "priority": priority, "weight": weight})
	}
	return out, nil
}
func jsonValue(value string) any { return jsonRaw(value) }

type jsonRaw string

func (j jsonRaw) MarshalJSON() ([]byte, error) { return []byte(j), nil }
func printNamedResources(title string, items []map[string]any) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "%s\tID\n", title)
	for _, item := range items {
		fmt.Fprintf(w, "%v\t%v\n", item["name"], item["id"])
	}
	return w.Flush()
}
func shortValue(value string) string {
	if value == "" {
		return "—"
	}
	if len(value) > 12 {
		return value[:12] + "…"
	}
	return value
}
