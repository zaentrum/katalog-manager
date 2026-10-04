package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// TokenCheckTimeout is how long a check of a TMDB token waits for TMDB: short,
// as an admin waits for the answer, within a caller's own budget (the portal
// gives its call to the catalog 6 seconds).
const TokenCheckTimeout = 4 * time.Second

// CheckSecret checks the TMDB key, tmdb.api_key, with TMDB before
// setSecretSetting stores it (graph.SecretChecker); it checks no other key.
//
// TMDB refusing the token (401) is a definite no: graph.SecretRefused, and it
// is not stored. TMDB taking it (200) is graph.SecretValid. Anything else (no
// answer within TokenCheckTimeout, no connection, another status) says nothing
// of the token: graph.SecretUnchecked, and it is stored all the same, so an
// outage never keeps an admin from saving one. The token goes to TMDB as the
// bearer, as every call of the client sends it, and nowhere else: no message
// holds it, and nothing is logged.
func (s *Service) CheckSecret(ctx context.Context, key, value string) graph.SecretCheck {
	if key != "tmdb.api_key" {
		return graph.SecretCheck{}
	}
	return s.tmdb.checkToken(ctx, value)
}

// checkToken asks TMDB whether it takes token, at GET /authentication, once:
// a check is not retried, as an admin waits for it.
func (c *client) checkToken(ctx context.Context, token string) graph.SecretCheck {
	wait := c.checkWait
	if wait <= 0 {
		wait = TokenCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	say := func(status, message string) graph.SecretCheck {
		message = processing.Redact(message)
		if token != "" {
			message = strings.ReplaceAll(message, token, processing.Redacted)
		}
		return graph.SecretCheck{Status: status, Message: message}
	}
	unchecked := func(why string) graph.SecretCheck {
		return say(graph.SecretUnchecked, "could not check the token with TMDB ("+why+"): it is saved all the same")
	}
	// A token with a line break or another control character in it (a paste
	// gone wrong) is no TMDB token, and one with a line break cannot even be
	// sent as a bearer: it is refused without asking TMDB, not saved
	// unchecked.
	if strings.ContainsFunc(token, unicode.IsControl) {
		return say(graph.SecretRefused, "the token holds a control character, a line break say, which no TMDB token does")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/authentication", nil)
	if err != nil {
		return unchecked(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return unchecked("no answer within " + wait.String())
		}
		return unchecked("TMDB could not be reached: " + err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return say(graph.SecretValid, "TMDB took the token")
	case http.StatusUnauthorized:
		why := "HTTP 401"
		if m := statusMessage(body); m != "" {
			why += ": " + m
		}
		return say(graph.SecretRefused, "TMDB refused the token ("+why+"): it is no TMDB API read access token, or one TMDB no longer takes")
	}
	return unchecked("HTTP " + strconv.Itoa(resp.StatusCode))
}

// statusMessage is the status_message of a TMDB error answer, on one line and
// at most 200 characters; "" when there is none.
func statusMessage(body []byte) string {
	var n struct {
		StatusMessage string `json:"status_message"`
	}
	if json.Unmarshal(body, &n) != nil {
		return ""
	}
	m := strings.Join(strings.Fields(n.StatusMessage), " ")
	if r := []rune(m); len(r) > 200 {
		m = string(r[:200]) + "…"
	}
	return m
}
