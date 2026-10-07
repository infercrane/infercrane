package secretcipher

import "testing"

func TestCipherBindsCiphertextToAssociatedData(t *testing.T) {
	cipher, err := New("01234567890123456789012345678901")
	if err != nil {
		t.Fatal(err)
	}
	sealed, nonce, err := cipher.Encrypt([]byte("provider-token"), []byte("tenant/connection"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := cipher.Decrypt(sealed, nonce, []byte("tenant/connection"))
	if err != nil || string(opened) != "provider-token" {
		t.Fatalf("opened=%q err=%v", opened, err)
	}
	if _, err = cipher.Decrypt(sealed, nonce, []byte("other/connection")); err == nil {
		t.Fatal("ciphertext opened under different associated data")
	}
}
