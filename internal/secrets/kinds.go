package secrets

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
)

// Resolver names. env reads the control plane's environment and is reserved
// for the operator's workspaces. sealed holds a workspace's own value,
// encrypted at rest. kubernetes and aws-secrets-manager name a secret that
// stays in the customer's own cluster or AWS account: workers read it there and
// the control plane never sees the value.
const (
	ResolverEnvironment       = "env"
	ResolverSealed            = "sealed"
	ResolverKubernetes        = "kubernetes"
	ResolverAWSSecretsManager = "aws-secrets-manager"
)

// MaxSealedValueBytes bounds one sealed value. Model hub tokens and object
// store keys are far smaller.
const MaxSealedValueBytes = 8192

// ErrWorkerHeld means the value lives in the customer's own secret store. Only
// a worker running there reads it; the control plane never does.
var ErrWorkerHeld = errors.New("this secret stays in your own secret store and is read only by your workers")

// ErrSealedUnavailable means no credential encryption key is configured, so
// nothing can be sealed or opened.
var ErrSealedUnavailable = errors.New("sealed secrets need INFERCRANE_CREDENTIAL_ENCRYPTION_KEY on the control plane")

// ErrSealedUnreadable means a sealed value is stored but cannot be opened
// with the configured key. Callers fail closed.
var ErrSealedUnreadable = errors.New("the sealed secret cannot be opened with this control plane's key")

var (
	kubernetesSecretName = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	kubernetesSecretKey  = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
	awsSecretARN         = regexp.MustCompile(`^arn:aws(?:-[a-z]+)*:secretsmanager:[a-z]{2}(?:-[a-z]+)+-\d:\d{12}:secret:[A-Za-z0-9/_+=.@-]{1,512}$`)
)

// KubernetesSecretKey splits a kubernetes reference into the Secret name and
// key a workload's secretKeyRef names.
func KubernetesSecretKey(reference string) (string, string, bool) {
	name, key, found := strings.Cut(reference, "/")
	if !found || !kubernetesSecretName.MatchString(name) || !kubernetesSecretKey.MatchString(key) || key == "." || key == ".." {
		return "", "", false
	}
	return name, key, true
}

// WorkerHeld reports whether only a worker can read the reference's value.
func WorkerHeld(resolver string) bool {
	return resolver == ResolverKubernetes || resolver == ResolverAWSSecretsManager
}

// ValidateSealedValue rejects an empty, oversized, or multi-line value without
// echoing it. Tokens and keys are single printable words.
func ValidateSealedValue(value string) error {
	if value == "" || len(value) > MaxSealedValueBytes {
		return errors.New("a sealed secret value must be 1 to 8192 bytes")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII {
			return errors.New("a sealed secret value must be one line of printable ASCII without spaces")
		}
	}
	return nil
}

// Sealed seals and opens workspace-owned values with the tenant credential
// key. The associated data binds a ciphertext to its workspace and name, so a
// row copied to another workspace or renamed does not open.
type Sealed struct {
	Cipher       *secretcipher.Cipher
	KeyReference string
}

func sealedAssociatedData(tenant, name string) []byte {
	return []byte("infercrane.secret-reference.v1\x00" + tenant + "\x00" + name)
}

// Available reports whether values can be sealed.
func (s Sealed) Available() bool { return s.Cipher != nil && s.KeyReference != "" }

// Seal encrypts one value for a workspace secret name.
func (s Sealed) Seal(tenant, name, value string) (domain.SealedCredential, string, error) {
	if !s.Available() {
		return domain.SealedCredential{}, "", ErrSealedUnavailable
	}
	if tenant == "" || name == "" {
		return domain.SealedCredential{}, "", errors.New("workspace and secret name are required")
	}
	if err := ValidateSealedValue(value); err != nil {
		return domain.SealedCredential{}, "", err
	}
	ciphertext, nonce, err := s.Cipher.Encrypt([]byte(value), sealedAssociatedData(tenant, name))
	if err != nil {
		return domain.SealedCredential{}, "", errors.New("the secret could not be sealed")
	}
	return domain.SealedCredential{Ciphertext: ciphertext, Nonce: nonce}, s.KeyReference, nil
}

// Resolve opens a sealed reference in memory.
func (s Sealed) Resolve(_ context.Context, reference domain.SecretReference) (string, error) {
	if reference.Resolver != ResolverSealed {
		return "", errors.New("secret reference is not sealed")
	}
	if !s.Available() {
		return "", ErrSealedUnavailable
	}
	if len(reference.Sealed.Ciphertext) == 0 || len(reference.Sealed.Nonce) == 0 || reference.KeyReference != s.KeyReference {
		return "", ErrSealedUnreadable
	}
	plaintext, err := s.Cipher.Decrypt(reference.Sealed.Ciphertext, reference.Sealed.Nonce, sealedAssociatedData(reference.TenantID, reference.Name))
	if err != nil {
		return "", ErrSealedUnreadable
	}
	return string(plaintext), nil
}

// Resolvers dispatches a reference to the resolver that owns its kind. A
// worker-held reference never resolves in the control plane.
type Resolvers struct {
	Environment Environment
	Sealed      Sealed
}

func (r Resolvers) Resolve(ctx context.Context, reference domain.SecretReference) (string, error) {
	switch reference.Resolver {
	case ResolverEnvironment:
		return r.Environment.Resolve(ctx, reference)
	case ResolverSealed:
		return r.Sealed.Resolve(ctx, reference)
	case ResolverKubernetes, ResolverAWSSecretsManager:
		return "", ErrWorkerHeld
	default:
		return "", errors.New("unsupported secret resolver")
	}
}

// AllowsTenant reports whether the workspace may create environment
// references.
func (r Resolvers) AllowsTenant(tenant string) bool { return r.Environment.AllowsTenant(tenant) }

// SealedSecrets exposes the sealer for the create path.
func (r Resolvers) SealedSecrets() Sealed { return r.Sealed }
