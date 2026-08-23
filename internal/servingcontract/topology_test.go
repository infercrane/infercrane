package servingcontract

import (
	"strings"
	"testing"
)

func validDynamo() Topology {
	return Topology{Backend: BackendDynamo, Mode: ModeAggregated, Routing: RoutingDirect, Worker: Pool{Replicas: 1, TensorParallelism: 1}}
}

func TestDynamoTopologyValidationAndDigest(t *testing.T) {
	topology := validDynamo()
	if err := topology.Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 1); err != nil {
		t.Fatal(err)
	}
	one, err := topology.Digest()
	if err != nil || one == "" {
		t.Fatalf("digest: %q %v", one, err)
	}
	two, _ := topology.Normalize().Digest()
	if one != two {
		t.Fatalf("normalization changed digest: %s != %s", one, two)
	}
}

func TestDynamoAdapterCannotBypassTheServingContract(t *testing.T) {
	err := (Topology{}).Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 1)
	if err == nil || !strings.Contains(err.Error(), "explicit Dynamo serving topology") {
		t.Fatalf("empty topology bypassed Dynamo ownership contract: %v", err)
	}
}

func TestDynamoTopologyRejectsConflictingOwnershipAndCacheCombinations(t *testing.T) {
	cases := []Topology{
		{Backend: BackendDynamo, Mode: ModeDisaggregated, Prefill: Pool{Replicas: 1, TensorParallelism: 1}, Decode: Pool{Replicas: 0}},
		{Backend: BackendDynamo, Mode: ModeAggregated, Worker: Pool{Replicas: 1}, Autoscaling: Autoscaling{Owner: AutoscalingDynamoPlanner}},
		{Backend: BackendDynamo, Mode: ModeAggregated, Worker: Pool{Replicas: 1}, Cache: Cache{Backend: CacheKVBM}},
		{Backend: BackendDynamo, Mode: ModeAggregated, Worker: Pool{Replicas: 1}, Cache: Cache{Backend: CacheHiCache}},
	}
	for i, topology := range cases {
		if err := topology.Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 1); err == nil {
			t.Fatalf("case %d unexpectedly passed", i)
		}
	}
	if err := validDynamo().Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 2); err == nil {
		t.Fatal("Dynamo accepted competing outer autoscaling")
	}
}

func TestDynamoTopologyAllowsDisaggregatedAndQualifiedAggregatedCache(t *testing.T) {
	topology := Topology{
		Backend: BackendDynamo, Mode: ModeDisaggregated, Routing: RoutingKVAware,
		Prefill: Pool{Replicas: 2, TensorParallelism: 1}, Decode: Pool{Replicas: 3, TensorParallelism: 2},
	}
	if err := topology.Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 1); err != nil {
		t.Fatal(err)
	}
	cached := validDynamo()
	cached.Cache = Cache{Backend: CacheKVBM, HostGiB: 100, MemoryGiB: 150, DiskGiB: 200, StorageClaim: "kv-cache", Metrics: true}
	if err := cached.Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 1); err != nil {
		t.Fatal(err)
	}
	topology.Cache = cached.Cache
	if err := topology.Validate("vllm", "kubernetes", "kubernetes-dynamo", 1, 1); err == nil {
		t.Fatal("unqualified disaggregated cache combination passed")
	}
}
