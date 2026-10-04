package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// claims decodes a token's claims as the verifier does (encoding/json).
func claims(t *testing.T, raw string) map[string]any {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A bearer's max_rating claim caps its caller at the age it says, a whole
// number of years; without it the caller is not capped, and a claim that is
// no whole number of years caps at the strictest age, 0, said once.
func TestMaxRatingOfABearer(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	malformedCap = sync.Once{}

	for raw, want := range map[string]string{
		`{"sub": "kid"}`:                     "uncapped",
		`{"sub": "kid", "max_rating": 12}`:   "12",
		`{"sub": "kid", "max_rating": 0}`:    "0",
		`{"sub": "kid", "max_rating": 18}`:   "18",
		`{"sub": "kid", "max_rating": 1e2}`:  "100",
		`{"sub": "kid", "max_rating": 12.0}`: "12",
		// malformed: the strictest cap
		`{"sub": "kid", "max_rating": 12.5}`:    "0",
		`{"sub": "kid", "max_rating": -1}`:      "0",
		`{"sub": "kid", "max_rating": "12"}`:    "0",
		`{"sub": "kid", "max_rating": "adult"}`: "0",
		`{"sub": "kid", "max_rating": true}`:    "0",
		`{"sub": "kid", "max_rating": null}`:    "0",
		`{"sub": "kid", "max_rating": [12]}`:    "0",
		`{"sub": "kid", "max_rating": {}}`:      "0",
		`{"sub": "kid", "max_rating": 1e30}`:    "0",
	} {
		age, capped := (&Principal{Subject: "kid", Claims: claims(t, raw)}).MaxRating()
		got := "uncapped"
		if capped {
			got = strconv.Itoa(age)
		}
		if got != want {
			t.Errorf("%s: %s, want %s", raw, got, want)
		}
	}
	if n := strings.Count(logged.String(), "no whole number of years"); n != 1 {
		t.Errorf("malformed claims said %d times, want once:\n%s", n, logged.String())
	}
	for _, p := range []*Principal{nil, {Subject: "anonymous", Unrestricted: true}, {Subject: "admin", Claims: map[string]any{}}} {
		if _, capped := p.MaxRating(); capped {
			t.Errorf("%+v is capped", p)
		}
	}
}

// mintCapped reproduces chino-api's Signer for a capped viewer: the cap after
// the subject in the user part.
func mintCapped(key []byte, subject, cap string, exp int64) string {
	return mint(key, subject+";max_rating="+cap, exp)
}

// A stream token carries the cap of the viewer it was minted for, and its
// subject without it; one without a cap, as every token before the caps, is
// not capped; a cap that is no whole number of years is 0.
func TestMaxRatingOfAStreamToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	v, err := NewStreamVerifier(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	for _, tc := range []struct {
		token, subject, cap string
	}{
		{mint(key, "user-1", exp), "user-1", "uncapped"},
		{mintCapped(key, "kid-1", "12", exp), "kid-1", "12"},
		{mintCapped(key, "kid-1", "0", exp), "kid-1", "0"},
		{mintCapped(key, "kid-1", "x", exp), "kid-1", "0"},
		{mintCapped(key, "kid-1", "-4", exp), "kid-1", "0"},
		{mintCapped(key, "kid-1", "", exp), "kid-1", "0"},
		{mintCapped(key, "a;max_rating=99", "6", exp), "a;max_rating=99", "6"}, // a subject holding the mark
	} {
		sub, maxRating, ok := v.VerifyCapped(tc.token)
		got := "uncapped"
		if maxRating != nil {
			got = strconv.Itoa(*maxRating)
		}
		if !ok || sub != tc.subject || got != tc.cap {
			t.Errorf("VerifyCapped: %q %s %v, want %q %s", sub, got, ok, tc.subject, tc.cap)
		}
		if sub2, ok2 := v.Verify(tc.token); !ok2 || sub2 != tc.subject {
			t.Errorf("Verify: %q %v, want the subject %q", sub2, ok2, tc.subject)
		}
		p := &Principal{Subject: sub, Stream: true, streamCap: maxRating}
		if age, capped := p.MaxRating(); capped != (maxRating != nil) || (capped && age != *maxRating) {
			t.Errorf("the principal of %s: %d %v", tc.subject, age, capped)
		}
	}
	// The cap is signed with the rest: changed, the token is refused.
	tok := mintCapped(key, "kid-1", "12", exp)
	payload, _ := base64.RawURLEncoding.DecodeString(tok[:strings.IndexByte(tok, '.')])
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), "=12|", "=99|", 1))) + tok[strings.IndexByte(tok, '.'):]
	if _, _, ok := v.VerifyCapped(forged); ok {
		t.Error("a token whose cap was changed was taken")
	}
	if _, _, ok := v.VerifyCapped(mintCapped(key, "kid-1", "12", time.Now().Add(-time.Minute).Unix())); ok {
		t.Error("an expired capped token was taken")
	}
}
