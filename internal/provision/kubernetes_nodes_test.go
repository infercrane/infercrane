package provision

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// cannedKubectl answers read commands by what they ask for and records them.
type cannedKubectl struct{ calls [][]string }

func (c *cannedKubectl) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	c.calls = append(c.calls, args)
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "get service current"):
		return []byte(`{"spec":{"selector":{"app":"qwen3","tier":"serve"}}}`), nil
	case strings.Contains(joined, "get pods -l app=qwen3,tier=serve"):
		return []byte("node-b node-b"), nil
	case strings.Contains(joined, "get pods -l app.kubernetes.io/instance=infercrane-"):
		return []byte("node-a"), nil
	}
	return nil, nil
}

// The node lookups only read: pods by the replica's instance label, or the
// pods a Service selects, in the namespace named.
func TestNodeLookupsReadPodsOnly(t *testing.T) {
	runner := &cannedKubectl{}
	k := Kubernetes{Runner: runner, Context: "kind", Namespace: "models"}
	nodes, err := k.ReplicaNodes(context.Background(), ProviderHandle{ExternalKey: "agent-candidate-window-1"})
	if err != nil || !slices.Equal(nodes, []string{"node-a"}) {
		t.Fatalf("replica nodes: %v %v", nodes, err)
	}
	nodes, err = k.ServiceNodes(context.Background(), "models", "current")
	if err != nil || !slices.Equal(nodes, []string{"node-b", "node-b"}) {
		t.Fatalf("service nodes: %v %v", nodes, err)
	}
	for _, call := range runner.calls {
		if !slices.Contains(call, "get") || slices.ContainsFunc(call, func(arg string) bool { return arg == "apply" || arg == "delete" || arg == "exec" }) {
			t.Fatalf("a lookup did more than read: %v", call)
		}
	}
}
