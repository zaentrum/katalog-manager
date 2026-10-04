package processing

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const jwtSample = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJ1c2VyLTEifQ.c2lnbmF0dXJl"

// A step's error loses every credential it carries and keeps the rest.
func TestRedact(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"a database URL", "dial postgres://katalog:hunter2@db.internal:5432/katalog: refused",
			"dial postgres://REDACTED@db.internal:5432/katalog: refused"},
		{"a URL with a user only", "smb://media@nas/share unreachable", "smb://REDACTED@nas/share unreachable"},
		{"an HTTP client error", `Client error '401 Unauthorized' for url 'https://svc:s3cr3t@api.example/v1/x?token=abc.def&q=1'`,
			`Client error '401 Unauthorized' for url 'https://REDACTED@api.example/v1/x?token=REDACTED&q=1'`},
		{"a stream token", "GET /api/play/i1?stream=dXNlci0xfDE3OTE.c2ln failed", "GET /api/play/i1?stream=REDACTED failed"},
		{"OAuth names", "/cb?code=c1&state=s&access_token=a&id_token=i&refresh_token=r&client_secret=cs",
			"/cb?code=REDACTED&state=s&access_token=REDACTED&id_token=REDACTED&refresh_token=REDACTED&client_secret=REDACTED"},
		{"API keys", "GET https://api.example/3/movie?api_key=k1&language=en and ?apikey=k2",
			"GET https://api.example/3/movie?api_key=REDACTED&language=en and ?apikey=REDACTED"},
		{"a presigned URL", "PUT https://s3.example/b/o?X-Amz-Credential=AKIA%2F1&X-Amz-Signature=abc123&x=1",
			"PUT https://s3.example/b/o?X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&x=1"},
		{"a value up to the space after it, a colon in it too (a password may hold one)",
			"GET /x?password=a:b:c 403 and ?token=t0k3n: refused", "GET /x?password=REDACTED 403 and ?token=REDACTED refused"},
		{"percent-encoded inside another parameter", "/x?next=%2Fplay%3Fstream%3Dabc", "/x?next=%2Fplay%3Fstream%3DREDACTED"},
		{"a connection string", "connect host=db user=km password=hunter2 dbname=katalog", "connect host=db user=km password=REDACTED dbname=katalog"},
		{"a secret written out", "config: client_secret: abc123, secret=xyz", "config: client_secret: REDACTED, secret=REDACTED"},
		{"a bearer", "upstream said 401 to Authorization: Bearer abc.def-ghi_jkl~mno+p/q=",
			"upstream said 401 to Authorization: Bearer REDACTED"},
		{"basic auth", "sent Authorization: Basic dXNlcjpwYXNz to the gateway", "sent Authorization: Basic REDACTED to the gateway"},
		{"a bare JWT", "token rejected: " + jwtSample + " (expired)", "token rejected: REDACTED (expired)"},
		{"a JWT cut after its payload", "token eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEi", "token REDACTED"},
		// Left alone.
		{"a path on disk", "source file missing: /var/lib/katalog/media/Sintel (2010)/Sintel.mkv",
			"source file missing: /var/lib/katalog/media/Sintel (2010)/Sintel.mkv"},
		{"ffmpeg", "ffmpeg exited 1: Conversion failed! codec=hevc res=1920x804", "ffmpeg exited 1: Conversion failed! codec=hevc res=1920x804"},
		{"a URL without credentials", "GET http://katalog-manager:8080/api/analyze/items/m1: 503", "GET http://katalog-manager:8080/api/analyze/items/m1: 503"},
		{"an e-mail address", "mail to ops@example.org failed", "mail to ops@example.org failed"},
		{"look-alike names", "/x?upstream=a&streams=b&tokens=d&sigma=e", "/x?upstream=a&streams=b&tokens=d&sigma=e"},
		{"the word bearer in prose", "a bearer of bad news; send a Bearer token", "a bearer of bad news; send a Bearer token"},
	}
	for _, tc := range cases {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// A kept error is redacted, trimmed, valid UTF-8 and at most 500
// characters, cut on a character; a blank one is none.
func TestCleanError(t *testing.T) {
	if got := CleanError("  \n\t "); got != nil {
		t.Errorf("a blank error: %q, want none", *got)
	}
	if got := cleanError(nil); got != nil {
		t.Errorf("no error: %q, want none", *got)
	}
	if got := *CleanError("  ffmpeg exited 1\n"); got != "ffmpeg exited 1" {
		t.Errorf("trimmed: %q", got)
	}
	// 600 characters of two bytes each: a cut by bytes would split one.
	long := strings.Repeat("ä", 600)
	got := *CleanError(long)
	if n := utf8.RuneCountInString(got); n != MaxErrorLength || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("a long error: %d characters, valid %v, ends %q", n, utf8.ValidString(got), got[len(got)-3:])
	}
	if got := *CleanError(strings.Repeat("x", 500)); got != strings.Repeat("x", 500) {
		t.Error("an error of exactly 500 characters was cut")
	}
	// The credential goes before the cut: a long error with a token near its
	// end keeps none of it.
	got = *CleanError(strings.Repeat("y", 480) + " ?token=" + strings.Repeat("s", 60))
	if strings.Contains(got, "sss") {
		t.Errorf("a token at the cut survived: %q", got[470:])
	}
	if got := *CleanError("bad \xff\xfe bytes"); !utf8.ValidString(got) || got != "bad \uFFFD bytes" {
		t.Errorf("invalid UTF-8: %q", got)
	}
}
