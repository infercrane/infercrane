package optimizationreport

import "testing"

func TestSetupFieldsShowEngineSettingsOnlyWhenSet(t *testing.T) {
	field := func(fields []ViewField, name string) (string, bool) {
		for _, item := range fields {
			if item.Name == name {
				return item.Value, true
			}
		}
		return "", false
	}
	control := Setup{Label: "vLLM defaults", Engine: "vllm", EngineVersion: "0.22.1"}
	if _, shown := field(setupFields(control), "Engine settings"); shown {
		t.Fatal("a setup without engine settings shows them")
	}
	aiter := control
	aiter.Label, aiter.Env = "AMD AITER kernels", map[string]string{"VLLM_ROCM_USE_AITER": "1"}
	if value, shown := field(setupFields(aiter), "Engine settings"); !shown || value != "VLLM_ROCM_USE_AITER=1" {
		t.Fatalf("engine settings=%q shown=%t", value, shown)
	}
}
