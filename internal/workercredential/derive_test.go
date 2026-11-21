package workercredential

import "testing"

func TestDeriveIsScopedAndStable(t *testing.T) {
	first := Derive("master-secret", "tenant-a", "deployment-a")
	if first == "" || first != Derive("master-secret", "tenant-a", "deployment-a") {
		t.Fatal("derived credential is not stable")
	}
	if first == Derive("master-secret", "tenant-b", "deployment-a") || first == Derive("master-secret", "tenant-a", "deployment-b") {
		t.Fatal("derived credential is not scoped")
	}
	if Derive("", "tenant-a", "deployment-a") != "" || Derive("master-secret", "", "deployment-a") != "" || Derive("master-secret", "tenant-a", "") != "" {
		t.Fatal("incomplete worker credential identity did not fail closed")
	}
}

func TestSourceNeverSendsTheSharedCredentialToAScopedOrTenantOwnedWorker(t *testing.T) {
	source := Source{Master: "master-secret", Shared: "shared-secret"}
	scoped := `{"provider":"runpod-pods","worker_credential_version":"tenant-hmac-v1"}`
	for name, test := range map[string]struct {
		connection, details, want string
	}{
		"scoped worker on operator compute":      {"", scoped, Derive("master-secret", "tenant-a", "deployment-a")},
		"scoped worker in a tenant account":      {"connection-a", scoped, Derive("master-secret", "tenant-a", "deployment-a")},
		"unscoped worker on operator compute":    {"", `{"worker_credential_version":""}`, "shared-secret"},
		"adapter that records no version":        {"", `{"provider":"kubernetes"}`, "shared-secret"},
		"no details on operator compute":         {"", "", "shared-secret"},
		"unscoped worker in a tenant account":    {"connection-a", `{"worker_credential_version":""}`, ""},
		"no details in a tenant account":         {"connection-a", "", ""},
		"unknown version is not guessed":         {"", `{"worker_credential_version":"tenant-hmac-v9"}`, ""},
		"unknown version in a tenant account":    {"connection-a", `{"worker_credential_version":"other"}`, ""},
		"malformed details in a tenant account":  {"connection-a", `{`, ""},
		"malformed details on operator compute":  {"", `{`, "shared-secret"},
		"version of the wrong type reads absent": {"", `{"worker_credential_version":7}`, "shared-secret"},
	} {
		if got := source.Credential("tenant-a", "deployment-a", test.connection, test.details); got != test.want {
			t.Errorf("%s: credential = %q, want %q", name, got, test.want)
		}
	}
	if got := (Source{Shared: "shared-secret"}).Credential("tenant-a", "deployment-a", "", scoped); got != "" {
		t.Fatalf("scoped worker without a master fell back to %q", got)
	}
}
