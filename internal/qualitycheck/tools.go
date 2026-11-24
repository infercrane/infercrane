package qualitycheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/infercrane/infercrane/internal/qualityevidence"
)

var functionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Message is one turn of a v2 case's conversation, in the OpenAI chat format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool is an OpenAI function tool the model may call.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function declares a tool's name and the JSON Schema of its arguments.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolCall is a function call: one the model made, or one an assistant turn
// in a case's conversation made earlier.
type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the called function and its arguments as JSON text.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Answer is one revision's reply to one case: its text and its tool calls.
type Answer struct {
	Content   string
	ToolCalls []ToolCall
}

// answered reports whether the reply holds answer text or a tool call.
func (a Answer) answered() bool { return Answered(a.Content) || len(a.ToolCalls) > 0 }

// HasTools reports whether the case declares function tools. Such a case is
// a tool case: it is scored on the calls and counts in the tool-call score.
func (p Prompt) HasTools() bool { return len(p.Tools) > 0 }

// validateTools checks a case's tools and tool_choice and resolves each
// function's parameters schema. Remote $ref is refused: there is no loader.
func validateTools(prompt Prompt) (map[string]*jsonschema.Resolved, error) {
	if prompt.Tools == nil {
		if prompt.ToolChoice != nil {
			return nil, errors.New("tool_choice needs tools")
		}
		return nil, nil
	}
	if len(prompt.Tools) < 1 || len(prompt.Tools) > maxTools {
		return nil, fmt.Errorf("tools must list 1 to %d functions", maxTools)
	}
	resolved := make(map[string]*jsonschema.Resolved, len(prompt.Tools))
	for _, tool := range prompt.Tools {
		name := tool.Function.Name
		if tool.Type != "function" {
			return nil, errors.New(`every tool must have type "function"`)
		}
		if !functionNamePattern.MatchString(name) {
			return nil, errors.New("function names must be 1 to 64 letters, digits, underscores, or dashes")
		}
		if _, duplicate := resolved[name]; duplicate {
			return nil, fmt.Errorf("function %q is declared twice", name)
		}
		if len(tool.Function.Description) > maxPromptBytes {
			return nil, fmt.Errorf("function %q description exceeds %d KiB", name, maxPromptBytes>>10)
		}
		parameters := bytes.TrimSpace(tool.Function.Parameters)
		if len(parameters) == 0 {
			// OpenAI treats a function without parameters as taking none.
			parameters = []byte(`{"type":"object"}`)
		}
		if parameters[0] != '{' {
			return nil, fmt.Errorf("function %q parameters must be a JSON Schema object", name)
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(parameters, &schema); err != nil {
			return nil, fmt.Errorf("function %q parameters are not a JSON Schema: %w", name, err)
		}
		switch schema.Schema {
		case "", "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2020-12/schema":
		default:
			// Arguments could never validate against another draft.
			return nil, fmt.Errorf("function %q parameters must use JSON Schema draft-07 or 2020-12", name)
		}
		schemaResolved, err := schema.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("function %q parameters are not a usable JSON Schema: %w", name, err)
		}
		resolved[name] = schemaResolved
	}
	if prompt.ToolChoice != nil {
		var mode string
		if json.Unmarshal(prompt.ToolChoice, &mode) == nil {
			if mode != "auto" && mode != "none" && mode != "required" {
				return nil, errors.New(`tool_choice must be "auto", "none", "required", or a named function`)
			}
			return resolved, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(prompt.ToolChoice))
		decoder.DisallowUnknownFields()
		var named struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if decoder.Decode(&named) != nil || named.Type != "function" {
			return nil, errors.New(`tool_choice must be "auto", "none", "required", or a named function`)
		}
		if _, declared := resolved[named.Function.Name]; !declared {
			return nil, fmt.Errorf("tool_choice names %q, which is not a declared function", named.Function.Name)
		}
	}
	return resolved, nil
}

// validateMessages checks a v2 conversation: known roles, bounded content,
// assistant calls to declared functions, tool results that answer an earlier
// call, and a last turn the model answers.
func validateMessages(messages []Message, functions map[string]*jsonschema.Resolved) error {
	if len(messages) < 1 || len(messages) > maxMessages {
		return fmt.Errorf("messages must hold 1 to %d turns", maxMessages)
	}
	calls := map[string]struct{}{}
	for index, message := range messages {
		turn := index + 1
		if len(message.Content) > maxPromptBytes {
			return fmt.Errorf("message %d exceeds %d KiB", turn, maxPromptBytes>>10)
		}
		blank := strings.TrimSpace(message.Content) == ""
		switch message.Role {
		case "system", "user":
			if blank || message.ToolCalls != nil || message.ToolCallID != "" {
				return fmt.Errorf("message %d (%s) needs content and nothing else", turn, message.Role)
			}
		case "assistant":
			if message.ToolCallID != "" || blank && len(message.ToolCalls) == 0 {
				return fmt.Errorf("message %d (assistant) needs content or tool_calls", turn)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || len(call.ID) > 64 || call.Type != "function" {
					return fmt.Errorf("message %d tool calls need an id of at most 64 characters and type \"function\"", turn)
				}
				if _, declared := functions[call.Function.Name]; !declared {
					return fmt.Errorf("message %d calls %q, which is not a declared function", turn, call.Function.Name)
				}
				if !json.Valid([]byte(call.Function.Arguments)) || len(call.Function.Arguments) > maxPromptBytes {
					return fmt.Errorf("message %d call %s arguments must be JSON of at most %d KiB", turn, call.ID, maxPromptBytes>>10)
				}
				calls[call.ID] = struct{}{}
			}
		case "tool":
			if blank || message.ToolCalls != nil {
				return fmt.Errorf("message %d (tool) needs content", turn)
			}
			if _, called := calls[message.ToolCallID]; !called {
				return fmt.Errorf("message %d answers tool call %q, which no earlier assistant turn made", turn, message.ToolCallID)
			}
		default:
			return fmt.Errorf("message %d role must be system, user, assistant, or tool", turn)
		}
	}
	if last := messages[len(messages)-1].Role; last != "user" && last != "tool" {
		return errors.New("the last message must be a user or tool turn")
	}
	return nil
}

// requestMessages is the conversation sent for the case.
func (p Prompt) requestMessages() []Message {
	if p.Messages != nil {
		return p.Messages
	}
	messages := make([]Message, 0, 2)
	if p.System != "" {
		messages = append(messages, Message{Role: "system", Content: p.System})
	}
	return append(messages, Message{Role: "user", Content: p.Prompt})
}

// call is a parsed tool call: the function, its canonical arguments, and
// whether the arguments are JSON, the function is declared, and the
// arguments validate against its parameters.
type call struct {
	name, arguments       string
	json, declared, valid bool
}

// parseCalls reads calls against the case's declared functions, sorted by
// function and arguments so the order of parallel calls does not count.
func (p Prompt) parseCalls(calls []ToolCall) []call {
	parsed := make([]call, 0, len(calls))
	for _, made := range calls {
		parsedCall := call{name: made.Function.Name, arguments: made.Function.Arguments}
		schema, declared := p.schemas[made.Function.Name]
		parsedCall.declared = declared
		var arguments any
		if json.Unmarshal([]byte(made.Function.Arguments), &arguments) == nil {
			encoded, _ := json.Marshal(arguments) // map keys come out sorted
			parsedCall.arguments, parsedCall.json = string(encoded), true
			parsedCall.valid = declared && schema.Validate(arguments) == nil
		}
		parsed = append(parsed, parsedCall)
	}
	slices.SortFunc(parsed, func(a, b call) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		return strings.Compare(a.arguments, b.arguments)
	})
	return parsed
}

// allValid reports whether every call is JSON, declared, and schema-valid.
func allValid(calls []call) bool {
	for _, made := range calls {
		if !made.valid {
			return false
		}
	}
	return true
}

// ownFailures marks how one side's calls fail on their own. Each kind counts
// once per case.
func ownFailures(calls []call) qualityevidence.ToolCallFailures {
	var failures qualityevidence.ToolCallFailures
	for _, made := range calls {
		if !made.json {
			failures.MalformedArguments = 1
		}
		if !made.declared {
			failures.WrongFunction = 1
		} else if made.json && !made.valid {
			failures.SchemaInvalid = 1
		}
	}
	return failures
}

// candidateFailures marks how the candidate's calls fail against the
// reference's: its own failures, plus calls the reference made and it did
// not (missing), calls to another function in their place (wrong), and calls
// beyond the reference's (extra).
func candidateFailures(reference, candidate []call) qualityevidence.ToolCallFailures {
	failures := ownFailures(candidate)
	unmatched := map[string]int{}
	for _, made := range reference {
		unmatched[made.name]++
	}
	extra := 0
	for _, made := range candidate {
		if unmatched[made.name] > 0 {
			unmatched[made.name]--
		} else {
			extra++
		}
	}
	missing := 0
	for _, count := range unmatched {
		missing += count
	}
	if swapped := min(missing, extra); swapped > 0 {
		failures.WrongFunction = 1
		missing, extra = missing-swapped, extra-swapped
	}
	if missing > 0 {
		failures.MissingCall = 1
	}
	if extra > 0 {
		failures.ExtraCalls = 1
	}
	return failures
}

// toolCaseResult is the score of one tool case and what it says about calls.
type toolCaseResult struct {
	score float64
	// broken means the reference's calls validated and the candidate's did
	// not: a different function, arguments that are not JSON or break the
	// schema, or no call at all.
	broken bool
	// referenceInvalid means the reference's own calls did not validate.
	referenceInvalid bool
	// candidate and reference mark each kind of failure at most once.
	candidate, reference qualityevidence.ToolCallFailures
}

// scoreToolCase scores a case that declares tools. When neither side called
// a function, the text answers are compared as in any other case. When the
// reference called functions, the candidate scores 1 only if it called the
// same functions with the same arguments as canonical JSON. Order does not
// count. When the reference's calls validate, equal calls validate too.
func scoreToolCase(prompt Prompt, reference, candidate Answer) toolCaseResult {
	if len(reference.ToolCalls) == 0 {
		if len(candidate.ToolCalls) > 0 {
			return toolCaseResult{candidate: qualityevidence.ToolCallFailures{UnexpectedCall: 1}}
		}
		return toolCaseResult{score: Agreement(reference.Content, candidate.Content)}
	}
	referenceCalls, candidateCalls := prompt.parseCalls(reference.ToolCalls), prompt.parseCalls(candidate.ToolCalls)
	result := toolCaseResult{
		referenceInvalid: !allValid(referenceCalls),
		candidate:        candidateFailures(referenceCalls, candidateCalls),
		reference:        ownFailures(referenceCalls),
	}
	if callsEqual(referenceCalls, candidateCalls) {
		result.score = 1
	}
	result.broken = !result.referenceInvalid && result.candidate != (qualityevidence.ToolCallFailures{})
	return result
}

// refusedToolCase scores a case the candidate refused although the reference
// accepted the request with tools. The candidate breaks it whatever the
// reference answered: a server that cannot take tools cannot serve this
// traffic. The reference's own calls are still counted as its own.
func refusedToolCase(prompt Prompt, reference Answer) toolCaseResult {
	referenceCalls := prompt.parseCalls(reference.ToolCalls)
	return toolCaseResult{
		broken:           true,
		referenceInvalid: len(referenceCalls) > 0 && !allValid(referenceCalls),
		candidate:        qualityevidence.ToolCallFailures{ToolsRefused: 1},
		reference:        ownFailures(referenceCalls),
	}
}

// callsEqual reports whether both sides made the same calls with arguments
// that are JSON and equal as canonical JSON.
func callsEqual(reference, candidate []call) bool {
	return len(reference) == len(candidate) && !slices.ContainsFunc(reference, func(made call) bool { return !made.json }) && slices.EqualFunc(reference, candidate, func(a, b call) bool {
		return a.name == b.name && a.arguments == b.arguments && b.json
	})
}

// digest commits to the reply without storing it. A reply without tool calls
// keeps the plain text digest.
func (a Answer) digest() string {
	if len(a.ToolCalls) == 0 {
		return answerDigest(a.Content)
	}
	calls := make([]map[string]string, 0, len(a.ToolCalls))
	for _, made := range a.ToolCalls {
		calls = append(calls, map[string]string{"name": made.Function.Name, "arguments": made.Function.Arguments})
	}
	encoded, _ := json.Marshal(map[string]any{"content": a.Content, "tool_calls": calls})
	return answerDigest(string(encoded))
}
