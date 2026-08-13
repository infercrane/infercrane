package main

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPServerExposesOnlyReadOnlyEvidenceTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	reader := func(_ context.Context, path string, output any) error {
		if path != "/api/v1/deployments" {
			return errors.New("unexpected path")
		}
		response := output.(*struct {
			Data []deploymentSummary `json:"data"`
		})
		response.Data = []deploymentSummary{{Name: "coder-production", ObservedState: "serving"}}
		return nil
	}
	server := newMCPServer(reader)
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "infercrane-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 6 {
		t.Fatalf("tools=%d want=6", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatalf("tool is not explicitly read-only: %+v", tool)
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "infercrane_list_deployments", Arguments: map[string]any{}})
	if err != nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	cancel()
	if err = <-serverErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
