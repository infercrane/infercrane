package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestRunReportsRequiredAndOptionalChecks(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name == "sky" {
			return "", errors.New("missing")
		}
		return "/bin/" + name, nil
	}
	r := Run(context.Background(), config.Config{DatabaseURL: "postgres://db", RouterBinary: "router"}, Dependencies{
		LookPath: lookup, Ping: func(context.Context, string) error { return nil },
	})
	if r.Ready {
		t.Fatal("missing API key must make report not ready")
	}
	if r.Checks[3].Status != Warn {
		t.Fatalf("expected optional SkyPilot warning: %#v", r.Checks)
	}
}

func TestRunReadyWithoutSkyPilot(t *testing.T) {
	lookup := func(name string) (string, error) {
		if name == "sky" {
			return "", errors.New("missing")
		}
		return "/bin/router", nil
	}
	r := Run(context.Background(), config.Config{APIKey: "secret", DatabaseURL: "postgres://db", RouterBinary: "router"}, Dependencies{
		LookPath: lookup, Ping: func(context.Context, string) error { return nil },
	})
	if !r.Ready || r.Err() != nil {
		t.Fatalf("optional warning should remain ready: %#v", r)
	}
}

func TestCloudCredentialCheckIsRequiredWhenRequested(t *testing.T) {
	check := CheckCloudCredentials(context.Background(), Dependencies{LookPath: func(string) (string, error) { return "/bin/sky", nil }, SkyCheck: func(context.Context) error { return errors.New("no credentials") }})
	if check.Status != Fail {
		t.Fatalf("unexpected check: %#v", check)
	}
}
