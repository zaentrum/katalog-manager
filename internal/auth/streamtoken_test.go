package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

// mint reproduces chino-api's stream.Signer to exercise the verifier.
func mint(key []byte, userID string, exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%d", userID, exp)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

func TestStreamVerify(t *testing.T) {
	rawKey := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	keyB64 := base64.StdEncoding.EncodeToString(rawKey)
	v, err := NewStreamVerifier(keyB64)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Configured() {
		t.Fatal("expected configured")
	}

	exp := time.Now().Add(time.Hour).Unix()
	sub, ok := v.Verify(mint(rawKey, "user-123", exp))
	if !ok || sub != "user-123" {
		t.Fatalf("valid token rejected: ok=%v sub=%q", ok, sub)
	}

	if _, ok := v.Verify(mint(rawKey, "user-123", time.Now().Add(-time.Minute).Unix())); ok {
		t.Fatal("expired token accepted")
	}

	tampered := mint(rawKey, "user-123", exp) + "x"
	if _, ok := v.Verify(tampered); ok {
		t.Fatal("tampered signature accepted")
	}

	wrongKey := make([]byte, 32)
	if _, ok := v.Verify(mint(wrongKey, "user-123", exp)); ok {
		t.Fatal("wrong-key token accepted")
	}
}

func TestStreamDisabledWhenNoKey(t *testing.T) {
	v, err := NewStreamVerifier("")
	if err != nil {
		t.Fatal(err)
	}
	if v.Configured() {
		t.Fatal("expected disabled")
	}
	if _, ok := v.Verify("anything.here"); ok {
		t.Fatal("disabled verifier accepted a token")
	}
}

func TestStreamKeyTooShort(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("0123456789")) // 10 bytes
	if _, err := NewStreamVerifier(short); err == nil {
		t.Fatal("expected error for <16 byte key")
	}
}
