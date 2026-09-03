// model-api-qualifier deliberately exercises one exact supplier contract and
// emits append-only, secret-free evidence for the Model API operator endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg := qualifierConfig{}
	flag.StringVar(&cfg.Profile, "profile", profileDeepSeekV4Flash, "qualification profile: deepseek-v4-flash, glm-5.2, glm-5.3, glm-5.3-flash, or qwen3.8-27b-runpod")
	flag.StringVar(&cfg.EndpointOrigin, "endpoint-origin", "", "exact RunPod load-balanced HTTPS endpoint origin (RunPod profile only)")
	flag.StringVar(&cfg.Region, "region", "", "exact supplier region (required for the RunPod profile)")
	flag.StringVar(&cfg.OfferID, "offer-id", "", "immutable supplier offer id")
	flag.Int64Var(&cfg.OfferVersion, "offer-version", 0, "immutable supplier offer version")
	flag.StringVar(&cfg.QualificationID, "qualification-id", "", "append-only qualification id")
	flag.StringVar(&cfg.TupleKey, "tuple-key", "", "exact tuple key from the supplier offer")
	flag.StringVar(&cfg.ExpectedRevision, "expected-revision", "", "operator-confirmed exact supplier revision")
	flag.StringVar(&cfg.EvidenceRef, "evidence-ref", "", "durable object reference for the raw evidence artifact")
	flag.StringVar(&cfg.EvidenceOutput, "evidence-output", "", "path for secret-free raw evidence JSON")
	flag.StringVar(&cfg.QualificationOutput, "qualification-output", "", "path for operator qualification JSON")
	flag.IntVar(&cfg.SamplesPerMode, "samples-per-mode", 3, "buffered and streaming samples to run per mode (1-20)")
	flag.IntVar(&cfg.MaxOutputTokens, "max-output-tokens", 64, "maximum generated tokens per sample (1-1024)")
	flag.DurationVar(&cfg.RequestTimeout, "request-timeout", 60*time.Second, "hard timeout for each supplier request")
	flag.DurationVar(&cfg.TotalTimeout, "total-timeout", 10*time.Minute, "hard timeout for the complete qualification run")
	flag.DurationVar(&cfg.ValidFor, "valid-for", time.Hour, "qualification validity window (up to 24h)")
	flag.Int64Var(&cfg.MaxStreamBytes, "max-stream-bytes", 8<<20, "maximum SSE bytes accepted per sample")
	flag.BoolVar(&cfg.ConfirmLive, "confirm-live", false, "required acknowledgement that qualification makes billable supplier calls")
	legacyDeepSeekConfirmation := false
	flag.BoolVar(&legacyDeepSeekConfirmation, "confirm-live-deepseek", false, "deprecated alias for --confirm-live")
	flag.Parse()
	profile, err := resolveQualificationProfile(cfg.Profile)
	if err != nil {
		fatal(err)
	}
	if legacyDeepSeekConfirmation && profile.Name != profileDeepSeekV4Flash {
		fatal(errors.New("--confirm-live-deepseek is accepted only by the deepseek-v4-flash profile; use --confirm-live"))
	}
	cfg.ConfirmLive = cfg.ConfirmLive || legacyDeepSeekConfirmation

	if err := cfg.Validate(); err != nil {
		fatal(err)
	}
	credential, ok := os.LookupEnv(profile.CredentialEnv)
	if !ok || credential == "" {
		fatal(errors.New(profile.CredentialEnv + " is required"))
	}
	secret := []byte(credential)
	credential = ""
	defer clearBytes(secret)

	client := &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.TotalTimeout)
	defer cancel()
	raw, manifest, err := runQualification(ctx, cfg, client, secret)
	if err != nil {
		fatal(err)
	}
	if err = writeArtifacts(cfg, raw, manifest); err != nil {
		fatal(err)
	}
	fmt.Printf("evidence: %s\nqualification: %s\ndigest: %s\n", cfg.EvidenceOutput, cfg.QualificationOutput, manifest.Evidence.EvidenceDigest)
}

func fatal(err error) {
	// Errors crossing the qualifier boundary are intentionally sanitized. Raw
	// HTTP bodies, request headers, and wrapped transport errors are never used.
	fmt.Fprintln(os.Stderr, "model api qualification:", err)
	os.Exit(1)
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
