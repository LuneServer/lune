package auth

import (
	"testing"
	"time"
)

func TestTOTPGenerateVerifyAndRecovery(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret failed: %v", err)
	}
	if len(secret) < 16 {
		t.Fatalf("expected base32 secret >= 16 chars, got %q", secret)
	}

	uri := BuildTOTPURI("dan", secret)
	if uri == "" {
		t.Fatalf("expected non-empty otpauth URI")
	}

	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	key, err := totpBase32.DecodeString(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	code := computeTOTPAtStep(key, now.Unix()/totpPeriodSeconds)

	if !VerifyTOTPCode(secret, code, now) {
		t.Fatalf("expected valid TOTP code %s to verify", code)
	}
	// Adjacent 30s step should also verify (clock skew tolerance).
	if !VerifyTOTPCode(secret, code, now.Add(30*time.Second)) {
		t.Fatalf("expected +1 step skew to verify")
	}
	// +2 steps (60s) should fail.
	if VerifyTOTPCode(secret, code, now.Add(60*time.Second)) {
		t.Fatalf("expected +2 step skew to fail")
	}

	plain, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes failed: %v", err)
	}
	if len(plain) != recoveryCodeCount || len(hashes) != recoveryCodeCount {
		t.Fatalf("expected %d recovery codes, got %d", recoveryCodeCount, len(plain))
	}

	ok, rem := consumeRecoveryCode(plain[0], hashes)
	if !ok || len(rem) != recoveryCodeCount-1 {
		t.Fatalf("expected recovery code to match and be consumed")
	}
	// Reuse must fail.
	if ok2, _ := consumeRecoveryCode(plain[0], rem); ok2 {
		t.Fatalf("expected consumed recovery code to fail on reuse")
	}
}
