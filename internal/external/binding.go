package external

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

const minimumCustomerWalletGrossMarginBPS = 1500

var supportedAdapters = map[string]struct{}{
	"openrouter": {}, "openai-compatible-external": {}, "modal": {}, "runpod-serverless-api": {}, "fly-io": {},
}

func SupportedAdapter(adapter string) bool {
	_, ok := supportedAdapters[adapter]
	return ok
}

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
	if !SupportedAdapter(config.Adapter) {
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
	if config.BillingMode != "" && config.BillingMode != "byoc" && config.BillingMode != "customer_wallet" {
		return errors.New("managed external binding billing_mode must be byoc or customer_wallet")
	}
	if config.BillingMode == "customer_wallet" {
		if config.InputMicrousdPerMTok < 0 || config.OutputMicrousdPerMTok < 0 || (config.InputMicrousdPerMTok == 0 && config.OutputMicrousdPerMTok == 0) {
			return errors.New("customer-wallet billing requires a non-negative, non-zero token price")
		}
		if config.CostBasisInputMicrousdPerMTok < 0 || config.CostBasisOutputMicrousdPerMTok < 0 || (config.CostBasisInputMicrousdPerMTok == 0 && config.CostBasisOutputMicrousdPerMTok == 0) {
			return errors.New("customer-wallet billing requires a non-negative, non-zero internal cost basis")
		}
		if config.MinimumGrossMarginBPS < minimumCustomerWalletGrossMarginBPS || config.MinimumGrossMarginBPS >= 10_000 {
			return fmt.Errorf("customer-wallet billing requires a gross margin floor between %d and 9999 basis points", minimumCustomerWalletGrossMarginBPS)
		}
		if strings.TrimSpace(config.CostBasisProvenance) == "" {
			return errors.New("customer-wallet billing requires internal cost-basis provenance")
		}
		if _, err := time.Parse(time.RFC3339, config.RateCardValidUntil); err != nil {
			return errors.New("customer-wallet billing requires an RFC3339 rate-card expiry")
		}
		for component, values := range map[string][2]int64{
			"input":  {config.CostBasisInputMicrousdPerMTok, config.InputMicrousdPerMTok},
			"output": {config.CostBasisOutputMicrousdPerMTok, config.OutputMicrousdPerMTok},
		} {
			minimumRetail, err := managedbilling.MinimumRetailPriceMicrousd(values[0], config.MinimumGrossMarginBPS)
			if err != nil {
				return fmt.Errorf("%s rate-card economics are invalid: %w", component, err)
			}
			if values[1] < minimumRetail {
				return fmt.Errorf("%s retail price does not satisfy the immutable gross-margin floor", component)
			}
		}
	} else if config.InputMicrousdPerMTok != 0 || config.OutputMicrousdPerMTok != 0 ||
		config.CostBasisInputMicrousdPerMTok != 0 || config.CostBasisOutputMicrousdPerMTok != 0 ||
		config.MinimumGrossMarginBPS != 0 || config.CostBasisProvenance != "" || config.RateCardValidUntil != "" {
		return errors.New("retail prices and internal rate-card economics are valid only for customer-wallet billing")
	}
	return nil
}
