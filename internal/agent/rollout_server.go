package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/provision"
)

// ServerExecutor starts and stops the servers a guarded rollout runs on an
// agent's endpoint, and finds them again after the agent restarts.
// KubernetesExecutor implements it.
type ServerExecutor interface {
	StartServer(context.Context, string, agentprotocol.StartServerSpec, func(string)) (LaunchedServer, agentprotocol.ServerResult, error)
	StopServer(context.Context, agentprotocol.StopServerSpec) (agentprotocol.ServerResult, error)
	LaunchedServers(context.Context) ([]LaunchedServer, error)
}

// LaunchedServer is a server the agent started and keeps running for an
// endpoint. The agent serves it like a server it found, with the worker key
// the setup requires.
type LaunchedServer struct {
	Endpoint, BaseURL, Token string
}

// rolloutPrefix starts the external key of every server a rollout started:
// agent-serve:ENDPOINT:SETUP:PORT. Measured windows use agent-candidate-, so
// a rollout server is never mistaken for a measured setup or the reverse.
const rolloutPrefix = "agent-serve:"

func rolloutKey(endpoint string, setup agentprotocol.CandidateSetup) string {
	encoded, _ := json.Marshal(setup)
	sum := sha256.Sum256(encoded)
	return rolloutPrefix + endpoint + ":" + hex.EncodeToString(sum[:])[:16] + ":" + strconv.Itoa(setup.Workload.Port)
}

// inventory is the listing provision.Kubernetes offers.
type inventory interface {
	Inventory(context.Context, provision.InventoryFilter) ([]provision.Resource, error)
}

// serviceAddresser names a replica's address whether or not it is ready, as
// provision.Kubernetes does for a Deployment.
type serviceAddresser interface {
	ServiceAddress(provision.ProviderHandle, int) string
}

// StartServer starts the setup and keeps it running; only a failed start is
// removed. Starting the same setup again adopts the running one.
func (e KubernetesExecutor) StartServer(ctx context.Context, id string, spec agentprotocol.StartServerSpec, progress func(string)) (LaunchedServer, agentprotocol.ServerResult, error) {
	setup := spec.Setup
	result := agentprotocol.ServerResult{ImageDigest: setup.Workload.Image}
	if !ImageAllowed(e.AllowedImages, setup.Workload.Image) {
		return LaunchedServer{}, result, candidateFailure("image_not_allowed", "this agent's operator has not allowed image %s", setup.Workload.Image)
	}
	if len(e.AllowedAccelerators) > 0 && !contains(e.AllowedAccelerators, setup.Accelerator) {
		return LaunchedServer{}, result, candidateFailure("accelerator_not_allowed", "this agent's operator has not allowed accelerator %s", setup.Accelerator)
	}
	started, err := e.start(ctx, rolloutKey(spec.Endpoint, setup), setup, spec.TimeoutSecs, progress)
	if err != nil {
		var cleaned bool
		started.cleanup(ctx, &cleaned)
		return LaunchedServer{}, result, err
	}
	result.ServerKey, result.ReadySeconds = ServerKey(started.server.BaseURL), started.readySeconds
	result.Node = e.replicaNode(ctx, started.handle)
	return LaunchedServer{Endpoint: spec.Endpoint, BaseURL: started.server.BaseURL, Token: e.WorkerKey}, result, nil
}

// StopServer deletes the server a rollout started for the endpoint under
// that key, serving or not: a rollback stops a server because it stopped
// serving. A key that names no such server (the customer's own server, for
// one) deletes nothing and says so.
func (e KubernetesExecutor) StopServer(ctx context.Context, spec agentprotocol.StopServerSpec) (agentprotocol.ServerResult, error) {
	result := agentprotocol.ServerResult{ServerKey: spec.ServerKey}
	servers, err := e.rolloutServers(ctx, rolloutPrefix+spec.Endpoint+":", true)
	if err != nil {
		return result, err
	}
	for _, server := range servers {
		if ServerKey(server.launched.BaseURL) != spec.ServerKey {
			continue
		}
		if err = e.Provider.DeleteReplica(ctx, server.handle); err != nil {
			return result, err
		}
		result.Stopped = true
		return result, nil
	}
	return result, candidateFailure("server_not_started_here", "no server this agent started for %s has key %s", spec.Endpoint, spec.ServerKey)
}

// LaunchedServers lists the ready servers rollouts started, so a restarted
// agent serves them again.
func (e KubernetesExecutor) LaunchedServers(ctx context.Context) ([]LaunchedServer, error) {
	servers, err := e.rolloutServers(ctx, rolloutPrefix, false)
	if err != nil {
		return nil, err
	}
	out := make([]LaunchedServer, 0, len(servers))
	for _, server := range servers {
		out = append(out, server.launched)
	}
	return out, nil
}

type rolloutServer struct {
	launched LaunchedServer
	handle   provision.ProviderHandle
}

// rolloutServers lists the servers rollouts started under prefix: the ready
// ones, or with anyState also those that are not serving, named by the
// address their Service would answer on.
func (e KubernetesExecutor) rolloutServers(ctx context.Context, prefix string, anyState bool) ([]rolloutServer, error) {
	listing, ok := e.Provider.(inventory)
	if !ok {
		return nil, errors.New("the provider cannot list the servers this agent started")
	}
	resources, err := listing.Inventory(ctx, provision.InventoryFilter{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	var out []rolloutServer
	for _, resource := range resources {
		rest, found := strings.CutPrefix(resource.ExternalKey, rolloutPrefix)
		parts := strings.Split(rest, ":")
		if !found || len(parts) != 3 || !agentprotocol.ValidEndpointName(parts[0]) {
			continue
		}
		port, portErr := strconv.Atoi(parts[2])
		if portErr != nil || port < 1 || port > 65535 {
			continue
		}
		endpoint := parts[0]
		handle := e.Provider.Handle(resource.ExternalKey)
		observation, observeErr := e.Provider.ObserveReplica(ctx, handle, port)
		address := observation.Endpoint
		if observeErr != nil || !observation.Exists {
			continue
		}
		if observation.State != "ready" || address == "" {
			addresser, ok := e.Provider.(serviceAddresser)
			if !anyState || !ok {
				continue
			}
			if address = addresser.ServiceAddress(handle, port); address == "" {
				continue
			}
		}
		out = append(out, rolloutServer{launched: LaunchedServer{Endpoint: endpoint, BaseURL: address, Token: e.WorkerKey}, handle: handle})
	}
	return out, nil
}
