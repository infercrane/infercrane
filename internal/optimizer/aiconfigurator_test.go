package optimizer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedEstimator struct {
	output estimatorOutput
	err    error
	input  estimatorInput
}

func (f *fixedEstimator) Estimate(_ context.Context, input estimatorInput) (estimatorOutput, error) {
	f.input = input
	return f.output, f.err
}

func modeledOutput(runtime string, candidates ...estimatorCandidate) estimatorOutput {
	return estimatorOutput{SchemaVersion: estimatorOutputSchema, Source: "aiconfigurator", SourceVersion: AIConfiguratorVersion, EvidenceClass: "modeled", ModelPath: "mistralai/Mistral-7B-Instruct-v0.3", System: "l40s", Backend: runtime, ResultDigest: "sha256:" + strings.Repeat("a", 64), Candidates: candidates}
}

func TestAIConfiguratorProposalIsModeledExecutableAndRequiresProof(t *testing.T) {
	ttft, tpot, throughput := 180.0, 18.0, 150.0
	runner := &fixedEstimator{output: modeledOutput("vllm", estimatorCandidate{Mode: "aggregated", Backend: "vllm", TotalGPUs: 3, Replicas: 3, GPUsPerReplica: 1, TensorParallelism: 1, EstimatedTTFTMS: &ttft, EstimatedTPOTMS: &tpot, EstimatedOutputTokensSecondPerGPU: &throughput})}
	proposal, err := (AIConfiguratorSource{Catalog: catalogSource(t), Runner: runner}).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", TargetConcurrency: number(8)})
	if err != nil {
		t.Fatal(err)
	}
	if runner.input.ModelPath != "mistralai/Mistral-7B-Instruct-v0.3" || runner.input.System != "l40s" || runner.input.TargetConcurrency != 8 || runner.input.InputTokens != 512 {
		t.Fatalf("input=%+v", runner.input)
	}
	if len(proposal.Candidates) != 1 {
		t.Fatalf("proposal=%+v", proposal)
	}
	candidate := proposal.Candidates[0]
	if candidate.EvidenceState != "modeled" || candidate.Status != "proposed-modeled-unqualified" || candidate.Deployment.Scaling.MinReplicas != 3 || candidate.Deployment.Scaling.MaxReplicas != 3 || candidate.ModeledEvidence == nil || candidate.ModeledEvidence.EstimatedTTFTMS == nil {
		t.Fatalf("candidate=%+v", candidate)
	}
	if !contains(candidate.RequiredEvidence, "semantic quality evidence when model precision or artifact changes") || !strings.Contains(proposal.SelectionBoundary, "Release Guard") {
		t.Fatalf("proof boundary lost: %+v", candidate)
	}
}

func TestAIConfiguratorRefusesUnexecutableProviderTopology(t *testing.T) {
	runner := &fixedEstimator{output: modeledOutput("vllm", estimatorCandidate{Mode: "aggregated", Backend: "vllm", TotalGPUs: 4, Replicas: 1, GPUsPerReplica: 4, TensorParallelism: 4})}
	_, err := (AIConfiguratorSource{Catalog: catalogSource(t), Runner: runner}).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "throughput"})
	if !errors.Is(err, ErrEstimatorUnavailable) || !strings.Contains(err.Error(), "no executable candidates") {
		t.Fatalf("err=%v", err)
	}
}

func TestAIConfiguratorDynamoCandidateDisablesCompetingAutoscalers(t *testing.T) {
	runner := &fixedEstimator{output: modeledOutput("vllm", estimatorCandidate{
		Mode: "disaggregated", Backend: "vllm", TotalGPUs: 4,
		Prefill: estimatorPool{Replicas: 1, TensorParallelism: 2},
		Decode:  estimatorPool{Replicas: 1, TensorParallelism: 2},
	})}
	proposal, err := (AIConfiguratorSource{Catalog: catalogSource(t), Runner: runner}).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "kubernetes-dynamo", GPU: "H100", Objective: "throughput", IncludeSimulated: true})
	if err != nil || len(proposal.Candidates) != 1 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	topology := proposal.Candidates[0].Deployment.Serving
	if topology.Backend != "dynamo" || topology.Mode != "disaggregated" || topology.Autoscaling.Owner != "disabled" || topology.Prefill.Replicas != 1 || topology.Decode.Replicas != 1 {
		t.Fatalf("candidate introduced competing mutation owners: %+v", topology)
	}
}

func TestAIConfiguratorOutputValidationFailsClosed(t *testing.T) {
	output := modeledOutput("vllm", estimatorCandidate{Mode: "aggregated", Backend: "vllm", TotalGPUs: 1, Replicas: 1, GPUsPerReplica: 1})
	output.SourceVersion = "unreviewed"
	err := validateEstimatorOutput(output, estimatorInput{ModelPath: output.ModelPath, System: output.System, Backend: output.Backend})
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("err=%v", err)
	}
}

func TestAIConfiguratorOutputRejectsInvalidDigestAndMetrics(t *testing.T) {
	negative := -1.0
	for name, mutate := range map[string]func(*estimatorOutput){
		"non-hex digest":  func(output *estimatorOutput) { output.ResultDigest = "sha256:" + strings.Repeat("z", 64) },
		"negative metric": func(output *estimatorOutput) { output.Candidates[0].EstimatedTTFTMS = &negative },
	} {
		t.Run(name, func(t *testing.T) {
			output := modeledOutput("vllm", estimatorCandidate{Mode: "aggregated", Backend: "vllm", TotalGPUs: 1, Replicas: 1, GPUsPerReplica: 1, TensorParallelism: 1})
			mutate(&output)
			if err := validateEstimatorOutput(output, estimatorInput{ModelPath: output.ModelPath, System: output.System, Backend: output.Backend}); err == nil {
				t.Fatalf("invalid estimator output passed: %+v", output)
			}
		})
	}
}

func TestPythonEstimatorScrubsCloudSecretsAndBoundsWireContract(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-leak")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/secret/credentials.json")
	directory := t.TempDir()
	binary := filepath.Join(directory, "fake-python")
	body := `#!/bin/sh
if [ -n "$AWS_SECRET_ACCESS_KEY" ] || [ -n "$GOOGLE_APPLICATION_CREDENTIALS" ]; then
  echo '{"error":"secret leaked"}'
  exit 3
fi
cat >/dev/null
printf '%s\n' '{"schema_version":"infercrane.optimizer.estimator-output/v1","source":"aiconfigurator","source_version":"0.11.0","evidence_class":"modeled","model_path":"org/model","system":"l40s","backend":"vllm","result_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","candidates":[{"mode":"aggregated","backend":"vllm","total_gpus":1,"replicas":1,"gpus_per_replica":1,"tensor_parallelism":1,"prefill":{},"decode":{}}]}'
`
	if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := (PythonEstimatorRunner{Python: binary}).Estimate(context.Background(), estimatorInput{SchemaVersion: estimatorInputSchema, RequiredVersion: AIConfiguratorVersion, RequiredPlotextVersion: AIConfiguratorPlotextVersion, ModelPath: "org/model", System: "l40s", Backend: "vllm", TargetConcurrency: 1, InputTokens: 1, OutputTokens: 1, TTFTMS: 1, TPOTMS: 1, TopN: 1})
	if err != nil || len(output.Candidates) != 1 {
		t.Fatalf("output=%+v err=%v", output, err)
	}
}

func TestPythonEstimatorProbeRequiresExactDependencyTuple(t *testing.T) {
	directory := t.TempDir()
	for name, tuple := range map[string]string{"supported": "0.11.0 5.3.2", "incompatible": "0.11.0 6.0.0"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(directory, name)
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' '"+tuple+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			version, err := (PythonEstimatorRunner{Python: binary}).Probe(context.Background())
			if name == "supported" && (err != nil || version != AIConfiguratorVersion) {
				t.Fatalf("version=%q err=%v", version, err)
			}
			if name == "incompatible" && !errors.Is(err, ErrEstimatorUnavailable) {
				t.Fatalf("incompatible tuple passed: version=%q err=%v", version, err)
			}
		})
	}
}

func TestPythonEstimatorEnforcesTimeoutAndOutputBound(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "slow-python")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec sleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (PythonEstimatorRunner{Python: binary, Timeout: 10 * time.Millisecond}).Estimate(context.Background(), estimatorInput{})
	if !errors.Is(err, ErrEstimatorUnavailable) || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("timeout did not fail closed: %v", err)
	}

	buffer := boundedBuffer{limit: 4}
	if written, writeErr := buffer.Write([]byte("123456")); writeErr != nil || written != 6 || buffer.String() != "1234" || !buffer.overflow {
		t.Fatalf("bounded buffer failed: written=%d value=%q overflow=%v err=%v", written, buffer.String(), buffer.overflow, writeErr)
	}
}
