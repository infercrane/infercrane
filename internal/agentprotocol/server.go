package agentprotocol

import (
	"errors"
	"math"
	"time"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// A guarded rollout on an agent's endpoint starts the proven setup as a
// server the agent owns, beside the customer's own server, and stops it again
// on a rollback. start_server keeps the setup running (a measured window
// always deletes it); the agent then serves it like any server it found,
// registered as a rollout server of the endpoint. stop_server deletes only a
// server the agent started; the customer's own server is never touched.

const (
	CommandStartServer = "start_server"
	CommandStopServer  = "stop_server"
)

// StartServerSpec asks the agent to start Setup and keep it running as a
// server of Endpoint.
type StartServerSpec struct {
	Endpoint    string         `json:"endpoint"`
	Setup       CandidateSetup `json:"setup"`
	TimeoutSecs int            `json:"timeout_seconds"`
}

// StopServerSpec asks the agent to delete a server it started for Endpoint.
type StopServerSpec struct {
	Endpoint  string `json:"endpoint"`
	ServerKey string `json:"server_key"`
}

// ServerResult is what starting or stopping a server reports: the server's
// key as the agent names it, how long it took to become ready, its image,
// the node digest, and whether it was stopped. No address is in it.
type ServerResult struct {
	ServerKey    string  `json:"server_key"`
	ReadySeconds float64 `json:"ready_seconds,omitempty"`
	ImageDigest  string  `json:"image_digest,omitempty"`
	Node         string  `json:"node,omitempty"`
	Stopped      bool    `json:"stopped,omitempty"`
}

// Validate bounds a start before it is queued and before the agent runs it.
func (s StartServerSpec) Validate() error {
	if !ValidEndpointName(s.Endpoint) {
		return errors.New("endpoint must be a lowercase endpoint name")
	}
	if err := s.Setup.Validate(); err != nil {
		return err
	}
	if s.TimeoutSecs < 60 || time.Duration(s.TimeoutSecs)*time.Second > MaxCandidateRunTimeout {
		return errors.New("timeout_seconds must be between 60 and 21600")
	}
	return nil
}

// Validate bounds a stop.
func (s StopServerSpec) Validate() error {
	if !ValidEndpointName(s.Endpoint) || !ValidServerKey(s.ServerKey) {
		return errors.New("a stop names the endpoint and the server key")
	}
	return nil
}

// Validate bounds a server result.
func (r ServerResult) Validate() error {
	if !ValidServerKey(r.ServerKey) {
		return errors.New("server_key is invalid")
	}
	if r.ReadySeconds < 0 || r.ReadySeconds > MaxCandidateRunTimeout.Seconds() || math.IsNaN(r.ReadySeconds) {
		return errors.New("ready_seconds is out of range")
	}
	if r.ImageDigest != "" && runtimecontract.ValidateImage(r.ImageDigest) != nil {
		return errors.New("image_digest must be a digest-pinned image")
	}
	if r.Node != "" && !nodePattern.MatchString(r.Node) {
		return errors.New("node is a digest, several, or unknown")
	}
	return nil
}
