package keychain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type call struct {
	stdin, name string
	args        []string
}

type fakeTool struct {
	mu      sync.Mutex
	calls   []call
	secrets map[string]string
}

func (f *fakeTool) run(_ context.Context, stdin, name string, args ...string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{stdin: stdin, name: name, args: append([]string(nil), args...)})
	if f.secrets == nil {
		f.secrets = map[string]string{}
	}
	switch {
	case name == "/usr/bin/security" && len(args) == 1 && args[0] == "-i":
		// add-generic-password -U -s 'svc' -a 'account' -l 'label' -w 'secret'
		fields := strings.Fields(strings.ReplaceAll(stdin, "'", ""))
		account, secret := "", ""
		for i := 0; i+1 < len(fields); i++ {
			switch fields[i] {
			case "-a":
				account = fields[i+1]
			case "-w":
				secret = fields[i+1]
			}
		}
		f.secrets[account] = secret
		return "", 0, nil
	case name == "/usr/bin/security" && args[0] == "find-generic-password":
		secret, ok := f.secrets[args[4]]
		if !ok {
			return "", macOSItemNotFound, nil
		}
		return secret + "\n", 0, nil
	case name == "/usr/bin/security" && args[0] == "delete-generic-password":
		if _, ok := f.secrets[args[4]]; !ok {
			return "", macOSItemNotFound, nil
		}
		delete(f.secrets, args[4])
		return "", 0, nil
	case args[0] == "store":
		f.secrets[args[len(args)-1]] = stdin
		return "", 0, nil
	case args[0] == "lookup":
		secret, ok := f.secrets[args[len(args)-1]]
		if !ok {
			return "", 1, nil
		}
		return secret, 0, nil
	case args[0] == "clear":
		delete(f.secrets, args[len(args)-1])
		return "", 0, nil
	}
	return "", 2, nil
}

func found(string) (string, error)   { return "/usr/bin/secret-tool", nil }
func missing(string) (string, error) { return "", errors.New("missing") }
func sessionBus(name string) string {
	return map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/bus"}[name]
}
func noSessionBus(name string) string { return "" }

// secret is a fake key built at runtime so secret scanners do not flag it.
var secret = "ic_" + strings.Repeat("0", 32)

func TestMacOSKeychainNeverPutsTheSecretOnTheCommandLine(t *testing.T) {
	tool := &fakeTool{}
	store := ForPlatform("darwin", tool.run, found, noSessionBus)
	if store.Name() != "macOS Keychain" {
		t.Fatalf("store=%s", store.Name())
	}
	if err := store.Set("default@127.0.0.1:8080", secret); err != nil {
		t.Fatal(err)
	}
	for _, c := range tool.calls {
		for _, arg := range c.args {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret reached argv: %v", c.args)
			}
		}
	}
	if !strings.Contains(tool.calls[0].stdin, secret) || tool.calls[0].args[0] != "-i" {
		t.Fatalf("secret was not written on stdin: %#v", tool.calls[0])
	}
	got, err := store.Get("default@127.0.0.1:8080")
	if err != nil || got != secret {
		t.Fatalf("get=%q err=%v", got, err)
	}
	if err = store.Delete("default@127.0.0.1:8080"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get("default@127.0.0.1:8080"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err=%v", err)
	}
	if err = store.Delete("default@127.0.0.1:8080"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err=%v", err)
	}
}

func TestKeychainRejectsValuesThatWouldNeedEscaping(t *testing.T) {
	store := ForPlatform("darwin", (&fakeTool{}).run, found, noSessionBus)
	for _, bad := range []struct{ account, secret string }{
		{"default'; delete-keychain", secret},
		{"default", "ic_secret'\nadd-generic-password"},
		{"default", "short"},
	} {
		if err := store.Set(bad.account, bad.secret); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Set(%q) err=%v", bad.account, err)
		}
	}
	if _, err := AccountName("prod team", "example.com"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("account with a space err=%v", err)
	}
	if account, err := AccountName("production", "api.example.com:443"); err != nil || account != "production@api.example.com:443" {
		t.Fatalf("account=%q err=%v", account, err)
	}
}

func TestSecretServiceWritesOnStdinAndNeedsASessionBus(t *testing.T) {
	if ForPlatform("linux", (&fakeTool{}).run, found, noSessionBus).Name() != "none" {
		t.Fatal("linux without a session bus must not use the Secret Service")
	}
	if ForPlatform("linux", (&fakeTool{}).run, missing, sessionBus).Name() != "none" {
		t.Fatal("linux without secret-tool must not use the Secret Service")
	}
	tool := &fakeTool{}
	store := ForPlatform("linux", tool.run, found, sessionBus)
	if err := store.Set("default@host", secret); err != nil {
		t.Fatal(err)
	}
	if tool.calls[0].stdin != secret || strings.Contains(strings.Join(tool.calls[0].args, " "), secret) {
		t.Fatalf("store call=%#v", tool.calls[0])
	}
	if got, err := store.Get("default@host"); err != nil || got != secret {
		t.Fatalf("get=%q err=%v", got, err)
	}
	if err := store.Delete("default@host"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("default@host"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err=%v", err)
	}
}

func TestUnsupportedPlatformsFallBack(t *testing.T) {
	store := ForPlatform("windows", (&fakeTool{}).run, found, sessionBus)
	if err := store.Set("default@host", secret); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("windows set err=%v", err)
	}
	t.Setenv("INFERCRANE_CREDENTIAL_STORE", "file")
	if err := Default().Set("default@host", secret); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("file mode set err=%v", err)
	}
	if Required() {
		t.Fatal("file mode must not require a keychain")
	}
	t.Setenv("INFERCRANE_CREDENTIAL_STORE", "keychain")
	if !Required() {
		t.Fatal("keychain mode must require a keychain")
	}
}
