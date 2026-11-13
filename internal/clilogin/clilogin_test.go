package clilogin

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/authz"
)

var userCodeShape = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

func TestUserCodesAreUnambiguousAndRetypable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := NewUserCode()
		if err != nil || !userCodeShape.MatchString(code) {
			t.Fatalf("code=%q err=%v", code, err)
		}
		seen[code] = true
	}
	if len(seen) < 195 {
		t.Fatalf("user codes repeat too often: %d unique of 200", len(seen))
	}
	for input, want := range map[string]string{
		"WXZB-KLMN":   "WXZB-KLMN",
		"wxzb-klmn":   "WXZB-KLMN",
		"wxzbklmn":    "WXZB-KLMN",
		" WXZB KLMN ": "WXZB-KLMN",
	} {
		if got, ok := NormalizeUserCode(input); !ok || got != want {
			t.Fatalf("NormalizeUserCode(%q)=%q,%v want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "WXZB-KLM", "WXZB-KLMNP", "WXZA-KLMN", "WXZB-KLM1", "../../etc"} {
		if _, ok := NormalizeUserCode(input); ok {
			t.Fatalf("NormalizeUserCode(%q) accepted an invalid code", input)
		}
	}
}

func TestDeviceCodesAreSecretAndOnlyTheirHashIsStored(t *testing.T) {
	code, hash, err := NewDeviceCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, DeviceCodePrefix) || len(code) != 68 || len(hash) != 64 || strings.Contains(hash, code[4:]) {
		t.Fatalf("code=%q hash=%q", code, hash)
	}
	if HashDeviceCode(code) != hash {
		t.Fatal("hash is not stable")
	}
	other, _, _ := NewDeviceCode()
	if other == code {
		t.Fatal("device codes repeat")
	}
	for _, invalid := range []string{"", "icd_", code[:67], "ic_" + code[4:] + "0", "icd_" + strings.Repeat("z", 64)} {
		if HashDeviceCode(invalid) != "" {
			t.Fatalf("HashDeviceCode(%q) accepted an invalid code", invalid)
		}
	}
}

func TestClientDescriptionIsBoundedAndStripped(t *testing.T) {
	client, err := Client{DeviceName: "  ada\x1b[31m-laptop\n", OS: "darwin/arm64", Version: "infercrane 1.0.0"}.Normalize()
	if err != nil || client.DeviceName != "ada[31m-laptop" {
		t.Fatalf("client=%#v err=%v", client, err)
	}
	for _, invalid := range []Client{
		{DeviceName: "", OS: "linux", Version: "1"},
		{DeviceName: strings.Repeat("x", 121), OS: "linux", Version: "1"},
		{DeviceName: "x", OS: strings.Repeat("o", 65), Version: "1"},
		{DeviceName: "x", OS: "linux", Version: " \t "},
	} {
		if _, err := invalid.Normalize(); !errors.Is(err, ErrInvalidClient) {
			t.Fatalf("Normalize(%#v) err=%v", invalid, err)
		}
	}
}

func TestIssuedKeysNeverExceedTheApprover(t *testing.T) {
	scopes, err := IssuedScopes(authz.Operator, []string{"read", "deploy"})
	if err != nil || len(scopes) != 2 || scopes[0] != authz.Read || scopes[1] != authz.Deploy {
		t.Fatalf("scopes=%v err=%v", scopes, err)
	}
	// A scope beyond the role ceiling is dropped, not delegated.
	scopes, err = IssuedScopes(authz.Viewer, []string{"read", "deploy", "manage_tenant"})
	if err != nil || len(scopes) != 1 || scopes[0] != authz.Read {
		t.Fatalf("viewer scopes=%v err=%v", scopes, err)
	}
	// Legacy principals without explicit scopes keep the legacy ceiling.
	scopes, err = IssuedScopes(authz.Admin, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range scopes {
		if scope == authz.ManageSecrets || scope == authz.ManageExternal {
			t.Fatalf("legacy admin delegated %s", scope)
		}
	}
	if _, err = IssuedScopes(authz.Role("guest"), []string{"read"}); err == nil {
		t.Fatal("unknown role delegated a scope")
	}
}

func TestOnlyDashboardPrincipalsApprove(t *testing.T) {
	for kind, want := range map[string]bool{"human": true, "service_account": true, "bootstrap": true, PrincipalKind: false, "inference_token": false, "": false} {
		if ApproverAllowed(kind) != want {
			t.Fatalf("ApproverAllowed(%q)=%v", kind, !want)
		}
	}
}

func TestPrincipalNameIsBounded(t *testing.T) {
	if got := PrincipalName("ada-laptop", "0123456789abcdef"); got != "cli:ada-laptop:01234567" {
		t.Fatalf("name=%q", got)
	}
	if got := PrincipalName(strings.Repeat("é", 200), "abc"); len([]rune(got)) != 4+80+1+3 {
		t.Fatalf("name length=%d", len([]rune(got)))
	}
}
