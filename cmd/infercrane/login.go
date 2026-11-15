package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/clilogin"
	"github.com/infercrane/infercrane/internal/config"
)

// loginOutput is where login and logout print progress. Results go to stdout
// so `--output json` stays parseable.
var loginOutput io.Writer = os.Stderr

// openBrowser opens a URL in the person's browser. Tests replace it.
var openBrowser = func(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no graphical session")
		}
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// loginSleep waits between polls. Tests replace it.
var loginSleep = func(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func contextFlagDefault() string {
	return os.Getenv("INFERCRANE_CONTEXT")
}

type cliLoginStartResponse struct {
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	ExpiresAt               time.Time `json:"expires_at"`
	ExpiresIn               int       `json:"expires_in"`
	Interval                int       `json:"interval"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete"`
}

type cliLoginPrincipal struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Kind      string     `json:"kind"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type cliLoginExchangeResponse struct {
	Status     string            `json:"status"`
	Interval   int               `json:"interval"`
	Credential string            `json:"credential"`
	OwnerName  string            `json:"owner_name"`
	Principal  cliLoginPrincipal `json:"principal"`
	ConsoleURL string            `json:"console_url"`
}

func loginCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	controlURL := fs.String("url", "", "control-plane URL (default: the context's URL, INFERCRANE_URL, or http://127.0.0.1:8080)")
	consoleURL := fs.String("console-url", "", "dashboard URL that approves the sign-in (default: INFERCRANE_CONSOLE_URL, the control plane's advertised dashboard, or the hosted dashboard)")
	contextName := fs.String("context", contextFlagDefault(), "context to sign in")
	noBrowser := fs.Bool("no-browser", false, "print the URL and code instead of opening a browser")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: infercrane login [--url URL] [--console-url URL] [--context NAME] [--no-browser] [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	target, err := config.LoadClientTarget(*contextName)
	if err != nil {
		return err
	}
	if target.ClientContextName == "" {
		target.ClientContextName = "default"
	}
	if *contextName != "" {
		target.ClientContextName = *contextName
	}
	if *controlURL != "" {
		parsed, parseErr := url.Parse(*controlURL)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("--url must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		if strings.TrimRight(*controlURL, "/") != strings.TrimRight(target.ControlURL, "/") {
			// A different control plane does not inherit the old dashboard.
			if target.ConsoleURL, err = config.ResolveConsoleURL(*controlURL, ""); err != nil {
				return err
			}
		}
		target.ControlURL = strings.TrimRight(*controlURL, "/")
	}
	explicitConsole := strings.TrimSpace(*consoleURL)
	if explicitConsole == "" {
		explicitConsole = strings.TrimSpace(os.Getenv("INFERCRANE_CONSOLE_URL"))
	}
	if explicitConsole != "" {
		if explicitConsole, err = config.NormalizeConsoleURL(explicitConsole); err != nil {
			return err
		}
	}
	target.APIKey = ""

	hostname, _ := os.Hostname()
	if strings.TrimSpace(hostname) == "" {
		hostname = "unknown device"
	}
	var started cliLoginStartResponse
	client := clilogin.Client{DeviceName: hostname, OS: runtime.GOOS + "/" + runtime.GOARCH, Version: "infercrane " + version}
	if err = controlJSON(ctx, target, http.MethodPost, "/api/v1/cli/logins", "", client, &started); err != nil {
		return fmt.Errorf("start sign-in: %w", err)
	}
	if started.DeviceCode == "" || started.UserCode == "" {
		return errors.New("start sign-in: the control plane returned no sign-in code")
	}

	dashboard := explicitConsole
	advertised := ""
	if started.VerificationURI != "" {
		advertised = strings.TrimSuffix(strings.TrimRight(started.VerificationURI, "/"), "/cli/authorize")
	}
	if dashboard == "" && advertised != "" {
		if dashboard, err = config.NormalizeConsoleURL(advertised); err != nil {
			dashboard = ""
		}
	}
	if dashboard == "" {
		dashboard = target.ConsoleURL
	}
	if dashboard == "" {
		return fmt.Errorf("no dashboard is configured for %s; pass --console-url URL or set INFERCRANE_CONSOLE_URL to the dashboard that should approve this sign-in", target.ControlURL)
	}
	approveURL := dashboard + "/cli/authorize?code=" + url.QueryEscape(started.UserCode)

	expires := time.Duration(started.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = clilogin.RequestTTL
	}
	fmt.Fprintf(loginOutput, "%s Your one-time code: %s\n", terminalColor("33", "!"), terminalColor("1", started.UserCode))
	fmt.Fprintln(loginOutput, "  Check that the dashboard shows the same code before you approve.")
	opened := false
	if !*noBrowser {
		if openErr := openBrowser(approveURL); openErr == nil {
			opened = true
			fmt.Fprintf(loginOutput, "Opened %s\n", approveURL)
		}
	}
	if !opened {
		fmt.Fprintf(loginOutput, "Open this page in a browser and approve the sign-in:\n  %s\n", approveURL)
	}
	fmt.Fprintf(loginOutput, "Waiting for approval (expires in %s). Press Ctrl-C to cancel.\n", formatLoginDuration(expires))

	exchange, err := pollCLILogin(ctx, target, started, expires)
	if err != nil {
		return err
	}
	if exchange.ConsoleURL != "" && explicitConsole == "" {
		if normalized, normalizeErr := config.NormalizeConsoleURL(exchange.ConsoleURL); normalizeErr == nil {
			dashboard = normalized
		}
	}
	stored, err := config.SaveClientLogin(config.ClientLogin{Context: target.ClientContextName, ControlURL: target.ControlURL, ConsoleURL: dashboard, APIKey: exchange.Credential})
	if err != nil {
		return fmt.Errorf("the sign-in was approved, but the key could not be saved: %w; revoke it in the dashboard under Settings → CLI sessions", err)
	}

	sessionsURL := dashboard + "/settings/cli-sessions"
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{
			"signed_in": true, "context": stored.Context, "control_url": target.ControlURL, "console_url": dashboard,
			"credential_store": stored.Store, "config_path": stored.Path, "owner_name": exchange.OwnerName,
			"principal": exchange.Principal, "dashboard_url": sessionsURL,
		}, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	who := exchange.OwnerName
	if who == "" {
		who = exchange.Principal.Name
	}
	fmt.Printf("%s Signed in to %s as %s (%s, tenant %s)\n", terminalColor("32", "✓"), target.ControlURL, who, exchange.Principal.Role, exchange.Principal.TenantID)
	fmt.Printf("  Context    %s\n", stored.Context)
	if stored.Store == config.CredentialFromConfigFile {
		reason := "no OS keychain was available"
		if strings.Contains(stored.KeychainError, "INFERCRANE_CREDENTIAL_STORE=file") {
			reason = "INFERCRANE_CREDENTIAL_STORE=file"
		}
		fmt.Printf("  Key        saved in %s (mode 0600); %s\n", stored.Path, reason)
	} else {
		fmt.Printf("  Key        saved in %s\n", stored.Store)
	}
	if exchange.Principal.ExpiresAt != nil {
		fmt.Printf("  Expires    %s\n", exchange.Principal.ExpiresAt.UTC().Format("2006-01-02"))
	}
	printDashboardLink(os.Stdout, sessionsURL)
	return nil
}

func pollCLILogin(ctx context.Context, target config.Config, started cliLoginStartResponse, expires time.Duration) (cliLoginExchangeResponse, error) {
	interval := time.Duration(started.Interval) * time.Second
	if interval < time.Second {
		interval = clilogin.PollInterval
	}
	deadline := time.Now().Add(expires + 5*time.Second)
	for {
		if err := loginSleep(ctx, interval); err != nil {
			return cliLoginExchangeResponse{}, errors.New("sign-in cancelled; nothing was saved")
		}
		var exchange cliLoginExchangeResponse
		err := controlJSON(ctx, target, http.MethodPost, "/api/v1/cli/logins/token", "", map[string]string{"device_code": started.DeviceCode}, &exchange)
		var controlErr *ControlError
		switch {
		case ctx.Err() != nil:
			return cliLoginExchangeResponse{}, errors.New("sign-in cancelled; nothing was saved")
		case errors.As(err, &controlErr) && controlErr.StatusCode == http.StatusNotFound:
			return cliLoginExchangeResponse{}, errors.New("the sign-in request is gone; run infercrane login again")
		case err != nil:
			// A transient network failure should not end a sign-in the person
			// is about to approve; the deadline still bounds it.
			if time.Now().After(deadline) {
				return cliLoginExchangeResponse{}, fmt.Errorf("sign-in expired: %w", err)
			}
			continue
		}
		switch exchange.Status {
		case clilogin.StatusApproved:
			if exchange.Credential == "" {
				return cliLoginExchangeResponse{}, errors.New("the control plane approved the sign-in but returned no key; run infercrane login again")
			}
			return exchange, nil
		case clilogin.StatusPending:
		case clilogin.StatusSlowDown:
			interval += 2 * time.Second
		case clilogin.StatusDenied:
			return cliLoginExchangeResponse{}, errors.New("the sign-in was denied in the dashboard; nothing was saved")
		case clilogin.StatusConsumed:
			return cliLoginExchangeResponse{}, errors.New("this sign-in was already completed elsewhere; run infercrane login again")
		case clilogin.StatusExpired:
			return cliLoginExchangeResponse{}, errors.New("the sign-in code expired before it was approved; run infercrane login again")
		default:
			return cliLoginExchangeResponse{}, fmt.Errorf("unexpected sign-in status %q", exchange.Status)
		}
		if exchange.Interval > 0 && time.Duration(exchange.Interval)*time.Second > interval {
			interval = time.Duration(exchange.Interval) * time.Second
		}
		if time.Now().After(deadline) {
			return cliLoginExchangeResponse{}, errors.New("the sign-in code expired before it was approved; run infercrane login again")
		}
	}
}

func formatLoginDuration(value time.Duration) string {
	if value >= time.Minute && value%time.Minute == 0 {
		minutes := int(value / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return value.Round(time.Second).String()
}

func logoutCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	contextName := fs.String("context", contextFlagDefault(), "context to sign out")
	output := fs.String("output", "human", "human or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: infercrane logout [--context NAME] [--output human|json]")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	environmentKey := os.Getenv("INFERCRANE_API_KEY") != ""
	if environmentKey {
		// Sign out the stored key, not the one in the environment.
		_ = os.Unsetenv("INFERCRANE_API_KEY")
	}
	target, err := config.LoadClientTarget(*contextName)
	if err != nil {
		return err
	}
	revocation := "no stored key"
	if target.APIKey != "" {
		callErr := controlJSON(ctx, target, http.MethodDelete, "/api/v1/cli/sessions/current", "", nil, nil)
		var controlErr *ControlError
		switch {
		case callErr == nil:
			revocation = "revoked"
		case errors.As(callErr, &controlErr) && controlErr.Code == "not_cli_session":
			revocation = "kept_active"
		case errors.As(callErr, &controlErr) && controlErr.StatusCode == http.StatusUnauthorized:
			revocation = "already_invalid"
		default:
			revocation = "unreachable"
		}
	}
	cleared, err := config.ClearClientCredential(target.ClientContextName)
	if err != nil {
		return err
	}
	if *output == "json" {
		encoded, _ := json.MarshalIndent(map[string]any{
			"signed_out": true, "context": cleared.Context, "control_url": target.ControlURL,
			"revocation": revocation, "removed_from": cleared.Removed, "environment_key_set": environmentKey,
		}, "", "  ")
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Printf("%s Signed out of %s (context %s)\n", terminalColor("32", "✓"), target.ControlURL, cleared.Context)
	switch revocation {
	case "revoked":
		fmt.Println("  The key was revoked on the control plane.")
	case "kept_active":
		fmt.Println("  The key was not issued by infercrane login, so it stays active on the control plane.")
		fmt.Println("  Revoke it with: infercrane principal revoke ID")
	case "already_invalid":
		fmt.Println("  The control plane no longer accepted the key.")
	case "unreachable":
		fmt.Println("  The control plane could not be reached, so the key is still valid there.")
		if target.ConsoleURL != "" {
			fmt.Printf("  Revoke it in the dashboard: %s/settings/cli-sessions\n", target.ConsoleURL)
		}
	}
	if len(cleared.Removed) > 0 {
		fmt.Printf("  Removed the key from %s.\n", strings.Join(cleared.Removed, " and "))
	}
	if environmentKey {
		fmt.Println("  INFERCRANE_API_KEY is still set in your shell; unset it to stop using that key.")
	}
	return nil
}

// dashboardURL joins the configured dashboard and a page path. It returns ""
// when no dashboard is configured, so local stacks print no link.
func dashboardURL(cfg config.Config, path string) string {
	base := strings.TrimRight(cfg.ConsoleURL, "/")
	if base == "" {
		return ""
	}
	if path == "" || path == "/" {
		return base
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

func dashboardPath(kind, name string) string {
	escaped := url.PathEscape(name)
	switch kind {
	case "endpoint":
		return "/endpoints/" + escaped
	case "deployment":
		return "/workloads/" + escaped
	case "run":
		return "/optimization/" + escaped
	case "model":
		return "/models/" + escaped
	case "operation":
		return "/activity/operations/" + escaped
	}
	return ""
}

func printDashboardLink(w io.Writer, target string) {
	if target == "" {
		return
	}
	fmt.Fprintf(w, "View in dashboard → %s\n", target)
}

// printResourceLink prints the dashboard link for a human-readable result.
func printResourceLink(cfg config.Config, kind, name string) {
	if name == "" {
		return
	}
	printDashboardLink(os.Stdout, dashboardURL(cfg, dashboardPath(kind, name)))
}

func openCommand(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	usage := "usage: infercrane open [endpoint NAME | deployment NAME | run ID | model NAME | operation ID] [--no-browser]"
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	path := ""
	switch len(positional) {
	case 0:
	case 2:
		path = dashboardPath(positional[0], positional[1])
		if path == "" {
			return errors.New(usage)
		}
	default:
		return errors.New(usage)
	}
	cfg, err := config.LoadClientTarget(contextFlagDefault())
	if err != nil {
		return err
	}
	target := dashboardURL(cfg, path)
	if target == "" {
		return fmt.Errorf("no dashboard is configured for %s; set INFERCRANE_CONSOLE_URL or run infercrane login --console-url URL", cfg.ControlURL)
	}
	if !*noBrowser {
		if err := openBrowser(target); err == nil {
			fmt.Printf("Opened %s\n", target)
			return nil
		}
	}
	fmt.Println(target)
	return nil
}
