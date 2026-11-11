package main

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/asyncinference"
	"github.com/infercrane/infercrane/internal/computeconnection"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/secretcipher"
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

// bindOptionalControlServices assigns each optional service only when it
// exists. A nil pointer stored in an interface field is not == nil, so the
// handlers' "not configured" branches would never run and the first request
// would panic on the nil receiver instead of returning the disabled state.
func bindOptionalControlServices(api *controlapi.API, async *asyncinference.Service, connections *computeconnection.Service) {
	if async != nil {
		api.AsyncInference = async
	}
	if connections != nil {
		api.ComputeConnections = connections
	}
}
