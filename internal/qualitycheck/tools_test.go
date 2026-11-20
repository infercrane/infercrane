package qualitycheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	if _, err := (HTTPCompleter{}).Complete(context.Background(), Target{Endpoint: server.URL, SubjectID: "r"}, set, set.Prompts[0]); err == nil || !strings.Contains(err.Error(), "may not accept tool calls") {
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
