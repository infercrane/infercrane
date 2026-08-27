// Package servingcontract owns the immutable boundary between InferCrane's
// outer lifecycle and an advanced serving backend's internal topology.
package servingcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	SchemaV1 = "infercrane.serving/v1"

	BackendDynamo = "dynamo"

	ModeAggregated    = "aggregated"
	ModeDisaggregated = "disaggregated"

	RoutingDirect  = "direct"
	RoutingKVAware = "kv-aware"

	AutoscalingDisabled      = "disabled"
	AutoscalingDynamoPlanner = "dynamo-planner"
	AutoscalingExternal      = "external"

	CacheNone    = "none"
	CacheKVBM    = "kvbm"
	CacheLMCache = "lmcache"
	CacheHiCache = "hicache"
)

// Pool is an internal backend pool. These counts never represent InferCrane
// provider replicas: one DynamoGraphDeployment owns all of its pools.
type Pool struct {
	Replicas          int `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	TensorParallelism int `json:"tensor_parallelism,omitempty" yaml:"tensor_parallelism,omitempty"`
}

type Autoscaling struct {
	Owner string `json:"owner,omitempty" yaml:"owner,omitempty"`
	Min   int    `json:"min,omitempty" yaml:"min,omitempty"`
	Max   int    `json:"max,omitempty" yaml:"max,omitempty"`
}

// Cache selects one backend. Cache implementations are alternatives, not
// composable tiers; an implementation may manage its own host/disk tiers.
type Cache struct {
	Backend          string `json:"backend,omitempty" yaml:"backend,omitempty"`
	HostGiB          int    `json:"host_gib,omitempty" yaml:"host_gib,omitempty"`
	DiskGiB          int    `json:"disk_gib,omitempty" yaml:"disk_gib,omitempty"`
	MemoryGiB        int    `json:"memory_gib,omitempty" yaml:"memory_gib,omitempty"`
	StorageClaim     string `json:"storage_claim,omitempty" yaml:"storage_claim,omitempty"`
	ConfigurationRef string `json:"configuration_ref,omitempty" yaml:"configuration_ref,omitempty"`
	Metrics          bool   `json:"metrics,omitempty" yaml:"metrics,omitempty"`
}

// Topology is persisted in an immutable deployment revision. It names
// portable ownership and intent; provider-specific CRD shape stays inside the
// adapter. Empty means the existing single-runtime provider path.
type Topology struct {
	SchemaVersion string      `json:"schema_version,omitempty" yaml:"schema_version,omitempty"`
	Backend       string      `json:"backend,omitempty" yaml:"backend,omitempty"`
	Profile       string      `json:"profile,omitempty" yaml:"profile,omitempty"`
	Mode          string      `json:"mode,omitempty" yaml:"mode,omitempty"`
	Routing       string      `json:"routing,omitempty" yaml:"routing,omitempty"`
	Worker        Pool        `json:"worker,omitzero" yaml:"worker,omitempty"`
	Prefill       Pool        `json:"prefill,omitzero" yaml:"prefill,omitempty"`
	Decode        Pool        `json:"decode,omitzero" yaml:"decode,omitempty"`
	Autoscaling   Autoscaling `json:"autoscaling,omitzero" yaml:"autoscaling,omitempty"`
	Cache         Cache       `json:"cache,omitzero" yaml:"cache,omitempty"`
}

func (t Topology) Empty() bool {
	return t.SchemaVersion == "" && t.Backend == "" && t.Profile == "" && t.Mode == "" && t.Routing == "" && t.Worker == (Pool{}) && t.Prefill == (Pool{}) && t.Decode == (Pool{}) && t.Autoscaling == (Autoscaling{}) && t.Cache == (Cache{})
}

// Normalize fills only structural defaults. It deliberately does not select
// an optimization profile from a model name or guessed workload.
func (t Topology) Normalize() Topology {
	if t.Empty() {
		return t
	}
	if t.SchemaVersion == "" {
		t.SchemaVersion = SchemaV1
	}
	if t.Profile == "" {
		t.Profile = "custom"
	}
	if t.Mode == "" {
		t.Mode = ModeAggregated
	}
	if t.Routing == "" {
		t.Routing = RoutingDirect
	}
	if t.Autoscaling.Owner == "" {
		t.Autoscaling.Owner = AutoscalingDisabled
	}
	if t.Cache.Backend == "" {
		t.Cache.Backend = CacheNone
	}
	if t.Mode == ModeAggregated && t.Worker.Replicas == 0 {
		t.Worker.Replicas = 1
	}
	if t.Worker.Replicas > 0 && t.Worker.TensorParallelism == 0 {
		t.Worker.TensorParallelism = 1
	}
	if t.Prefill.Replicas > 0 && t.Prefill.TensorParallelism == 0 {
		t.Prefill.TensorParallelism = 1
	}
	if t.Decode.Replicas > 0 && t.Decode.TensorParallelism == 0 {
		t.Decode.TensorParallelism = 1
	}
	return t
}

// Validate enforces a single mutation owner and rejects topology combinations
// that the adapter cannot preserve without guessing.
func (t Topology) Validate(runtime, cloud, providerAdapter string, outerMin, outerMax int) error {
	if t.Empty() {
		if providerAdapter == "kubernetes-dynamo" {
			return errors.New("provider_adapter kubernetes-dynamo requires an explicit Dynamo serving topology")
		}
		return nil
	}
	t = t.Normalize()
	if t.SchemaVersion != SchemaV1 {
		return fmt.Errorf("serving.schema_version must be %q", SchemaV1)
	}
	if t.Backend != BackendDynamo {
		return fmt.Errorf("serving.backend %q is unsupported", t.Backend)
	}
	if cloud != "kubernetes" || providerAdapter != "kubernetes-dynamo" {
		return errors.New("Dynamo serving requires cloud kubernetes and provider_adapter kubernetes-dynamo")
	}
	if runtime != "vllm" && runtime != "sglang" {
		return errors.New("Dynamo serving currently requires runtime vllm or sglang")
	}
	if outerMin != 1 || outerMax != 1 {
		return errors.New("Dynamo serving owns one graph; InferCrane outer replica bounds must both equal 1")
	}
	if t.Profile != "custom" && t.Profile != "baseline" {
		return errors.New("serving.profile must be baseline or custom; workload-tuned profiles require measured qualification before they become executable")
	}
	if t.Mode != ModeAggregated && t.Mode != ModeDisaggregated {
		return errors.New("serving.mode must be aggregated or disaggregated")
	}
	if t.Routing != RoutingDirect && t.Routing != RoutingKVAware {
		return errors.New("serving.routing must be direct or kv-aware")
	}
	if err := validatePool("worker", t.Worker); err != nil {
		return err
	}
	if err := validatePool("prefill", t.Prefill); err != nil {
		return err
	}
	if err := validatePool("decode", t.Decode); err != nil {
		return err
	}
	if t.Mode == ModeAggregated {
		if t.Worker.Replicas < 1 || t.Prefill.Replicas != 0 || t.Decode.Replicas != 0 {
			return errors.New("aggregated serving requires worker replicas and forbids prefill/decode pools")
		}
	} else if t.Worker.Replicas != 0 || t.Prefill.Replicas < 1 || t.Decode.Replicas < 1 {
		return errors.New("disaggregated serving requires prefill and decode replicas and forbids a worker pool")
	}
	if t.Autoscaling.Owner != AutoscalingDisabled && t.Autoscaling.Owner != AutoscalingDynamoPlanner && t.Autoscaling.Owner != AutoscalingExternal {
		return errors.New("serving.autoscaling.owner must be disabled, dynamo-planner, or external")
	}
	if t.Autoscaling.Owner == AutoscalingDisabled {
		if t.Autoscaling.Min != 0 || t.Autoscaling.Max != 0 {
			return errors.New("disabled serving autoscaling cannot declare min or max")
		}
	} else if t.Autoscaling.Min < 1 || t.Autoscaling.Max < t.Autoscaling.Min || t.Autoscaling.Max > 10000 {
		return errors.New("serving autoscaling bounds must satisfy 1 <= min <= max <= 10000")
	}
	if t.Autoscaling.Owner != AutoscalingDisabled {
		return errors.New("Dynamo Planner and external autoscaling ownership are registered but not executable until their mutation and drain contracts are qualified")
	}
	if err := t.Cache.validate(runtime); err != nil {
		return err
	}
	if t.Mode == ModeDisaggregated {
		return errors.New("NIXL prefill/decode disaggregation is registered for argument translation but not executable until its transport, routing, and failure contracts are qualified")
	}
	if t.Cache.Backend == CacheLMCache || t.Cache.Backend == CacheHiCache {
		return errors.New("LMCache and HiCache are registered but not executable until their runtime and lifecycle contracts are qualified")
	}
	return nil
}

func validatePool(name string, pool Pool) error {
	if pool.Replicas < 0 || pool.Replicas > 10000 || pool.TensorParallelism < 0 || pool.TensorParallelism > 1024 {
		return fmt.Errorf("serving.%s replicas and tensor_parallelism exceed supported bounds", name)
	}
	if pool.Replicas > 0 && pool.TensorParallelism < 1 {
		return fmt.Errorf("serving.%s.tensor_parallelism must be positive", name)
	}
	return nil
}

func (c Cache) validate(runtime string) error {
	if c.Backend != CacheNone && c.Backend != CacheKVBM && c.Backend != CacheLMCache && c.Backend != CacheHiCache {
		return errors.New("serving.cache.backend must be none, kvbm, lmcache, or hicache")
	}
	if c.HostGiB < 0 || c.DiskGiB < 0 || c.MemoryGiB < 0 || c.HostGiB > 1_000_000 || c.DiskGiB > 10_000_000 || c.MemoryGiB > 1_000_000 {
		return errors.New("serving cache sizes exceed supported bounds")
	}
	if c.Backend == CacheNone && (c.HostGiB != 0 || c.DiskGiB != 0 || c.MemoryGiB != 0 || c.StorageClaim != "" || c.ConfigurationRef != "" || c.Metrics) {
		return errors.New("serving cache settings require a cache backend")
	}
	if c.Backend == CacheKVBM && runtime != "vllm" {
		return errors.New("KVBM is currently qualified only with vllm")
	}
	if c.Backend == CacheLMCache && runtime != "vllm" {
		return errors.New("LMCache is currently qualified only with vllm")
	}
	if c.Backend == CacheHiCache && runtime != "sglang" {
		return errors.New("HiCache requires sglang")
	}
	if c.Backend == CacheKVBM && c.HostGiB == 0 && c.DiskGiB == 0 {
		return errors.New("KVBM requires an explicit host or disk cache size")
	}
	if c.Backend == CacheKVBM && c.MemoryGiB <= c.HostGiB {
		return errors.New("KVBM memory_gib must exceed host_gib so the runtime is not starved by the cache")
	}
	if c.DiskGiB > 0 && c.HostGiB == 0 {
		return errors.New("disk cache requires a host cache tier")
	}
	if c.DiskGiB > 0 && c.DiskGiB < c.HostGiB {
		return errors.New("disk cache must not be smaller than host cache")
	}
	if c.DiskGiB > 0 && c.StorageClaim == "" {
		return errors.New("disk cache requires an explicit storage_claim")
	}
	return nil
}

func (t Topology) Digest() (string, error) {
	if t.Empty() {
		return "", nil
	}
	body, err := json.Marshal(t.Normalize())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
