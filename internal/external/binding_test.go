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
