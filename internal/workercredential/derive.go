package workercredential

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

const Version = "tenant-hmac-v1"

func Derive(master, tenant, deploymentID string) string {
	if master == "" || tenant == "" || deploymentID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte("infercrane.worker.v1\x00" + tenant + "\x00" + deploymentID))
	return "icw_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VersionOf reads the credential version a provider recorded for one worker.
// Details that are absent, malformed, or silent about the credential read as
// the empty version.
func VersionOf(providerDetails string) string {
	var evidence struct {
		WorkerCredentialVersion string `json:"worker_credential_version"`
	}
	if json.Unmarshal([]byte(providerDetails), &evidence) != nil {
		return ""
	}
	return evidence.WorkerCredentialVersion
}

// Source answers which bearer credential one worker expects. Anything that
// calls a worker directly asks it instead of holding one key for every worker,
// so a worker is never sent a credential it was not started with.
type Source struct {
	// Master derives the tenant-and-deployment-scoped credentials.
	Master string
	// Shared is the single credential of workers on operator-owned compute
	// that were not given a scoped one.
	Shared string
}

// Credential returns the credential of the worker described by its
// deployment and the details its provider recorded. It returns "" when no
// credential may be sent: a worker in a tenant's own account never receives
// the shared credential, and an unknown version is not guessed.
func (s Source) Credential(tenant, deploymentID, computeConnectionID, providerDetails string) string {
	switch version := VersionOf(providerDetails); {
	case version == Version:
		return Derive(s.Master, tenant, deploymentID)
	case version == "" && computeConnectionID == "":
		return s.Shared
	default:
		return ""
	}
}
