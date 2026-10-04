package model

import "testing"

// A secret is a key the service reads as an API key, or one that names a
// credential; the rest keep their values.
func TestIsSecretSetting(t *testing.T) {
	for key, want := range map[string]bool{
		"tmdb.api_key": true, "omdb.api_key": true, "fanart.api_key": true, "fanart.client_key": true,
		"TMDB_API_KEY": true, "subtitles.apikey": true, "webhooks.api-key": true, "smtp.password": true,
		"db.passwd": true, "oauth.client_secret": true, "Secret": true, "trailers.token": true,
		"registry.credentials": true, "license.key": true, "signing_key": true, "key": true,
		" license.key ": true, "signing_key\n": true, "\tkey": true,
		"validate.small_file_threshold_mb": false, "packager.languages": false, "scanner.roots": false,
		"packager.language_whitelist": false, "packager.keep_original_if_single": false,
		"monkey.count": false, "keyboard.layout": false, "keys.sorted": false, "": false,
	} {
		if got := IsSecretSetting(key); got != want {
			t.Errorf("IsSecretSetting(%q) = %v, want %v", key, got, want)
		}
	}
}
