package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpIssuer        = "Lune"
	totpPeriodSeconds = 30
	totpDigits        = 6
	totpSkewSteps     = 1
	recoveryCodeCount = 8
)

var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret creates a cryptographically random 160-bit Base32 TOTP secret.
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return totpBase32.EncodeToString(raw), nil
}

// BuildTOTPURI constructs a standard otpauth:// URI for authenticator apps.
func BuildTOTPURI(username, secret string) string {
	label := url.PathEscape(totpIssuer + ":" + username)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// VerifyTOTPCode validates a 6-digit RFC 6238 TOTP code against secret at time now
// allowing +-1 time step (30s) for clock skew.
func VerifyTOTPCode(secret, code string, now time.Time) bool {
	code = normalizeTOTPCode(code)
	if len(code) != totpDigits {
		return false
	}
	key, err := totpBase32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return false
	}
	step := now.UTC().Unix() / totpPeriodSeconds
	for delta := int64(-totpSkewSteps); delta <= totpSkewSteps; delta++ {
		expected := computeTOTPAtStep(key, step+delta)
		if subtle.ConstantTimeCompare([]byte(code), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

func computeTOTPAtStep(key []byte, step int64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	binCode := (uint32(sum[offset])&0x7f)<<24 |
		(uint32(sum[offset+1])&0xff)<<16 |
		(uint32(sum[offset+2])&0xff)<<8 |
		(uint32(sum[offset+3]) & 0xff)

	return fmt.Sprintf("%06d", binCode%1_000_000)
}

func normalizeTOTPCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// GenerateRecoveryCodes generates 8 human-readable one-time recovery codes (XXXX-XXXX)
// along with their SHA-256 hex digests for storage.
func GenerateRecoveryCodes() (plain []string, hashes []string, err error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	plain = make([]string, 0, recoveryCodeCount)
	hashes = make([]string, 0, recoveryCodeCount)
	buf := make([]byte, 8)
	for i := 0; i < recoveryCodeCount; i++ {
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, err
		}
		var raw [8]byte
		for j := 0; j < 8; j++ {
			raw[j] = alphabet[int(buf[j])%len(alphabet)]
		}
		code := string(raw[:4]) + "-" + string(raw[4:])
		plain = append(plain, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return plain, hashes, nil
}

func hashRecoveryCode(code string) string {
	norm := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	sum := sha256.Sum256([]byte("lune-recovery:" + norm))
	return hex.EncodeToString(sum[:])
}

// consumeRecoveryCode checks code against stored hashes and returns (matched, remainingHashes).
func consumeRecoveryCode(code string, hashes []string) (bool, []string) {
	cand := hashRecoveryCode(code)
	for i, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(cand), []byte(h)) == 1 {
			remaining := make([]string, 0, len(hashes)-1)
			remaining = append(remaining, hashes[:i]...)
			remaining = append(remaining, hashes[i+1:]...)
			return true, remaining
		}
	}
	return false, hashes
}
