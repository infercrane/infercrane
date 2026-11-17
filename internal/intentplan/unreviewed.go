package intentplan

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/modelcheck"
)

const (
	checkedWarning   = "Not reviewed: checked automatically. This setup comes from the model's published metadata and the pinned runtimes' model registries. Throughput, latency, answer quality, capacity, and final cost are unknown until measured."
	uncheckedWarning = "Not reviewed and not checked: InferCrane could not read this model, so its runtime support, size, license, and access are unknown."
)

// UnreviewedRepository returns the exact Hugging Face repository the request
// names, or "" when the model field is not one. Only an explicit model field
// counts: InferCrane never reads a repository out of free-form intent text.
func UnreviewedRepository(request Request) string {
	model := strings.TrimSpace(request.Model)
	model = strings.TrimPrefix(strings.TrimPrefix(model, "https://huggingface.co/"), "hf://")
	if repository, _, found := strings.Cut(model, "@"); found {
		model = repository
	}
	if !modelcheck.ValidRepository(model) {
		return ""
	}
	return model
}

// UnreviewedRevision returns the revision to check: the explicit field, then
// a revision written after "@" in the model, then the default branch.
func UnreviewedRevision(request Request) string {
	if revision := strings.TrimSpace(request.ModelRevision); revision != "" {
		return revision
	}
	if _, revision, found := strings.Cut(strings.TrimSpace(request.Model), "@"); found && strings.TrimSpace(revision) != "" {
		return strings.TrimSpace(revision)
	}
	return "main"
}

// ResolvesReviewed reports whether the request names exactly one reviewed
// model, in which case no automatic check is needed.
func ResolvesReviewed(recipes []curatedrecipe.Entry, request Request) bool {
	request = normalizeRequest(request)
	if len(recipes) == 0 {
		recipes = curatedrecipe.All()
	}
	_, matched, _ := resolveModel(recipes, request.Model, request.Intent)
	return matched
}

func (p Planner) planUnreviewed(result Plan, request Request, recipes []curatedrecipe.Entry) (Plan, error) {
	unreviewed := *p.Unreviewed
	repository := unreviewed.Repository
	result.Warnings = []string{checkedWarning}
	if unreviewed.Report == nil {
		result.Warnings = []string{uncheckedWarning}
		remediation := "Check the repository name, or choose a reviewed model."
		switch unreviewed.Code {
		case modelcheck.CodeUnavailable:
			remediation = "Try again in a moment, or choose a reviewed model."
		case modelcheck.CodeTokenRejected:
			remediation = "Replace the saved Hugging Face token under Settings › Connections, or choose a reviewed model."
		case "metadata_access_disabled":
			remediation = "Ask the operator to enable Hugging Face metadata access, or choose a reviewed model."
		}
		options := modelSuggestions(recipes, repository)
		result.Missing = append(result.Missing, MissingChoice{Field: "model", Prompt: "InferCrane could not check " + repository + ".", Reason: unreviewed.Reason, Options: options, Remediation: remediation})
		result.Choices = append(result.Choices, Choice{Field: "model", Label: "Model", Value: repository, Editable: true, Required: true, Options: options})
		result.Architecture = baseArchitecture(nil)
		return result, nil
	}
	report := *unreviewed.Report
	result.ModelCheck = &report
	runtime, version := "", ""
	if report.Architecture.Runtime != nil {
		runtime, version = *report.Architecture.Runtime, *report.Architecture.RuntimeVersion
	}
	tasks := []string{}
	if report.Task != "" {
		tasks = append(tasks, report.Task)
	}
	result.Model = &Model{
		Review: ReviewCheckedAutomatically, Name: repository, DisplayName: repository, Repository: report.Repository, Revision: report.Revision,
		Runtime: runtime, Tasks: tasks, Gated: report.Access.Gated || report.Access.Private, EvidenceClass: evidenceChecked,
		EvidenceSummary: "Not reviewed: checked automatically from Hugging Face metadata at this commit and the pinned runtimes' model registries. " + report.Reason,
	}
	result.Evidence.Configuration = checkedEvidence(report)
	modelOption := Option{Value: report.Repository, Label: report.Repository, State: evidenceChecked, Reason: report.Reason}

	if report.Verdict == modelcheck.Unsupported || report.Architecture.State != modelcheck.Supported {
		alternatives := nearestReviewed(recipes, report)
		prompt, status := "This model cannot run on InferCrane's runtimes yet. Choose a reviewed model instead.", "unsupported"
		if report.Verdict != modelcheck.Unsupported {
			prompt, status = "InferCrane could not confirm that this model runs on its runtimes.", "needs_input"
		}
		result.Status = status
		result.Missing = append(result.Missing, MissingChoice{Field: "model", Prompt: prompt, Reason: report.Reason, Options: alternatives, Remediation: "Choose one of the nearest reviewed models, or another Hugging Face model."})
		result.Choices = append(result.Choices, Choice{Field: "model", Label: "Model", Value: report.Repository, Editable: true, Required: true, Options: append([]Option{modelOption}, alternatives...)})
		result.Architecture = baseArchitecture(result.Model)
		return result, nil
	}
	if report.Access.State == modelcheck.AccessTokenRequired {
		result.Missing = append(result.Missing, MissingChoice{
			Field: "model_secret_reference_id", Prompt: "Add a Hugging Face token to deploy " + report.Repository + ".",
			Reason:      "The model is gated or private. InferCrane needs a token whose account accepted its terms to read its config and pin its files.",
			Remediation: "Accept the model's terms on Hugging Face, save a token under Settings › Connections, then choose it here.",
		})
	}
	if result.Interpretation.ComputeMode != "elastic" {
		result.Missing = append(result.Missing, MissingChoice{Field: "compute_mode", Prompt: "Use elastic compute for this model.", Reason: "Automatically checked models deploy on elastic compute only.", Options: []Option{{Value: "elastic", Label: "Elastic", State: evidenceChecked}}, Remediation: "Choose elastic compute."})
	}
	layouts := report.Memory.Layouts
	gpus := make([]Option, 0, len(layouts))
	for _, layout := range layouts {
		gpus = append(gpus, Option{Value: layout.GPU, Label: fmt.Sprintf("%d × %s", layout.GPUCount, layout.GPU), State: evidenceChecked})
	}
	gpu, gpuCount := "", 0
	switch {
	case request.GPU != "":
		gpu, gpuCount = request.GPU, 1
		index := slices.IndexFunc(layouts, func(layout modelcheck.Layout) bool { return strings.EqualFold(layout.GPU, request.GPU) })
		switch {
		case index >= 0:
			gpu, gpuCount = layouts[index].GPU, layouts[index].GPUCount
		case report.Memory.State == modelcheck.Fits:
			result.Missing = append(result.Missing, MissingChoice{Field: "gpu", Prompt: "Choose a GPU this model fits.", Reason: "The model does not fit " + request.GPU + " at this load in up to eight GPUs.", Options: gpus, Remediation: "Choose one of the listed GPUs, or lower the context length or concurrency."})
		default:
			result.Warnings = append(result.Warnings, "GPU memory fit is unknown: "+report.Memory.Reason)
		}
	case report.Memory.State == modelcheck.Fits:
		gpu, gpuCount = *report.Memory.GPU, *report.Memory.GPUCount
	default:
		options := make([]Option, 0, len(report.Memory.ConsideredGPUs))
		for _, name := range report.Memory.ConsideredGPUs {
			options = append(options, Option{Value: name, Label: name, State: "fit-unknown"})
		}
		result.Missing = append(result.Missing, MissingChoice{Field: "gpu", Prompt: "Choose a GPU for this model.", Reason: report.Memory.Reason, Options: options, Remediation: "Choose a GPU with enough memory for the model's weights and KV cache."})
		result.Choices = append(result.Choices, Choice{Field: "model", Label: "Model", Value: report.Repository, Editable: true, Required: true, Options: []Option{modelOption}})
		result.Architecture = baseArchitecture(result.Model)
		return result, nil
	}
	profile := checkedProfile(report, runtime, version, gpu, gpuCount, layouts)
	entry := curatedrecipe.Entry{Name: deploymentName(report.Repository), DisplayName: report.Repository, Model: report.Repository, Revision: report.Revision, Runtime: runtime, Profiles: []curatedrecipe.ServingProfile{profile}}
	return p.complete(result, request, planTarget{entry: entry, profile: profile, gpu: gpu, gpuCount: gpuCount, review: ReviewCheckedAutomatically, modelOption: modelOption, gpuOptions: gpus, evidence: checkedEvidence(report)})
}

func checkedEvidence(report modelcheck.Report) string {
	versions := make([]string, 0, len(report.Architecture.Sources))
	for _, source := range report.Architecture.Sources {
		versions = append(versions, source.Runtime+" "+source.Version)
	}
	return "checked automatically: Hugging Face metadata at " + shortCommit(report.Revision) + " and the " + strings.Join(versions, " and ") + " model registries"
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// checkedProfile is the runtime's own defaults, with the context bounded to
// the length the GPU was sized for and tensor parallelism across the GPUs.
func checkedProfile(report modelcheck.Report, runtime, version, gpu string, gpuCount int, layouts []modelcheck.Layout) curatedrecipe.ServingProfile {
	contextTokens := strconv.Itoa(report.Memory.ContextTokens)
	args := []string{"--max-model-len", contextTokens}
	if runtime == "sglang" {
		args = []string{"--context-length", contextTokens}
	}
	if gpuCount > 1 {
		if runtime == "sglang" {
			args = append(args, "--tp-size", strconv.Itoa(gpuCount))
		} else {
			args = append(args, "--tensor-parallel-size", strconv.Itoa(gpuCount))
		}
	}
	compatible := []string{gpu}
	for _, layout := range layouts {
		if !slices.ContainsFunc(compatible, func(name string) bool { return strings.EqualFold(name, layout.GPU) }) {
			compatible = append(compatible, layout.GPU)
		}
	}
	display := map[string]string{"vllm": "vLLM", "sglang": "SGLang"}[runtime]
	limitations := []string{
		fmt.Sprintf("GPU sized for %d requests at %d tokens each; change the load to resize it.", report.Memory.Concurrency, report.Memory.ContextTokens),
	}
	if report.License != nil {
		limitations = append(limitations, "License: "+*report.License+". Review its terms before production use.")
	} else {
		limitations = append(limitations, "The model card declares no license. Review the repository before production use.")
	}
	return curatedrecipe.ServingProfile{
		Name: runtime + "-checked", DisplayName: display + " " + version + " defaults",
		Description: "The runtime's defaults, with the context bounded to the checked length. Not reviewed: checked automatically.",
		Runtime:     runtime, RuntimeVersion: version, ComputeMode: "elastic", GPUHint: gpu, CompatibleGPUs: compatible, GPUCount: gpuCount,
		MinReplicas: 1, MaxReplicas: 1, RuntimeArgs: args, EvidenceClass: evidenceChecked,
		QualificationScope: "Architecture, size, license, and access were read automatically from Hugging Face at the pinned commit. Nothing was reviewed or measured.",
		Limitations:        limitations,
	}
}

// deploymentName turns a repository into a valid deployment and endpoint
// name: "microsoft/Phi-4" becomes "phi-4".
func deploymentName(repository string) string {
	_, name, _ := strings.Cut(repository, "/")
	var out strings.Builder
	for _, char := range strings.ToLower(name) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '.', char == '-', char == '_':
			out.WriteRune(char)
		default:
			out.WriteRune('-')
		}
	}
	value := strings.Trim(out.String(), "-._")
	if len(value) > 64 {
		value = strings.Trim(value[:64], "-._")
	}
	if value == "" {
		return "model"
	}
	return value
}

// nearestReviewed ranks reviewed models by the same task, the same
// publisher, and shared name words. It never claims they are equivalent.
func nearestReviewed(recipes []curatedrecipe.Entry, report modelcheck.Report) []Option {
	owner, name, _ := strings.Cut(report.Repository, "/")
	words := normalizedWords(name)
	type scored struct {
		entry curatedrecipe.Entry
		score int
	}
	items := make([]scored, 0, len(recipes))
	for _, entry := range recipes {
		score := 0
		entryTask := modelcheck.TaskChat
		if entry.Protocol == "embeddings" {
			entryTask = modelcheck.TaskEmbeddings
		}
		if report.Task != "" && entryTask == report.Task {
			score += 4
		}
		if entryOwner, _, _ := strings.Cut(entry.Model, "/"); strings.EqualFold(entryOwner, owner) {
			score += 3
		}
		haystack := normalizedWords(entry.Name + " " + entry.DisplayName + " " + entry.Model)
		for word := range words {
			if len(word) >= 3 && hasWord(haystack, word) {
				score++
			}
		}
		items = append(items, scored{entry: entry, score: score})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].entry.Name < items[j].entry.Name
	})
	options := make([]Option, 0, 3)
	for _, item := range items {
		if len(options) == 3 {
			break
		}
		options = append(options, Option{Value: item.entry.Name, Label: item.entry.DisplayName, State: "reviewed", Reason: item.entry.UseCase})
	}
	return options
}
