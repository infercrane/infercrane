package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type renderedRow struct {
	InputLength  int   `json:"input_length"`
	OutputLength int   `json:"output_length"`
	HashIDs      []int `json:"hash_ids"`
}

func tokens(value int) *int { return &value }

func agenticTrace(t *testing.T) domain.ReplayTrace {
	t.Helper()
	start := time.Unix(1000, 0).UTC()
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }
	observations := []Observation{
		{StartedAt: at(1), DurationMS: 900, InputTokens: tokens(1500), OutputTokens: tokens(40), Operation: "chat", Streaming: true, SessionIDHash: "s1", SharedPrefixHash: "system"},
		{StartedAt: at(2), DurationMS: 700, InputTokens: tokens(700), OutputTokens: tokens(30), Operation: "chat", Streaming: true, SessionIDHash: "s2", SharedPrefixHash: "system"},
		{StartedAt: at(3), DurationMS: 800, InputTokens: tokens(2100), OutputTokens: tokens(50), Operation: "chat", Streaming: true, SessionIDHash: "s1", SharedPrefixHash: "system"},
		{StartedAt: at(4), DurationMS: 100, InputTokens: tokens(300), OutputTokens: tokens(10), Operation: "chat", SharedPrefixHash: "system"},
		{StartedAt: at(5), DurationMS: 50, InputTokens: tokens(64), Operation: "embeddings"},
		{StartedAt: at(6), DurationMS: 50, InputTokens: tokens(64), Operation: "chat"},
		{StartedAt: at(7), DurationMS: 400, InputTokens: tokens(400), OutputTokens: tokens(20), Operation: "completions", SessionIDHash: "s1"},
	}
	trace, err := Build("dep", "prod", "rev", start, start.Add(time.Minute), observations)
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

// jsonbText imitates PostgreSQL returning a jsonb column as text: keys are
// reordered and whitespace is inserted, so only a canonical re-encoding can
// recover the digest.
func jsonbText(t *testing.T, value string) string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(decoded, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func rendered(t *testing.T, trace AIPerfTrace, namespace int) []renderedRow {
	t.Helper()
	var buffer bytes.Buffer
	if err := trace.Write(&buffer, namespace); err != nil {
		t.Fatal(err)
	}
	var rows []renderedRow
	scanner := bufio.NewScanner(&buffer)
	for scanner.Scan() {
		var row renderedRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestAIPerfTracePreservesPrefixAndSessionStructureWithoutContent(t *testing.T) {
	stored := agenticTrace(t)
	stored.ShapeJSON, stored.SummaryJSON = jsonbText(t, stored.ShapeJSON), jsonbText(t, stored.SummaryJSON)
	trace, err := NewAIPerfTrace(stored)
	if err != nil {
		t.Fatal(err)
	}
	rows := rendered(t, trace, 0)
	if len(rows) != 5 || trace.Requests() != 5 {
		t.Fatalf("rows=%+v", rows)
	}
	for _, row := range rows {
		if len(row.HashIDs) != (row.InputLength+AIPerfHashBlockTokens-1)/AIPerfHashBlockTokens {
			t.Fatalf("hash IDs do not cover the input length: %+v", row)
		}
	}
	system := rows[0].HashIDs[0]
	if rows[1].HashIDs[0] != system {
		t.Fatalf("requests with one system prefix do not share a leading block: %+v", rows)
	}
	if rows[2].HashIDs[0] != system || rows[2].HashIDs[1] != rows[0].HashIDs[1] || rows[2].HashIDs[2] == rows[0].HashIDs[2] {
		t.Fatalf("the next session turn did not reuse exactly the previous prompt's whole blocks: %+v", rows)
	}
	if len(rows[3].HashIDs) != 1 || rows[3].HashIDs[0] == system {
		t.Fatalf("a one-block request became a complete cache hit: %+v", rows[3])
	}
	if rows[4].HashIDs[0] == system || rows[4].HashIDs[0] == rows[2].HashIDs[0] {
		t.Fatalf("a shorter session turn was assumed to resend history: %+v", rows[4])
	}
	properties := trace.Properties
	if properties.DroppedNonGeneration != 1 || properties.DroppedMissingTokens != 1 || properties.SharedPrefixGroups != 1 || properties.SharedPrefixRequests != 2 || properties.SessionTurnsReusingPrompt != 1 || properties.Sessions != 2 || properties.StreamingRequests != 3 {
		t.Fatalf("properties=%+v", properties)
	}
	if properties.TraceDigest != "sha256:"+stored.ShapeDigest || properties.DatasetType != "mooncake_trace" || properties.HashBlockTokens != 512 {
		t.Fatalf("identity=%+v", properties)
	}
	for _, expected := range []string{"shared_system_prefix_groups", "session_prompt_history"} {
		if !strings.Contains(strings.Join(properties.Preserved, ","), expected) {
			t.Fatalf("preserved=%v", properties.Preserved)
		}
	}
	for _, expected := range []string{"prompt_content", "arrival_timing", "shared_prefix_length"} {
		if !strings.Contains(strings.Join(properties.NotPreserved, ","), expected) {
			t.Fatalf("not preserved=%v", properties.NotPreserved)
		}
	}
}

func TestAIPerfTraceNamespacesKeepStructureButNeverShareBlocks(t *testing.T) {
	trace, err := NewAIPerfTrace(agenticTrace(t))
	if err != nil {
		t.Fatal(err)
	}
	first, second := rendered(t, trace, 0), rendered(t, trace, 3)
	seen := map[int]struct{}{}
	for _, row := range first {
		for _, id := range row.HashIDs {
			seen[id] = struct{}{}
		}
	}
	for index, row := range second {
		if row.InputLength != first[index].InputLength || len(row.HashIDs) != len(first[index].HashIDs) {
			t.Fatalf("namespace changed the load shape: %+v vs %+v", row, first[index])
		}
		for position, id := range row.HashIDs {
			if _, reused := seen[id]; reused {
				t.Fatalf("namespaces shared hash ID %d", id)
			}
			if id-second[0].HashIDs[0] != first[index].HashIDs[position]-first[0].HashIDs[0] {
				t.Fatalf("namespace changed the sharing structure")
			}
		}
	}
	if err = trace.Write(&bytes.Buffer{}, -1); err == nil {
		t.Fatal("negative namespace was accepted")
	}
}

func TestAIPerfTraceRefusesTamperedContentBearingOrEmptyTraces(t *testing.T) {
	tampered := agenticTrace(t)
	tampered.ShapeJSON = strings.Replace(tampered.ShapeJSON, `"input_tokens":1500`, `"input_tokens":1501`, 1)
	if _, err := NewAIPerfTrace(tampered); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered trace err=%v", err)
	}
	content := agenticTrace(t)
	content.SummaryJSON = strings.Replace(content.SummaryJSON, `"content_stored":false`, `"content_stored":true`, 1)
	if _, err := NewAIPerfTrace(content); err == nil {
		t.Fatal("content-bearing trace was accepted")
	}
	unknownSchema := agenticTrace(t)
	unknownSchema.SchemaVersion = "infercrane.replay/v2"
	if _, err := NewAIPerfTrace(unknownSchema); err == nil {
		t.Fatal("unknown schema was accepted")
	}
	start := time.Unix(1000, 0).UTC()
	empty, err := Build("dep", "prod", "rev", start, start.Add(time.Minute), []Observation{{StartedAt: start, Operation: "embeddings", InputTokens: tokens(8)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewAIPerfTrace(empty); err == nil {
		t.Fatal("a trace with no replayable generation requests was accepted")
	}
}

func TestAIPerfTraceReplaysOnlyTheLeadingBoundedWindow(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	observations := make([]Observation, 0, MaxAIPerfTraceRequests+5)
	for index := range MaxAIPerfTraceRequests + 5 {
		observations = append(observations, Observation{StartedAt: start.Add(time.Duration(index) * time.Millisecond), DurationMS: 10, InputTokens: tokens(100 + index), OutputTokens: tokens(8), Operation: "chat"})
	}
	trace, err := Build("dep", "prod", "rev", start, start.Add(time.Minute), observations)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := NewAIPerfTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Requests() != MaxAIPerfTraceRequests || rendered.Properties.SourceRequestsBeyondLimit != 5 || rendered.rows[0].input != 100 || !strings.Contains(strings.Join(rendered.Properties.NotPreserved, ","), "requests_beyond_limit") {
		t.Fatalf("bounded window=%d properties=%+v", rendered.Requests(), rendered.Properties)
	}
}

func TestTraceOutputVolumeCountsGenerationOutputOnly(t *testing.T) {
	trace := domain.ReplayTrace{ShapeJSON: `[{"operation":"chat","output_tokens":40},{"operation":"embeddings"},{"operation":"responses","output_tokens":60},{"operation":"completions"}]`}
	volume, err := TraceOutputVolume(trace)
	if err != nil {
		t.Fatal(err)
	}
	if volume.GenerationRequests != 3 || volume.OutputTokens != 100 || volume.Missing != 1 {
		t.Fatalf("volume = %+v", volume)
	}
	if _, err = TraceOutputVolume(domain.ReplayTrace{ShapeJSON: "{"}); err == nil {
		t.Fatal("a malformed shape was read as an empty trace")
	}
}
