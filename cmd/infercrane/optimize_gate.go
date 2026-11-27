package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/proofgate"
)

// optimize gate turns one run's signed report into a merge verdict: PASS,
// WATCH, BLOCK, or UNKNOWN, for the setup a change introduced against the
// current setup. It only reads: the control API's report, or a report saved
// with optimize report --output json. It exits non-zero when the check
// fails: BLOCK, UNKNOWN, or WATCH with --on-watch fail.

const optimizeGateUsage = "usage: infercrane optimize gate RUN [--setup ID] [--on-watch neutral|fail|pass] [--wait DURATION] [--require-signature] [--public-key KEY] [--save-report FILE] [--output text|json|markdown] | optimize gate --file REPORT.json [flags]"

// errGateFailed is returned after the result was printed, so the exit status
// carries the check's conclusion.
type errGateFailed struct{ verdict string }

func (e errGateFailed) Error() string {
	return "proof gate: " + e.verdict + " fails this check"
}

type gateOptions struct {
	policy      proofgate.Policy
	publicKey   string
	output      string
	commentKey  string
	saveReport  string
	wait, poll  time.Duration
	file        string
	evidenceURL string
}

func optimizeGateCommand(ctx context.Context, args []string) error {
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	fs := newFlagSet("optimize gate")
	setup := fs.String("setup", "", "ID of the setup the change introduced; defaults to the only setup besides the current one")
	onWatch := fs.String("on-watch", "neutral", "conclusion for WATCH: neutral (does not fail), fail, or pass")
	wait := fs.Duration("wait", 0, "wait up to this long for the run to finish; 0 reads the report once")
	poll := fs.Duration("poll", 30*time.Second, "how often to read the report while waiting")
	requireSignature := fs.Bool("require-signature", false, "treat an unsigned report as UNKNOWN")
	publicKey := fs.String("public-key", "", "base64 Ed25519 key the report must be signed by; implies --require-signature")
	file := fs.String("file", "", "decide from a report saved with optimize report RUN --output json instead of the control API")
	saveReport := fs.String("save-report", "", "write the signed report, byte for byte, to FILE")
	commentKey := fs.String("comment-key", "default", "names this gate in the pull-request comment marker")
	output := fs.String("output", "text", "text, json, or markdown")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(optimizeGateUsage)
	}
	if (id == "") == (*file == "") || (*output != "text" && *output != "json" && *output != "markdown") || *wait < 0 || *poll < time.Second {
		return errors.New(optimizeGateUsage)
	}
	if _, err := proofgate.NormalizeOnWatch(*onWatch); err != nil {
		return err
	}
	options := gateOptions{
		policy:    proofgate.Policy{Setup: *setup, OnWatch: *onWatch, RequireSignature: *requireSignature || strings.TrimSpace(*publicKey) != ""},
		publicKey: strings.TrimSpace(*publicKey), output: *output, commentKey: *commentKey, saveReport: *saveReport,
		wait: *wait, poll: *poll, file: *file,
	}
	var raw []byte
	if *file != "" {
		body, err := os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("read saved report: %w", err)
		}
		raw = body
	} else {
		cfg, err := config.LoadClient()
		if err != nil {
			return err
		}
		if raw, err = waitForReport(ctx, cfg, id, options.wait, options.poll, os.Stderr); err != nil {
			return err
		}
		options.evidenceURL = dashboardURL(cfg, dashboardPath("run", id))
		if options.evidenceURL == "" {
			options.evidenceURL = strings.TrimRight(cfg.ControlURL, "/") + "/api/v1/optimization/campaigns/" + url.PathEscape(id) + "/report"
		}
	}
	return writeGate(os.Stdout, raw, options)
}

// waitForReport reads the report until the run finishes or the wait ends.
// A run that has not finished by then is decided as it stands: UNKNOWN.
func waitForReport(ctx context.Context, cfg config.Config, id string, wait, poll time.Duration, progress io.Writer) ([]byte, error) {
	deadline := time.Now().Add(wait)
	for {
		raw, document, err := loadOptimizationReport(ctx, cfg, id)
		if err != nil {
			return nil, err
		}
		if document.Report.Finished || wait == 0 || !time.Now().Add(poll).Before(deadline) {
			return raw, nil
		}
		fmt.Fprintf(progress, "Run %s is %s; reading its report again in %s.\n", id, document.Report.CampaignState, poll)
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// decideGate verifies a saved or fetched report and applies the gate.
func decideGate(raw []byte, options gateOptions) (proofgate.Result, error) {
	var document optimizationreport.Document
	if json.Unmarshal(raw, &document) != nil || document.Report.SchemaVersion == "" {
		return proofgate.Result{}, errors.New("not an optimization report: expected the JSON of infercrane optimize report RUN --output json")
	}
	signature := proofgate.Signature{UnsignedReason: document.UnsignedReason}
	if document.Signature != nil {
		signature.Present = true
		var trusted []byte
		if options.publicKey != "" {
			key, err := passport.DecodePublicKey(options.publicKey)
			if err != nil {
				return proofgate.Result{}, fmt.Errorf("--public-key: %w", err)
			}
			trusted = key
		}
		verified, err := optimizationreport.Verify(raw, trusted)
		if err != nil {
			signature.Error = err.Error()
		} else {
			signature.Verified, signature.Pinned, signature.KeyID = true, verified.Pinned, verified.KeyID
		}
	}
	result := proofgate.Decide(document, signature, options.policy)
	result.EvidenceURL = options.evidenceURL
	return result, nil
}

func writeGate(w io.Writer, raw []byte, options gateOptions) error {
	result, err := decideGate(raw, options)
	if err != nil {
		return err
	}
	if options.saveReport != "" {
		if err = os.WriteFile(options.saveReport, raw, 0o600); err != nil {
			return fmt.Errorf("save report: %w", err)
		}
	}
	markdown := proofgate.Markdown(result, options.commentKey)
	switch options.output {
	case "json":
		encoded, encodeErr := json.MarshalIndent(struct {
			proofgate.Result
			Markdown string `json:"markdown"`
		}{result, markdown}, "", "  ")
		if encodeErr != nil {
			return encodeErr
		}
		fmt.Fprintln(w, string(encoded))
	case "markdown":
		fmt.Fprint(w, markdown)
	default:
		fmt.Fprint(w, gateText(result))
	}
	if result.Conclusion == proofgate.ConclusionFailure {
		return errGateFailed{verdict: result.Verdict}
	}
	return nil
}

// gateText is the plain-text verdict for a terminal or a CI log.
func gateText(result proofgate.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Proof gate · %s · check %s\n%s\n", result.Verdict, result.Conclusion, result.Headline)
	if result.Banner != "" {
		fmt.Fprintf(&b, "%s\n", result.Banner)
	}
	b.WriteString("\n")
	if result.Baseline != nil {
		fmt.Fprintf(&b, "Current setup  %s\n", result.Baseline.Label)
	}
	if result.Setup != nil {
		fmt.Fprintf(&b, "This change    %s\n", result.Setup.Label)
	}
	speed := "unknown"
	if result.Speed.ChangePercent != nil && result.Speed.NoiseFloorPercent != nil {
		speed = fmt.Sprintf("%+.1f%% (noise ±%.1f%%) %s", *result.Speed.ChangePercent, *result.Speed.NoiseFloorPercent, result.Speed.Decision)
	}
	fmt.Fprintf(&b, "Measured       %s: %s\n", result.Speed.Metric, speed)
	if result.Speed.Confirmation != nil {
		fmt.Fprintf(&b, "Fresh servers  %+.1f%% (noise ±%.1f%%) %s; gain %s\n", result.Speed.Confirmation.ChangePercent, result.Speed.Confirmation.NoiseFloorPercent, result.Speed.Confirmation.Decision, result.Speed.Claim)
	}
	answers := "UNKNOWN: not run"
	if result.Answers != nil {
		answers = result.Answers.Verdict + ": " + result.Answers.Reason
	}
	fmt.Fprintf(&b, "Answers        %s\n", answers)
	if len(result.Reasons) > 0 {
		b.WriteString("\nWhy\n")
		for _, reason := range result.Reasons {
			fmt.Fprintf(&b, "  - %s\n", reason)
		}
	}
	fmt.Fprintf(&b, "\nRun %s · report %s\n", result.CampaignID, result.ReportDigest)
	switch {
	case result.Signature.Error != "":
		fmt.Fprintf(&b, "Signature did not verify: %s\n", result.Signature.Error)
	case result.Signature.Verified:
		fmt.Fprintf(&b, "Signed by key %s\n", result.Signature.KeyID)
	default:
		fmt.Fprintf(&b, "Not signed: %s\n", optimizationreport.UnsignedText(result.Signature.UnsignedReason))
	}
	if result.EvidenceURL != "" {
		fmt.Fprintf(&b, "Evidence %s\n", result.EvidenceURL)
	}
	return b.String()
}
