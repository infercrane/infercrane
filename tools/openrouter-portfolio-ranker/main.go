package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/openrouterportfolio"
)

type profileFile struct {
	SchemaVersion string                               `json:"schema_version"`
	Profiles      []openrouterportfolio.ServingProfile `json:"profiles"`
}

func main() {
	defaultKey := filepath.Join(userHome(), ".config", "infercrane", "openrouter-key")
	keyFile := flag.String("api-key-file", defaultKey, "owner-only file containing an OpenRouter API key")
	profilesFile := flag.String("profiles", "", "optional serving profiles JSON")
	output := flag.String("output", "", "optional report path; stdout when empty")
	lookback := flag.Int("lookback-days", 7, "inclusive demand lookback ending yesterday")
	limit := flag.Int("market-limit", 100, "maximum open-weight models with endpoint and weight-footprint details")
	share := flag.Float64("assumed-market-share", 0.005, "routing-share scenario, expressed as a fraction")
	flag.Parse()
	if *lookback < 1 || *lookback > 90 {
		fatal(errors.New("--lookback-days must be between 1 and 90"))
	}
	key, err := readSecret(*keyFile)
	if err != nil {
		fatal(fmt.Errorf("read OpenRouter API key: %w", err))
	}
	now := time.Now().UTC()
	end := now.AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -(*lookback - 1))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := openrouterportfolio.Client{APIKey: key, Now: func() time.Time { return now }}
	markets, err := client.Snapshot(ctx, start, end, *limit)
	if err != nil {
		fatal(err)
	}
	profiles := []openrouterportfolio.ServingProfile{}
	if strings.TrimSpace(*profilesFile) != "" {
		profiles, err = readProfiles(*profilesFile)
		if err != nil {
			fatal(err)
		}
	}
	policy := openrouterportfolio.DefaultPolicy()
	policy.AssumedMarketShare = *share
	report, err := openrouterportfolio.Rank(now, markets, profiles, policy)
	if err != nil {
		fatal(err)
	}
	report.MarketWindowStart = start.UTC()
	report.MarketWindowEnd = end.UTC()
	report.Sources = []string{
		"https://openrouter.ai/api/v1/datasets/rankings-daily",
		"https://openrouter.ai/api/v1/models",
		"https://huggingface.co/api/models",
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
	}
	body = append(body, '\n')
	if strings.TrimSpace(*output) == "" {
		_, err = os.Stdout.Write(body)
	} else {
		err = os.WriteFile(*output, body, 0o644)
	}
	if err != nil {
		fatal(err)
	}
}

func readProfiles(path string) ([]openrouterportfolio.ServingProfile, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profiles: %w", err)
	}
	var document profileFile
	if err = json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode profiles: %w", err)
	}
	if document.SchemaVersion != "infercrane.dev/openrouter-serving-profiles/v1" {
		return nil, fmt.Errorf("unsupported serving profile schema %q", document.SchemaVersion)
	}
	return document.Profiles, nil
}

func readSecret(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return "", errors.New("secret must be an owner-only regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(body))
	if value == "" {
		return "", errors.New("secret file is empty")
	}
	return value, nil
}

func userHome() string {
	value, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return value
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "openrouter-portfolio-ranker:", err)
	os.Exit(1)
}
