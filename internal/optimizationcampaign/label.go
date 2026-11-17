package optimizationcampaign

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// labelFeatures are the proposal feature names whose source may carry a
// proposal-authored, human description of the setup.
var labelFeatures = map[string]bool{"runtime_args": true, "technique": true, "setup": true, "label": true}

const maxLabelRunes = 80

// CandidateLabels names every candidate by what it changes, keyed by candidate
// run ID. A description written into the immutable proposal wins; otherwise
// the label is derived from the recognized serving flags and checkpoint
// features, and a setup with none is "<engine> defaults". Labels describe
// configuration only and never claim performance. Equal labels are told apart
// by their planning rank.
func CandidateLabels(campaign domain.OptimizationCampaign) map[string]string {
	proposed := map[string]optimizer.Candidate{}
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) == nil {
		for _, candidate := range proposal.Candidates {
			proposed[candidate.ID] = candidate
		}
	}
	labels := make(map[string]string, len(campaign.Candidates))
	counts := map[string]int{}
	for _, run := range campaign.Candidates {
		candidate, found := proposed[run.ProposalCandidateID]
		if !found {
			_ = json.Unmarshal([]byte(run.DeploymentSpecJSON), &candidate.Deployment)
		}
		label := candidateLabel(candidate)
		if label == "" {
			label = "Setup " + strconv.Itoa(run.Rank)
		}
		labels[run.ID] = label
		counts[label]++
	}
	for _, run := range campaign.Candidates {
		if counts[labels[run.ID]] > 1 {
			labels[run.ID] += " (setup " + strconv.Itoa(run.Rank) + ")"
		}
	}
	return labels
}

func candidateLabel(candidate optimizer.Candidate) string {
	for _, feature := range candidate.Features {
		if labelFeatures[feature.Name] && prose(feature.Source) {
			return strings.TrimSpace(feature.Source)
		}
	}
	draft := candidate.Deployment
	args := append(append([]string(nil), draft.Runtime.Args...), draft.Runtime.Workload.Command...)
	techniques := flagTechniques(args)
	for _, feature := range candidate.Features {
		if feature.Name == "quantization" && strings.TrimSpace(feature.State) != "" {
			techniques = append([]string{strings.ToUpper(strings.TrimSpace(feature.State)) + " checkpoint"}, techniques...)
		}
	}
	if len(techniques) > 0 {
		return capitalize(strings.Join(techniques, ", "))
	}
	engine := engineName(draft.Runtime.Engine)
	if engine == "" {
		return ""
	}
	if len(draft.Runtime.Args) == 0 {
		return engine + " defaults"
	}
	return truncateLabel(engine + " " + strings.Join(draft.Runtime.Args, " "))
}

// flagTechniques reads the serving flags InferCrane recognizes. Unrecognized
// flags add nothing, so a label never guesses what an unknown flag does.
func flagTechniques(args []string) []string {
	value := func(index int) string {
		if index+1 < len(args) {
			return strings.TrimSpace(args[index+1])
		}
		return ""
	}
	var out []string
	seen := map[string]bool{}
	add := func(text string) {
		if text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	algorithm, draftTokens := "", ""
	for index, raw := range args {
		flag, inline, hasInline := strings.Cut(raw, "=")
		next := value(index)
		if hasInline {
			next = inline
		}
		switch flag {
		case "--speculative-config":
			var config struct {
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(next), &config) == nil && config.Method != "" {
				add("speculative decoding (" + speculativeMethod(config.Method) + ")")
			} else {
				add("speculative decoding")
			}
		case "--speculative-algorithm":
			algorithm = next
		case "--speculative-num-draft-tokens":
			draftTokens = next
		case "--kv-cache-dtype":
			if strings.HasPrefix(strings.ToLower(next), "fp8") {
				add("FP8 KV cache")
			}
		case "--max-num-batched-tokens", "--chunked-prefill-size":
			if tokens, err := strconv.Atoi(next); err == nil && tokens > 0 {
				add("prefill chunks of " + groupThousands(tokens) + " tokens")
			}
		case "--enable-prefix-caching":
			add("prefix caching")
		case "--no-enable-prefix-caching", "--disable-radix-cache":
			add("no prefix caching")
		case "--cuda-graph-bs-decode", "--cuda-graph-bs-prefill", "--cuda-graph-max-bs":
			add("bounded CUDA graphs")
		case "--enforce-eager", "--disable-cuda-graph":
			add("no CUDA graphs")
		case "--max-num-seqs", "--max-running-requests":
			if count, err := strconv.Atoi(next); err == nil && count > 0 {
				add("up to " + groupThousands(count) + " concurrent requests")
			}
		case "--quantization":
			if next != "" {
				add(strings.ToUpper(next) + " weights")
			}
		}
	}
	if algorithm != "" {
		detail := speculativeMethod(algorithm)
		if draftTokens != "" {
			detail += ", " + draftTokens + " draft tokens"
		}
		out = append([]string{"speculative decoding (" + detail + ")"}, out...)
	}
	return out
}

func speculativeMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "ngram", "n-gram", "n_gram":
		return "n-gram"
	case "mtp", "nextn", "deepseek_mtp":
		return "MTP"
	case "eagle":
		return "EAGLE"
	case "eagle3":
		return "EAGLE-3"
	case "draft_model", "draft-model":
		return "draft model"
	case "":
		return "unspecified method"
	default:
		return strings.TrimSpace(method)
	}
}

func engineName(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "":
		return ""
	case "vllm":
		return "vLLM"
	case "sglang":
		return "SGLang"
	case "trtllm", "tensorrt-llm":
		return "TensorRT-LLM"
	case "custom-oci":
		return "Custom image"
	default:
		return engine
	}
}

// prose accepts a short human sentence fragment and rejects identifiers,
// digests, image references, and URLs that features also carry.
func prose(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > maxLabelRunes || !strings.Contains(text, " ") || strings.Contains(text, "://") || strings.Contains(text, "sha256:") {
		return false
	}
	return strings.IndexFunc(text, unicode.IsLetter) >= 0
}

func capitalize(text string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return text
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func truncateLabel(text string) string {
	runes := []rune(text)
	if len(runes) <= maxLabelRunes {
		return text
	}
	return string(runes[:maxLabelRunes-1]) + "…"
}

func groupThousands(value int) string {
	digits := strconv.Itoa(value)
	var b strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}
