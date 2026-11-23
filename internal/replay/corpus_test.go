package replay

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type corpusRenderedRow struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	OutputLength int   `json:"output_length"`
	InputLength  *int  `json:"input_length"`
	HashIDs      []int `json:"hash_ids"`
}

// testCorpusDocument builds a corpus whose passage i is "passage-i" with
// sizes[i%len(sizes)] tokens and two questions, plus standalone prompts of
// 10, 40, 100 and 300 tokens.
func testCorpusDocument(passages int, sizes ...int) map[string]any {
	rows := make([]map[string]any, 0, passages)
	for index := range passages {
		rows = append(rows, map[string]any{"tokens": sizes[index%len(sizes)], "text": fmt.Sprintf("passage-%d", index), "questions": []map[string]any{
			{"tokens": 12, "text": fmt.Sprintf("question-%d-a", index)}, {"tokens": 14, "text": fmt.Sprintf("question-%d-b", index)},
		}})
	}
	prompts := []map[string]any{}
	for _, size := range []int{10, 40, 100, 300} {
		prompts = append(prompts, map[string]any{"tokens": size, "text": fmt.Sprintf("prompt-%d", size)})
	}
	return map[string]any{"schema_version": PromptCorpusSchemaVersion, "name": "test corpus", "license": "CC-BY-4.0", "source": "https://example.test/corpus@rev",
		"tokenizer": "example/tokenizer", "passages": rows, "prompts": prompts}
}

func testCorpus(t *testing.T, passages int, sizes ...int) *PromptCorpus {
	t.Helper()
	body, err := json.Marshal(testCorpusDocument(passages, sizes...))
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := ParsePromptCorpus(body)
	if err != nil {
		t.Fatal(err)
	}
	return corpus
}

func corpusRendered(t *testing.T, trace AIPerfTrace, namespace int) []corpusRenderedRow {
	t.Helper()
	var buffer bytes.Buffer
	if err := trace.Write(&buffer, namespace); err != nil {
		t.Fatal(err)
	}
	var rows []corpusRenderedRow
	scanner := bufio.NewScanner(&buffer)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var row corpusRenderedRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		if len(row.Messages) != 2 || row.Messages[0].Role != "system" || row.Messages[1].Role != "user" || row.InputLength != nil || row.HashIDs != nil {
			t.Fatalf("a corpus row must carry one system and one user message and no synthetic fields: %s", scanner.Text())
		}
		rows = append(rows, row)
	}
	return rows
}

// systemParts splits a rendered system message into its chain line and its
// passages.
func systemParts(t *testing.T, row corpusRenderedRow) (string, []string) {
	t.Helper()
	parts := strings.Split(row.Messages[0].Content, "\n\n")
	line, found := strings.CutPrefix(parts[0], "Session ")
	if !found || len(line) != 16 {
		t.Fatalf("system message does not open with a chain line: %q", parts[0])
	}
	return line, parts[1:]
}

func TestPromptCorpusKeepsTheTraceStructureWithRealText(t *testing.T) {
	corpus := testCorpus(t, 40, 200, 350, 500)
	synthetic, err := NewAIPerfTrace(agenticTrace(t))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := synthetic.WithPromptCorpus(corpus)
	if err != nil {
		t.Fatal(err)
	}
	rows := corpusRendered(t, trace, 0)
	if len(rows) != 5 || trace.Requests() != 5 {
		t.Fatalf("rows=%d", len(rows))
	}
	for index, expected := range []int{40, 30, 50, 10, 20} {
		if rows[index].OutputLength != expected {
			t.Fatalf("row %d output length %d, want %d", index, rows[index].OutputLength, expected)
		}
	}
	first, firstPassages := systemParts(t, rows[0])
	second, secondPassages := systemParts(t, rows[1])
	third, thirdPassages := systemParts(t, rows[2])
	single, _ := systemParts(t, rows[3])
	shorter, _ := systemParts(t, rows[4])
	// Requests with one system prefix share their chain line and the passages
	// of the shared leading block, and nothing after it.
	if first != second || len(firstPassages) == 0 || len(secondPassages) == 0 || firstPassages[0] != secondPassages[0] {
		t.Fatalf("requests with one system prefix do not share their leading text: %q %v / %q %v", first, firstPassages, second, secondPassages)
	}
	if strings.HasPrefix(rows[1].Messages[0].Content, rows[0].Messages[0].Content) {
		t.Fatal("a request with a shared system prefix resent the other request's whole prompt")
	}
	// The next turn of a session resends the previous prompt's whole blocks
	// and adds new passages; the previous turn's last block stays new.
	reused := 0
	for index := range min(len(firstPassages), len(thirdPassages)) {
		if firstPassages[index] != thirdPassages[index] {
			break
		}
		reused++
	}
	if third != first || reused < 2 || reused >= len(firstPassages) || len(thirdPassages) <= len(firstPassages) {
		t.Fatalf("session history was not resent as a prefix: reused %d of %v in %v", reused, firstPassages, thirdPassages)
	}
	if single == first || shorter == first || single == shorter {
		t.Fatalf("unrelated requests share a chain line: %q %q %q", first, single, shorter)
	}
	// Every question belongs to a passage in its own request.
	for index, row := range rows {
		_, passages := systemParts(t, row)
		question := row.Messages[1].Content
		owner := strings.TrimSuffix(strings.TrimSuffix(strings.Replace(question, "question", "passage", 1), "-a"), "-b")
		if !strings.HasPrefix(question, "question-") || !slices.Contains(passages, owner) {
			t.Fatalf("row %d asks %q, which no passage of %v answers", index, question, passages)
		}
	}
	text := trace.Properties.PromptText
	if text == nil || text.Source != PromptTextOperatorCorpus || text.Corpus != corpus.Identity() || text.StandaloneRequests != 0 || text.PassagesUsed == 0 || text.PassageUses < text.PassagesUsed {
		t.Fatalf("prompt text=%+v", text)
	}
	// Passages are never cut, so a request's planned length is within one
	// passage of its recorded length.
	if difference := text.PlannedInputTokensMean - trace.Properties.InputTokensMean; difference > 500 || difference < -500 {
		t.Fatalf("planned mean %d against recorded mean %d", text.PlannedInputTokensMean, trace.Properties.InputTokensMean)
	}
	preserved, lost := strings.Join(trace.Properties.Preserved, ","), strings.Join(trace.Properties.NotPreserved, ",")
	if !strings.Contains(preserved, "input_token_lengths_to_the_nearest_passage") || strings.Contains(preserved, "input_token_lengths,") || !strings.Contains(preserved, "shared_system_prefix_groups") {
		t.Fatalf("preserved=%v", trace.Properties.Preserved)
	}
	if !strings.Contains(lost, "prompt_content") || !strings.Contains(lost, "exact_input_token_lengths") {
		t.Fatalf("not preserved=%v", trace.Properties.NotPreserved)
	}
	// The synthetic rendering is unchanged and names no text source, so
	// evidence recorded before a corpus existed keeps its identity.
	encoded, err := json.Marshal(synthetic.Properties)
	if err != nil || strings.Contains(string(encoded), "prompt_text") || synthetic.Properties.PromptText != nil {
		t.Fatalf("synthetic properties changed: %s err=%v", encoded, err)
	}
	if got := rendered(t, synthetic, 0); len(got) != 5 || got[0].InputLength != 1500 {
		t.Fatalf("synthetic rendering changed: %+v", got)
	}
}

func TestPromptCorpusNamespacesKeepTheTextButNeverShareAPrefix(t *testing.T) {
	synthetic, err := NewAIPerfTrace(agenticTrace(t))
	if err != nil {
		t.Fatal(err)
	}
	trace, err := synthetic.WithPromptCorpus(testCorpus(t, 40, 200, 350, 500))
	if err != nil {
		t.Fatal(err)
	}
	first, again, other := corpusRendered(t, trace, 0), corpusRendered(t, trace, 0), corpusRendered(t, trace, 7)
	lines := map[string]bool{}
	for index, row := range first {
		if row.Messages[0].Content != again[index].Messages[0].Content || row.Messages[1].Content != again[index].Messages[1].Content {
			t.Fatal("one namespace rendered two different texts")
		}
		line, passages := systemParts(t, row)
		lines[line] = true
		otherLine, otherPassages := systemParts(t, other[index])
		if strings.Join(passages, "|") != strings.Join(otherPassages, "|") || row.Messages[1].Content != other[index].Messages[1].Content || row.OutputLength != other[index].OutputLength {
			t.Fatalf("a namespace changed the load: row %d", index)
		}
		if otherLine == line {
			t.Fatalf("two namespaces open row %d alike, so a server could reuse the first pass's cache", index)
		}
	}
	for _, row := range other {
		if line, _ := systemParts(t, row); lines[line] {
			t.Fatalf("chain line %q appears in two namespaces", line)
		}
	}
}

func TestPromptCorpusSendsAStandalonePromptWhenNoPassageFits(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	observations := []Observation{
		{StartedAt: start, DurationMS: 10, InputTokens: tokens(60), OutputTokens: tokens(8), Operation: "chat"},
		{StartedAt: start.Add(time.Second), DurationMS: 10, InputTokens: tokens(130), OutputTokens: tokens(8), Operation: "chat"},
		{StartedAt: start.Add(2 * time.Second), DurationMS: 10, InputTokens: tokens(900), OutputTokens: tokens(8), Operation: "chat"},
	}
	stored, err := Build("dep", "prod", "rev", start, start.Add(time.Minute), observations)
	if err != nil {
		t.Fatal(err)
	}
	synthetic, err := NewAIPerfTrace(stored)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := synthetic.WithPromptCorpus(testCorpus(t, 12, 400))
	if err != nil {
		t.Fatal(err)
	}
	rows := corpusRendered(t, trace, 0)
	for index, expected := range []string{"prompt-40", "prompt-100"} {
		if _, passages := systemParts(t, rows[index]); len(passages) != 0 || rows[index].Messages[1].Content != expected {
			t.Fatalf("short request %d sent %q with passages %v, want %s alone", index, rows[index].Messages[1].Content, passages, expected)
		}
	}
	if _, passages := systemParts(t, rows[2]); len(passages) != 2 || !strings.HasPrefix(rows[2].Messages[1].Content, "question-") {
		t.Fatalf("a 900-token request did not take two 400-token passages: %v", passages)
	}
	if trace.Properties.PromptText.StandaloneRequests != 2 {
		t.Fatalf("prompt text=%+v", trace.Properties.PromptText)
	}
}

func TestPromptCorpusKeepsASharedLeadingBlockWhenPassagesAreLongerThanABlock(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	observations := []Observation{
		{StartedAt: start, DurationMS: 10, InputTokens: tokens(4000), OutputTokens: tokens(8), Operation: "chat", SharedPrefixHash: "system"},
		{StartedAt: start.Add(time.Second), DurationMS: 10, InputTokens: tokens(3000), OutputTokens: tokens(8), Operation: "chat", SharedPrefixHash: "system"},
	}
	stored, err := Build("dep", "prod", "rev", start, start.Add(time.Minute), observations)
	if err != nil {
		t.Fatal(err)
	}
	synthetic, err := NewAIPerfTrace(stored)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := synthetic.WithPromptCorpus(testCorpus(t, 12, 1400))
	if err != nil {
		t.Fatal(err)
	}
	rows := corpusRendered(t, trace, 0)
	first, firstPassages := systemParts(t, rows[0])
	second, secondPassages := systemParts(t, rows[1])
	if first != second || len(firstPassages) < 2 || len(secondPassages) < 2 || firstPassages[0] != secondPassages[0] || firstPassages[1] == secondPassages[1] {
		t.Fatalf("the shared leading block is empty or the requests share more than it: %v / %v", firstPassages, secondPassages)
	}
}

func TestPromptCorpusFileIsValidatedAndContentAddressed(t *testing.T) {
	document := testCorpusDocument(3, 200)
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "corpus.json")
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	corpus, err := LoadPromptCorpus(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	identity := corpus.Identity()
	if identity.Digest != "sha256:"+hex.EncodeToString(sum[:]) || identity.Name != "test corpus" || identity.License != "CC-BY-4.0" || identity.Tokenizer != "example/tokenizer" || identity.Passages != 3 || identity.Prompts != 4 {
		t.Fatalf("identity=%+v", identity)
	}
	encoded, err := json.Marshal(identity)
	if err != nil || strings.Contains(string(encoded), "passage-0") || strings.Contains(string(encoded), "prompt-10") {
		t.Fatalf("corpus text reached the identity recorded in evidence: %s", encoded)
	}
	if _, err = LoadPromptCorpus(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing corpus file was accepted")
	}
	broken := func(change func(map[string]any)) error {
		copied := testCorpusDocument(3, 200)
		change(copied)
		raw, marshalErr := json.Marshal(copied)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		_, parseErr := ParsePromptCorpus(raw)
		return parseErr
	}
	passages := func(document map[string]any) []map[string]any { return document["passages"].([]map[string]any) }
	for name, change := range map[string]func(map[string]any){
		"another schema":             func(d map[string]any) { d["schema_version"] = "infercrane.prompt-corpus/v2" },
		"no license":                 func(d map[string]any) { d["license"] = "" },
		"no source":                  func(d map[string]any) { delete(d, "source") },
		"no tokenizer":               func(d map[string]any) { d["tokenizer"] = " " },
		"no passages":                func(d map[string]any) { d["passages"] = []map[string]any{} },
		"no standalone prompts":      func(d map[string]any) { d["prompts"] = []map[string]any{} },
		"a passage without question": func(d map[string]any) { passages(d)[1]["questions"] = []map[string]any{} },
		"a passage without tokens":   func(d map[string]any) { passages(d)[0]["tokens"] = 0 },
		"an empty passage":           func(d map[string]any) { passages(d)[2]["text"] = "  " },
		"a question without tokens": func(d map[string]any) {
			passages(d)[0]["questions"] = []map[string]any{{"tokens": 0, "text": "question"}}
		},
	} {
		if err := broken(change); err == nil {
			t.Fatalf("a corpus with %s was accepted", name)
		}
	}
	if _, err = ParsePromptCorpus([]byte("{")); err == nil {
		t.Fatal("a malformed corpus was accepted")
	}
	synthetic, err := NewAIPerfTrace(agenticTrace(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = synthetic.WithPromptCorpus(nil); err == nil {
		t.Fatal("a rendering accepted no corpus")
	}
	if _, err = (AIPerfTrace{}).WithPromptCorpus(corpus); err == nil {
		t.Fatal("an unrendered trace accepted a corpus")
	}
}
