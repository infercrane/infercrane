package main

import (
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/alert"
	"github.com/infercrane/infercrane/internal/asyncinference"
	"github.com/infercrane/infercrane/internal/computeconnection"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/notification"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/reconcile"
	"github.com/infercrane/infercrane/internal/secretcipher"
	"github.com/infercrane/infercrane/internal/upstreamcredential"
)

// newComputeConnectionService returns nil when tenant compute credential
// storage is not configured, which disables self-serve BYOC.
func newComputeConnectionService(cfg config.Config, connections computeconnection.Store) (*computeconnection.Service, error) {
	if cfg.CredentialEncryptionKey == "" {
		return nil, nil
	}
	cipher, err := secretcipher.New(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("configure compute credential encryption: %w", err)
	}
	return &computeconnection.Service{
		Store: connections, Cipher: cipher, KeyReference: cfg.CredentialEncryptionKeyReference,
		Verifiers: map[string]computeconnection.CredentialVerifier{
			"runpod": provision.RunPodPods{BaseURL: cfg.RunPodRESTURL},
		},
	}, nil
}

// newNotificationService returns nil when credential encryption is not
// configured: destination URLs and signing secrets are sealed with the same
// key as tenant compute credentials.
func newNotificationService(cfg config.Config, store notification.Store) (*notification.Service, error) {
	if cfg.CredentialEncryptionKey == "" {
		return nil, nil
	}
	cipher, err := secretcipher.New(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("configure notification destination encryption: %w", err)
	}
	return &notification.Service{Store: store, Cipher: cipher, KeyReference: cfg.CredentialEncryptionKeyReference, Poster: alert.Deliverer{}, ConsoleURL: cfg.ConsoleURL}, nil
}

// bindOptionalControlServices assigns each optional service only when it
// exists. A nil pointer stored in an interface field is not == nil, so the
// handlers' "not configured" branches would never run and the first request
// would panic on the nil receiver instead of returning the disabled state.
func bindOptionalControlServices(api *controlapi.API, async *asyncinference.Service, connections *computeconnection.Service, notifications *notification.Service) {
	if async != nil {
		api.AsyncInference = async
	}
	if connections != nil {
		api.ComputeConnections = connections
	}
	if notifications != nil {
		api.Notifications = notifications
	}
}

// newUpstreamCredentialSealer returns the server-credential sealer. Without a
// tenant credential key it cannot seal, so `infercrane connect --token-env`
// is refused and the agent (which keeps the key on the server's machine) is
// the path; it still resolves, so a credential stored under an earlier key
// fails closed instead of being skipped.
func newUpstreamCredentialSealer(cfg config.Config, store upstreamcredential.Store) (*upstreamcredential.Sealer, error) {
	sealer := &upstreamcredential.Sealer{Store: store}
	if cfg.CredentialEncryptionKey == "" {
		return sealer, nil
	}
	cipher, err := secretcipher.New(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("configure server credential encryption: %w", err)
	}
	sealer.Cipher = &cipher
	return sealer, nil
}

// bindUpstreamCredentials gives the reconciler the resolver always, and the
// API the sealer only when it can seal, so the API's "not configured" branch
// runs (a typed nil in an interface field would not be nil).
func bindUpstreamCredentials(api *controlapi.API, reconciler *reconcile.Reconciler, sealer *upstreamcredential.Sealer) {
	reconciler.UpstreamCredentials = sealer
	if sealer.Cipher != nil {
		api.UpstreamCredentials = sealer
	}
}

// closedAPICredentials lets a migration's quality check ask the closed API
// with the credential sealed when it was connected, and fail closed when that
// credential cannot be opened.
type closedAPICredentials struct{ *upstreamcredential.Sealer }

func (c closedAPICredentials) Unreadable(err error) bool {
	return errors.Is(err, upstreamcredential.ErrUnreadable)
}
