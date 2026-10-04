package auth

import (
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
)

// A kid's account is capped at an age: its access token carries the claim
// max_rating, a whole number of years, and the kid is served no title rated
// above it. A stream token chino-api mints for a capped viewer carries the
// cap too. A caller whose token carries no max_rating is not capped (an
// admin, the service account, an adult); one whose claim is no whole number
// of years is held to the strictest cap, 0, so a claim gone wrong shows less,
// never more, and the service says so once.

// MaxRatingClaim is the claim of an access token that caps its viewer at an
// age.
const MaxRatingClaim = "max_rating"

// streamCapMark joins the subject and the cap in the user part of a capped
// viewer's stream token, "<subject>;max_rating=<age>|<expiry>", as
// chino-api's auth.Signer mints it. The cap is read after the last mark, so a
// subject that holds the mark itself is still read whole; chino-stream takes
// the user part for an opaque word, as ever.
const streamCapMark = ";" + MaxRatingClaim + "="

var malformedCap sync.Once

// MaxRating is the age p is capped at, from its token's max_rating claim, or
// the cap its stream token carries; capped is false for a caller without one,
// and with the service's auth off. A claim that is no whole number of years
// (a string, a fraction, a negative number, null) caps at 0.
func (p *Principal) MaxRating() (age int, capped bool) {
	if p == nil {
		return 0, false
	}
	if p.Stream {
		if p.streamCap == nil {
			return 0, false
		}
		return *p.streamCap, true
	}
	v, ok := p.Claims[MaxRatingClaim]
	if !ok {
		return 0, false
	}
	if age, ok := wholeYears(v); ok {
		return age, true
	}
	malformedCap.Do(func() {
		log.Printf("auth: the %s claim of %s is no whole number of years (%T %v): it is held to the strictest cap, 0 "+
			"(said once, of the first such caller)", MaxRatingClaim, p.Subject, v, v)
	})
	return 0, true
}

// wholeYears reads a claim's value, as JSON decodes it, as a whole number of
// years: a number without a fraction, 0 or more.
func wholeYears(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 || f > math.MaxInt32 || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// splitCap splits the user part of a stream token into the subject and the
// cap a capped viewer's token carries; nil when it carries none. A cap that is
// no whole number of years is the strictest, 0.
func splitCap(user string) (string, *int) {
	i := strings.LastIndex(user, streamCapMark)
	if i < 0 {
		return user, nil
	}
	age, err := strconv.Atoi(user[i+len(streamCapMark):])
	if err != nil || age < 0 {
		age = 0
	}
	return user[:i], &age
}
