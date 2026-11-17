package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/infercrane/infercrane/internal/config"
)

const connectUsage = "usage: infercrane connect URL --as NAME [--model UPSTREAM_MODEL] [--type auto|vllm|litellm|openai-compatible] [--token-env VAR]"

func connectCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New(connectUsage)
	}
	rawURL := args[0]
	fs := newFlagSet("connect")
	name := fs.String("as", "", "stable endpoint and logical model name")
	upstreamModel := fs.String("model", "", "physical upstream model; discovered when omitted")
	typeName := fs.String("type", "auto", "auto, vllm, litellm, or openai-compatible")
	manageTraffic := fs.Bool("manage-traffic", false, "route requests through InferCrane after qualification")
	tokenEnv := fs.String("token-env", "", "environment variable holding the server's API key; it is sent once, stored encrypted, and used for discovery and health checks")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *name == "" {
		return errors.New(connectUsage)
	}
	if err := checkReachableFromControlPlane(rawURL, cfg.ControlURL); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	if *typeName != "auto" && *typeName != "vllm" && *typeName != "litellm" && *typeName != "openai-compatible" {
		return errors.New("--type must be auto, vllm, litellm, or openai-compatible")
	}
	ownership := "observe-only"
	if *manageTraffic {
		ownership = "traffic-managed"
	}
	request := map[string]any{"name": *name, "logical_model": *name, "upstream_model": *upstreamModel, "url": rawURL, "source": "openai-compatible", "ownership_mode": ownership, "runtime": "openai-compatible", "discover": true, "connector": *typeName}
	if *tokenEnv != "" {
		token := strings.TrimSpace(os.Getenv(*tokenEnv))
		if token == "" {
			return fmt.Errorf("environment variable %s is empty or unset", *tokenEnv)
		}
		request["upstream_credential"] = token
	}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/adoptions/endpoints", "", request, &response); err != nil {
		var controlErr *ControlError
		if errors.As(err, &controlErr) && controlErr.Code == "upstream_credential_unavailable" {
			return fmt.Errorf("%s cannot store server credentials. Keep the key on the server's machine instead:\n  %s=... infercrane agent run --server %s --token-env %s", cfg.ControlURL, *tokenEnv, rawURL, *tokenEnv)
		}
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	discovery, _ := response["discovery"].(map[string]any)
	fmt.Printf("Connected %s\n\nUpstream      %s\nRuntime       %v\nModel         %v\nOwnership     %s\nHealth        %v\n", *name, rawURL, discovery["runtime"], discovery["model"], ownership, discovery["health"])
	if *tokenEnv != "" {
		fmt.Println("Credential    stored encrypted; used for discovery and health checks, never shown again")
	}
	fmt.Printf("\nApplication endpoint\n%s/v1\n\nNext\n  infercrane doctor %s\n  infercrane request %s\n", strings.TrimSuffix(cfg.ControlURL, "/"), *name, *name)
	if !*manageTraffic {
		fmt.Printf("  infercrane adopt promote %s --ownership traffic-managed\n", *name)
	}
	return nil
}

// checkReachableFromControlPlane refuses a loopback server URL when the
// control plane runs elsewhere: it would probe its own machine, not this one.
func checkReachableFromControlPlane(serverURL, controlURL string) error {
	server, err := url.Parse(serverURL)
	if err != nil {
		return nil
	}
	control, err := url.Parse(controlURL)
	if err != nil {
		return nil
	}
	if isLoopbackHost(server.Hostname()) && !isLoopbackHost(control.Hostname()) {
		return fmt.Errorf("%s is on this machine, and the control plane at %s cannot reach it. Connect it through the agent instead, which only dials out:\n  infercrane agent run --server %s", serverURL, controlURL, serverURL)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func adoptCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) >= 2 && args[0] == "promote" {
		fs := newFlagSet("adopt promote")
		ownership := fs.String("ownership", "traffic-managed", "traffic-managed")
		output := fs.String("output", "human", "human or json")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: infercrane adopt promote NAME --ownership traffic-managed")
		}
		var response map[string]any
		if err := controlJSON(ctx, cfg, http.MethodPut, "/api/v1/adoptions/endpoints/"+url.PathEscape(args[1])+"/ownership", "", map[string]string{"ownership_mode": *ownership}, &response); err != nil {
			return err
		}
		if *output == "json" {
			return printJSON(response)
		}
		fmt.Printf("endpoint %s promoted to traffic-managed; provider lifecycle remains external\n", args[1])
		return nil
	}
	if len(args) < 2 || args[0] != "endpoint" {
		return errors.New("usage: infercrane adopt endpoint NAME --url URL --model LOGICAL_MODEL [--ownership observe-only]")
	}
	name := args[1]
	fs := newFlagSet("adopt endpoint")
	rawURL := fs.String("url", "", "existing OpenAI-compatible base URL")
	model := fs.String("model", "", "stable logical model name")
	upstreamModel := fs.String("upstream-model", "", "physical model name exposed by the workload")
	source := fs.String("source", "vllm", "vllm or openai-compatible")
	ownership := fs.String("ownership", "observe-only", "observe-only or traffic-managed")
	runtimeName := fs.String("runtime", "vllm", "qualified runtime inspector")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || name == "" || *rawURL == "" || *model == "" {
		return errors.New("usage: infercrane adopt endpoint NAME --url URL --model LOGICAL_MODEL [--ownership observe-only]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	request := map[string]string{"name": name, "logical_model": *model, "upstream_model": *upstreamModel, "url": *rawURL, "source": *source, "ownership_mode": *ownership, "runtime": *runtimeName}
	var response map[string]any
	if err := controlJSON(ctx, cfg, http.MethodPost, "/api/v1/adoptions/endpoints", "", request, &response); err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(response)
	}
	fmt.Printf("endpoint %s adopted · ownership %s · no provider lifecycle transferred\n", name, *ownership)
	if *ownership == "observe-only" {
		fmt.Println("traffic is unchanged; promote ownership to traffic-managed explicitly before routing through InferCrane")
	}
	return nil
}
