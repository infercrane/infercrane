package contextpassport

import (
	"testing"
	"time"
)

func TestDirectoryTenantSubjectAndExpiry(t *testing.T) {
	d := New()
	now := time.Now()
	d.Put(Hint{ID: "s", TenantID: "a", SubjectID: "e", PreferredBindingID: "b", ExpiresAt: now.Add(time.Minute)})
	if h, ok := d.Resolve("a", "s", "e", now); !ok || h.PreferredBindingID != "b" {
		t.Fatal("missing")
	}
	if _, ok := d.Resolve("b", "s", "e", now); ok {
		t.Fatal("tenant leak")
	}
	if _, ok := d.Resolve("a", "s", "e", now.Add(time.Hour)); ok {
		t.Fatal("expired")
	}
}
