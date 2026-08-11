// Package runtimecontract owns the portable, declarative workload boundary
// between InferCrane lifecycle policy and provider container mechanisms.
package runtimecontract

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const OpenAIProtocol = "openai"

var digestPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*(?::[a-zA-Z0-9._-]+)?@sha256:[a-f0-9]{64}$`)

// Workload is persisted in an immutable deployment revision. Command is argv,
// never a shell fragment. Providers may translate it to their native launch
// representation but must preserve argument boundaries.
type Workload struct {
	Image                string   `json:"image,omitempty" yaml:"image,omitempty"`
	Command              []string `json:"command,omitempty" yaml:"command,omitempty"`
	Protocol             string   `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Port                 int      `json:"port,omitempty" yaml:"port,omitempty"`
	ReadinessPath        string   `json:"readiness_path,omitempty" yaml:"readiness_path,omitempty"`
	ModelsPath           string   `json:"models_path,omitempty" yaml:"models_path,omitempty"`
	MetricsPath          string   `json:"metrics_path,omitempty" yaml:"metrics_path,omitempty"`
	Cancellation         string   `json:"cancellation,omitempty" yaml:"cancellation,omitempty"`
	Drain                string   `json:"drain,omitempty" yaml:"drain,omitempty"`
	ShutdownGraceSeconds int      `json:"shutdown_grace_seconds,omitempty" yaml:"shutdown_grace_seconds,omitempty"`
}

func (w Workload) Empty() bool { return w.Image == "" && len(w.Command) == 0 && w.Protocol == "" }

// Validate rejects mutable or underspecified workloads before provisioning.
func (w Workload) Validate() error {
	if len(w.Image) > 512 {
		return errors.New("workload.image must not exceed 512 characters")
	}
	if !digestPattern.MatchString(strings.TrimSpace(w.Image)) {
		return errors.New("workload.image must be an OCI reference pinned by @sha256:<64 lowercase hex characters>")
	}
	if len(w.Command) == 0 || len(w.Command) > 128 || strings.TrimSpace(w.Command[0]) == "" {
		return errors.New("workload.command must contain executable argv")
	}
	for _, arg := range w.Command {
		if len(arg) > 4096 {
			return errors.New("workload.command arguments must not exceed 4096 characters")
		}
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("workload.command cannot contain NUL bytes")
		}
	}
	if w.Protocol != OpenAIProtocol {
		return fmt.Errorf("workload.protocol %q is unsupported; supported protocol: openai", w.Protocol)
	}
	if w.Port < 1 || w.Port > 65535 {
		return errors.New("workload.port must be between 1 and 65535")
	}
	for name, path := range map[string]string{"readiness_path": w.ReadinessPath, "models_path": w.ModelsPath, "metrics_path": w.MetricsPath} {
		if !validPath(path) {
			return fmt.Errorf("workload.%s must be an absolute path without query or fragment", name)
		}
	}
	if w.ReadinessPath != "/health" || w.ModelsPath != "/v1/models" || w.MetricsPath != "/metrics" {
		return errors.New("v0.6 workload paths must be /health, /v1/models, and /metrics")
	}
	if w.Cancellation != "http-disconnect" {
		return errors.New("workload.cancellation must be http-disconnect")
	}
	if w.Drain != "connection" {
		return errors.New("workload.drain must be connection")
	}
	if w.ShutdownGraceSeconds < 1 || w.ShutdownGraceSeconds > 3600 {
		return errors.New("workload.shutdown_grace_seconds must be between 1 and 3600")
	}
	return nil
}

func validPath(value string) bool {
	return len(value) <= 256 && strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "?#\r\n")
}
