package external

import "testing"

func TestManagedExternalBindingConfigIsStrictAndBudgeted(t *testing.T) {
	valid := `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`
	config, managed, err := ParseManagedBindingConfig(valid)
	if err != nil || !managed || config.Adapter != "openrouter" || config.RequestLimit != 10 {
		t.Fatalf("parse valid config: config=%#v managed=%t err=%v", config, managed, err)
	}
	for name, raw := range map[string]string{
		"raw credential": `{"adapter":"openrouter","secret_reference_id":"secret","api_key":"leak","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`,
		"no consent":     `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100}`,
		"no cost bound":  `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, managed, err := ParseManagedBindingConfig(raw); !managed || err == nil {
				t.Fatalf("managed=%t err=%v", managed, err)
			}
		})
	}
	if _, managed, err := ParseManagedBindingConfig(`{}`); err != nil || managed {
		t.Fatalf("ordinary external config: managed=%t err=%v", managed, err)
	}
}

func TestCustomerWalletRateCardFailsClosedOnMissingOrUnprofitableEconomics(t *testing.T) {
	valid := `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"supplier quote quote-1","rate_card_valid_until":"2099-01-01T00:00:00Z"}`
	config, managed, err := ParseManagedBindingConfig(valid)
	if err != nil || !managed || config.MinimumGrossMarginBPS != 2000 {
		t.Fatalf("valid rate card config=%#v managed=%t err=%v", config, managed, err)
	}
	for name, raw := range map[string]string{
		"missing cost basis":  `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000}`,
		"margin below floor":  `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":1000,"cost_basis_provenance":"supplier quote quote-1","rate_card_valid_until":"2099-01-01T00:00:00Z"}`,
		"unprofitable output": `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":399999,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"supplier quote quote-1","rate_card_valid_until":"2099-01-01T00:00:00Z"}`,
		"invalid expiry":      `{"adapter":"openrouter","secret_reference_id":"secret","enabled":true,"privacy_acknowledged":true,"request_limit":10,"cost_limit_microusd":1000,"max_request_cost_microusd":100,"billing_mode":"customer_wallet","input_microusd_per_mtok":100000,"output_microusd_per_mtok":400000,"cost_basis_input_microusd_per_mtok":80000,"cost_basis_output_microusd_per_mtok":320000,"minimum_gross_margin_bps":2000,"cost_basis_provenance":"supplier quote quote-1","rate_card_valid_until":"someday"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, managed, err := ParseManagedBindingConfig(raw); !managed || err == nil {
				t.Fatalf("managed=%t err=%v", managed, err)
			}
		})
	}
}
