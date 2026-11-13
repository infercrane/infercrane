package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/infercrane/infercrane/internal/keychain"
)

const (
	// CredentialStoreKeychain marks a context whose key is in the OS keychain.
	CredentialStoreKeychain = "keychain"
	// CredentialFromEnvironment and CredentialFromConfigFile describe where
	// the CLI found its key. They are shown by `auth status`.
	CredentialFromEnvironment = "INFERCRANE_API_KEY"
	CredentialFromConfigFile  = "config file"
	// HostedConsoleURL is the InferCrane dashboard for the hosted product.
	HostedConsoleURL = "https://console.infercrane.com"
)

// secretStore is replaced in tests so they never touch a real keychain.
var secretStore = keychain.Default

// NormalizeConsoleURL validates a dashboard URL and drops a trailing slash.
func NormalizeConsoleURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("INFERCRANE_CONSOLE_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// ResolveConsoleURL picks the dashboard for a control plane:
// INFERCRANE_CONSOLE_URL, then the context's saved console URL, then the
// hosted dashboard when the control plane is the hosted product. Local and
// self-hosted control planes have no dashboard until one is configured, and
// the empty result means no links are printed.
func ResolveConsoleURL(controlURL, configured string) (string, error) {
	if value := strings.TrimSpace(os.Getenv("INFERCRANE_CONSOLE_URL")); value != "" {
		return NormalizeConsoleURL(value)
	}
	if strings.TrimSpace(configured) != "" {
		return NormalizeConsoleURL(configured)
	}
	if parsed, err := url.Parse(controlURL); err == nil && parsed.Scheme == "https" {
		host := strings.ToLower(parsed.Hostname())
		if host == "infercrane.com" || strings.HasSuffix(host, ".infercrane.com") {
			return HostedConsoleURL, nil
		}
	}
	return "", nil
}

// ClientLogin is a credential issued by `infercrane login`.
type ClientLogin struct {
	Context, ControlURL, ConsoleURL, APIKey string
}

// StoredLogin says where a login was saved. It never contains the key.
type StoredLogin struct {
	Path, Context string
	// Store is the keychain name, or CredentialFromConfigFile.
	Store string
	// KeychainError explains why the keychain was not used, when it was not.
	KeychainError string
}

// SaveClientLogin stores the key in the OS keychain when one is available and
// falls back to the 0600 config file otherwise. The context is selected.
func SaveClientLogin(login ClientLogin) (StoredLogin, error) {
	if login.Context == "" {
		login.Context = "default"
	}
	if login.APIKey == "" {
		return StoredLogin{}, errors.New("a credential is required")
	}
	parsed, err := url.Parse(login.ControlURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return StoredLogin{}, fmt.Errorf("InferCrane URL must be absolute HTTP(S) without credentials, query, or fragment")
	}
	if login.ConsoleURL != "" {
		if login.ConsoleURL, err = NormalizeConsoleURL(login.ConsoleURL); err != nil {
			return StoredLogin{}, err
		}
	}
	stored, err := readClientFile()
	if err != nil {
		return StoredLogin{}, err
	}
	contexts := upgradedContexts(stored)
	previous := contexts[login.Context]
	next := ClientContext{URL: login.ControlURL, ConsoleURL: login.ConsoleURL}
	result := StoredLogin{Context: login.Context, Store: CredentialFromConfigFile}

	secrets := secretStore()
	account, accountErr := keychain.AccountName(login.Context, parsed.Host)
	if accountErr == nil {
		accountErr = secrets.Set(account, login.APIKey)
	}
	if accountErr == nil {
		next.CredentialStore, next.KeychainAccount = CredentialStoreKeychain, account
		result.Store = secrets.Name()
	} else {
		if keychain.Required() {
			return StoredLogin{}, fmt.Errorf("INFERCRANE_CREDENTIAL_STORE=keychain, but the key could not be stored: %w", accountErr)
		}
		next.APIKey = login.APIKey
		result.KeychainError = accountErr.Error()
	}
	if previous.CredentialStore == CredentialStoreKeychain && previous.KeychainAccount != "" && previous.KeychainAccount != next.KeychainAccount {
		_ = secrets.Delete(previous.KeychainAccount)
	}
	contexts[login.Context] = next
	path, err := writeClientFile(clientFile{Current: login.Context, Contexts: contexts})
	if err != nil {
		if next.KeychainAccount != "" {
			_ = secrets.Delete(next.KeychainAccount)
		}
		return StoredLogin{}, err
	}
	result.Path = path
	return result, nil
}

// ClearedLogin reports what `infercrane logout` removed locally.
type ClearedLogin struct {
	Path, Context string
	// Removed lists where a key was deleted: the keychain name and/or the
	// config file.
	Removed []string
}

// ClearClientCredential removes a context's key from the keychain and the
// config file. The context keeps its control-plane and dashboard URLs so the
// next `infercrane login` reuses them.
func ClearClientCredential(contextName string) (ClearedLogin, error) {
	stored, err := readClientFile()
	if err != nil {
		return ClearedLogin{}, err
	}
	if contextName == "" {
		contextName = stored.Current
	}
	if contextName == "" {
		contextName = "default"
	}
	contexts := upgradedContexts(stored)
	selected, ok := contexts[contextName]
	result := ClearedLogin{Context: contextName}
	if !ok {
		return result, nil
	}
	if selected.CredentialStore == CredentialStoreKeychain && selected.KeychainAccount != "" {
		secrets := secretStore()
		if deleteErr := secrets.Delete(selected.KeychainAccount); deleteErr == nil {
			result.Removed = append(result.Removed, secrets.Name())
		} else if !errors.Is(deleteErr, keychain.ErrNotFound) {
			return ClearedLogin{}, fmt.Errorf("remove the InferCrane credential from the %s: %w", secrets.Name(), deleteErr)
		}
	}
	if selected.APIKey != "" {
		result.Removed = append(result.Removed, CredentialFromConfigFile)
	}
	selected.APIKey, selected.CredentialStore, selected.KeychainAccount = "", "", ""
	contexts[contextName] = selected
	current := stored.Current
	if current == "" {
		current = contextName
	}
	if result.Path, err = writeClientFile(clientFile{Current: current, Contexts: contexts}); err != nil {
		return ClearedLogin{}, err
	}
	return result, nil
}

func upgradedContexts(stored clientFile) map[string]ClientContext {
	contexts := make(map[string]ClientContext, len(stored.Contexts)+1)
	for name, item := range stored.Contexts {
		contexts[name] = item
	}
	if stored.Contexts == nil && stored.URL != "" {
		contexts["default"] = ClientContext{URL: stored.URL, APIKey: stored.APIKey}
	}
	return contexts
}

func writeClientFile(file clientFile) (string, error) {
	path, err := clientConfigPath()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	encoded, _ := json.MarshalIndent(file, "", "  ")
	encoded = append(encoded, '\n')
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		return "", fmt.Errorf("write client config: %w", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("secure client config: %w", err)
	}
	return path, nil
}
