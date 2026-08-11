package doctor

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/provision"
)

type capacityAdvisor func(context.Context, provision.AvailabilityRequest) (provision.Availability, error)

func (f capacityAdvisor) Availability(ctx context.Context, request provision.AvailabilityRequest) (provision.Availability, error) {
	return f(ctx, request)
}

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

func TestRunPodServerlessCheckIsRequiredWhenRequested(t *testing.T) {
	cfg := config.Config{RunPodAPIKey: "secret", RunPodServerlessTemplateID: "template"}
	check := CheckRunPodServerless(context.Background(), cfg, Dependencies{RunPodCheck: func(context.Context) error { return errors.New("mutable template") }})
	if check.Status != Fail || check.Remediation == "" {
		t.Fatalf("unexpected check: %#v", check)
	}
	check = CheckRunPodServerless(context.Background(), cfg, Dependencies{RunPodCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("unexpected check: %#v", check)
	}
}

func TestAWSBYOCCheckIsReadOnlyAndRequiredWhenRequested(t *testing.T) {
	cfg := config.Config{AWSRoleARN: "arn:aws:iam::123456789012:role/infercrane", AWSRegion: "eu-central-1"}
	check := CheckAWSBYOC(context.Background(), cfg, Dependencies{AWSCheck: func(context.Context) error { return nil }})
	if check.Status != Pass {
		t.Fatalf("unexpected check: %#v", check)
	}
	check = CheckAWSBYOC(context.Background(), config.Config{}, Dependencies{})
	if check.Status != Fail || check.Remediation == "" {
		t.Fatalf("unexpected unconfigured check: %#v", check)
	}
}

func TestCapacityCheckWarnsWithoutChangingHardware(t *testing.T) {
	check := CheckCapacity(context.Background(), "provider-a", "L40S", capacityAdvisor(func(_ context.Context, request provision.AvailabilityRequest) (provision.Availability, error) {
		if request.GPU != "L40S" || request.Count != 1 {
			t.Fatalf("unexpected request: %#v", request)
		}
		return provision.Availability{State: "constrained", Message: "low stock"}, nil
	}))
	if check.Status != Warn || check.Message != "low stock" || check.Remediation == "" {
		t.Fatalf("unexpected check: %#v", check)
	}
}
