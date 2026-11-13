package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/keychain"
)

type memoryKeychain struct {
	mu        sync.Mutex
	items     map[string]string
	available bool
}

func (m *memoryKeychain) Name() string { return "test keychain" }
func (m *memoryKeychain) Get(account string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.available {
		return "", keychain.ErrUnavailable
	}
	value, ok := m.items[account]
	if !ok {
		return "", keychain.ErrNotFound
	}
	return value, nil
}
func (m *memoryKeychain) Set(account, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.available {
		return keychain.ErrUnavailable
	}
	m.items[account] = secret
	return nil
}
func (m *memoryKeychain) Delete(account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[account]; !ok {
		return keychain.ErrNotFound
	}
	delete(m.items, account)
	return nil
}

func useKeychain(t *testing.T, available bool) *memoryKeychain {
	t.Helper()
	fake := &memoryKeychain{items: map[string]string{}, available: available}
	previous := secretStore
	secretStore = func() keychain.Store { return fake }
	t.Cleanup(func() { secretStore = previous })
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_URL", "")
	t.Setenv("INFERCRANE_CONSOLE_URL", "")
	t.Setenv("INFERCRANE_CREDENTIAL_STORE", "")
	return fake
}

const loginKey = "ic_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestLoginKeyGoesToTheKeychainAndNeverToTheConfigFile(t *testing.T) {
	fake := useKeychain(t, true)
	stored, err := SaveClientLogin(ClientLogin{Context: "work", ControlURL: "http://127.0.0.1:8080", ConsoleURL: "http://127.0.0.1:3200/", APIKey: loginKey})
	if err != nil || stored.Store != "test keychain" || stored.KeychainError != "" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	body, err := os.ReadFile(stored.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), loginKey) || !strings.Contains(string(body), `"credential_store": "keychain"`) {
		t.Fatalf("config file=%s", body)
	}
	if fake.items["work@127.0.0.1:8080"] != loginKey {
		t.Fatalf("keychain=%v", fake.items)
	}
	cfg, err := LoadClient()
	if err != nil || cfg.APIKey != loginKey || cfg.ConsoleURL != "http://127.0.0.1:3200" || cfg.CredentialSource != "test keychain" || cfg.ClientContextName != "work" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	// The environment still overrides a stored login.
	t.Setenv("INFERCRANE_API_KEY", "ic_from_environment_0000000000")
	if cfg, err = LoadClient(); err != nil || cfg.APIKey != "ic_from_environment_0000000000" || cfg.CredentialSource != CredentialFromEnvironment {
		t.Fatalf("env cfg source=%q err=%v", cfg.CredentialSource, err)
	}
	t.Setenv("INFERCRANE_API_KEY", "")

	cleared, err := ClearClientCredential("")
	if err != nil || len(cleared.Removed) != 1 || cleared.Removed[0] != "test keychain" {
		t.Fatalf("cleared=%#v err=%v", cleared, err)
	}
	if len(fake.items) != 0 {
		t.Fatalf("keychain kept %v", fake.items)
	}
	if _, err = LoadClient(); err == nil || !strings.Contains(err.Error(), "infercrane login") {
		t.Fatalf("signed-out load err=%v", err)
	}
	target, err := LoadClientTarget("")
	if err != nil || target.ControlURL != "http://127.0.0.1:8080" || target.ConsoleURL != "http://127.0.0.1:3200" || target.APIKey != "" {
		t.Fatalf("target=%#v err=%v", target, err)
	}
}

func TestLoginFallsBackToAPrivateConfigFileWithoutAKeychain(t *testing.T) {
	useKeychain(t, false)
	stored, err := SaveClientLogin(ClientLogin{ControlURL: "http://127.0.0.1:8080", APIKey: loginKey})
	if err != nil || stored.Store != CredentialFromConfigFile || stored.KeychainError == "" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	info, err := os.Stat(stored.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	cfg, err := LoadClient()
	if err != nil || cfg.APIKey != loginKey || cfg.CredentialSource != CredentialFromConfigFile {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	cleared, err := ClearClientCredential("default")
	if err != nil || len(cleared.Removed) != 1 || cleared.Removed[0] != CredentialFromConfigFile {
		t.Fatalf("cleared=%#v err=%v", cleared, err)
	}
	body, _ := os.ReadFile(stored.Path)
	if strings.Contains(string(body), loginKey) {
		t.Fatalf("logout left the key in %s", body)
	}

	t.Setenv("INFERCRANE_CREDENTIAL_STORE", "keychain")
	if _, err = SaveClientLogin(ClientLogin{ControlURL: "http://127.0.0.1:8080", APIKey: loginKey}); err == nil {
		t.Fatal("INFERCRANE_CREDENTIAL_STORE=keychain must not fall back to the file")
	}
}

func TestInitWithAPastedKeyRemovesAnOlderKeychainLogin(t *testing.T) {
	fake := useKeychain(t, true)
	if _, err := SaveClientLogin(ClientLogin{ControlURL: "http://127.0.0.1:8080", ConsoleURL: "http://127.0.0.1:3200", APIKey: loginKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeClientContext("default", "http://127.0.0.1:8080", "ic_pasted_00000000000000000000", true); err != nil {
		t.Fatal(err)
	}
	if len(fake.items) != 0 {
		t.Fatalf("keychain kept a replaced key: %v", fake.items)
	}
	cfg, err := LoadClient()
	if err != nil || cfg.APIKey != "ic_pasted_00000000000000000000" || cfg.ConsoleURL != "http://127.0.0.1:3200" {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
}

func TestDashboardURLDefaultsOnlyForTheHostedProduct(t *testing.T) {
	t.Setenv("INFERCRANE_CONSOLE_URL", "")
	for _, test := range []struct{ control, configured, want string }{
		{"https://api.infercrane.com", "", HostedConsoleURL},
		{"https://infercrane.com", "", HostedConsoleURL},
		{"http://api.infercrane.com", "", ""},
		{"https://infercrane.com.evil.example", "", ""},
		{"http://127.0.0.1:8080", "", ""},
		{"https://infercrane.example", "", ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:3200/", "http://127.0.0.1:3200"},
	} {
		got, err := ResolveConsoleURL(test.control, test.configured)
		if err != nil || got != test.want {
			t.Fatalf("ResolveConsoleURL(%q,%q)=%q,%v want %q", test.control, test.configured, got, err, test.want)
		}
	}
	t.Setenv("INFERCRANE_CONSOLE_URL", "https://dashboard.example/")
	if got, _ := ResolveConsoleURL("http://127.0.0.1:8080", "http://127.0.0.1:3200"); got != "https://dashboard.example" {
		t.Fatalf("environment override=%q", got)
	}
	t.Setenv("INFERCRANE_CONSOLE_URL", "https://user:pass@dashboard.example")
	if _, err := ResolveConsoleURL("http://127.0.0.1:8080", ""); err == nil {
		t.Fatal("console URL with credentials was accepted")
	}
}

func TestServerConsoleURLIsValidated(t *testing.T) {
	t.Setenv("INFERCRANE_DATABASE_URL", "postgres://localhost/infercrane")
	t.Setenv("INFERCRANE_API_KEY", "development-key")
	t.Setenv("INFERCRANE_CONSOLE_URL", "http://127.0.0.1:3200/")
	cfg, err := Load()
	if err != nil || cfg.ConsoleURL != "http://127.0.0.1:3200" {
		t.Fatalf("console=%q err=%v", cfg.ConsoleURL, err)
	}
	t.Setenv("INFERCRANE_CONSOLE_URL", "javascript:alert(1)")
	if _, err = Load(); err == nil {
		t.Fatal("invalid console URL was accepted")
	}
}
