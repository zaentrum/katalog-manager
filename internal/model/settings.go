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

// WorkerSettings are the settings ss as the workers read them (GET
// /api/settings), by key: a key without the spaces around it, of a key the
// table holds twice the setting of the lowest id, and no secret
// (IsSecretSetting), set or not. The compliance check reads the packager's
// language settings so too.
func WorkerSettings(ss []*Setting) map[string]*Setting {
	out := map[string]*Setting{}
	for _, s := range ss {
		key := strings.TrimSpace(s.Key)
		if key == "" || IsSecretSetting(key) {
			continue
		}
		if twin, ok := out[key]; ok && twin.ID <= s.ID {
			continue
		}
		out[key] = s
	}
	return out
}
