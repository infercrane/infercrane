package servingcontract

import (
	"errors"
	"reflect"
	"testing"
)

func TestDeferredLMCacheTranslationIsExactAndCannotCompile(t *testing.T) {
	topology := validDynamo()
	topology.Cache = Cache{Backend: CacheLMCache, ConfigurationRef: "/etc/lmcache/config.yaml"}
	mechanism, err := TranslateDeferredMechanism(topology, "vllm")
	if err != nil || mechanism.Environment["LMCACHE_CONFIG_FILE"] != "/etc/lmcache/config.yaml" || !reflect.DeepEqual(mechanism.Components["worker"], []string{"--kv-transfer-config", `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}`}) {
		t.Fatalf("mechanism=%#v err=%v", mechanism, err)
	}
	if err = mechanism.Compile(); !errors.Is(err, ErrDeferredMechanism) {
		t.Fatalf("deferred LMCache compiled: %v", err)
	}
}

func TestDeferredNIXLTranslationIsRuntimeSpecificAndCannotCompile(t *testing.T) {
	topology := Topology{Backend: BackendDynamo, Mode: ModeDisaggregated, Routing: RoutingKVAware, Prefill: Pool{Replicas: 1, TensorParallelism: 1}, Decode: Pool{Replicas: 1, TensorParallelism: 1}}
	for _, runtimeName := range []string{"vllm", "sglang"} {
		mechanism, err := TranslateDeferredMechanism(topology, runtimeName)
		if err != nil || len(mechanism.Components["prefill"]) == 0 || len(mechanism.Components["decode"]) == 0 || mechanism.Executable {
			t.Fatalf("runtime=%s mechanism=%#v err=%v", runtimeName, mechanism, err)
		}
		if err = mechanism.Compile(); !errors.Is(err, ErrDeferredMechanism) {
			t.Fatalf("deferred NIXL compiled for %s: %v", runtimeName, err)
		}
	}
}
