package agentintegration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/qualitycheck"
)

// kindEnv reads one setting the kind script exported.
func kindEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv("INFERCRANE_AGENT_KIND_" + name)
	if value == "" {
		t.Fatalf("INFERCRANE_AGENT_KIND_%s is not set", name)
	}
	return value
}

func kindJSON(t *testing.T, method, url, key string, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	if response.StatusCode >= 500 {
		t.Logf("%s %s: %d %s", method, url, response.StatusCode, raw)
	}
	return response.StatusCode
}

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command("kubectl", append([]string{"--context", kindEnv(t, "CONTEXT")}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// TestAgentDataPlaneOnKind runs inside scripts/test-agent-data-plane-kind.sh.
func TestAgentDataPlaneOnKind(t *testing.T) {
	if os.Getenv("INFERCRANE_AGENT_KIND_CONTEXT") == "" {
		t.Skip("run through scripts/test-agent-data-plane-kind.sh")
	}
	control, operator, agentID := kindEnv(t, "CONTROL_URL"), kindEnv(t, "API_KEY"), kindEnv(t, "AGENT_ID")
	dataPlane, captureURL, engineImage := kindEnv(t, "DATA_PLANE_URL"), kindEnv(t, "CAPTURE_URL"), kindEnv(t, "ENGINE_IMAGE")
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	marker := "CUSTOMER-PROMPT-" + hex.EncodeToString(raw)

	// 1. The agent registered the in-cluster server as an observed endpoint.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var endpoint map[string]any
		if kindJSON(t, http.MethodGet, control+"/api/v1/endpoints/support-bot", operator, nil, &endpoint) == http.StatusOK {
			t.Logf("endpoint registered by the agent: %v", endpoint["endpoint"])
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent did not register support-bot")
		}
		time.Sleep(2 * time.Second)
	}

	// 2. An application key, and the endpoint made agent-managed by a person.
	var principal struct {
		Credential string `json:"credential"`
	}
	if status := kindJSON(t, http.MethodPost, control+"/api/v1/principals", operator, map[string]any{"name": "support-app", "role": "viewer"}, &principal); status != http.StatusCreated || principal.Credential == "" {
		t.Fatalf("create application key: %d", status)
	}
	appKey := principal.Credential
	var promoted map[string]any
	if status := kindJSON(t, http.MethodPut, control+"/api/v1/adoptions/endpoints/support-bot/ownership", operator, map[string]string{"ownership_mode": "agent-managed"}, &promoted); status != http.StatusOK {
		t.Fatalf("promote: %d %v", status, promoted)
	}
	var endpoint map[string]any
	kindJSON(t, http.MethodGet, control+"/api/v1/endpoints/support-bot", operator, nil, &endpoint)
	if !strings.Contains(mustJSON(endpoint), `"residency":"customer-cloud"`) {
		t.Fatalf("endpoint residency: %s", mustJSON(endpoint))
	}

	// 3. Requests are served inside the cluster once the agent pulls the
	// snapshot (one poll interval).
	chatBody := func(stream bool) map[string]any {
		return map[string]any{"model": "support-bot", "stream": stream, "messages": []map[string]string{{"role": "system", "content": "You answer support tickets."}, {"role": "user", "content": marker}}}
	}
	deadline = time.Now().Add(2 * time.Minute)
	for {
		var answer map[string]any
		status := kindJSON(t, http.MethodPost, dataPlane+"/v1/chat/completions", appKey, chatBody(false), &answer)
		if status == http.StatusOK && strings.Contains(mustJSON(answer), "response from qwen3") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never served the endpoint: last status %d %v", status, answer)
		}
		time.Sleep(2 * time.Second)
	}
	served := 1
	for index := 0; index < 6; index++ {
		request, _ := json.Marshal(chatBody(index%2 == 0))
		post, _ := http.NewRequest(http.MethodPost, dataPlane+"/v1/chat/completions", bytes.NewReader(request))
		post.Header.Set("Authorization", "Bearer "+appKey)
		post.Header.Set("X-InferCrane-Session-ID", marker+"-session")
		response, err := http.DefaultClient.Do(post)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(answer), "response from qwen3") {
			t.Fatalf("data plane request %d: %d %s", index, response.StatusCode, answer)
		}
		served++
	}
	if status := kindJSON(t, http.MethodPost, dataPlane+"/v1/chat/completions", "ic_not_a_key", chatBody(false), nil); status != http.StatusUnauthorized {
		t.Fatalf("unknown key on the data plane: %d", status)
	}
	// The hosted gateway refuses the endpoint instead of carrying its prompts.
	var refused map[string]any
	if status := kindJSON(t, http.MethodPost, control+"/v1/chat/completions", appKey, chatBody(false), &refused); status != http.StatusForbidden || !strings.Contains(mustJSON(refused), "residency_error") {
		t.Fatalf("hosted gateway on a customer-cloud endpoint: %d %v", status, refused)
	}
	t.Logf("served %d requests inside the cluster; the hosted gateway refused the endpoint", served)

	// 4. Content-free records arrive with the agent's next syncs.
	deadline = time.Now().Add(2 * time.Minute)
	for {
		count := psql(t, "SELECT count(*) FROM request_records WHERE compute_mode='agent-data-plane' AND status_code=200")
		if n, _ := strconv.Atoi(strings.TrimSpace(count)); n >= served {
			t.Logf("request_records from the data plane: %s", strings.TrimSpace(count))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("request records: %s, want %d", count, served)
		}
		time.Sleep(3 * time.Second)
	}

	// 5. One candidate run through the command channel, inside the cluster.
	spec := map[string]any{
		"endpoint": "support-bot", "label": "fake engine candidate", "model": "Qwen/Qwen3-0.6B",
		"workload": map[string]any{"image": engineImage, "command": []string{"/fake-vllm", "--port", "8000", "--model", "Qwen/Qwen3-0.6B", "--worker", "qwen3"},
			"protocol": "openai", "port": 8000, "readiness_path": "/health", "models_path": "/v1/models", "metrics_path": "/metrics",
			"cancellation": "http-disconnect", "drain": "connection", "shutdown_grace_seconds": 5},
		"accelerator": "SIMULATED-CPU", "gpu_count": 1,
		"load":    map[string]any{"requests": 16, "concurrency": 2, "input_tokens": 32, "output_tokens": 8, "streaming": true, "tokenizer": "Qwen/Qwen3-0.6B"},
		"quality": map[string]any{"eval_set": "default"}, "timeout_seconds": 1200,
	}
	var queued struct {
		Run map[string]any `json:"candidate_run"`
	}
	if status := kindJSON(t, http.MethodPost, control+"/api/v1/agents/"+agentID+"/candidate-runs", operator, spec, &queued); status != http.StatusCreated {
		t.Fatalf("queue candidate run: %d %v", status, queued)
	}
	runID := queued.Run["id"].(string)
	deadline = time.Now().Add(25 * time.Minute)
	var run map[string]any
	for {
		var read struct {
			Run map[string]any `json:"candidate_run"`
		}
		kindJSON(t, http.MethodGet, control+"/api/v1/agents/"+agentID+"/candidate-runs/"+runID, operator, nil, &read)
		run = read.Run
		if run["finished_at"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("candidate run did not finish: %s", mustJSON(run))
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("candidate run: %s", mustJSON(run))
	if run["state"] != "succeeded" {
		t.Fatalf("candidate run failed: %s", mustJSON(run))
	}
	result := run["result"].(map[string]any)
	for _, field := range []string{"control", "candidate", "control_after", "quality"} {
		if result[field] == nil {
			t.Fatalf("result has no %s: %s", field, mustJSON(result))
		}
	}
	if result["cleaned_up"] != true || result["quality"].(map[string]any)["verdict"] != "PASS" {
		t.Fatalf("result: %s", mustJSON(result))
	}
	if remaining := kubectl(t, "--namespace", "models", "get", "deployments", "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "name"); strings.TrimSpace(remaining) != "" {
		t.Fatalf("the candidate was not removed: %s", remaining)
	}

	// 6. Nothing the agent sent, nothing PostgreSQL holds, and nothing the
	// control plane logged contains a prompt, an answer, or a key.
	response, err := http.Get(captureURL + "/captured")
	if err != nil {
		t.Fatal(err)
	}
	captured, _ := io.ReadAll(response.Body)
	response.Body.Close()
	stats, _ := http.Get(captureURL + "/stats")
	statsBody, _ := io.ReadAll(stats.Body)
	stats.Body.Close()
	t.Logf("capture-proxy saw %d bytes from the agent: %s", len(captured), statsBody)
	forbidden := []string{marker, "You answer support tickets", "response from qwen3", appKey}
	for _, prompt := range qualitycheck.Default().Prompts {
		forbidden = append(forbidden, prompt.Prompt)
	}
	dump := psqlDump(t)
	logs := kubectl(t, "--namespace", "hosted", "logs", "deployment/infercrane", "--tail=-1")
	for _, value := range forbidden {
		for where, text := range map[string]string{"the wire to the control plane": string(captured), "PostgreSQL": dump, "control plane logs": logs} {
			if strings.Contains(text, value) {
				t.Fatalf("%q reached %s", truncate(value), where)
			}
		}
	}
	for _, expected := range []string{"/api/v1/agent/data-plane", "/api/v1/agent/quota-leases", "/api/v1/agent/commands", `"source":"data-plane"`, `"state":"succeeded"`} {
		if expected == "/api/v1/agent/quota-leases" {
			continue // no quota is configured in this run, so none is leased
		}
		if !strings.Contains(string(captured), expected) {
			t.Fatalf("expected %s in what the agent sent", expected)
		}
	}
	t.Logf("no prompt, answer, eval prompt, or key reached the hosted namespace (%d bytes on the wire, %d bytes in PostgreSQL)", len(captured), len(dump))
}

func psql(t *testing.T, query string) string {
	t.Helper()
	return kubectl(t, "--namespace", "hosted", "exec", "deployment/postgres", "--", "psql", "-U", "infercrane", "-d", "infercrane", "-tAc", query)
}

func psqlDump(t *testing.T) string {
	t.Helper()
	return kubectl(t, "--namespace", "hosted", "exec", "deployment/postgres", "--", "pg_dump", "-U", "infercrane", "-d", "infercrane", "--data-only")
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func truncate(value string) string {
	if len(value) > 40 {
		return value[:40] + "…"
	}
	return value
}
