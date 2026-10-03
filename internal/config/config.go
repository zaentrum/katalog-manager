// Package config reads the runtime configuration from the environment.
// Names mirror the CAP service so existing k8s manifests keep working
// (SPEC §6). Unknown/blank optional values disable the corresponding feature.
package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/model"
)

type Config struct {
	// HTTP
	Port string // SERVER_PORT (default 8080)

	// Database (SPRING_DATASOURCE_*). DatabaseURL is a libpq/pgx DSN or URL.
	DatabaseURL      string
	DatabaseUser     string
	DatabasePassword string

	// Auth
	OIDCIssuer       string // SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI
	Audience         string // KATALOG_AUDIENCE (default "katalog")
	AudienceRequired bool   // katalog.audience.required (default false -> issuer-only)
	AuthDisabled     bool   // AUTH_DISABLED (default false)

	// Stream token (base64-encoded HMAC key; blank -> stream tokens disabled)
	StreamSigningKey string // STREAM_SIGNING_KEY

	// Filesystem roots
	NFSRoot      string // SCANNER_NFS_ROOT / NFS_ROOT (default /var/lib/katalog/media)
	PackagesRoot string // PACKAGES_ROOT (default /var/lib/katalog/packages)

	// TMDB
	TMDBAPIKey   string // TMDB_API_KEY (blank -> enrichment disabled)
	TMDBLanguage string // TMDB_LANGUAGE (default en-US)
	// How often the people and titles the catalog holds are refreshed from
	// TMDB's change lists. Idle while there is no TMDB key.
	TMDBRefreshInterval time.Duration // TMDB_REFRESH_INTERVAL (default 24h; 0 or off disables)
	// The roles a title's credits follow TMDB in: credits in them are read
	// from TMDB, and a refresh drops a title's credits in any other role. Empty
	// is every role TMDB's credits give (model.CreditRoles).
	CreditRoles []string // KATALOG_CREDIT_ROLES (a comma-separated list; default all of them)

	// fanart.tv artwork fallback (fills poster/backdrop TMDB is missing). Blank -> off.
	FanartAPIKey    string // FANART_API_KEY (project key)
	FanartClientKey string // FANART_CLIENT_KEY (optional personal key, fresher images)

	// OMDb (omdbapi.com) metadata fallback: fills description/rating/poster TMDB
	// lacks, and can match items TMDB misses entirely (by title+year). Blank -> off.
	// Overridable at runtime via the `omdb.api_key` setting.
	OMDBAPIKey string // OMDB_API_KEY

	// chaptersdb
	ChaptersDBEnabled bool   // CHAPTERSDB_ENABLED (default false)
	ChaptersDBBaseURL string // CHAPTERSDB_BASE_URL (default https://chaptersdb.com)

	// download-gateway (command side)
	DownloadGatewayURL string // DOWNLOAD_GATEWAY_URL (blank -> disabled)

	// download events (Kafka read side)
	DownloadEventsEnabled bool   // DOWNLOAD_GATEWAY_EVENTS_ENABLED (default false)
	KafkaBrokers          string // KAFKA_BROKERS
	KafkaTopicPrefix      string // KAFKA_TOPIC_PREFIX (default "stube." — per-tenant on a shared cluster)
	KafkaGroupID          string // KAFKA_GROUP_ID / DOWNLOAD_GATEWAY_KAFKA_GROUP_ID
	KafkaCertDir          string // dir holding user.crt/user.key/ca.crt (default /etc/kafka-cert)

	// catalog pipeline events (Kafka producer + the discovered->enrich consumer).
	// This is the event-driven trigger spine: scan emits stube.catalog.item.discovered,
	// the enricher consumes it (replacing the old 60s poll ticker) and emits
	// stube.catalog.item.enriched, which the analyzer consumes, and so on down the
	// chain. Defaults ON whenever KAFKA_BROKERS is set (env override to force off).
	CatalogEventsEnabled bool // CATALOG_EVENTS_ENABLED (default = KAFKA_BROKERS != "")

	// oDownloader
	ODownloaderURL     string // ODOWNLOADER_URL / ODOWNLOADER_API_URL
	ODownloaderToken   string // ODOWNLOADER_TOKEN / ODOWNLOADER_API_TOKEN (blank -> disabled)
	ODownloaderPollSec int    // poll interval seconds (default 15)
	ODownloaderInbox   string // inbox dir (default <PackagesRoot>/_inbox)
	ODownloaderTimeout int    // per-job timeout minutes (default 60)
}

func env(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envDefault(def string, keys ...string) string {
	if v := env(keys...); v != "" {
		return v
	}
	return def
}

func envBool(def bool, keys ...string) bool {
	v := env(keys...)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(def int, keys ...string) int {
	v := env(keys...)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envDuration reads a Go duration ("24h", "90m"); "0", "off", "false" and
// "disabled" are zero, and anything it cannot read is def.
func envDuration(def time.Duration, keys ...string) time.Duration {
	v := strings.ToLower(env(keys...))
	switch v {
	case "":
		return def
	case "0", "off", "false", "disabled", "none":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return max(d, 0)
}

// normalizeDSN makes a Spring/JDBC-style datasource URL pgx-friendly: it strips
// a leading `jdbc:` (so `jdbc:postgresql://h/db` → `postgresql://h/db`) and, when
// no sslmode is given, defaults to `disable` (the in-cluster demo Postgres is
// plaintext; a TLS deployment specifies sslmode explicitly).
func normalizeDSN(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "jdbc:")
	if s == "" {
		return s
	}
	if (strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")) && !strings.Contains(s, "sslmode=") {
		sep := "?"
		if strings.Contains(s, "?") {
			sep = "&"
		}
		s += sep + "sslmode=disable"
	}
	return s
}

// creditRoles reads KATALOG_CREDIT_ROLES: a comma-separated list of the roles
// credits are read from TMDB in, each a role TMDB's credits give; every one of
// them when v is blank. The roles come in the order of model.CreditRoles, each
// once. A token that is not such a role, or a list that names none, is an
// error: a mistyped role would otherwise drop every credit in the role meant.
func creditRoles(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return slices.Clone(model.CreditRoles), nil
	}
	named := map[string]bool{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		switch {
		case tok == "":
			continue
		case !model.ValidRole(tok):
			return nil, fmt.Errorf("KATALOG_CREDIT_ROLES: %q is not a role: a role is a lowercase letter, "+
				"then up to 39 lowercase letters, digits and hyphens", tok)
		case !slices.Contains(model.CreditRoles, tok):
			return nil, fmt.Errorf("KATALOG_CREDIT_ROLES: TMDB's credits give no role %q; they give %s",
				tok, strings.Join(model.CreditRoles, ", "))
		}
		named[tok] = true
	}
	var out []string
	for _, role := range model.CreditRoles {
		if named[role] {
			out = append(out, role)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("KATALOG_CREDIT_ROLES names no role; leave it unset for all of them: " +
			strings.Join(model.CreditRoles, ", "))
	}
	return out, nil
}

// Load reads configuration from the process environment. A value that cannot
// mean anything sensible fails it: KATALOG_CREDIT_ROLES naming something that
// is not a role TMDB's credits give.
func Load() (Config, error) {
	roles, err := creditRoles(env("KATALOG_CREDIT_ROLES"))
	if err != nil {
		return Config{}, err
	}
	packages := envDefault("/var/lib/katalog/packages", "PACKAGES_ROOT")
	c := Config{
		Port:             envDefault("8080", "SERVER_PORT"),
		DatabaseURL:      normalizeDSN(env("SPRING_DATASOURCE_URL", "DATABASE_URL")),
		DatabaseUser:     env("SPRING_DATASOURCE_USERNAME", "DATABASE_USER"),
		DatabasePassword: env("SPRING_DATASOURCE_PASSWORD", "DATABASE_PASSWORD"),

		OIDCIssuer:       env("SPRING_SECURITY_OAUTH2_RESOURCESERVER_JWT_ISSUER_URI", "OIDC_ISSUER"),
		Audience:         envDefault("katalog", "KATALOG_AUDIENCE"),
		AudienceRequired: envBool(false, "KATALOG_AUDIENCE_REQUIRED"),
		AuthDisabled:     envBool(false, "AUTH_DISABLED"),

		StreamSigningKey: env("STREAM_SIGNING_KEY"),

		NFSRoot:      envDefault("/var/lib/katalog/media", "SCANNER_NFS_ROOT", "NFS_ROOT"),
		PackagesRoot: packages,

		TMDBAPIKey:          envDefault(DefaultTMDBToken, "TMDB_API_KEY"),
		TMDBLanguage:        envDefault("en-US", "TMDB_LANGUAGE"),
		TMDBRefreshInterval: envDuration(24*time.Hour, "TMDB_REFRESH_INTERVAL"),
		CreditRoles:         roles,

		FanartAPIKey:    envDefault(DefaultFanartKey, "FANART_API_KEY"),
		FanartClientKey: envDefault("", "FANART_CLIENT_KEY"),
		OMDBAPIKey:      envDefault(DefaultOMDBKey, "OMDB_API_KEY"),

		ChaptersDBEnabled: envBool(false, "CHAPTERSDB_ENABLED"),
		ChaptersDBBaseURL: envDefault("https://chaptersdb.com", "CHAPTERSDB_BASE_URL"),

		DownloadGatewayURL: env("DOWNLOAD_GATEWAY_URL"),

		DownloadEventsEnabled: envBool(false, "DOWNLOAD_GATEWAY_EVENTS_ENABLED"),
		KafkaBrokers:          env("KAFKA_BROKERS"),
		KafkaTopicPrefix:      envDefault("stube.", "KAFKA_TOPIC_PREFIX"),
		KafkaGroupID:          envDefault("stube-katalog-manager", "KAFKA_GROUP_ID", "DOWNLOAD_GATEWAY_KAFKA_GROUP_ID"),
		KafkaCertDir:          envDefault("/etc/kafka-cert", "KAFKA_CERT_DIR"),
		CatalogEventsEnabled:  envBool(env("KAFKA_BROKERS") != "", "CATALOG_EVENTS_ENABLED"),

		ODownloaderURL:     env("ODOWNLOADER_URL", "ODOWNLOADER_API_URL"),
		ODownloaderToken:   env("ODOWNLOADER_TOKEN", "ODOWNLOADER_API_TOKEN"),
		ODownloaderPollSec: envInt(15, "ODOWNLOADER_POLL_SEC"),
		ODownloaderInbox:   envDefault(packages+"/_inbox", "ODOWNLOADER_INBOX"),
		ODownloaderTimeout: envInt(60, "ODOWNLOADER_TIMEOUT_MIN"),
	}
	return c, nil
}

// DefaultTMDBToken is the bundled default TMDB v4 read-access token, injected at
// build time via -ldflags "-X .../config.DefaultTMDBToken=<token>". It is EMPTY in
// source (no secret committed to a public repo) so enrichment works out of the box
// only when the image is built with a token baked in. An operator's TMDB_API_KEY
// env always overrides it (their own token → higher, unshared rate limits).
var DefaultTMDBToken = ""

// DefaultFanartKey is the bundled fanart.tv PROJECT api key, injected at build
// time via -ldflags "-X .../config.DefaultFanartKey=<key>" (empty in source — no
// secret committed). fanart.tv's model is a per-project shared key, so baking one
// in ships the artwork fallback out of the box; an operator's FANART_API_KEY env
// overrides it, and FANART_CLIENT_KEY adds their personal (fresher-images) key.
var DefaultFanartKey = ""

// DefaultOMDBKey is the bundled OMDb (omdbapi.com) api key, injected at build time
// via -ldflags "-X .../config.DefaultOMDBKey=<key>" (empty in source — no secret
// committed; OMDb's free tier is per-user + 1,000 req/day, so it is NOT bundled by
// default). An operator's OMDB_API_KEY env or the `omdb.api_key` setting overrides.
var DefaultOMDBKey = ""

// TMDBEnabled reports whether TMDB enrichment is configured (a bundled default or
// an operator override).
func (c Config) TMDBEnabled() bool { return c.TMDBAPIKey != "" }

// DownloadGatewayEnabled reports whether the command side is configured.
func (c Config) DownloadGatewayEnabled() bool { return c.DownloadGatewayURL != "" }

// ODownloaderEnabled reports whether trailer ingestion is configured.
func (c Config) ODownloaderEnabled() bool { return c.ODownloaderURL != "" && c.ODownloaderToken != "" }
