package processing

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxErrorLength is the most characters a step's error keeps (the column is
// VARCHAR(500)); a longer one is cut and ends in an ellipsis.
const MaxErrorLength = 500

// Redacted is what a credential in a step's error is replaced with.
const Redacted = "REDACTED"

var (
	// urlCredentials matches the user and password of a URL,
	// scheme://user:password@host: everything between :// and @.
	urlCredentials = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/?#@\s"'<>]+@`)
	// credentialParam matches a credential-carrying query parameter and its
	// value, as chino-api redacts them (the stream and access tokens, the
	// OAuth names, passwords and API keys) and the signatures of a presigned
	// URL, also percent-encoded inside another parameter.
	credentialParam = regexp.MustCompile(`(?i)((?:^|[?&;]|%3f|%26)` +
		`(?:token|stream|access_token|id_token|refresh_token|code|client_secret|password|passwd|secret|api_key|apikey|` +
		`signature|sig|x-amz-signature|x-amz-credential|x-amz-security-token)` +
		`(?:=|%3d))[^&#\s"'\\]+`)
	// secretAssignment matches a password or secret written out as
	// name=value or name: value in text (a connection string, a config dump).
	secretAssignment = regexp.MustCompile(`(?i)(\b(?:password|passwd|pwd|client_secret|secret)\s*[=:]\s*)[^\s,;&"']+`)
	// bearer and basic match an Authorization header value quoted in text.
	// Real tokens are long; the minimum length leaves prose alone.
	bearer = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	basic  = regexp.MustCompile(`(?i)(\bauthorization:\s*basic\s+)[A-Za-z0-9+/=]+`)
	// jwt matches a JWT anywhere else, also one cut short after its payload.
	jwt = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]*)?`)
)

// Redact returns s with every credential it recognises replaced by Redacted:
// the user and password of a URL, the value of a credential query parameter,
// a password or secret written out, a bearer or basic Authorization, a JWT.
// The rest of s is left as it is.
func Redact(s string) string {
	s = urlCredentials.ReplaceAllString(s, "${1}"+Redacted+"@")
	s = credentialParam.ReplaceAllString(s, "${1}"+Redacted)
	s = secretAssignment.ReplaceAllString(s, "${1}"+Redacted)
	s = bearer.ReplaceAllString(s, "${1}"+Redacted)
	s = basic.ReplaceAllString(s, "${1}"+Redacted)
	return jwt.ReplaceAllString(s, Redacted)
}

// CleanError is a step's error as it is kept: credentials redacted, valid
// UTF-8, the spaces around it trimmed, and at most MaxErrorLength characters
// (cut on a character, never inside one, so Postgres takes it). An error
// that is blank is none.
func CleanError(s string) *string {
	s = strings.TrimSpace(Redact(strings.ToValidUTF8(s, "�")))
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > MaxErrorLength {
		r := []rune(s)
		s = strings.TrimRight(string(r[:MaxErrorLength-1]), " ") + "…"
	}
	return &s
}

func cleanError(p *string) *string {
	if p == nil {
		return nil
	}
	return CleanError(*p)
}
