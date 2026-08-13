package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpNamedInput struct {
	Name string `json:"name" jsonschema:"deployment or endpoint name"`
}

type mcpRequestInput struct {
	RequestID string `json:"request_id" jsonschema:"InferCrane request identifier"`
}

type mcpOperationInput struct {
	OperationID string `json:"operation_id" jsonschema:"durable InferCrane operation identifier"`
}

type mcpSearchInput struct {
	Query string `json:"query,omitempty" jsonschema:"optional model, runtime, protocol, or use-case search"`
}

type mcpOutput struct {
	Data any `json:"data"`
}

type controlReader func(context.Context, string, any) error

func mcpCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: infercrane mcp")
	}
	reader := func(ctx context.Context, path string, output any) error {
		return controlJSON(ctx, cfg, http.MethodGet, path, "", nil, output)
	}
	return newMCPServer(reader).Run(ctx, &mcp.StdioTransport{})
}

func newMCPServer(read controlReader) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "infercrane", Version: version}, &mcp.ServerOptions{Instructions: "Read-only access to persisted InferCrane operational evidence. No tool mutates deployments, traffic, budgets, secrets, or provider resources."})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(false)}
	mcp.AddTool(server, &mcp.Tool{Name: "infercrane_list_deployments", Title: "List InferCrane deployments", Description: "List logical deployments and observed lifecycle state from the configured control plane.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcpOutput, error) {
		var response struct {
			Data []deploymentSummary `json:"data"`
		}
		err := read(ctx, "/api/v1/deployments", &response)
		return nil, mcpOutput{Data: response.Data}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "infercrane_inspect_deployment", Title: "Inspect deployment evidence", Description: "Read desired/observed state, revisions, replicas, operations, measurements, and Release Guard evidence for one deployment.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpNamedInput) (*mcp.CallToolResult, mcpOutput, error) {
		var response map[string]any
		err := read(ctx, "/api/v1/deployments/"+url.PathEscape(input.Name), &response)
		return nil, mcpOutput{Data: response}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "infercrane_inspect_endpoint", Title: "Inspect stable endpoint", Description: "Read logical model, environment, bindings, active plan, and candidate plan for one stable application endpoint.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpNamedInput) (*mcp.CallToolResult, mcpOutput, error) {
		var response map[string]any
		err := read(ctx, "/api/v1/endpoints/"+url.PathEscape(input.Name), &response)
		return nil, mcpOutput{Data: response}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "infercrane_inspect_request", Title: "Inspect inference request", Description: "Read content-free routing, latency, token, retry, fallback, deployment, and revision evidence for one request.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpRequestInput) (*mcp.CallToolResult, mcpOutput, error) {
		var response map[string]any
		err := read(ctx, "/api/v1/requests/"+url.PathEscape(input.RequestID), &response)
		return nil, mcpOutput{Data: response}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "infercrane_inspect_operation", Title: "Inspect durable operation", Description: "Read the state and resume identity of one durable control-plane operation.", Annotations: readOnly}, func(ctx context.Context, _ *mcp.CallToolRequest, input mcpOperationInput) (*mcp.CallToolResult, mcpOutput, error) {
		var response map[string]any
		err := read(ctx, "/api/v1/operations/"+url.PathEscape(input.OperationID), &response)
		return nil, mcpOutput{Data: response}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "infercrane_list_curated_recipes", Title: "Search curated inference recipes", Description: "Search reviewed, commit-pinned configuration recipes. Results are configuration-only, never benchmark claims.", Annotations: readOnly}, func(_ context.Context, _ *mcp.CallToolRequest, input mcpSearchInput) (*mcp.CallToolResult, mcpOutput, error) {
		return nil, mcpOutput{Data: curatedrecipe.Search(input.Query)}, nil
	})
	return server
}

func boolPointer(value bool) *bool { return &value }
