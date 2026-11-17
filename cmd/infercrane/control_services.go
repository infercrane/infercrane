package main

import (
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

// newUpstreamCredentialSealer returns nil when credential encryption is not
// configured: then `infercrane connect --token-env` is refused and the agent,
// which keeps the credential on the server's machine, is the path.
func newUpstreamCredentialSealer(cfg config.Config, store upstreamcredential.Store) (*upstreamcredential.Sealer, error) {
	if cfg.CredentialEncryptionKey == "" {
		return nil, nil
	}
	cipher, err := secretcipher.New(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("configure server credential encryption: %w", err)
	}
	return &upstreamcredential.Sealer{Cipher: cipher, Store: store}, nil
}

// bindUpstreamCredentials wires the sealer only when it exists, for the same
// typed-nil reason as bindOptionalControlServices.
func bindUpstreamCredentials(api *controlapi.API, reconciler *reconcile.Reconciler, sealer *upstreamcredential.Sealer) {
	if sealer == nil {
		return
	}
	api.UpstreamCredentials = sealer
	reconciler.UpstreamCredentials = sealer
}
