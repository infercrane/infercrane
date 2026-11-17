package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/infercrane/infercrane/internal/keychain"
)

// AgentCredential is an InferCrane agent key saved on this machine for one
// CLI context and control plane. The key itself lives in the OS keychain
// when one is available and otherwise in a separate 0600 file, never in the
// CLI's own config, so signing in again does not discard it.
type AgentCredential struct {
	AgentID, Name, Key string
	// Store is the keychain name, or CredentialFromConfigFile.
	Store, Path string
}

type agentCredentialFile struct {
	Agents map[string]agentCredentialEntry `json:"agents"`
	// HashKeys hold, per context and control plane, the secret that keys
	// the agent's request hashes. It never leaves this machine.
	HashKeys map[string]string `json:"hash_keys,omitempty"`
}

type agentCredentialEntry struct {
	AgentID         string `json:"agent_id"`
	Name            string `json:"name"`
	CredentialStore string `json:"credential_store,omitempty"`
	KeychainAccount string `json:"keychain_account,omitempty"`
	Key             string `json:"key,omitempty"`
}

func agentCredentialPath() (string, error) {
	path, err := clientConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "agent-credentials.json"), nil
}

func agentCredentialSlot(contextName, controlURL string) (string, error) {
	parsed, err := url.Parse(controlURL)
	if err != nil || parsed.Host == "" {
		return "", errors.New("a control-plane URL is required")
	}
	if contextName == "" {
		contextName = "default"
	}
	return keychain.AccountName(contextName, parsed.Host)
}

func readAgentCredentialFile() (agentCredentialFile, string, error) {
	path, err := agentCredentialPath()
	if err != nil {
		return agentCredentialFile{}, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return agentCredentialFile{Agents: map[string]agentCredentialEntry{}}, path, nil
	}
	if err != nil {
		return agentCredentialFile{}, path, fmt.Errorf("read agent credentials: %w", err)
	}
	var stored agentCredentialFile
	if err = json.Unmarshal(data, &stored); err != nil {
		return agentCredentialFile{}, path, fmt.Errorf("parse agent credentials: %w", err)
	}
	if stored.Agents == nil {
		stored.Agents = map[string]agentCredentialEntry{}
	}
	return stored, path, nil
}

func writeAgentCredentialFile(path string, file agentCredentialFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	encoded, _ := json.MarshalIndent(file, "", "  ")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write agent credentials: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// SaveAgentCredential stores an agent key for a context and control plane.
func SaveAgentCredential(contextName, controlURL string, credential AgentCredential) (AgentCredential, error) {
	if credential.Key == "" || credential.AgentID == "" {
		return AgentCredential{}, errors.New("an agent ID and key are required")
	}
	slot, err := agentCredentialSlot(contextName, controlURL)
	if err != nil {
		return AgentCredential{}, err
	}
	stored, path, err := readAgentCredentialFile()
	if err != nil {
		return AgentCredential{}, err
	}
	previous := stored.Agents[slot]
	entry := agentCredentialEntry{AgentID: credential.AgentID, Name: credential.Name}
	secrets := secretStore()
	account := "agent:" + slot
	result := AgentCredential{AgentID: credential.AgentID, Name: credential.Name, Key: credential.Key, Store: CredentialFromConfigFile, Path: path}
	if setErr := secrets.Set(account, credential.Key); setErr == nil {
		entry.CredentialStore, entry.KeychainAccount = CredentialStoreKeychain, account
		result.Store = secrets.Name()
	} else {
		if keychain.Required() {
			return AgentCredential{}, fmt.Errorf("INFERCRANE_CREDENTIAL_STORE=keychain, but the agent key could not be stored: %w", setErr)
		}
		entry.Key = credential.Key
	}
	if previous.KeychainAccount != "" && previous.KeychainAccount != entry.KeychainAccount {
		_ = secrets.Delete(previous.KeychainAccount)
	}
	stored.Agents[slot] = entry
	if err = writeAgentCredentialFile(path, stored); err != nil {
		if entry.KeychainAccount != "" {
			_ = secrets.Delete(entry.KeychainAccount)
		}
		return AgentCredential{}, err
	}
	return result, nil
}

// LoadAgentCredential returns the saved agent key, or ok=false when none is
// saved for this context and control plane.
func LoadAgentCredential(contextName, controlURL string) (AgentCredential, bool, error) {
	slot, err := agentCredentialSlot(contextName, controlURL)
	if err != nil {
		return AgentCredential{}, false, err
	}
	stored, path, err := readAgentCredentialFile()
	if err != nil {
		return AgentCredential{}, false, err
	}
	entry, ok := stored.Agents[slot]
	if !ok {
		return AgentCredential{}, false, nil
	}
	out := AgentCredential{AgentID: entry.AgentID, Name: entry.Name, Key: entry.Key, Store: CredentialFromConfigFile, Path: path}
	if entry.CredentialStore == CredentialStoreKeychain {
		secrets := secretStore()
		key, getErr := secrets.Get(entry.KeychainAccount)
		if getErr != nil {
			return AgentCredential{}, false, fmt.Errorf("read the agent key from the %s: %w", secrets.Name(), getErr)
		}
		out.Key, out.Store = key, secrets.Name()
	}
	return out, out.Key != "", nil
}

// ClearAgentCredential forgets the saved agent key for a context.
func ClearAgentCredential(contextName, controlURL string) error {
	slot, err := agentCredentialSlot(contextName, controlURL)
	if err != nil {
		return err
	}
	stored, path, err := readAgentCredentialFile()
	if err != nil {
		return err
	}
	entry, ok := stored.Agents[slot]
	if !ok {
		return nil
	}
	if entry.KeychainAccount != "" {
		_ = secretStore().Delete(entry.KeychainAccount)
	}
	delete(stored.Agents, slot)
	return writeAgentCredentialFile(path, stored)
}

// LoadOrCreateAgentHashKey returns this machine's secret for keying request
// hashes, creating it on first use. It is independent of the agent key, so
// neither the control plane nor anything that sees the agent key can derive
// it, and rotating the agent key keeps hashes comparable.
func LoadOrCreateAgentHashKey(contextName, controlURL string) ([]byte, error) {
	slot, err := agentCredentialSlot(contextName, controlURL)
	if err != nil {
		return nil, err
	}
	stored, path, err := readAgentCredentialFile()
	if err != nil {
		return nil, err
	}
	if encoded, ok := stored.HashKeys[slot]; ok {
		if key, decodeErr := hex.DecodeString(encoded); decodeErr == nil && len(key) == 32 {
			return key, nil
		}
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	if stored.HashKeys == nil {
		stored.HashKeys = map[string]string{}
	}
	stored.HashKeys[slot] = hex.EncodeToString(key)
	if err = writeAgentCredentialFile(path, stored); err != nil {
		return nil, err
	}
	return key, nil
}
