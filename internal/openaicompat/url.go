// Package openaicompat owns URL composition shared by OpenAI-compatible
// runtimes and governed external targets.
package openaicompat

import (
	"errors"
	"net/url"
	"strings"
)

// Endpoint accepts either a service root (https://host/api) or the conventional
// versioned base (https://host/api/v1) without producing a duplicated /v1/v1.
func Endpoint(base, resource string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("OpenAI-compatible base URL is invalid")
	}
	resource = strings.Trim(resource, "/")
	if resource == "" {
		return "", errors.New("OpenAI-compatible resource is required")
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/v1") {
		return url.JoinPath(base, resource)
	}
	return url.JoinPath(base, "v1", resource)
}
