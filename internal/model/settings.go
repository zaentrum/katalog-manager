package model

import (
	"regexp"
	"slices"
	"strings"
)

// secretSettingKeys are the settings the service reads as credentials: the
// enrichment providers' API keys, which override the environment's.
var secretSettingKeys = []string{"tmdb.api_key", "omdb.api_key", "fanart.api_key", "fanart.client_key"}

var (
	// secretWords name a credential anywhere in a key.
	secretWords = regexp.MustCompile(`(?i)secret|token|passw(or)?d|credential|api[._-]?key`)
	// secretKeySuffix names one at its end: "fanart.client_key", "x.key".
	secretKeySuffix = regexp.MustCompile(`(?i)(^|[._-])key$`)
)

// IsSecretSetting reports whether the setting key holds a credential: one of
// the keys the service reads as API keys, or a key that names one, spaces
// around it aside. Its value is write-only: no GraphQL field returns it, only
// setSecretSetting and clearSecretSetting change it, and the workers' settings
// route leaves it out. A key taken for a secret that is none is merely
// write-only too; one taken for none that is a secret would be read back, so
// the words are wide.
func IsSecretSetting(key string) bool {
	key = strings.TrimSpace(key)
	return slices.Contains(secretSettingKeys, key) || secretWords.MatchString(key) || secretKeySuffix.MatchString(key)
}
