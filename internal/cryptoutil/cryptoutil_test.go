package cryptoutil

import "testing"

func TestEncryptDecryptRoundTrip(t *testing.T) {
	passphrase := "my-secret-key"
	plaintext := "ssh-password-123"

	encoded, err := Encrypt(passphrase, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if encoded == plaintext {
		t.Fatal("encrypted string should differ from plaintext")
	}
	if !IsEncrypted(encoded) {
		t.Fatal("IsEncrypted should return true for encrypted string")
	}

	decoded, err := Decrypt(passphrase, encoded)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if decoded != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, decoded)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	encoded, err := Encrypt("correct-key", "secret")
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	_, err = Decrypt("wrong-key", encoded)
	if err == nil {
		t.Fatal("expected error with wrong key, got nil")
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	plaintext := "not-encrypted"
	if IsEncrypted(plaintext) {
		t.Fatal("plaintext should not be detected as encrypted")
	}
	decoded, err := Decrypt("any-key", plaintext)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decoded != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, decoded)
	}
}

func TestEncryptEmptyString(t *testing.T) {
	encoded, err := Encrypt("key", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if encoded != "" {
		t.Fatalf("expected empty string, got %q", encoded)
	}
}

func TestDecryptEmptyString(t *testing.T) {
	decoded, err := Decrypt("key", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decoded != "" {
		t.Fatalf("expected empty string, got %q", decoded)
	}
}

func TestUniqueNonces(t *testing.T) {
	// 相同明文加密两次应产生不同密文（随机 nonce）
	e1, _ := Encrypt("key", "same-plaintext")
	e2, _ := Encrypt("key", "same-plaintext")
	if e1 == e2 {
		t.Fatal("two encryptions of the same plaintext should differ due to random nonce")
	}
}
