package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// runPodSecret is the subset of RunPod REST v2's account secret that
// InferCrane reads. The value is write-only and never returned.
type runPodSecret struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// runPodModelSecretName is the deterministic RunPod secret that holds one
// workspace credential in the account that runs the Pod. It never names the
// workspace or the credential in clear.
func runPodModelSecretName(tenant, referenceID string) string {
	sum := sha256.Sum256([]byte("infercrane.runpod-model-secret.v1\x00" + tenant + "\x00" + referenceID))
	return "infercrane_model_" + hex.EncodeToString(sum[:10])
}

// ensureModelSecret stores the workspace's sealed model credential as a RunPod
// secret in the account whose key this adapter holds: the tenant's own account
// for a tenant compute connection. The Pod then reads it only through the
// {{ RUNPOD_SECRET_<name> }} placeholder; the value never appears in the Pod
// specification or in RunPod's Pod API responses.
func (r RunPodPods) ensureModelSecret(ctx context.Context, name string, credential *ModelCredential) error {
	if credential.Open == nil {
		return fmt.Errorf("%w: the model credential cannot be opened for RunPod", ErrInvalidReplicaSpec)
	}
	value, err := credential.Open(ctx)
	if err != nil || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: the model credential could not be read", ErrInvalidReplicaSpec)
	}
	existing, err := r.accountSecret(ctx, name)
	if err != nil {
		return err
	}
	if existing.ID == "" {
		createErr := r.do(ctx, http.MethodPost, "/account/secrets", map[string]string{"name": name, "value": value, "description": "InferCrane model credential. InferCrane keeps its value current."}, nil)
		if createErr == nil {
			return nil
		}
		// A lost create response or a concurrent replica may have stored it.
		if existing, err = r.accountSecret(ctx, name); err != nil || existing.ID == "" {
			if errors.Is(createErr, ErrProviderAuthorization) {
				return createErr
			}
			return fmt.Errorf("%w: RunPod did not store the model credential secret", ErrRequestFailed)
		}
	}
	// Patch so a replaced workspace secret reaches every new Pod.
	if err = r.do(ctx, http.MethodPatch, "/account/secrets/"+url.PathEscape(existing.ID), map[string]string{"value": value}, nil); err != nil {
		if errors.Is(err, ErrProviderAuthorization) {
			return err
		}
		return fmt.Errorf("%w: RunPod did not update the model credential secret", ErrRequestFailed)
	}
	return nil
}

func (r RunPodPods) accountSecret(ctx context.Context, name string) (runPodSecret, error) {
	var listed struct {
		Secrets []runPodSecret `json:"secrets"`
	}
	if err := r.do(ctx, http.MethodGet, "/account/secrets", nil, &listed); err != nil {
		return runPodSecret{}, err
	}
	for _, secret := range listed.Secrets {
		if secret.Name == name {
			return secret, nil
		}
	}
	return runPodSecret{}, nil
}
