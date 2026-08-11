package lab

import (
	"encoding/json"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func number(v float64) *float64 { return &v }
func TestEvaluateLabelsOnlyMeasuredEvidenceAndNeverInventsCost(t *testing.T) {
	limit := 250.0
	rows := []domain.BenchmarkResult{{ID: "slow", ModelIdentity: "model@commit", DeploymentName: "a", RevisionID: "r1", Runtime: "vllm", RuntimeVersion: "1", Provider: "aws", GPU: "H100", ComputeMode: "elastic", RequestCount: 10, Failed: 1, TTFTP95MS: number(300), WorkloadJSON: `{"requests":10}`, CostMetadataJSON: `{"available":false}`}, {ID: "fast", ModelIdentity: "model@commit", DeploymentName: "b", RevisionID: "r2", Runtime: "sglang", RuntimeVersion: "1", Provider: "gcp", GPU: "H100", ComputeMode: "elastic", RequestCount: 10, TTFTP95MS: number(200), WorkloadJSON: `{"requests":10}`, CostMetadataJSON: `{"available":false}`}, {ID: "other", ModelIdentity: "other", RequestCount: 1, WorkloadJSON: `{}`, CostMetadataJSON: `{}`}}
	result, err := Evaluate(Input{ModelIdentity: "model@commit", MaxTTFTP95MS: &limit}, rows)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []Candidate
	if json.Unmarshal([]byte(result.ResultsJSON), &candidates) != nil || len(candidates) != 2 || candidates[0].EvidenceID != "fast" || candidates[0].EvidenceClass != "measured" || candidates[0].MeetsSLO == nil || !*candidates[0].MeetsSLO || candidates[1].ErrorRate != 0.1 {
		t.Fatalf("%#v", candidates)
	}
	if result.InputDigest == "" || result.AlgorithmVersion != AlgorithmVersion {
		t.Fatalf("%#v", result)
	}
}
