package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// StreamVerifier verifies the HMAC-SHA256 stream tokens minted by chino-api's
// auth.Signer (chino-api/internal/auth/stream.go). Both services share the same
// base64 STREAM_SIGNING_KEY. Token format (URL-safe, opaque):
//
//	base64url(userID "|" expUnix) "." base64url(HMAC-SHA256(payload, key))
//
// where the HMAC is computed over the ASCII bytes of the first (base64url)
// segment — NOT over the raw "userID|expUnix". Reproduced byte-for-byte from
// the Java StreamTokenSigner so existing tokens keep validating.
type StreamVerifier struct {
	key []byte // nil => disabled
}

// NewStreamVerifier decodes the base64 key. A blank key disables verification
// (Verify always returns "", false). A present-but-invalid or <16-byte key is
// an error (matches the Java constructor's hard failure).
func NewStreamVerifier(keyB64 string) (*StreamVerifier, error) {
	keyB64 = strings.TrimSpace(keyB64)
	if keyB64 == "" {
		return &StreamVerifier{key: nil}, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, errStreamKeyNotBase64
	}
	if len(decoded) < 16 {
		return nil, errStreamKeyTooShort
	}
	return &StreamVerifier{key: decoded}, nil
}

// Configured reports whether a key is present.
func (v *StreamVerifier) Configured() bool { return v != nil && v.key != nil }

// Verify returns the embedded userID when the signature is valid and the token
// has not expired, else ("", false). Never panics for malformed/expired input.
func (v *StreamVerifier) Verify(token string) (string, bool) {
	if v == nil || v.key == nil || token == "" {
		return "", false
	}
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return "", false
	}
	payload := token[:dot]
	sig := token[dot+1:]

	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(payload)) // ASCII bytes of the base64url payload segment
	expected := mac.Sum(nil)

	got, err := urlDecode(sig)
	if err != nil {
		return "", false
	}
	body, err := urlDecode(payload)
	if err != nil {
		return "", false
	}
	if !hmac.Equal(expected, got) {
		return "", false
	}

	decoded := string(body)
	pipe := strings.IndexByte(decoded, '|')
	if pipe < 1 {
		return "", false
	}
	userID := decoded[:pipe]
	expUnix, err := strconv.ParseInt(decoded[pipe+1:], 10, 64)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() > expUnix {
		return "", false
	}
	return userID, true
}

// urlDecode accepts URL-safe base64 with or without padding (Java's
// Base64.getUrlDecoder tolerates both; the Go minter emits unpadded).
func urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}
