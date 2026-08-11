package asyncinference

import "testing"

func TestCipherRoundTripAndBinding(t *testing.T) {
	c, err := NewCipher("01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	sealed, nonce, err := c.Encrypt([]byte(`{"prompt":"private"}`), []byte("tenant/job"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed) == `{"prompt":"private"}` {
		t.Fatal("plaintext was persisted")
	}
	opened, err := c.Decrypt(sealed, nonce, []byte("tenant/job"))
	if err != nil || string(opened) != `{"prompt":"private"}` {
		t.Fatalf("round trip: %q %v", opened, err)
	}
	if _, err = c.Decrypt(sealed, nonce, []byte("other/job")); err == nil {
		t.Fatal("associated-data mismatch accepted")
	}
}

func TestCipherRejectsWeakKey(t *testing.T) {
	if _, err := NewCipher("short"); err == nil {
		t.Fatal("weak key accepted")
	}
}
