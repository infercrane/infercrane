package openaicompat

import "testing"

func TestEndpointAcceptsRootAndVersionedBase(t *testing.T) {
	for base, expected := range map[string]string{
		"http://worker:8000":            "http://worker:8000/v1/chat/completions",
		"https://openrouter.ai/api":     "https://openrouter.ai/api/v1/chat/completions",
		"https://openrouter.ai/api/v1/": "https://openrouter.ai/api/v1/chat/completions",
	} {
		actual, err := Endpoint(base, "chat/completions")
		if err != nil || actual != expected {
			t.Fatalf("base=%q actual=%q err=%v", base, actual, err)
		}
	}
}

func TestEndpointRejectsCredentialAndQueryMaterial(t *testing.T) {
	for _, base := range []string{"https://user:secret@host/v1", "https://host/v1?key=secret"} {
		if _, err := Endpoint(base, "models"); err == nil {
			t.Fatalf("unsafe base %q accepted", base)
		}
	}
}
