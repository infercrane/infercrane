// Package passport creates and verifies canonical signed release evidence.
package passport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
)

const Algorithm = "Ed25519-SHA256"

type Payload struct {
	Schema          string                        `json:"schema"`
	Deployment      string                        `json:"deployment"`
	RevisionID      string                        `json:"revision_id"`
	RevisionNumber  int                           `json:"revision_number"`
	RevisionSpec    domain.DeploymentRevisionSpec `json:"revision_spec"`
	Artifact        *Artifact                     `json:"model_artifact,omitempty"`
	Benchmarks      []Benchmark                   `json:"benchmarks"`
	ColdStart       domain.ColdStartStats         `json:"cold_start"`
	GuardEvaluation *GuardEvidence                `json:"release_guard,omitempty"`
	Qualification   QualificationEvidence         `json:"qualification"`
	MissingEvidence []string                      `json:"missing_evidence"`
	Reproduce       Reproduction                  `json:"reproduction"`
	IssuedAt        time.Time                     `json:"issued_at"`
}

func FinalizeEvidence(payload *Payload) {
	missing := make([]string, 0, 6)
	if payload.Artifact == nil || payload.Artifact.ImmutableRevision == "" || payload.Artifact.ModelIdentity == "" {
		missing = append(missing, "immutable_model_artifact")
	}
	if len(payload.Benchmarks) == 0 {
		missing = append(missing, "revision_benchmark")
	}
	if payload.GuardEvaluation == nil {
		missing = append(missing, "release_guard_evaluation")
	}
	if len(payload.Qualification.Runtimes) == 0 {
		missing = append(missing, "runtime_qualification")
	}
	if payload.RevisionSpec.ComputeMode != "existing" && len(payload.Qualification.Providers) == 0 {
		missing = append(missing, "provider_qualification")
	}
	if payload.RevisionSpec.ComputeMode != "existing" && len(payload.Qualification.Compatibility) == 0 {
		missing = append(missing, "runtime_provider_compatibility")
	}
	payload.MissingEvidence = missing
}

type QualificationEvidence struct {
	ProviderContract string                             `json:"provider_contract"`
	RuntimeContract  string                             `json:"runtime_contract"`
	Providers        []integration.ProviderProfile      `json:"providers"`
	Runtimes         []integration.RuntimeProfile       `json:"runtimes"`
	Compatibility    []integration.RuntimeCompatibility `json:"compatibility"`
}

func SelectQualification(snapshot integration.Snapshot, spec domain.DeploymentRevisionSpec) QualificationEvidence {
	out := QualificationEvidence{ProviderContract: snapshot.ProviderContract, RuntimeContract: snapshot.RuntimeContract, Providers: []integration.ProviderProfile{}, Runtimes: []integration.RuntimeProfile{}, Compatibility: []integration.RuntimeCompatibility{}}
	selectedAdapter := spec.ProviderAdapter
	if selectedAdapter == "" {
		for _, entry := range snapshot.Compatibility {
			if entry.Runtime != spec.Runtime || entry.Cloud != spec.Cloud || string(entry.Mode) != spec.ComputeMode {
				continue
			}
			if selectedAdapter == "" {
				selectedAdapter = entry.Adapter
			} else if selectedAdapter != entry.Adapter {
				// Composition is ambiguous without an explicit provider adapter;
				// do not attach unrelated provider evidence to the passport.
				selectedAdapter = ""
				break
			}
		}
	}
	for _, profile := range snapshot.Providers {
		modeMatch := false
		for _, mode := range profile.Modes {
			if string(mode) == spec.ComputeMode {
				modeMatch = true
				break
			}
		}
		if (profile.Cloud == spec.Cloud || profile.Adapter == spec.Cloud) && modeMatch && (selectedAdapter == "" || profile.Adapter == selectedAdapter) {
			out.Providers = append(out.Providers, profile)
		}
	}
	for _, profile := range snapshot.Runtimes {
		if profile.Runtime == spec.Runtime {
			out.Runtimes = append(out.Runtimes, profile)
		}
	}
	for _, entry := range snapshot.Compatibility {
		if entry.Runtime == spec.Runtime && entry.Cloud == spec.Cloud && string(entry.Mode) == spec.ComputeMode && (selectedAdapter == "" || entry.Adapter == selectedAdapter) {
			out.Compatibility = append(out.Compatibility, entry)
		}
	}
	return out
}

type Artifact struct {
	ID                   string `json:"id"`
	Source               string `json:"source"`
	Repository           string `json:"repository"`
	ImmutableRevision    string `json:"immutable_revision"`
	ModelIdentity        string `json:"model_identity"`
	CacheState           string `json:"cache_state"`
	ApproximateSizeBytes *int64 `json:"approximate_size_bytes,omitempty"`
}

type Benchmark struct {
	ID                    string          `json:"id"`
	Tool                  string          `json:"tool"`
	ToolVersion           string          `json:"tool_version"`
	Runtime               string          `json:"runtime"`
	RuntimeVersion        string          `json:"runtime_version"`
	Provider              string          `json:"provider"`
	Region                string          `json:"region"`
	GPU                   string          `json:"gpu"`
	GPUCount              int             `json:"gpu_count"`
	ComputeMode           string          `json:"compute_mode"`
	Workload              json.RawMessage `json:"workload"`
	ReproductionCommand   string          `json:"reproduction_command"`
	RequestCount          int             `json:"request_count"`
	Succeeded             int             `json:"succeeded"`
	Failed                int             `json:"failed"`
	TTFTP95MS             *float64        `json:"ttft_p95_ms,omitempty"`
	LatencyP95MS          *float64        `json:"latency_p95_ms,omitempty"`
	OutputTokenThroughput *float64        `json:"output_token_throughput,omitempty"`
	CostMetadata          json.RawMessage `json:"cost_metadata"`
	CreatedAt             time.Time       `json:"created_at"`
}

type GuardEvidence struct {
	ID                  string          `json:"id"`
	ActiveRevisionID    string          `json:"active_revision_id"`
	CandidateRevisionID string          `json:"candidate_revision_id"`
	Decision            string          `json:"decision"`
	Reasons             json.RawMessage `json:"reasons"`
	Metrics             json.RawMessage `json:"metrics"`
	Policy              json.RawMessage `json:"policy"`
	CreatedAt           time.Time       `json:"created_at"`
}

type Reproduction struct {
	DeploymentSpec    domain.DeploymentRevisionSpec `json:"deployment_spec"`
	BenchmarkCommands []string                      `json:"benchmark_commands"`
}

type Envelope struct {
	PayloadJSON string `json:"payload"`
	Digest      string `json:"digest"`
	Signature   string `json:"signature"`
	PublicKey   string `json:"public_key"`
	Algorithm   string `json:"algorithm"`
	KeyID       string `json:"key_id"`
}

func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

func DecodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode passport private key: %w", err)
	}
	if len(raw) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(raw), nil
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("passport private key must be an Ed25519 seed or private key")
	}
	return ed25519.PrivateKey(raw), nil
}

func EncodePrivateKey(key ed25519.PrivateKey) string { return base64.StdEncoding.EncodeToString(key) }

func Sign(payload any, privateKey ed25519.PrivateKey) (Envelope, error) {
	canonical, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode passport payload: %w", err)
	}
	digest := sha256.Sum256(canonical)
	signed, err := SignDigest("sha256:"+hex.EncodeToString(digest[:]), privateKey)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{PayloadJSON: string(canonical), Digest: signed.Digest, Signature: signed.Signature, PublicKey: signed.PublicKey, Algorithm: signed.Algorithm, KeyID: signed.KeyID}, nil
}

func Verify(envelope Envelope) error {
	if envelope.Algorithm != Algorithm {
		return fmt.Errorf("unsupported passport algorithm %q", envelope.Algorithm)
	}
	canonical, err := compactPayload([]byte(envelope.PayloadJSON))
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if envelope.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
		return errors.New("passport payload digest mismatch")
	}
	if err = VerifyDigest(DigestSignature{Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, PublicKey: envelope.PublicKey, Digest: envelope.Digest, Signature: envelope.Signature}); err != nil {
		return fmt.Errorf("passport %w", err)
	}
	return nil
}

// DigestSignature is an Ed25519 signature over the 32 bytes of a SHA-256
// digest, with the public key that made it and that key's ID. Inference
// Passports, quality evidence, and optimization reports share it, so one key
// ID names the control plane's signing key everywhere.
type DigestSignature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Digest    string `json:"digest"`
	Signature string `json:"signature"`
}

// KeyID names a public key: "sha256:" and the first 8 bytes of its SHA-256.
func KeyID(publicKey ed25519.PublicKey) string {
	sum := sha256.Sum256(publicKey)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// SignDigest signs a "sha256:<64 hex>" digest. Ed25519 is deterministic, so
// the same digest and key always give the same signature.
func SignDigest(digest string, privateKey ed25519.PrivateKey) (DigestSignature, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return DigestSignature{}, errors.New("invalid Ed25519 private key")
	}
	raw, err := digestBytes(digest)
	if err != nil {
		return DigestSignature{}, err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return DigestSignature{Algorithm: Algorithm, KeyID: KeyID(publicKey), PublicKey: base64.StdEncoding.EncodeToString(publicKey), Digest: digest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, raw))}, nil
}

// VerifyDigest checks the algorithm, the Ed25519 signature over the digest,
// and that the key ID names the public key. It does not say whose key it is:
// a caller that trusts one key compares PublicKey with it.
func VerifyDigest(signed DigestSignature) error {
	if signed.Algorithm != Algorithm {
		return fmt.Errorf("unsupported signature algorithm %q", signed.Algorithm)
	}
	raw, err := digestBytes(signed.Digest)
	if err != nil {
		return err
	}
	publicKey, err := DecodePublicKey(signed.PublicKey)
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid Ed25519 signature")
	}
	if !ed25519.Verify(publicKey, raw, signature) {
		return errors.New("signature verification failed")
	}
	if signed.KeyID != KeyID(publicKey) {
		return errors.New("key ID does not match the public key")
	}
	return nil
}

// DecodePublicKey reads a base64 Ed25519 public key, the form every signature
// envelope carries.
func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key: want 32 bytes in base64")
	}
	return ed25519.PublicKey(raw), nil
}

func digestBytes(digest string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(digest, "sha256:")
	raw, err := hex.DecodeString(encoded)
	if !ok || err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != encoded {
		return nil, fmt.Errorf("invalid SHA-256 digest %q", digest)
	}
	return raw, nil
}

// compactPayload makes signed payload verification insensitive to JSON
// presentation whitespace. The API signs compact json.Marshal output, while
// clients may legitimately indent an envelope when saving it to disk. This is
// deliberately narrower than semantic JSON canonicalization: key order and
// number representation remain part of the signed evidence.
func compactPayload(payload []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return nil, fmt.Errorf("invalid passport payload JSON: %w", err)
	}
	return compact.Bytes(), nil
}
