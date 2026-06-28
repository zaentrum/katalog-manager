// Package config reads the runtime configuration from the environment.
// Names mirror the CAP service so existing k8s manifests keep working
// (SPEC §6). Unknown/blank optional values disable the corresponding feature.
package config

import (
	"os"
	"strconv"
	"strings"
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

	// chaptersdb
	ChaptersDBEnabled bool   // CHAPTERSDB_ENABLED (default false)
	ChaptersDBBaseURL string // CHAPTERSDB_BASE_URL (default https://chaptersdb.com)

	// download-gateway (command side)
	DownloadGatewayURL string // DOWNLOAD_GATEWAY_URL (blank -> disabled)

	// download events (Kafka read side)
	DownloadEventsEnabled bool   // DOWNLOAD_GATEWAY_EVENTS_ENABLED (default false)
	KafkaBrokers          string // KAFKA_BROKERS
	KafkaGroupID          string // KAFKA_GROUP_ID / DOWNLOAD_GATEWAY_KAFKA_GROUP_ID
	KafkaCertDir          string // dir holding user.crt/user.key/ca.crt (default /etc/kafka-cert)

	// oDownloader
	ODownloaderURL      string // ODOWNLOADER_URL / ODOWNLOADER_API_URL
	ODownloaderToken    string // ODOWNLOADER_TOKEN / ODOWNLOADER_API_TOKEN (blank -> disabled)
	ODownloaderPollSec  int    // poll interval seconds (default 15)
	ODownloaderInbox    string // inbox dir (default <PackagesRoot>/_inbox)
	ODownloaderTimeout  int    // per-job timeout minutes (default 60)
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

// Load reads configuration from the process environment.
func Load() Config {
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

		TMDBAPIKey:   env("TMDB_API_KEY"),
		TMDBLanguage: envDefault("en-US", "TMDB_LANGUAGE"),

		ChaptersDBEnabled: envBool(false, "CHAPTERSDB_ENABLED"),
		ChaptersDBBaseURL: envDefault("https://chaptersdb.com", "CHAPTERSDB_BASE_URL"),

		DownloadGatewayURL: env("DOWNLOAD_GATEWAY_URL"),

		DownloadEventsEnabled: envBool(false, "DOWNLOAD_GATEWAY_EVENTS_ENABLED"),
		KafkaBrokers:          env("KAFKA_BROKERS"),
		KafkaGroupID:          envDefault("stube-katalog-manager", "KAFKA_GROUP_ID", "DOWNLOAD_GATEWAY_KAFKA_GROUP_ID"),
		KafkaCertDir:          envDefault("/etc/kafka-cert", "KAFKA_CERT_DIR"),

		ODownloaderURL:     env("ODOWNLOADER_URL", "ODOWNLOADER_API_URL"),
		ODownloaderToken:   env("ODOWNLOADER_TOKEN", "ODOWNLOADER_API_TOKEN"),
		ODownloaderPollSec: envInt(15, "ODOWNLOADER_POLL_SEC"),
		ODownloaderInbox:   envDefault(packages+"/_inbox", "ODOWNLOADER_INBOX"),
		ODownloaderTimeout: envInt(60, "ODOWNLOADER_TIMEOUT_MIN"),
	}
	return c
}

// TMDBEnabled reports whether TMDB enrichment is configured.
func (c Config) TMDBEnabled() bool { return c.TMDBAPIKey != "" }

// DownloadGatewayEnabled reports whether the command side is configured.
func (c Config) DownloadGatewayEnabled() bool { return c.DownloadGatewayURL != "" }

// ODownloaderEnabled reports whether trailer ingestion is configured.
func (c Config) ODownloaderEnabled() bool { return c.ODownloaderURL != "" && c.ODownloaderToken != "" }
