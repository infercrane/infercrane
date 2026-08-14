package external

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

// ParseManagedBindingConfig strictly decodes the immutable policy stored on a
// managed external backend binding. An empty JSON object denotes an ordinary
// customer-managed OpenAI-compatible endpoint and is not a managed binding.
func ParseManagedBindingConfig(raw string) (domain.ManagedExternalBindingConfig, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return domain.ManagedExternalBindingConfig{}, false, nil
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var config domain.ManagedExternalBindingConfig
	if err := decoder.Decode(&config); err != nil {
		return domain.ManagedExternalBindingConfig{}, true, fmt.Errorf("decode managed external binding config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.ManagedExternalBindingConfig{}, true, errors.New("managed external binding config contains trailing JSON")
	}
	if err := ValidateManagedBindingConfig(config); err != nil {
		return domain.ManagedExternalBindingConfig{}, true, err
	}
	return config, true, nil
}

func ValidateManagedBindingConfig(config domain.ManagedExternalBindingConfig) error {
	if config.Adapter != "openrouter" && config.Adapter != "openai-compatible-external" {
		return fmt.Errorf("unsupported managed external adapter %q", config.Adapter)
	}
	if strings.TrimSpace(config.SecretReferenceID) == "" {
		return errors.New("managed external binding requires a secret reference")
	}
	if config.Enabled && !config.PrivacyAcknowledged {
		return errors.New("enabled managed external binding requires explicit privacy acknowledgement")
	}
	if config.RequestLimit < 1 {
		return errors.New("managed external binding requires a positive request limit")
	}
	if config.CostLimitMicrousd < 1 || config.MaxRequestCostMicrousd < 1 || config.MaxRequestCostMicrousd > config.CostLimitMicrousd {
		return errors.New("managed external binding requires a hard cost limit and bounded worst-case request reservation")
	}
	return nil
}
