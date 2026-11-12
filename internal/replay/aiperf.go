package replay

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

const (
	// AIPerfDatasetType is the AIPerf custom dataset that synthesizes prompts
	// from token lengths and hash IDs. Equal hash IDs produce equal prompt
	// blocks, which is how a content-free trace keeps its prefix structure.
	AIPerfDatasetType = "mooncake_trace"
	// AIPerfHashBlockTokens is the mooncake_trace loader's default block size.
	// InferCrane does not override it, so each hash ID stands for this many
	// prompt tokens.
	AIPerfHashBlockTokens = 512
	// SharedPrefixBlocksAssumed is how many leading blocks requests with the
	// same shared-prefix hash reuse. Replay records which requests share their
	// system messages but not how long those messages are, so one block is the
	// smallest prefix the format can express. It understates long system
	// prompts rather than inventing reuse.
	SharedPrefixBlocksAssumed = 1

	// MaxAIPerfTraceRequests bounds one replay pass so a run fits a bounded
	// AIPerf invocation. Longer traces are replayed from their first requests
	// in arrival order, and the cut is recorded.
	MaxAIPerfTraceRequests = 2000

	maxAIPerfTraceBlocks = 2_000_000
	replaySchemaVersion  = "infercrane.replay/v1"
)

// TraceProperties records what the AIPerf rendering preserved from the source
// trace and what it could not. It is part of the workload identity: two
// renderings that preserve different properties are different workloads.
type TraceProperties struct {
	TraceDigest               string   `json:"trace_digest"`
	SchemaVersion             string   `json:"schema_version"`
	DatasetType               string   `json:"dataset_type"`
	HashBlockTokens           int      `json:"hash_block_tokens"`
	SourceRequests            int      `json:"source_requests"`
	Requests                  int      `json:"requests"`
	DroppedMissingTokens      int      `json:"dropped_missing_tokens"`
	DroppedNonGeneration      int      `json:"dropped_non_generation"`
	SourceRequestsBeyondLimit int      `json:"source_requests_beyond_limit"`
	InputTokensMean           int      `json:"input_tokens_mean"`
	OutputTokensMean          int      `json:"output_tokens_mean"`
	TracePeakConcurrency      int      `json:"trace_peak_concurrency"`
	StreamingRequests         int      `json:"streaming_requests"`
	SharedPrefixGroups        int      `json:"shared_prefix_groups"`
	SharedPrefixRequests      int      `json:"shared_prefix_requests"`
	SharedPrefixBlocksAssumed int      `json:"shared_prefix_blocks_assumed"`
	Sessions                  int      `json:"sessions"`
	SessionTurnsReusingPrompt int      `json:"session_turns_reusing_prompt"`
	Preserved                 []string `json:"preserved"`
	NotPreserved              []string `json:"not_preserved"`
}

type aiperfRow struct {
	input, output int
	blocks        []int
}

// AIPerfTrace is a content-free AIPerf rendering of one stored Replay trace.
type AIPerfTrace struct {
	rows        []aiperfRow
	localBlocks int
	Properties  TraceProperties
}

// NewAIPerfTrace verifies a stored trace against its digest and renders its
// token lengths and prefix structure for AIPerf. Prompt content is never
// stored, so AIPerf synthesizes it; arrival timing is not replayed because the
// campaign drives load closed-loop at the declared concurrency.
func NewAIPerfTrace(trace domain.ReplayTrace) (AIPerfTrace, error) {
	if trace.SchemaVersion != replaySchemaVersion {
		return AIPerfTrace{}, fmt.Errorf("replay schema %q is not supported", trace.SchemaVersion)
	}
	shapes, summary, err := verifiedShapes(trace)
	if err != nil {
		return AIPerfTrace{}, err
	}
	sort.SliceStable(shapes, func(i, j int) bool { return shapes[i].ArrivalMS < shapes[j].ArrivalMS })
	result := AIPerfTrace{Properties: TraceProperties{
		TraceDigest: "sha256:" + trace.ShapeDigest, SchemaVersion: trace.SchemaVersion, DatasetType: AIPerfDatasetType,
		HashBlockTokens: AIPerfHashBlockTokens, SourceRequests: len(shapes), TracePeakConcurrency: summary.PeakConcurrency,
		SharedPrefixBlocksAssumed: SharedPrefixBlocksAssumed,
	}}
	type turn struct {
		input  int
		blocks []int
	}
	prefixBlocks, previousTurn := map[string]int{}, map[string]turn{}
	sessions := map[string]struct{}{}
	inputTotal, outputTotal := 0, 0
	for index, row := range shapes {
		if len(result.rows) == MaxAIPerfTraceRequests {
			result.Properties.SourceRequestsBeyondLimit = len(shapes) - index
			break
		}
		if !generationOperation(row.Operation) {
			result.Properties.DroppedNonGeneration++
			continue
		}
		if row.InputTokens == nil || row.OutputTokens == nil || *row.InputTokens < 1 || *row.OutputTokens < 1 {
			result.Properties.DroppedMissingTokens++
			continue
		}
		input, output := *row.InputTokens, *row.OutputTokens
		count := (input + AIPerfHashBlockTokens - 1) / AIPerfHashBlockTokens
		if result.localBlocks+count > maxAIPerfTraceBlocks {
			return AIPerfTrace{}, errors.New("replay trace is too large to render for AIPerf")
		}
		blocks := make([]int, 0, count)
		// A later turn of the same session that is at least as long as the
		// previous prompt is treated as resending that prompt as history. Only
		// whole blocks are reused, and the last block always stays new, so a
		// repeated request never becomes a complete cache hit.
		if row.SessionIDHash != "" {
			sessions[row.SessionIDHash] = struct{}{}
			if previous, found := previousTurn[row.SessionIDHash]; found && input >= previous.input {
				reuse := min(previous.input/AIPerfHashBlockTokens, count-1, len(previous.blocks))
				if reuse > 0 {
					blocks = append(blocks, previous.blocks[:reuse]...)
					result.Properties.SessionTurnsReusingPrompt++
				}
			}
		}
		if len(blocks) == 0 && row.SharedPrefixHash != "" && count > SharedPrefixBlocksAssumed {
			block, found := prefixBlocks[row.SharedPrefixHash]
			if !found {
				block = result.localBlocks
				result.localBlocks++
				prefixBlocks[row.SharedPrefixHash] = block
			}
			blocks = append(blocks, block)
			result.Properties.SharedPrefixRequests++
		}
		for len(blocks) < count {
			blocks = append(blocks, result.localBlocks)
			result.localBlocks++
		}
		if row.SessionIDHash != "" {
			previousTurn[row.SessionIDHash] = turn{input: input, blocks: blocks}
		}
		if row.Streaming {
			result.Properties.StreamingRequests++
		}
		inputTotal += input
		outputTotal += output
		result.rows = append(result.rows, aiperfRow{input: input, output: output, blocks: blocks})
	}
	if len(result.rows) == 0 {
		return AIPerfTrace{}, errors.New("replay trace has no generation requests with recorded input and output tokens")
	}
	properties := &result.Properties
	properties.Requests = len(result.rows)
	properties.InputTokensMean, properties.OutputTokensMean = inputTotal/len(result.rows), outputTotal/len(result.rows)
	properties.SharedPrefixGroups, properties.Sessions = len(prefixBlocks), len(sessions)
	properties.Preserved = []string{"request_order", "input_token_lengths", "output_token_lengths"}
	properties.NotPreserved = []string{"prompt_content", "arrival_timing", "tool_pauses", "parent_session_links", "assistant_output_reuse"}
	if properties.SourceRequestsBeyondLimit > 0 {
		properties.NotPreserved = append(properties.NotPreserved, "requests_beyond_limit")
	}
	if properties.SharedPrefixRequests > 0 {
		properties.Preserved = append(properties.Preserved, "shared_system_prefix_groups")
		properties.NotPreserved = append(properties.NotPreserved, "shared_prefix_length")
	}
	if properties.SessionTurnsReusingPrompt > 0 {
		properties.Preserved = append(properties.Preserved, "session_prompt_history")
	}
	return result, nil
}

// Requests is the number of requests in one pass over the trace.
func (t AIPerfTrace) Requests() int { return len(t.rows) }

// Write renders the trace as AIPerf mooncake_trace JSONL. Each namespace maps
// every hash ID to a disjoint range, so separate AIPerf invocations against the
// same server never reuse each other's prefix-cache entries while keeping the
// sharing structure inside one pass identical.
func (t AIPerfTrace) Write(w io.Writer, namespace int) error {
	if namespace < 0 || t.localBlocks < 1 {
		return errors.New("AIPerf trace namespace must be nonnegative and the trace must be rendered")
	}
	offset := namespace * t.localBlocks
	buffered := bufio.NewWriter(w)
	encoder := json.NewEncoder(buffered)
	for _, row := range t.rows {
		hashes := make([]int, len(row.blocks))
		for index, block := range row.blocks {
			hashes[index] = offset + block
		}
		if err := encoder.Encode(struct {
			InputLength  int   `json:"input_length"`
			OutputLength int   `json:"output_length"`
			HashIDs      []int `json:"hash_ids"`
		}{row.input, row.output, hashes}); err != nil {
			return err
		}
	}
	return buffered.Flush()
}

// verifiedShapes decodes the stored trace and recomputes its digest, so a
// campaign replays exactly the shape its fingerprint names.
func verifiedShapes(trace domain.ReplayTrace) ([]shape, Summary, error) {
	var shapes []shape
	if err := json.Unmarshal([]byte(trace.ShapeJSON), &shapes); err != nil {
		return nil, Summary{}, fmt.Errorf("decode replay shape: %w", err)
	}
	var summary Summary
	if err := json.Unmarshal([]byte(trace.SummaryJSON), &summary); err != nil {
		return nil, Summary{}, fmt.Errorf("decode replay summary: %w", err)
	}
	if summary.ContentStored || summary.Requests != len(shapes) || trace.RequestCount != len(shapes) {
		return nil, Summary{}, errors.New("replay summary is inconsistent or contains content")
	}
	shapeJSON, err := json.Marshal(shapes)
	if err != nil {
		return nil, Summary{}, err
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return nil, Summary{}, err
	}
	if digestEnvelope(shapeJSON, summaryJSON) != strings.ToLower(strings.TrimPrefix(trace.ShapeDigest, "sha256:")) {
		return nil, Summary{}, errors.New("replay trace does not match its shape digest")
	}
	return shapes, summary, nil
}

func digestEnvelope(shapeJSON, summaryJSON []byte) string {
	envelope, _ := json.Marshal(struct {
		Shape   json.RawMessage `json:"shape"`
		Summary json.RawMessage `json:"summary"`
	}{shapeJSON, summaryJSON})
	digest := sha256.Sum256(envelope)
	return hex.EncodeToString(digest[:])
}

func generationOperation(operation string) bool {
	switch operation {
	case "chat", "completions", "responses":
		return true
	default:
		return false
	}
}

// OutputVolume is the generation output-token volume a stored trace observed
// during its capture window. Missing counts generation requests without a
// recorded output-token count; a caller must not treat a total with missing
// requests as the window's volume.
type OutputVolume struct {
	GenerationRequests int   `json:"generation_requests"`
	OutputTokens       int64 `json:"output_tokens"`
	Missing            int   `json:"missing"`
}

// TraceOutputVolume sums the output tokens of every generation request in a
// stored trace. It reads the content-free shape only and estimates nothing.
func TraceOutputVolume(trace domain.ReplayTrace) (OutputVolume, error) {
	var shapes []shape
	if err := json.Unmarshal([]byte(trace.ShapeJSON), &shapes); err != nil {
		return OutputVolume{}, fmt.Errorf("decode replay trace shape: %w", err)
	}
	var volume OutputVolume
	for _, row := range shapes {
		if !generationOperation(row.Operation) {
			continue
		}
		volume.GenerationRequests++
		if row.OutputTokens == nil || *row.OutputTokens < 0 {
			volume.Missing++
			continue
		}
		volume.OutputTokens += int64(*row.OutputTokens)
	}
	return volume, nil
}
