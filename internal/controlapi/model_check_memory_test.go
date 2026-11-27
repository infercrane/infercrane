package controlapi

import (
	"testing"

	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func TestTheModelCheckGivesTheDiagnosisTheModelsMemory(t *testing.T) {
	weights, perToken := int64(16381470720), int64(147456)
	report := modelcheck.Report{Memory: modelcheck.MemoryFit{WeightsBytes: &weights}, Config: modelcheck.ConfigFacts{KVCacheBytesPerToken: &perToken}}
	diagnosis := &optimizer.Diagnosis{Signals: map[string]float64{"runtime_requests_running_per_replica": 16}}
	addModelMemory(diagnosis, report)
	if diagnosis.Signals[optimizer.SignalModelWeightsBytes] != 16381470720 || diagnosis.Signals[optimizer.SignalKVCacheBytesPerToken] != 147456 || diagnosis.Signals["runtime_requests_running_per_replica"] != 16 {
		t.Fatalf("signals=%v", diagnosis.Signals)
	}
	// A fact the check could not read is left out; no diagnosis, nothing to add.
	unread := &optimizer.Diagnosis{}
	addModelMemory(unread, modelcheck.Report{Memory: modelcheck.MemoryFit{WeightsBytes: &weights}})
	if len(unread.Signals) != 0 {
		t.Fatalf("a check without the KV cache size added %v", unread.Signals)
	}
	addModelMemory(nil, report)
}
