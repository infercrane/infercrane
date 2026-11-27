package agentintegration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
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

	// 3b. A control plane outage longer than the snapshot's validity (1m
	// here): inside the agent's offline grace (10m) the endpoint's own server
	// keeps serving; a key the agent never saw is still refused.
	outage, err := http.Post(captureURL+"/outage?seconds=150", "text/plain", nil)
	if err != nil || outage.StatusCode != http.StatusNoContent {
		t.Fatalf("start the outage: %v %v", outage, err)
	}
	outage.Body.Close()
	time.Sleep(90 * time.Second)
	for index := range 3 {
		var answer map[string]any
		if status := kindJSON(t, http.MethodPost, dataPlane+"/v1/chat/completions", appKey, chatBody(false), &answer); status != http.StatusOK {
			t.Fatalf("request %d during the outage, past the snapshot's validity: %d %s", index, status, mustJSON(answer))
		}
		served++
	}
	if status := kindJSON(t, http.MethodPost, dataPlane+"/v1/chat/completions", "ic_not_a_key", chatBody(false), nil); status != http.StatusUnauthorized {
		t.Fatalf("an unknown key during the outage: %d", status)
	}
	time.Sleep(75 * time.Second) // the outage ends; the agent refreshes
	t.Log("the endpoint's own server served through a control plane outage longer than the snapshot's validity")

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
		"workload": map[string]any{"image": engineImage, "command": []string{"/fake-vllm", "--port", "8000", "--model", "Qwen/Qwen3-0.6B", "--worker", "qwen3", "--api-key-env", "VLLM_API_KEY"},
			"protocol": "openai", "port": 8000, "readiness_path": "/health", "models_path": "/v1/models", "metrics_path": "/metrics",
			"cancellation": "http-disconnect", "drain": "connection", "shutdown_grace_seconds": 5},
		"accelerator": "SIMULATED-CPU", "gpu_count": 1,
		"load":    map[string]any{"requests": 16, "concurrency": 2, "input_tokens": 32, "output_tokens": 8, "streaming": true, "tokenizer": "Qwen/Qwen3-0.6B"},
		"quality": map[string]any{"eval_set": "default"}, "timeout_seconds": 1200,
	}
	// The candidate reads the worker key from VLLM_API_KEY, as vLLM does. A
	// pod without the key checks that the candidate refuses it.
	probe := startKeyProbe(t)
	var queued struct {
		Run map[string]any `json:"candidate_run"`
	}
	if status := kindJSON(t, http.MethodPost, control+"/api/v1/agents/"+agentID+"/candidate-runs", operator, spec, &queued); status != http.StatusCreated {
		t.Fatalf("queue candidate run: %d %v", status, queued)
	}
	runID := queued.Run["id"].(string)
	checkCandidateRequiresKey(t, probe)
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

	// 6. An optimization run on the endpoint, measured by the agent here: the
	// endpoint's own server, a copy of it, and a setup that answers
	// differently. Only numbers come back; the control plane judges them.
	campaignID, winnerID := runOnAgentEndpoint(t, control, operator, engineImage)

	// 7. The winner's guarded rollout on the agent's data plane, and its
	// automatic rollback when the new server stops serving in the window.
	rollOutOnAgentEndpoint(t, control, operator, appKey, dataPlane, campaignID, winnerID, chatBody)

	// 8. Nothing the agent sent, nothing PostgreSQL holds, and nothing the
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
	forbidden := []string{marker, "You answer support tickets", "response from qwen3", "response from other-worker", appKey}
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

// runOnAgentEndpoint creates, approves, and follows an optimization run on
// the agent's endpoint until every setup has its decision, and checks that
// the agent measured each one in bracketed windows and the evidence is
// stored under the setups' own identities.
func runOnAgentEndpoint(t *testing.T, control, operator, engineImage string) (string, string) {
	t.Helper()
	proposal, err := agentCampaignProposal(engineImage)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Campaign map[string]any `json:"campaign"`
	}
	body := map[string]any{"proposal": proposal, "intent": "evolve_endpoint", "target_endpoint": "support-bot"}
	if status := kindJSONHeader(t, http.MethodPost, control+"/api/v1/optimization/campaigns", operator, "Idempotency-Key", "kind-agent-run-1", body, &created); status != http.StatusCreated {
		t.Fatalf("create the run: %d %s", status, mustJSON(created))
	}
	id := created.Campaign["id"].(string)
	if created.Campaign["agent_id"] == nil || created.Campaign["target_endpoint"] != "support-bot" {
		t.Fatalf("the run does not name its agent: %s", mustJSON(created.Campaign))
	}
	var approved map[string]any
	if status := kindJSON(t, http.MethodPost, control+"/api/v1/optimization/campaigns/"+id+"/approve", operator, map[string]any{"max_cost_usd": 1, "expires_in_seconds": 3 * 3600}, &approved); status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("approve the run: %d %s", status, mustJSON(approved))
	}
	decided := map[string]bool{"guard_passed": true, "qualified": true, "rejected": true, "inconclusive": true, "failed": true, "cancelled": true, "cleaned": true}
	// Steps 7 and 8 need time after this one, so the wait ends well inside
	// the test's own deadline and reports what the run was doing.
	deadline := time.Now().Add(25 * time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-12*time.Minute).Before(deadline) {
		deadline = testDeadline.Add(-12 * time.Minute)
	}
	var candidates []any
	last := ""
	for {
		var read struct {
			Campaign map[string]any `json:"campaign"`
		}
		kindJSON(t, http.MethodGet, control+"/api/v1/optimization/campaigns/"+id, operator, nil, &read)
		candidates, _ = read.Campaign["candidates"].([]any)
		done := len(candidates) == 3
		states := fmt.Sprint(read.Campaign["state"], " ", read.Campaign["failure_code"])
		for _, raw := range candidates {
			candidate := raw.(map[string]any)
			done = done && decided[candidate["state"].(string)]
			states += fmt.Sprintf(" | %v %v %v", candidate["proposal_candidate_id"], candidate["state"], candidate["failure_code"])
		}
		if states != last {
			t.Logf("run %s: %s", id, states)
			last = states
		}
		if done {
			t.Logf("run %s: %s", id, mustJSON(read.Campaign["candidates"]))
			break
		}
		if time.Now().After(deadline) {
			t.Logf("operations: %s", psql(t, "SELECT kind,status,attempt,max_attempts,COALESCE(error_code,''),COALESCE(waiting_reason,''),left(message,300),next_attempt_at FROM operations ORDER BY created_at"))
			t.Logf("agent commands: %s", psql(t, "SELECT kind,state,COALESCE(phase,''),attempts,COALESCE(failure_code,''),left(COALESCE(failure_detail,''),300) FROM agent_commands ORDER BY created_at"))
			t.Logf("control plane log tail: %s", kubectl(t, "--namespace", "hosted", "logs", "deployment/infercrane", "--tail=80"))
			t.Logf("agent log tail: %s", kubectl(t, "--namespace", "models", "logs", "deployment/agent-infercrane-agent", "--tail=80"))
			t.Fatalf("the run did not decide every setup: %s", mustJSON(read.Campaign))
		}
		time.Sleep(10 * time.Second)
	}
	winner := ""
	for _, raw := range candidates {
		candidate := raw.(map[string]any)
		if candidate["control"] != true && candidate["state"] == "guard_passed" {
			winner = candidate["id"].(string)
		}
		if candidate["state"] != "failed" && (candidate["benchmark_id"] == "" || !strings.HasPrefix(candidate["revision_id"].(string), "agent")) {
			t.Fatalf("a measured setup lacks agent-scoped evidence: %s", mustJSON(candidate))
		}
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM agent_commands WHERE kind='measure_window' AND state='succeeded'":             3,
		"SELECT count(*) FROM benchmark_results WHERE agent_setup_id IS NOT NULL AND deployment_id IS NULL": 3,
	} {
		if got, _ := strconv.Atoi(strings.TrimSpace(psql(t, query))); got < want {
			t.Fatalf("%s: %d, want at least %d", query, got, want)
		}
	}
	if remaining := kubectl(t, "--namespace", "models", "get", "deployments", "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "name"); strings.TrimSpace(remaining) != "" {
		t.Fatalf("a measured setup was not removed: %s", remaining)
	}
	if winner == "" {
		t.Fatalf("no setup passed: the one without the live server's delay should win: %s", mustJSON(candidates))
	}
	return id, winner
}

// rollOutOnAgentEndpoint activates the run's winner and follows its guarded
// rollout: the agent starts the setup, the endpoint switches to it with its
// own server as the fallback, and traffic is served by it. The test then
// stops the new server inside the watch window; the rollout must switch the
// endpoint back and remove the server, and the own server must keep serving.
func rollOutOnAgentEndpoint(t *testing.T, control, operator, appKey, dataPlane, campaignID, winnerID string, chatBody func(bool) map[string]any) {
	t.Helper()
	var activated map[string]any
	if status := kindJSON(t, http.MethodPost, control+"/api/v1/optimization/campaigns/"+campaignID+"/activate", operator, map[string]any{"candidate_id": winnerID}, &activated); status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("activate: %d %s", status, mustJSON(activated))
	}
	waitFor(t, 15*time.Minute, "the endpoint switched to the rollout's server", func() bool {
		count, _ := strconv.Atoi(strings.TrimSpace(psql(t, "SELECT count(*) FROM endpoint_release_guard_evaluations g JOIN endpoints e ON e.id=g.endpoint_id WHERE e.name='support-bot' AND g.decision='PASS'")))
		return count >= 1
	})
	rollout := strings.TrimSpace(kubectl(t, "--namespace", "models", "get", "deployments", "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "name"))
	if rollout == "" || strings.Contains(rollout, "\n") {
		t.Fatalf("one rollout server should run: %q", rollout)
	}
	// Traffic through the agent reaches the rollout's server once the agent
	// pulls the switched plan.
	waitFor(t, 3*time.Minute, "a request served by the rollout's server", func() bool {
		var answer map[string]any
		kindJSON(t, http.MethodPost, dataPlane+"/v1/chat/completions", appKey, chatBody(false), &answer)
		count, _ := strconv.Atoi(strings.TrimSpace(psql(t, "SELECT count(*) FROM request_records r JOIN agent_rollout_servers s ON s.binding_id=r.binding_id WHERE r.status_code=200")))
		return count >= 1
	})
	// The new server stops serving inside the window.
	kubectl(t, "--namespace", "models", "scale", rollout, "--replicas=0")
	deadline := time.Now().Add(12 * time.Minute)
	for {
		for range 5 {
			if status := kindJSON(t, http.MethodPost, dataPlane+"/v1/chat/completions", appKey, chatBody(false), nil); status != http.StatusOK {
				t.Fatalf("the endpoint's own server must keep serving during a rollback: %d", status)
			}
		}
		var read struct {
			Campaign map[string]any `json:"campaign"`
		}
		kindJSON(t, http.MethodGet, control+"/api/v1/optimization/campaigns/"+campaignID, operator, nil, &read)
		state := ""
		for _, raw := range read.Campaign["candidates"].([]any) {
			if candidate := raw.(map[string]any); candidate["id"] == winnerID {
				state = fmt.Sprint(candidate["state"], " ", candidate["failure_code"])
			}
		}
		if state == "rejected release_guard_rejected_after_rollout" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rollout was not rolled back: winner is %q", state)
		}
		time.Sleep(10 * time.Second)
	}
	waitFor(t, 5*time.Minute, "the rollout's server removed", func() bool {
		return strings.TrimSpace(kubectl(t, "--namespace", "models", "get", "deployments", "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "name")) == ""
	})
	if bindings := strings.TrimSpace(psql(t, "SELECT count(*) FROM serving_plan_bindings b JOIN endpoints e ON e.active_serving_plan_id=b.serving_plan_id WHERE e.name='support-bot'")); bindings != "1" {
		t.Fatalf("after the rollback the endpoint's plan routes to %s bindings, want its own server alone", bindings)
	}
	t.Logf("rollout of %s switched, served, and rolled back when its server stopped", winnerID)
}

// waitFor polls check every 5 seconds until it holds or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Second)
	}
}

// kindJSONHeader is kindJSON with one extra request header.
func kindJSONHeader(t *testing.T, method, url, key, header, value string, body any, out any) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(header, value)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	if response.StatusCode >= 400 {
		t.Logf("%s %s: %d %s", method, url, response.StatusCode, raw)
	}
	return response.StatusCode
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

// startKeyProbe starts a pod in the candidates' namespace that holds no key,
// from the Postgres image the cluster already has (its BusyBox wget), and
// checks it reaches the customer's model server.
func startKeyProbe(t *testing.T) string {
	t.Helper()
	const name = "key-probe"
	image := strings.Trim(kubectl(t, "--namespace", "hosted", "get", "deployment/postgres", "-o", "jsonpath={.spec.template.spec.containers[0].image}"), "' \n")
	kubectl(t, "--namespace", "models", "run", name, "--image", image, "--image-pull-policy", "IfNotPresent", "--restart", "Never", "--labels", "infercrane.test/role=key-probe", "--command", "--", "sleep", "3600")
	kubectl(t, "--namespace", "models", "wait", "--for=condition=Ready", "pod/"+name, "--timeout=180s")
	if status := probeStatus(t, name, "http://qwen3.models.svc:8000/health", ""); status != http.StatusOK {
		t.Fatalf("the probe pod cannot reach the customer's model server: HTTP %d", status)
	}
	return name
}

// checkCandidateRequiresKey waits for the agent's candidate to be ready and
// calls it from the probe pod: an API route without the key, or with a wrong
// key, is refused, and the health path stays open for the kubelet. The
// candidate must still be ready afterwards, so a refusal is the engine's
// answer. The agent calling the same routes with the worker key is shown by
// the run's measurements and answer check.
func checkCandidateRequiresKey(t *testing.T, probe string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		pod, ready := readyCandidate(t)
		if !ready {
			if time.Now().After(deadline) {
				t.Fatal("no candidate became ready for the key check")
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		base := "http://" + pod.ip + ":8000"
		statuses := map[string]int{
			"GET /health without a key":       probeStatus(t, probe, base+"/health", ""),
			"GET /v1/models without a key":    probeStatus(t, probe, base+"/v1/models", ""),
			"GET /v1/models with a wrong key": probeStatus(t, probe, base+"/v1/models", "Authorization: Bearer wrong-key"),
		}
		if _, stillReady := readyCandidate(t); !stillReady {
			t.Fatalf("the candidate ended during the key check, so it shows nothing: %v", statuses)
		}
		for call, status := range statuses {
			want := http.StatusUnauthorized
			if strings.Contains(call, "/health") {
				want = http.StatusOK
			}
			if status != want && !(want == http.StatusUnauthorized && status == http.StatusForbidden) {
				t.Fatalf("candidate %s: %s answered HTTP %d, want %d", pod.name, call, status, want)
			}
		}
		t.Logf("candidate %s without the worker key: %v", pod.name, statuses)
		return
	}
}

type candidatePod struct{ name, ip string }

func readyCandidate(t *testing.T) (candidatePod, bool) {
	t.Helper()
	var pods struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				PodIP      string `json:"podIP"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(kubectl(t, "--namespace", "models", "get", "pods", "-l", "app.kubernetes.io/managed-by=infercrane", "-o", "json")), &pods); err != nil {
		t.Fatal(err)
	}
	for _, item := range pods.Items {
		if item.Metadata.DeletionTimestamp != nil || item.Status.PodIP == "" {
			continue
		}
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				return candidatePod{name: item.Metadata.Name, ip: item.Status.PodIP}, true
			}
		}
	}
	return candidatePod{}, false
}

var wgetStatus = regexp.MustCompile(`HTTP/1\.[01] (\d{3})`)

// probeStatus returns the HTTP status the probe pod got from url. The check
// is about the answer: a connection that times out or is refused is no
// answer, so it is tried again for up to 30 seconds (a pod kubelet reports
// ready can take a few seconds to take traffic from other pods on a loaded
// node), and only then fails the test.
func probeStatus(t *testing.T, probe, url, header string) int {
	t.Helper()
	args := []string{"--context", kindEnv(t, "CONTEXT"), "--namespace", "models", "exec", probe, "--", "wget", "-q", "-T", "5", "-O", "/dev/null"}
	if header != "" {
		args = append(args, "--header", header)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		output, err := exec.Command("kubectl", append(args, url)...).CombinedOutput()
		if err == nil {
			return http.StatusOK
		}
		if match := wgetStatus.FindStringSubmatch(string(output)); match != nil {
			status, _ := strconv.Atoi(match[1])
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe %s: %v\n%s", url, err, output)
		}
		time.Sleep(time.Second)
	}
}
