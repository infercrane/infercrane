// Package keychain stores CLI credentials in the operating system's secret
// store. It shells out to the platform tool rather than linking a library:
// macOS uses /usr/bin/security and Linux uses secret-tool (Secret Service).
// The secret is always written on standard input, never on the command line,
// so it does not appear in the process list. Other platforms report
// ErrUnavailable and the CLI falls back to its 0600 config file.
package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Service names every InferCrane item in the OS secret store.
const Service = "infercrane-cli"

var (
	ErrUnavailable = errors.New("no OS keychain is available")
	ErrNotFound    = errors.New("credential was not found in the OS keychain")
)

// accountPattern keeps account names safe to pass to the platform tools.
var accountPattern = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,128}$`)

// tokenPattern bounds what may be written; InferCrane keys are ic_ + hex.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{16,256}$`)

// Store is an OS secret store.
type Store interface {
	// Name is shown to the person, for example "macOS Keychain".
	Name() string
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
}

// Runner executes a platform tool. Tests replace it.
type Runner func(ctx context.Context, stdin string, name string, args ...string) (stdout string, exitCode int, err error)

func execRunner(ctx context.Context, stdin string, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return stdout.String(), 0, nil
}

// AccountName derives the keychain account for a CLI context and control
// plane host.
func AccountName(contextName, host string) (string, error) {
	account := contextName + "@" + host
	if !accountPattern.MatchString(account) {
		return "", fmt.Errorf("%w: context %q cannot be used as a keychain account name", ErrUnavailable, contextName)
	}
	return account, nil
}

// Default returns the store for this machine. INFERCRANE_CREDENTIAL_STORE=file
// disables the keychain; "keychain" requires it; "auto" (the default) uses it
// when present.
func Default() Store {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("INFERCRANE_CREDENTIAL_STORE")))
	if mode == "file" {
		return unavailable{reason: "INFERCRANE_CREDENTIAL_STORE=file"}
	}
	return ForPlatform(runtime.GOOS, execRunner, exec.LookPath, os.Getenv)
}

// Required reports whether INFERCRANE_CREDENTIAL_STORE demands the keychain.
func Required() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("INFERCRANE_CREDENTIAL_STORE")), "keychain")
}

// ForPlatform selects a store; it is separated from Default for tests.
func ForPlatform(goos string, run Runner, lookPath func(string) (string, error), getenv func(string) string) Store {
	switch goos {
	case "darwin":
		if _, err := lookPath("/usr/bin/security"); err != nil {
			return unavailable{reason: "the macOS security tool was not found"}
		}
		return macOS{run: run}
	case "linux":
		if getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
			return unavailable{reason: "no desktop session bus for the Secret Service"}
		}
		path, err := lookPath("secret-tool")
		if err != nil {
			return unavailable{reason: "secret-tool is not installed"}
		}
		return secretService{run: run, path: path}
	default:
		return unavailable{reason: "no supported keychain on " + goos}
	}
}

func boundedContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

type unavailable struct{ reason string }

func (u unavailable) Name() string { return "none" }
func (u unavailable) Get(string) (string, error) {
	return "", fmt.Errorf("%w: %s", ErrUnavailable, u.reason)
}
func (u unavailable) Set(string, string) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, u.reason)
}
func (u unavailable) Delete(string) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, u.reason)
}

// macOS stores a generic password in the login keychain.
type macOS struct{ run Runner }

// macOSItemNotFound is security(1)'s exit status for a missing item.
const macOSItemNotFound = 44

func (macOS) Name() string { return "macOS Keychain" }

func (m macOS) Get(account string) (string, error) {
	if !accountPattern.MatchString(account) {
		return "", ErrNotFound
	}
	ctx, cancel := boundedContext()
	defer cancel()
	out, code, err := m.run(ctx, "", "/usr/bin/security", "find-generic-password", "-s", Service, "-a", account, "-w")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if code == macOSItemNotFound {
		return "", ErrNotFound
	}
	if code != 0 {
		return "", fmt.Errorf("read the macOS Keychain: security exited %d", code)
	}
	secret := strings.TrimRight(out, "\r\n")
	if secret == "" {
		return "", ErrNotFound
	}
	return secret, nil
}

// Set uses security's interactive mode so the secret travels on stdin, not
// argv. Inputs are restricted to characters that need no escaping.
func (m macOS) Set(account, secret string) error {
	if !accountPattern.MatchString(account) || !tokenPattern.MatchString(secret) {
		return fmt.Errorf("%w: account or credential has unsupported characters", ErrUnavailable)
	}
	ctx, cancel := boundedContext()
	defer cancel()
	command := fmt.Sprintf("add-generic-password -U -s '%s' -a '%s' -l 'InferCrane CLI' -w '%s'\n", Service, account, secret)
	_, code, err := m.run(ctx, command, "/usr/bin/security", "-i")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if code != 0 {
		return fmt.Errorf("%w: security exited %d", ErrUnavailable, code)
	}
	if stored, getErr := m.Get(account); getErr != nil || stored != secret {
		return fmt.Errorf("%w: the macOS Keychain did not keep the credential", ErrUnavailable)
	}
	return nil
}

func (m macOS) Delete(account string) error {
	if !accountPattern.MatchString(account) {
		return ErrNotFound
	}
	ctx, cancel := boundedContext()
	defer cancel()
	_, code, err := m.run(ctx, "", "/usr/bin/security", "delete-generic-password", "-s", Service, "-a", account)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if code == macOSItemNotFound {
		return ErrNotFound
	}
	if code != 0 {
		return fmt.Errorf("delete from the macOS Keychain: security exited %d", code)
	}
	return nil
}

// secretService stores the item through the freedesktop Secret Service.
type secretService struct {
	run  Runner
	path string
}

func (secretService) Name() string { return "Secret Service keyring" }

func (s secretService) Get(account string) (string, error) {
	if !accountPattern.MatchString(account) {
		return "", ErrNotFound
	}
	ctx, cancel := boundedContext()
	defer cancel()
	out, code, err := s.run(ctx, "", s.path, "lookup", "service", Service, "account", account)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	secret := strings.TrimRight(out, "\r\n")
	if code != 0 || secret == "" {
		return "", ErrNotFound
	}
	return secret, nil
}

func (s secretService) Set(account, secret string) error {
	if !accountPattern.MatchString(account) || !tokenPattern.MatchString(secret) {
		return fmt.Errorf("%w: account or credential has unsupported characters", ErrUnavailable)
	}
	ctx, cancel := boundedContext()
	defer cancel()
	_, code, err := s.run(ctx, secret, s.path, "store", "--label=InferCrane CLI ("+account+")", "service", Service, "account", account)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if code != 0 {
		return fmt.Errorf("%w: secret-tool exited %d", ErrUnavailable, code)
	}
	if stored, getErr := s.Get(account); getErr != nil || stored != secret {
		return fmt.Errorf("%w: the Secret Service did not keep the credential", ErrUnavailable)
	}
	return nil
}

func (s secretService) Delete(account string) error {
	if !accountPattern.MatchString(account) {
		return ErrNotFound
	}
	if _, err := s.Get(account); err != nil {
		return err
	}
	ctx, cancel := boundedContext()
	defer cancel()
	_, code, err := s.run(ctx, "", s.path, "clear", "service", Service, "account", account)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if code != 0 {
		return fmt.Errorf("delete from the Secret Service: secret-tool exited %d", code)
	}
	return nil
}
