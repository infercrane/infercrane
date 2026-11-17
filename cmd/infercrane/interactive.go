package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
)

// Interactive pieces for a person at a terminal: a live progress line while
// --wait follows an operation, and confirmations before deleting or approving
// spend. Neither appears when output is redirected, NO_COLOR or TERM=dumb is
// set, or --output json is used; prompts also need stdin to be a terminal.
// --yes always skips a prompt.

// promptInput and promptOutput are the prompt's streams; tests replace them.
var (
	promptInput  io.Reader = os.Stdin
	promptOutput io.Writer = os.Stdout
	// promptAllowed decides whether a command may ask a question.
	promptAllowed = func(output string) bool {
		return output != "json" && styleAllowed(os.Stdout) && term.IsTerminal(os.Stdin.Fd())
	}
)

// errDeclined is returned when a person answers no.
var errDeclined = errors.New("cancelled; nothing was changed")

// confirm asks a yes/no question that defaults to no.
func confirm(t theme, question string) (bool, error) {
	fmt.Fprint(promptOutput, t.title(question)+" "+t.muted("[y/N]")+" ")
	answer, err := readAnswer()
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", nil
}

// confirmTyped asks for an exact word, such as the name of what is deleted.
func confirmTyped(t theme, question, word string) (bool, error) {
	fmt.Fprint(promptOutput, t.title(question)+"\n"+t.muted("Type ")+t.title(word)+t.muted(" to confirm, or press Enter to cancel: "))
	answer, err := readAnswer()
	if err != nil {
		return false, err
	}
	return answer == word, nil
}

func readAnswer() (string, error) {
	line, err := bufio.NewReader(promptInput).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// progressMode is how --wait shows an operation's progress on stderr.
type progressMode int

const (
	// progressNone prints nothing.
	progressNone progressMode = iota
	// progressLines prints one line each time the progress changes. It is
	// what scripts, pipes, and --output json see.
	progressLines
	// progressLive redraws one animated line and keeps a line per finished
	// phase. Only a person at a terminal sees it.
	progressLive
)

// progressFor picks how --wait shows progress for this output format.
func progressFor(output string) progressMode {
	if output == "human" && styleAllowed(os.Stdout) && styleAllowed(os.Stderr) {
		return progressLive
	}
	return progressLines
}

// progressForHuman shows progress only in human output, as rollout commands
// always have.
func progressForHuman(output string) progressMode {
	if output != "human" {
		return progressNone
	}
	return progressFor(output)
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// liveProgress is one spinner line redrawn in place on a terminal.
type liveProgress struct {
	out   io.Writer
	t     theme
	mu    sync.Mutex
	text  string
	frame int
	stop  chan struct{}
	done  chan struct{}
}

func startLiveProgress(out io.Writer, t theme) *liveProgress {
	p := &liveProgress{out: out, t: t, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.frame++
				p.draw()
				p.mu.Unlock()
			}
		}
	}()
	return p
}

// draw rewrites the line; the caller holds mu.
func (p *liveProgress) draw() {
	if p.text == "" {
		return
	}
	frame := p.t.tone(toneInfo, spinnerFrames[p.frame%len(spinnerFrames)])
	fmt.Fprint(p.out, "\r\x1b[2K"+ansi.Truncate(frame+" "+p.text, p.t.width-1, "…"))
}

// set replaces the live line.
func (p *liveProgress) set(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.text = text
	p.draw()
}

// commit prints a permanent line above the live line, then draws next as
// the live line.
func (p *liveProgress) commit(line, next string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprint(p.out, "\r\x1b[2K"+line+"\n")
	p.text = next
	p.draw()
}

// finish stops the animation and clears the live line.
func (p *liveProgress) finish() {
	close(p.stop)
	<-p.done
	fmt.Fprint(p.out, "\r\x1b[2K")
}

// operationTracker turns operation polls into a live line and one finished
// line per phase.
type operationTracker struct {
	live       *liveProgress
	t          theme
	phase      string
	phaseStart time.Time
}

func newOperationTracker(out io.Writer, t theme) *operationTracker {
	return &operationTracker{live: startLiveProgress(out, t), t: t}
}

func (o *operationTracker) update(operation domain.Operation, now time.Time) {
	phase, line := operationPhase(operation), renderLiveOperationProgress(operation, now, o.t)
	if phase != o.phase && o.phase != "" {
		o.live.commit(o.finishedLine(o.phase, now), line)
	} else {
		o.live.set(line)
	}
	if phase != o.phase {
		o.phase, o.phaseStart = phase, now
	}
}

func (o *operationTracker) finishedLine(phase string, now time.Time) string {
	return "  " + o.t.tone(toneGood, "✓") + " " + padRight(phase, 22) + o.t.muted(now.Sub(o.phaseStart).Round(time.Second).String())
}

// done clears the live line and prints the final state.
func (o *operationTracker) done(operation domain.Operation, now time.Time) {
	o.live.finish()
	if operation.ID == "" {
		return
	}
	tone, mark := toneGood, "✓"
	switch operation.Status {
	case "failed", "cancelled":
		tone, mark = toneBad, "✗"
	case "succeeded":
	default:
		return
	}
	line := "  " + o.t.tone(tone, mark+" "+padRight(operationPhase(operation), 22))
	if !operation.CreatedAt.IsZero() {
		line += o.t.muted(now.Sub(operation.CreatedAt).Round(time.Second).String() + " in all")
	}
	fmt.Fprintln(o.live.out, line)
}

// renderLiveOperationProgress is renderOperationProgress for the live line:
// a progress bar, the phase as a badge, and the same message and timings.
func renderLiveOperationProgress(operation domain.Operation, now time.Time, t theme) string {
	const cells = 12
	filled := min(cells, max(0, operation.Progress*cells/100))
	bar := t.tone(statusTone(operation.Status), strings.Repeat("━", filled)) + t.muted(strings.Repeat("─", cells-filled))
	elapsed := "0s"
	if !operation.CreatedAt.IsZero() {
		elapsed = now.Sub(operation.CreatedAt).Round(time.Second).String()
	}
	details := []string{elapsed}
	if operation.MaxAttempts > 0 {
		details = append(details, fmt.Sprintf("check %d/%d", operation.Attempt, operation.MaxAttempts))
	}
	if operation.NextAttemptAt != nil && operation.NextAttemptAt.After(now) {
		details = append(details, "next "+operation.NextAttemptAt.Sub(now).Round(time.Second).String())
	}
	line := fmt.Sprintf("%s %3d%%  %s", bar, operation.Progress, t.tone(statusTone(operation.Status), operationPhase(operation)))
	if message := strings.TrimSpace(operation.Message); message != "" {
		line += "  " + message
	}
	return line + "  " + t.muted(strings.Join(details, " · "))
}

// confirmDeletion shows what deleting a deployment does and asks for its
// name. The deployment's current state is shown when the control plane
// answers; the plan is shown either way.
func confirmDeletion(ctx context.Context, cfg config.Config, t theme, name string, actions []string) error {
	var b strings.Builder
	b.WriteString(t.tone(toneBad, "● DELETE") + "  " + t.title(name) + "\n")
	if view, err := fetchDeployment(ctx, cfg, name); err == nil {
		d := view.Deployment
		b.WriteString(t.muted(emptyAs(d.Model, "unknown model")+" · ") + t.badge(emptyAs(d.ObservedState, "unknown")) + t.muted(fmt.Sprintf(" · %d–%d replicas", d.MinReplicas, d.MaxReplicas)) + "\n")
	}
	b.WriteString("\n")
	for _, action := range actions {
		for index, line := range wrapWords(action, t.width-10) {
			prefix := "  "
			if index == 0 {
				prefix = t.muted("→ ")
			}
			b.WriteString(prefix + line + "\n")
		}
	}
	b.WriteString("\n" + t.muted("Provider cleanup continues if you close this terminal."))
	fmt.Fprintln(promptOutput, t.card(b.String(), min(t.width, 88), toneBad))
	ok, err := confirmTyped(t, "Delete "+name+"?", name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("deletion of %s %w", name, errDeclined)
	}
	return nil
}

// approvalSetup is the part of a setup's DeploymentSpec that says
// what a paid run starts.
type approvalSetup struct {
	ID    string `json:"id"`
	State string `json:"state"`
	// Label is the control API's name for the setup; older control planes
	// leave it empty and the engine and flags describe it instead.
	Label          string `json:"label"`
	DeploymentSpec struct {
		Runtime struct {
			Engine string   `json:"engine"`
			Args   []string `json:"args"`
		} `json:"runtime"`
		Resources struct {
			GPU      string `json:"gpu"`
			GPUCount int    `json:"gpu_count"`
		} `json:"resources"`
		Provider struct {
			Cloud   string `json:"cloud"`
			Adapter string `json:"adapter"`
			Region  string `json:"region"`
		} `json:"provider"`
	} `json:"deployment_spec"`
}

// describe says what the setup runs, or "" when its spec was not returned.
func (s approvalSetup) describe() string {
	spec := s.DeploymentSpec
	if spec.Runtime.Engine == "" && s.Label == "" {
		return ""
	}
	setup := s.Label
	if setup == "" {
		setup = spec.Runtime.Engine
		if len(spec.Runtime.Args) == 0 {
			setup += ", default settings"
		} else {
			setup += ", " + strings.Join(spec.Runtime.Args, " ")
		}
	}
	parts := []string{setup}
	if spec.Resources.GPU != "" {
		count := "?"
		if spec.Resources.GPUCount > 0 {
			count = fmt.Sprint(spec.Resources.GPUCount)
		}
		parts = append(parts, count+" × "+spec.Resources.GPU)
	} else {
		parts = append(parts, "GPU unknown")
	}
	where := strings.TrimSpace(emptyAs(spec.Provider.Cloud, spec.Provider.Adapter) + " " + spec.Provider.Region)
	parts = append(parts, emptyAs(where, "provider unknown"))
	return strings.Join(parts, " · ")
}

// confirmApproval shows the spend cap, when the approval expires, and every
// setup the run starts, then asks before approving.
func confirmApproval(ctx context.Context, cfg config.Config, t theme, id string, maxCost float64, expiresIn time.Duration, now time.Time) error {
	var response struct {
		Campaign struct {
			ModelIdentity string          `json:"model_identity"`
			Objective     string          `json:"objective"`
			Warnings      *[]string       `json:"warnings"`
			Candidates    []approvalSetup `json:"candidates"`
		} `json:"campaign"`
	}
	readErr := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns/"+url.PathEscape(id), "", nil, &response)
	campaign := response.Campaign
	var b strings.Builder
	b.WriteString(t.tone(toneWatch, "● APPROVE PAID RUN") + "  " + t.title(id) + "\n\n")
	names := []string{"Model", "Spend cap", "Expires"}
	values := []string{
		emptyAs(campaign.ModelIdentity, "unknown") + " · goal " + emptyAs(campaign.Objective, "unknown"),
		fmt.Sprintf("$%.2f, a hard maximum for this run", maxCost),
		now.Add(expiresIn).Local().Format("15:04 MST") + " (" + relativeTime(now, now.Add(expiresIn)) + "); then unfinished setups and a winner that was not rolled out are stopped and cleaned up, and the approval cannot be extended",
	}
	whatRuns := fmt.Sprintf("%d setups", len(campaign.Candidates))
	if readErr != nil {
		whatRuns = "unknown: the control plane did not return the run (" + readErr.Error() + ")"
	}
	names, values = append(names, "What runs"), append(values, whatRuns)
	inner := min(t.width, 96) - 4
	approvalTheme := t
	approvalTheme.width = inner
	b.WriteString(approvalTheme.definitions(0, 10, names, values, func(index int, text string) string {
		if names[index] == "Spend cap" {
			return t.title(text)
		}
		return text
	}))
	for index, setup := range campaign.Candidates {
		description := setup.describe()
		if description == "" {
			description = "setup " + setup.ID + ": unknown"
		}
		for line, text := range wrapWords(description, inner-16) {
			prefix := strings.Repeat(" ", 16)
			if line == 0 {
				prefix = strings.Repeat(" ", 12) + t.muted(fmt.Sprintf("%2d", index+1)) + "  "
			}
			b.WriteString(prefix + text + "\n")
		}
	}
	if campaign.Warnings != nil && len(*campaign.Warnings) > 0 {
		b.WriteString("\n")
		for _, warning := range *campaign.Warnings {
			for line, text := range wrapWords(warning, inner-2) {
				prefix := "  "
				if line == 0 {
					prefix = t.tone(toneWatch, "▲ ")
				}
				b.WriteString(prefix + text + "\n")
			}
		}
	}
	fmt.Fprintln(promptOutput, t.card(strings.TrimRight(b.String(), "\n"), inner+4, toneWatch))
	ok, err := confirm(t, fmt.Sprintf("Approve spending up to $%.2f?", maxCost))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("approval of %s %w", id, errDeclined)
	}
	return nil
}
