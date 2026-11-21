package qualitycheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/testtools/fakevllm"
)

const weatherTools = `[
	{"type":"function","function":{"name":"weather","description":"Current weather","parameters":{"type":"object","properties":{"city":{"type":"string","minLength":1}},"required":["city"],"additionalProperties":false}}},
	{"type":"function","function":{"name":"time","parameters":{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"]}}}
]`

// toolSet has textCases plain prompts and one tool case, so a lost tool case
// alone keeps the mean above the BLOCK threshold when textCases is 20.
func toolSet(t *testing.T, textCases int) EvalSet {
	t.Helper()
	cases := make([]string, 0, textCases+1)
	for index := range textCases {
		cases = append(cases, fmt.Sprintf(`{"id":"text-%d","prompt":"question %d"}`, index, index))
	}
	cases = append(cases, `{"id":"weather","messages":[{"role":"system","content":"Use tools."},{"role":"user","content":"Weather in Berlin?"}],"tools":`+weatherTools+`,"tool_choice":"auto"}`)
	set, err := DecodeEvalSet([]byte(`{"schema":"infercrane.dev/quality-eval-set/v2","name":"agent","version":"1","prompts":[` + strings.Join(cases, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func fakeServer(t *testing.T, call fakevllm.ToolCall) Target {
	t.Helper()
	server := httptest.NewServer(fakevllm.Handler(fakevllm.Options{Worker: "gpu", Model: "qwen", ToolCall: call}))
	t.Cleanup(server.Close)
	return Target{Endpoint: server.URL, Model: "qwen"}
}

func TestDecodeEvalSetV2AcceptsConversationsAndTools(t *testing.T) {
	set := toolSet(t, 1)
	weather := set.Prompts[1]
	if !weather.HasTools() || len(weather.Messages) != 2 || string(weather.ToolChoice) != `"auto"` || weather.schemas["weather"] == nil || weather.schemas["time"] == nil {
		t.Fatalf("tool case = %+v", weather)
	}
	if set.Prompts[0].HasTools() {
		t.Fatal("a plain prompt is not a tool case")
	}
	// A conversation may carry an earlier call and its result.
	body := `{"schema":"infercrane.dev/quality-eval-set/v2","name":"agent","version":"1","prompts":[{"id":"follow-up","messages":[
		{"role":"user","content":"Weather in Berlin?"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Berlin\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"12 C, cloudy"}
	],"tools":` + weatherTools + `,"tool_choice":{"type":"function","function":{"name":"weather"}}}]}`
	if _, err := DecodeEvalSet([]byte(body)); err != nil {
		t.Fatalf("conversation with a tool result: %v", err)
	}
	// A graded set scores answer text, so it may hold a conversation.
	graded := `{"schema":"infercrane.dev/quality-eval-set/v2","name":"s","version":"1","grading":"exact_match","prompts":[{"id":"a","messages":[{"role":"user","content":"Capital of France?"}],"expected":["Paris"]}]}`
	if set, err := DecodeEvalSet([]byte(graded)); err != nil || !set.Graded() {
		t.Fatalf("graded conversation: %v", err)
	}
}

func TestDecodeEvalSetV2RejectsMalformedCases(t *testing.T) {
	wrap := func(schema, prompt string) string {
		return `{"schema":"infercrane.dev/quality-eval-set/` + schema + `","name":"s","version":"1","prompts":[` + prompt + `]}`
	}
	tool := func(parameters string) string {
		return `[{"type":"function","function":{"name":"weather","parameters":` + parameters + `}}]`
	}
	for name, body := range map[string]string{
		"tools in v1":              wrap("v1", `{"id":"a","prompt":"hi","tools":`+weatherTools+`}`),
		"messages in v1":           wrap("v1", `{"id":"a","messages":[{"role":"user","content":"hi"}]}`),
		"unknown schema":           wrap("v3", `{"id":"a","prompt":"hi"}`),
		"prompt and messages":      wrap("v2", `{"id":"a","prompt":"hi","messages":[{"role":"user","content":"hi"}]}`),
		"system and messages":      wrap("v2", `{"id":"a","system":"x","messages":[{"role":"user","content":"hi"}]}`),
		"empty messages":           wrap("v2", `{"id":"a","messages":[]}`),
		"neither":                  wrap("v2", `{"id":"a"}`),
		"ends with assistant":      wrap("v2", `{"id":"a","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`),
		"unknown role":             wrap("v2", `{"id":"a","messages":[{"role":"developer","content":"hi"}]}`),
		"blank user":               wrap("v2", `{"id":"a","messages":[{"role":"user","content":" "}]}`),
		"orphan tool result":       wrap("v2", `{"id":"a","messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"call_9","content":"x"}],"tools":`+weatherTools+`}`),
		"undeclared earlier call":  wrap("v2", `{"id":"a","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"stocks","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"x"}],"tools":`+weatherTools+`}`),
		"earlier call not JSON":    wrap("v2", `{"id":"a","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"weather","arguments":"{"}}]},{"role":"tool","tool_call_id":"c","content":"x"}],"tools":`+weatherTools+`}`),
		"unknown message field":    wrap("v2", `{"id":"a","messages":[{"role":"user","content":"hi","expected":"x"}]}`),
		"unknown tool field":       wrap("v2", `{"id":"a","prompt":"hi","tools":[{"type":"function","function":{"name":"weather"},"expected":"x"}]}`),
		"empty tools":              wrap("v2", `{"id":"a","prompt":"hi","tools":[]}`),
		"tool type":                wrap("v2", `{"id":"a","prompt":"hi","tools":[{"type":"retrieval","function":{"name":"weather"}}]}`),
		"function name":            wrap("v2", `{"id":"a","prompt":"hi","tools":[{"type":"function","function":{"name":"get weather"}}]}`),
		"duplicate function":       wrap("v2", `{"id":"a","prompt":"hi","tools":[{"type":"function","function":{"name":"w"}},{"type":"function","function":{"name":"w"}}]}`),
		"parameters not an object": wrap("v2", `{"id":"a","prompt":"hi","tools":`+tool(`"string"`)+`}`),
		"parameters bad keyword":   wrap("v2", `{"id":"a","prompt":"hi","tools":`+tool(`{"type":"object","minProperties":"two"}`)+`}`),
		"parameters remote ref":    wrap("v2", `{"id":"a","prompt":"hi","tools":`+tool(`{"$ref":"https://example.invalid/schema.json"}`)+`}`),
		"parameters old draft":     wrap("v2", `{"id":"a","prompt":"hi","tools":`+tool(`{"$schema":"http://json-schema.org/draft-04/schema#","type":"object"}`)+`}`),
		"tool_choice without tool": wrap("v2", `{"id":"a","prompt":"hi","tool_choice":"auto"}`),
		"tool_choice mode":         wrap("v2", `{"id":"a","prompt":"hi","tools":`+weatherTools+`,"tool_choice":"always"}`),
		"tool_choice unknown name": wrap("v2", `{"id":"a","prompt":"hi","tools":`+weatherTools+`,"tool_choice":{"type":"function","function":{"name":"stocks"}}}`),
		"tool_choice null":         wrap("v2", `{"id":"a","prompt":"hi","tools":`+weatherTools+`,"tool_choice":null}`),
		"tools in a graded set":    `{"schema":"infercrane.dev/quality-eval-set/v2","name":"s","version":"1","grading":"exact_match","prompts":[{"id":"a","prompt":"hi","expected":["hello"],"tools":` + weatherTools + `}]}`,
	} {
		if _, err := DecodeEvalSet([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCompareToolCasesAgainstFakeVLLM(t *testing.T) {
	set := toolSet(t, 20)
	reference := fakeServer(t, fakevllm.DefaultToolCall)
	reference.SubjectID = "ref"
	for _, tc := range []struct {
		name               string
		call               fakevllm.ToolCall
		verdict, code      string
		toolScore          float64
		broken, refInvalid int
		failures           qualityevidence.ToolCallFailures
	}{
		{name: "same call", call: fakevllm.ToolCall{Name: "weather", Arguments: `{ "city" : "Berlin" }`}, verdict: releaseguard.QualityPass, toolScore: 1},
		{name: "wrong function", call: fakevllm.ToolCall{Name: "time", Arguments: `{"zone":"Europe/Berlin"}`}, verdict: releaseguard.QualityBlock, code: CodeToolCallBroken, broken: 1, failures: qualityevidence.ToolCallFailures{WrongFunction: 1}},
		{name: "undeclared function", call: fakevllm.ToolCall{Name: "stocks", Arguments: `{"city":"Berlin"}`}, verdict: releaseguard.QualityBlock, code: CodeToolCallBroken, broken: 1, failures: qualityevidence.ToolCallFailures{WrongFunction: 1}},
		{name: "invalid JSON", call: fakevllm.ToolCall{Name: "weather", Arguments: `{"city":"Berlin"`}, verdict: releaseguard.QualityBlock, code: CodeToolCallBroken, broken: 1, failures: qualityevidence.ToolCallFailures{MalformedArguments: 1}},
		{name: "schema-invalid arguments", call: fakevllm.ToolCall{Name: "weather", Arguments: `{"city":42}`}, verdict: releaseguard.QualityBlock, code: CodeToolCallBroken, broken: 1, failures: qualityevidence.ToolCallFailures{SchemaInvalid: 1}},
		{name: "extra argument", call: fakevllm.ToolCall{Name: "weather", Arguments: `{"city":"Berlin","units":"C"}`}, verdict: releaseguard.QualityBlock, code: CodeToolCallBroken, broken: 1, failures: qualityevidence.ToolCallFailures{SchemaInvalid: 1}},
		// A valid call with other arguments is a wrong answer, not a broken
		// call: it counts in the mean like any other disagreement.
		{name: "different valid arguments", call: fakevllm.ToolCall{Name: "weather", Arguments: `{"city":"Paris"}`}, verdict: releaseguard.QualityPass, toolScore: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := fakeServer(t, tc.call)
			candidate.SubjectID = "cand"
			comparison, err := Compare(context.Background(), HTTPCompleter{}, set, reference, candidate, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := &qualityevidence.ToolCallScores{Cases: 1, Score: tc.toolScore, Broken: tc.broken, ReferenceInvalid: tc.refInvalid, Candidate: tc.failures}
			if *comparison.ToolCalls != *want {
				t.Fatalf("tool calls = %+v, want %+v", *comparison.ToolCalls, *want)
			}
			// The 20 text cases agree, so the mean alone never blocks.
			if wantScore := round((20 + tc.toolScore) / 21); comparison.Score != wantScore {
				t.Fatalf("score = %v, want %v", comparison.Score, wantScore)
			}
			if comparison.Eval.Verdict != tc.verdict || tc.code != "" && comparison.Eval.Code != tc.code {
				t.Fatalf("verdict = %s %s (%s), want %s %s", comparison.Eval.Verdict, comparison.Eval.Code, comparison.Eval.Message, tc.verdict, tc.code)
			}
			payload := comparison.Payload("svc-opt-1", false)
			if err = payload.Validate(); err != nil {
				t.Fatal(err)
			}
			if payload.Passed != (tc.verdict != releaseguard.QualityBlock) || payload.Distribution.ToolCalls == nil || *payload.Distribution.ToolCalls != *want {
				t.Fatalf("candidate payload = %+v tool calls %+v", payload, payload.Distribution.ToolCalls)
			}
			referencePayload := comparison.Payload("svc-opt-0", true)
			if err = referencePayload.Validate(); err != nil || !referencePayload.Passed || referencePayload.Distribution.ToolCalls.Score != 1 || referencePayload.Distribution.ToolCalls.Broken != 0 || referencePayload.Distribution.ToolCalls.Candidate != (qualityevidence.ToolCallFailures{}) {
				t.Fatalf("reference payload = %+v %v", referencePayload.Distribution.ToolCalls, err)
			}
			encoded, _ := json.Marshal(payload)
			for _, content := range []string{"Berlin", "Paris", "weather"} {
				if strings.Contains(string(encoded), content) {
					t.Fatalf("evidence carries call content %q: %s", content, encoded)
				}
			}
		})
	}
}

func TestCompareToolCasesWhenTheReferenceCallIsInvalid(t *testing.T) {
	set := toolSet(t, 1)
	invalid := fakevllm.ToolCall{Name: "weather", Arguments: `{"city":""}`}
	reference := fakeServer(t, invalid)
	reference.SubjectID = "ref"
	for name, tc := range map[string]struct {
		call      fakevllm.ToolCall
		score     float64
		candidate qualityevidence.ToolCallFailures
	}{
		// The same call fails the schema on both sides; that is counted,
		// but nothing broke.
		"same invalid call": {call: invalid, score: 1, candidate: qualityevidence.ToolCallFailures{SchemaInvalid: 1}},
		"valid call":        {call: fakevllm.DefaultToolCall, score: 0},
	} {
		candidate := fakeServer(t, tc.call)
		candidate.SubjectID = "cand"
		comparison, err := Compare(context.Background(), HTTPCompleter{}, set, reference, candidate, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing the reference did correctly can break, so the call alone
		// never blocks; the case measures agreement.
		if got := *comparison.ToolCalls; got != (qualityevidence.ToolCallScores{Cases: 1, Score: tc.score, ReferenceInvalid: 1, Candidate: tc.candidate, Reference: qualityevidence.ToolCallFailures{SchemaInvalid: 1}}) || comparison.Eval.Code == CodeToolCallBroken {
			t.Errorf("%s: tool calls = %+v verdict %s", name, got, comparison.Eval.Code)
		}
	}
}

func TestCompareAgainstItselfScoresToolCasesOne(t *testing.T) {
	set := toolSet(t, 1)
	reference := fakeServer(t, fakevllm.ToolCall{Name: "weather", Arguments: `not json`})
	reference.SubjectID = "ref"
	comparison, err := Compare(context.Background(), HTTPCompleter{}, set, reference, reference, nil)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Score != 1 || *comparison.ToolCalls != (qualityevidence.ToolCallScores{Cases: 1, Score: 1, ReferenceInvalid: 1, Reference: qualityevidence.ToolCallFailures{MalformedArguments: 1}}) || comparison.Eval.Verdict != releaseguard.QualityPass {
		t.Fatalf("self comparison = %+v %+v", comparison.Eval, comparison.ToolCalls)
	}
}

func TestCompareCountsACallAsAnAnswer(t *testing.T) {
	// The fake answers a tool request with content null and one call. Before
	// version 3 that was no answer text, and the check stopped.
	set := toolSet(t, 0)
	reference, candidate := fakeServer(t, fakevllm.DefaultToolCall), fakeServer(t, fakevllm.DefaultToolCall)
	reference.SubjectID, candidate.SubjectID = "ref", "cand"
	comparison, err := Compare(context.Background(), HTTPCompleter{}, set, reference, candidate, nil)
	if err != nil || comparison.Score != 1 {
		t.Fatalf("comparison = %+v, %v", comparison, err)
	}
}

func TestToolCaseFailureKinds(t *testing.T) {
	set := toolSet(t, 0)
	prompt := set.Prompts[0]
	called := func(name, arguments string) ToolCall {
		return ToolCall{Type: "function", Function: ToolCallFunction{Name: name, Arguments: arguments}}
	}
	weather, clock := called("weather", `{"city":"Berlin"}`), called("time", `{"zone":"UTC"}`)
	paris := called("weather", `{"city":"Paris"}`)
	type kinds = qualityevidence.ToolCallFailures
	for _, tc := range []struct {
		name                 string
		reference, candidate Answer
		score                float64
		broken               bool
		candidateKinds       kinds
		referenceKinds       kinds
	}{
		{name: "parallel calls in another order", reference: Answer{ToolCalls: []ToolCall{weather, clock}}, candidate: Answer{ToolCalls: []ToolCall{clock, weather}}, score: 1},
		{name: "nested arguments in another key order", reference: Answer{ToolCalls: []ToolCall{called("time", `{"zone":"UTC","when":{"day":1,"hour":[9,17]}}`)}}, candidate: Answer{ToolCalls: []ToolCall{called("time", `{"when":{"hour":[9,17],"day":1.0},"zone":"UTC"}`)}}, score: 1},
		{name: "a dropped parallel call", reference: Answer{ToolCalls: []ToolCall{weather, clock}}, candidate: Answer{ToolCalls: []ToolCall{weather}}, broken: true, candidateKinds: kinds{MissingCall: 1}},
		{name: "text instead of a call", reference: Answer{ToolCalls: []ToolCall{weather}}, candidate: Answer{Content: "It is 12 C in Berlin."}, broken: true, candidateKinds: kinds{MissingCall: 1}},
		{name: "an extra call", reference: Answer{ToolCalls: []ToolCall{weather}}, candidate: Answer{ToolCalls: []ToolCall{weather, clock}}, broken: true, candidateKinds: kinds{ExtraCalls: 1}},
		{name: "another function in its place", reference: Answer{ToolCalls: []ToolCall{weather}}, candidate: Answer{ToolCalls: []ToolCall{clock}}, broken: true, candidateKinds: kinds{WrongFunction: 1}},
		{name: "valid call with other arguments", reference: Answer{ToolCalls: []ToolCall{weather}}, candidate: Answer{ToolCalls: []ToolCall{paris}}},
		{name: "several failures in one case", reference: Answer{ToolCalls: []ToolCall{weather}}, candidate: Answer{ToolCalls: []ToolCall{called("weather", `{"city":7}`), called("stocks", `{`)}}, broken: true, candidateKinds: kinds{MalformedArguments: 1, WrongFunction: 1, SchemaInvalid: 1, ExtraCalls: 1}},
		{name: "the right answer is no call", reference: Answer{Content: "Which city?"}, candidate: Answer{Content: "which city"}, score: 1},
		{name: "a call where the reference asked back", reference: Answer{Content: "Which city?"}, candidate: Answer{ToolCalls: []ToolCall{weather}}, candidateKinds: kinds{UnexpectedCall: 1}},
		{name: "the reference's own call is invalid", reference: Answer{ToolCalls: []ToolCall{called("stocks", `{"city":"Berlin"}`)}}, candidate: Answer{ToolCalls: []ToolCall{weather}}, candidateKinds: kinds{WrongFunction: 1}, referenceKinds: kinds{WrongFunction: 1}},
	} {
		result := scoreToolCase(prompt, tc.reference, tc.candidate)
		if result.score != tc.score || result.broken != tc.broken || result.candidate != tc.candidateKinds || result.reference != tc.referenceKinds {
			t.Errorf("%s: %+v", tc.name, result)
		}
	}
}

func TestHTTPCompleterSendsToolsAndReadsCalls(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		// One server sends arguments as an object instead of a string.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Berlin\"}"}},{"id":"c2","type":"function","function":{"name":"time","arguments":{"zone":"UTC"}}}]}}]}`))
	}))
	defer server.Close()
	set := toolSet(t, 0)
	answer, err := HTTPCompleter{}.Complete(context.Background(), Target{Endpoint: server.URL, SubjectID: "r"}, set, set.Prompts[0])
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "" || len(answer.ToolCalls) != 2 || answer.ToolCalls[0].Function.Arguments != `{"city":"Berlin"}` || answer.ToolCalls[1].Function.Arguments != `{"zone":"UTC"}` {
		t.Fatalf("answer = %+v", answer)
	}
	if tools, _ := seen["tools"].([]any); len(tools) != 2 || seen["tool_choice"] != "auto" {
		t.Fatalf("request tools = %v choice %v", seen["tools"], seen["tool_choice"])
	}
	parameters := seen["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if parameters["additionalProperties"] != false {
		t.Fatalf("parameters were not sent as declared: %v", parameters)
	}
	if messages, _ := seen["messages"].([]any); len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", seen["messages"])
	}
}

func TestHTTPCompleterNamesAServerThatRefusesTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `"auto" tool choice requires --enable-auto-tool-choice`, http.StatusBadRequest)
	}))
	defer server.Close()
	set := toolSet(t, 0)
	if _, err := (HTTPCompleter{}).Complete(context.Background(), Target{Endpoint: server.URL, SubjectID: "r"}, set, set.Prompts[0]); !errors.Is(err, ErrToolsRefused) || !strings.Contains(err.Error(), "may not accept tool calls") {
		t.Fatalf("err = %v", err)
	}
}

func TestExampleToolEvalSetDecodes(t *testing.T) {
	body, err := os.ReadFile("../../examples/evaluation/eval-set-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	set, err := DecodeEvalSet(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := 0
	for _, prompt := range set.Prompts {
		if prompt.HasTools() {
			tools++
		}
	}
	if set.Schema != EvalSetSchemaV2 || tools < 2 || tools == len(set.Prompts) {
		t.Fatalf("example has %d tool cases of %d", tools, len(set.Prompts))
	}
}

func TestV1EvalSetsKeepTheirIdentityAndScores(t *testing.T) {
	set := testSet(t)
	if set.Schema != EvalSetSchemaV1 || set.Digest != Digest([]byte(`{"schema":"infercrane.dev/quality-eval-set/v1","name":"support","version":"1","prompts":[{"id":"a","prompt":"1"},{"id":"b","prompt":"2"},{"id":"c","prompt":"3"},{"id":"d","prompt":"4"}]}`)) {
		t.Fatalf("v1 set = %+v", set)
	}
	reference, candidate := fakeServer(t, fakevllm.DefaultToolCall), fakeServer(t, fakevllm.DefaultToolCall)
	reference.SubjectID, candidate.SubjectID = "ref", "cand"
	comparison, err := Compare(context.Background(), HTTPCompleter{}, set, reference, candidate, func() time.Time { return time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	// A v1 set never sends tools, so the fake answers in text.
	if comparison.Score != 1 || comparison.ToolCalls != nil || comparison.Payload("svc", false).Distribution.ToolCalls != nil {
		t.Fatalf("v1 comparison = %+v", comparison)
	}
}

// recordingCompleter records which cases each target was asked.
type recordingCompleter struct {
	Completer
	mu    sync.Mutex
	asked map[string][]string
}

func (c *recordingCompleter) Complete(ctx context.Context, target Target, set EvalSet, prompt Prompt) (Answer, error) {
	c.mu.Lock()
	if c.asked == nil {
		c.asked = map[string][]string{}
	}
	c.asked[target.SubjectID] = append(c.asked[target.SubjectID], prompt.ID)
	c.mu.Unlock()
	return c.Completer.Complete(ctx, target, set, prompt)
}

func refusingServer(t *testing.T, subject string) Target {
	t.Helper()
	server := httptest.NewServer(fakevllm.Handler(fakevllm.Options{Worker: "gpu", Model: "qwen", RefuseTools: true}))
	t.Cleanup(server.Close)
	return Target{Endpoint: server.URL, Model: "qwen", SubjectID: subject}
}

func TestCompareLeavesToolCasesTheReferenceRefusesUnchecked(t *testing.T) {
	set := toolSet(t, 20)
	reference := refusingServer(t, "ref")
	for name, candidate := range map[string]Target{"candidate takes tools": fakeServer(t, fakevllm.DefaultToolCall), "candidate refuses too": refusingServer(t, "")} {
		t.Run(name, func(t *testing.T) {
			candidate.SubjectID = "cand"
			completer := &recordingCompleter{Completer: HTTPCompleter{}}
			comparison, err := Compare(context.Background(), completer, set, reference, candidate, nil)
			if err != nil {
				t.Fatal(err)
			}
			// The case is unknown: no score, not asked of the candidate, and
			// never a reason to block.
			want := qualityevidence.ToolCallScores{NotChecked: 1}
			if *comparison.ToolCalls != want || len(comparison.Scores) != 20 || len(comparison.Checked) != 20 || slices.Contains(comparison.Checked, "weather") || slices.Contains(completer.asked["cand"], "weather") || len(completer.asked["ref"]) != 21 {
				t.Fatalf("comparison = %+v tool calls %+v asked %v", comparison, *comparison.ToolCalls, completer.asked)
			}
			if comparison.Score != 1 || comparison.Eval.Verdict != releaseguard.QualityPass {
				t.Fatalf("verdict = %s %s (%s) score %v", comparison.Eval.Verdict, comparison.Eval.Code, comparison.Eval.Message, comparison.Score)
			}
			// Both sides pair on the 20 scored prompts, not on the set's 21.
			for _, reference := range []bool{false, true} {
				payload := comparison.Payload("svc", reference)
				if err = payload.Validate(); err != nil {
					t.Fatal(err)
				}
				if payload.SampleCount != 20 || payload.Distribution.PairingDigest != comparison.PairingDigest() || payload.Distribution.PairingDigest == set.PairingDigest() || *payload.Distribution.ToolCalls != want {
					t.Fatalf("payload (reference %v) = %+v tool calls %+v", reference, payload, payload.Distribution.ToolCalls)
				}
			}
		})
	}
}

func TestCompareBlocksACandidateThatRefusesTools(t *testing.T) {
	set := toolSet(t, 20)
	reference := fakeServer(t, fakevllm.DefaultToolCall)
	reference.SubjectID = "ref"
	comparison, err := Compare(context.Background(), HTTPCompleter{}, set, reference, refusingServer(t, "cand"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := qualityevidence.ToolCallScores{Cases: 1, Broken: 1, Candidate: qualityevidence.ToolCallFailures{ToolsRefused: 1}}
	if *comparison.ToolCalls != want || comparison.Eval.Verdict != releaseguard.QualityBlock || comparison.Eval.Code != CodeToolCallBroken || !strings.Contains(comparison.Eval.Message, "refused tools") {
		t.Fatalf("tool calls = %+v verdict %s %s (%s)", *comparison.ToolCalls, comparison.Eval.Verdict, comparison.Eval.Code, comparison.Eval.Message)
	}
	if payload := comparison.Payload("svc", false); payload.Validate() != nil || payload.Passed {
		t.Fatalf("payload = %+v", payload)
	}
	// Refusing is broken even where the reference answered without a call:
	// the request with tools still failed.
	noCall := scriptedToolCompleter{reference: Answer{Content: "Which city?"}}
	comparison, err = Compare(context.Background(), noCall, toolSet(t, 0), Target{SubjectID: "ref"}, Target{SubjectID: "cand"}, nil)
	if err != nil || comparison.ToolCalls.Broken != 1 || comparison.Eval.Verdict != releaseguard.QualityBlock {
		t.Fatalf("no-call case: %+v %v", comparison.ToolCalls, err)
	}
	// Any other candidate failure is still an error, never a verdict.
	failing := scriptedToolCompleter{reference: Answer{Content: "Which city?"}, candidateErr: errors.New("connection refused")}
	if _, err = Compare(context.Background(), failing, toolSet(t, 0), Target{SubjectID: "ref"}, Target{SubjectID: "cand"}, nil); err == nil || errors.Is(err, ErrToolsRefused) {
		t.Fatalf("candidate error = %v", err)
	}
}

// scriptedToolCompleter answers as the reference and fails as the candidate:
// with ErrToolsRefused unless candidateErr is set.
type scriptedToolCompleter struct {
	reference    Answer
	candidateErr error
}

func (c scriptedToolCompleter) Complete(_ context.Context, target Target, _ EvalSet, prompt Prompt) (Answer, error) {
	if target.SubjectID == "ref" {
		return c.reference, nil
	}
	if c.candidateErr != nil {
		return Answer{}, c.candidateErr
	}
	return Answer{}, fmt.Errorf("prompt %s: %w", prompt.ID, ErrToolsRefused)
}

func TestCompareAgainstItselfLeavesRefusedToolCasesUnchecked(t *testing.T) {
	reference := refusingServer(t, "ref")
	comparison, err := Compare(context.Background(), HTTPCompleter{}, toolSet(t, 3), reference, reference, nil)
	if err != nil || *comparison.ToolCalls != (qualityevidence.ToolCallScores{NotChecked: 1}) || comparison.Score != 1 || comparison.Eval.Verdict != releaseguard.QualityPass {
		t.Fatalf("self comparison = %+v %v", comparison, err)
	}
}

func TestCompareChecksNothingWhenTheReferenceRefusesEveryCase(t *testing.T) {
	candidate := fakeServer(t, fakevllm.DefaultToolCall)
	candidate.SubjectID = "cand"
	if _, err := Compare(context.Background(), HTTPCompleter{}, toolSet(t, 0), refusingServer(t, "ref"), candidate, nil); !errors.Is(err, ErrNothingChecked) {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPCompleterRefusalIsOnlyForRequestsWithTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()
	set := toolSet(t, 1)
	target := Target{Endpoint: server.URL, SubjectID: "r"}
	if _, err := (HTTPCompleter{}).Complete(context.Background(), target, set, set.Prompts[1]); !errors.Is(err, ErrToolsRefused) {
		t.Fatalf("tool case err = %v", err)
	}
	// A plain prompt the server rejects is a failed request, not a refusal.
	if _, err := (HTTPCompleter{}).Complete(context.Background(), target, set, set.Prompts[0]); err == nil || errors.Is(err, ErrToolsRefused) {
		t.Fatalf("plain prompt err = %v", err)
	}
}

func TestDefaultSuiteKeepsTheAnswerPromptsAndAddsToolCases(t *testing.T) {
	set := Default()
	answers := []string{
		"arithmetic-sum", "arithmetic-product", "arithmetic-word", "arithmetic-percent", "unit-minutes",
		"fact-capital", "fact-planet", "fact-symbol", "fact-boiling", "text-uppercase", "text-word-count",
		"sentiment-positive", "sentiment-negative", "extract-json", "extract-year", "code-output", "code-sql",
		"logic-order", "logic-sequence", "date-weekday", "translate-french", "translate-spanish", "list-colors", "summarize-short",
	}
	tools := []string{"tool-single-call", "tool-parallel-calls", "tool-nested-arguments", "tool-enum-argument", "tool-no-call"}
	ids := make([]string, 0, len(set.Prompts))
	for index, prompt := range set.Prompts {
		ids = append(ids, prompt.ID)
		// Every case is a plain prompt, so tests and the agent can treat its
		// text as content that must never leave the machine.
		if strings.TrimSpace(prompt.Prompt) == "" || prompt.Messages != nil || prompt.HasTools() != (index >= len(answers)) {
			t.Errorf("case %s = %+v", prompt.ID, prompt)
		}
		if prompt.HasTools() && string(prompt.ToolChoice) != `"auto"` {
			t.Errorf("case %s tool_choice = %s", prompt.ID, prompt.ToolChoice)
		}
	}
	if !slices.Equal(ids, append(answers, tools...)) {
		t.Fatalf("default suite cases = %v", ids)
	}
}

func TestDefaultToolCasesCatchBrokenCalls(t *testing.T) {
	cases := map[string]Prompt{}
	for _, prompt := range Default().Prompts {
		cases[prompt.ID] = prompt
	}
	called := func(name, arguments string) ToolCall {
		return ToolCall{Type: "function", Function: ToolCallFunction{Name: name, Arguments: arguments}}
	}
	calls := func(made ...ToolCall) Answer { return Answer{ToolCalls: made} }
	paris, tokyo, london := called("get_weather", `{"city":"Paris"}`), called("get_weather", `{"city":"Tokyo"}`), called("get_weather", `{"city":"London"}`)
	event := called("create_event", `{"title":"Design review","start":{"date":"2026-12-03","time":"14:00"},"attendees":[{"name":"Ana"},{"name":"Ben"}]}`)
	cool := called("set_thermostat", `{"mode":"cool","temperature_c":21}`)
	type kinds = qualityevidence.ToolCallFailures
	for _, tc := range []struct {
		name, id             string
		reference, candidate Answer
		score                float64
		broken               bool
		failures             kinds
	}{
		{name: "same call", id: "tool-single-call", reference: calls(paris), candidate: calls(called("get_weather", `{ "city": "Paris" }`)), score: 1},
		{name: "empty city", id: "tool-single-call", reference: calls(paris), candidate: calls(called("get_weather", `{"city":""}`)), broken: true, failures: kinds{SchemaInvalid: 1}},
		{name: "undeclared argument", id: "tool-single-call", reference: calls(paris), candidate: calls(called("get_weather", `{"city":"Paris","unit":"C"}`)), broken: true, failures: kinds{SchemaInvalid: 1}},
		{name: "answer in text", id: "tool-single-call", reference: calls(paris), candidate: Answer{Content: "It is sunny in Paris."}, broken: true, failures: kinds{MissingCall: 1}},
		{name: "both cities in another order", id: "tool-parallel-calls", reference: calls(tokyo, london), candidate: calls(london, tokyo), score: 1},
		{name: "one city dropped", id: "tool-parallel-calls", reference: calls(tokyo, london), candidate: calls(tokyo), broken: true, failures: kinds{MissingCall: 1}},
		{name: "same nested arguments", id: "tool-nested-arguments", reference: calls(event), candidate: calls(called("create_event", `{"attendees":[{"name":"Ana"},{"name":"Ben"}],"start":{"time":"14:00","date":"2026-12-03"},"title":"Design review"}`)), score: 1},
		{name: "attendees flattened", id: "tool-nested-arguments", reference: calls(event), candidate: calls(called("create_event", `{"title":"Design review","start":{"date":"2026-12-03","time":"14:00"},"attendees":["Ana","Ben"]}`)), broken: true, failures: kinds{SchemaInvalid: 1}},
		{name: "date in words", id: "tool-nested-arguments", reference: calls(event), candidate: calls(called("create_event", `{"title":"Design review","start":{"date":"December 3","time":"14:00"},"attendees":[{"name":"Ana"},{"name":"Ben"}]}`)), broken: true, failures: kinds{SchemaInvalid: 1}},
		{name: "same enum value", id: "tool-enum-argument", reference: calls(cool), candidate: calls(called("set_thermostat", `{"temperature_c":21,"mode":"cool"}`)), score: 1},
		{name: "value outside the enum", id: "tool-enum-argument", reference: calls(cool), candidate: calls(called("set_thermostat", `{"mode":"cooling","temperature_c":21}`)), broken: true, failures: kinds{SchemaInvalid: 1}},
		{name: "temperature as text", id: "tool-enum-argument", reference: calls(cool), candidate: calls(called("set_thermostat", `{"mode":"cool","temperature_c":"21"}`)), broken: true, failures: kinds{SchemaInvalid: 1}},
		{name: "another legal mode", id: "tool-enum-argument", reference: calls(cool), candidate: calls(called("set_thermostat", `{"mode":"heat","temperature_c":21}`))},
		{name: "answers without a call", id: "tool-no-call", reference: Answer{Content: "366"}, candidate: Answer{Content: "366."}, score: 1},
		{name: "calls a tool anyway", id: "tool-no-call", reference: Answer{Content: "366"}, candidate: calls(paris), failures: kinds{UnexpectedCall: 1}},
	} {
		prompt, ok := cases[tc.id]
		if !ok {
			t.Fatalf("no case %s", tc.id)
		}
		result := scoreToolCase(prompt, tc.reference, tc.candidate)
		if result.referenceInvalid || result.score != tc.score || result.broken != tc.broken || result.candidate != tc.failures {
			t.Errorf("%s: %s: %+v", tc.id, tc.name, result)
		}
	}
}

func TestDefaultSuiteChecksToolCallsUnlessTheReferenceRefusesThem(t *testing.T) {
	set := Default()
	takesTools := fakeServer(t, fakevllm.DefaultToolCall)
	takesTools.SubjectID = "takes-tools"
	// The reference runs without tool calling: the 24 answer prompts are
	// checked and the five tool cases stay unknown.
	comparison, err := Compare(context.Background(), HTTPCompleter{}, set, refusingServer(t, "ref"), takesTools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *comparison.ToolCalls != (qualityevidence.ToolCallScores{NotChecked: 5}) || len(comparison.Scores) != 24 || comparison.Eval.Verdict != releaseguard.QualityPass {
		t.Fatalf("refusing reference: %+v verdict %s (%s)", *comparison.ToolCalls, comparison.Eval.Verdict, comparison.Eval.Message)
	}
	if payload := comparison.Payload("svc", false); payload.Validate() != nil || payload.SampleCount != 24 {
		t.Fatalf("payload = %+v", payload)
	}
	// The reference takes tools and the candidate does not: blocked.
	comparison, err = Compare(context.Background(), HTTPCompleter{}, set, takesTools, refusingServer(t, "cand"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls := *comparison.ToolCalls; calls.Cases != 5 || calls.NotChecked != 0 || calls.Broken != 5 || calls.Candidate.ToolsRefused != 5 || comparison.Eval.Verdict != releaseguard.QualityBlock || comparison.Eval.Code != CodeToolCallBroken {
		t.Fatalf("refusing candidate: %+v verdict %s", calls, comparison.Eval.Verdict)
	}
}
