// Package doctor performs read-only checks of an InferCrane environment.
package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Report struct {
	Ready  bool    `json:"ready"`
	Checks []Check `json:"checks"`
}

type Dependencies struct {
	LookPath func(string) (string, error)
	Ping     func(context.Context, string) error
	SkyCheck func(context.Context) error
}

func CheckCloudCredentials(ctx context.Context, deps Dependencies) Check {
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if _, err := deps.LookPath("sky"); err != nil {
		return Check{"Cloud credentials", Fail, "SkyPilot CLI is not installed", "Install and configure SkyPilot, then run `sky check`."}
	}
	check := deps.SkyCheck
	if check == nil {
		check = func(ctx context.Context) error {
			command := exec.CommandContext(ctx, "sky", "check")
			output, err := command.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
			}
			return nil
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return Check{"Cloud credentials", Fail, "SkyPilot credential check failed: " + err.Error(), "Configure at least one supported cloud and confirm `sky check` succeeds."}
	}
	return Check{"Cloud credentials", Pass, "SkyPilot reports usable cloud credentials", ""}
}

func (r *Report) Add(check Check) {
	r.Checks = append(r.Checks, check)
	if check.Status == Fail {
		r.Ready = false
	}
}

func Run(ctx context.Context, cfg config.Config, deps Dependencies) Report {
	if deps.LookPath == nil {
		deps.LookPath = exec.LookPath
	}
	if deps.Ping == nil {
		deps.Ping = pingDatabase
	}
	r := Report{Ready: true}
	add := func(c Check) {
		r.Checks = append(r.Checks, c)
		if c.Status == Fail {
			r.Ready = false
		}
	}
	if cfg.APIKey == "" {
		add(Check{"API authentication", Fail, "INFERCRANE_API_KEY is not set", "Set a strong, secret API key in the runtime environment."})
	} else {
		add(Check{"API authentication", Pass, "API key is configured", ""})
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := deps.Ping(pingCtx, cfg.DatabaseURL); err != nil {
		add(Check{"PostgreSQL", Fail, "Database is unreachable: " + err.Error(), "Verify INFERCRANE_DATABASE_URL, networking, TLS, and PostgreSQL readiness."})
	} else {
		add(Check{"PostgreSQL", Pass, "Database connection succeeded", ""})
	}
	if path, err := deps.LookPath(cfg.RouterBinary); err != nil {
		add(Check{"vLLM Router", Fail, fmt.Sprintf("%q was not found on PATH", cfg.RouterBinary), "Install vllm-router or set INFERCRANE_ROUTER_BINARY to its executable path."})
	} else {
		add(Check{"vLLM Router", Pass, "Router binary found at " + path, ""})
	}
	if path, err := deps.LookPath("sky"); err != nil {
		add(Check{"SkyPilot", Warn, "SkyPilot CLI is not installed; existing-target deployments still work", "Install SkyPilot before using --cloud/--gpu provisioning."})
	} else {
		add(Check{"SkyPilot", Pass, "SkyPilot CLI found at " + path, ""})
	}
	return r
}

func pingDatabase(ctx context.Context, databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.PingContext(ctx)
}

func (r Report) Err() error {
	if r.Ready {
		return nil
	}
	return errors.New("doctor found required checks that need attention")
}
