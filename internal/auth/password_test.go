package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected error for password shorter than 8 characters")
	}

	hash, err := HashPassword("celestial-orbit-42")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if !VerifyPassword("celestial-orbit-42", hash) {
		t.Fatal("expected valid password to verify")
	}
	if VerifyPassword("wrong-password-42", hash) {
		t.Fatal("expected wrong password to fail verification")
	}
	tooLong := strings.Repeat("a", MaxPasswordBytes+1)
	if _, err := HashPassword(tooLong); err == nil {
		t.Fatal("expected error for password longer than MaxPasswordBytes")
	}
	if VerifyPassword(tooLong, hash) {
		t.Fatal("expected overly long password to fail verification immediately")
	}
}
