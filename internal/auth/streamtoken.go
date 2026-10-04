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
// The userID is the subject, without the rating cap a capped viewer's token
// carries (VerifyCapped).
func (v *StreamVerifier) Verify(token string) (string, bool) {
	sub, _, ok := v.VerifyCapped(token)
	return sub, ok
}

// VerifyCapped is Verify with the rating cap the token carries: the age a
// capped viewer's token was minted with ("<subject>;max_rating=<age>" in the
// user part), nil for a token that carries none (an uncapped viewer's, and
// every token older than the caps).
func (v *StreamVerifier) VerifyCapped(token string) (subject string, maxRating *int, ok bool) {
	if v == nil || v.key == nil || token == "" {
		return "", nil, false
	}
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return "", nil, false
	}
	payload := token[:dot]
	sig := token[dot+1:]

	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(payload)) // ASCII bytes of the base64url payload segment
	expected := mac.Sum(nil)

	got, err := urlDecode(sig)
	if err != nil {
		return "", nil, false
	}
	body, err := urlDecode(payload)
	if err != nil {
		return "", nil, false
	}
	if !hmac.Equal(expected, got) {
		return "", nil, false
	}

	decoded := string(body)
	pipe := strings.IndexByte(decoded, '|')
	if pipe < 1 {
		return "", nil, false
	}
	userID := decoded[:pipe]
	expUnix, err := strconv.ParseInt(decoded[pipe+1:], 10, 64)
	if err != nil {
		return "", nil, false
	}
	if time.Now().Unix() > expUnix {
		return "", nil, false
	}
	subject, maxRating = splitCap(userID)
	return subject, maxRating, true
}

// urlDecode accepts URL-safe base64 with or without padding (Java's
// Base64.getUrlDecoder tolerates both; the Go minter emits unpadded).
func urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}
