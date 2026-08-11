// Package secrets resolves reference-only credentials at the final process
// boundary. Resolved values must never be persisted or returned by public APIs.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/infercrane/infercrane/internal/domain"
)

var environmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,127}$`)

type Resolver interface {
	Resolve(context.Context, domain.SecretReference) (string, error)
}

type Environment struct {
	Lookup func(string) (string, bool)
}

func ValidateReference(reference domain.SecretReference) error {
	if reference.Resolver != "env" || !environmentName.MatchString(reference.Reference) {
		return errors.New("secret reference is not a valid environment reference")
	}
	return nil
}

func (e Environment) Resolve(_ context.Context, reference domain.SecretReference) (string, error) {
	if err := ValidateReference(reference); err != nil {
		return "", err
	}
	lookup := e.Lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, found := lookup(reference.Reference)
	if !found || value == "" {
		return "", fmt.Errorf("secret environment reference %q is unavailable", reference.Reference)
	}
	return value, nil
}
