package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidToken = errors.New("invalid stream token")
	ErrExpiredToken = errors.New("stream token expired")
)

// TokenClaims binds a signed stream URL to a specific user, media item, session, and playback mode.
type TokenClaims struct {
	SessionID string `json:"sid"`
	UserID    string `json:"uid"`
	ItemID    string `json:"iid"`
	Mode      string `json:"mode"` // "direct" | "remux" | "transcode"
	ExpiresAt int64  `json:"exp"`
}

// SignToken encodes and signs TokenClaims with HMAC-SHA256.
func SignToken(secret []byte, claims TokenClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := computeMAC(secret, encodedPayload)
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// VerifyToken validates the HMAC signature and expiration timestamp.
func VerifyToken(secret []byte, token string, now time.Time) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return TokenClaims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	expected := computeMAC(secret, parts[0])
	if !hmac.Equal(sig, expected) {
		return TokenClaims{}, ErrInvalidToken
	}

	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	var claims TokenClaims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return TokenClaims{}, ErrInvalidToken
	}
	if claims.SessionID == "" || claims.ItemID == "" || claims.UserID == "" {
		return TokenClaims{}, ErrInvalidToken
	}
	if now.UTC().Unix() > claims.ExpiresAt {
		return TokenClaims{}, ErrExpiredToken
	}
	return claims, nil
}

func computeMAC(secret []byte, data string) []byte {
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}
