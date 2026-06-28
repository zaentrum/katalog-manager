package downloads

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	kafka "github.com/segmentio/kafka-go"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The four explicit topics the read-side consumer subscribes to. The Java
// listener uses topicPattern "stube\\.download\\.client\\..*"; kafka-go has no
// regex topic support, so we subscribe to the four concrete topic names that
// the gateway emits (events.go). The event kind is the last dot-segment.
var downloadTopics = []string{
	"stube.download.client.started",
	"stube.download.client.progress",
	"stube.download.client.completed",
	"stube.download.client.failed",
}

// Consumer is the read side of the download CQRS split: it consumes the
// gateway's stube.download.client.* events and projects them into the
// downloadjobs read model. It mirrors DownloadEventConsumer +
// DownloadKafkaConfig.
//
// Defensive by design (DownloadKafkaConfig's autoStartup=false intent): if the
// brokers are unset or the mTLS PEM material is missing/unreadable, Run logs and
// returns nil rather than crashing the process. main only starts it when
// cfg.DownloadEventsEnabled is true, but Run is safe to call regardless.
type Consumer struct {
	st  *store.Store
	cfg config.Config
}

// NewConsumer builds the read-side projector. It opens no resources; Kafka is
// dialed lazily in Run.
func NewConsumer(st *store.Store, cfg config.Config) *Consumer {
	return &Consumer{st: st, cfg: cfg}
}

// Run consumes download events until ctx is cancelled, projecting each into the
// downloadjobs read model. It returns nil when ctx is cancelled, and nil
// (without consuming) when the consumer cannot start because brokers or the
// mTLS certs are missing — matching the Java "stay healthy, just inactive"
// behaviour. Per-message errors are logged and swallowed (poison-message safe),
// never stalling the partition.
func (c *Consumer) Run(ctx context.Context) error {
	brokers := splitBrokers(c.cfg.KafkaBrokers)
	if len(brokers) == 0 {
		log.Printf("download-event consumer: no Kafka brokers configured; consumer will NOT start (service stays healthy)")
		return nil
	}

	tlsCfg, err := loadPEMTLS(c.cfg.KafkaCertDir)
	if err != nil {
		// Enabled but the cert isn't mounted/readable — stay up, just inactive.
		log.Printf("download-event consumer: Kafka client certs missing/unreadable in %s (%v); "+
			"consumer will NOT start (service stays healthy). Mount the cert and restart.",
			c.cfg.KafkaCertDir, err)
		return nil
	}

	dialer := &kafka.Dialer{
		Timeout:   10 * time.Second,
		DualStack: true,
		TLS:       tlsCfg,
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     brokers,
		GroupID:     c.cfg.KafkaGroupID,
		GroupTopics: downloadTopics,
		Dialer:      dialer,
		StartOffset: kafka.FirstOffset, // auto.offset.reset=earliest
	})
	defer reader.Close()

	log.Printf("download-event consumer active (brokers=%s, group=%s) projecting stube.download.client.* -> downloadjobs",
		c.cfg.KafkaBrokers, c.cfg.KafkaGroupID)

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // graceful shutdown
			}
			// Transient read error — log and keep going.
			log.Printf("download-event consumer: read error: %v", err)
			continue
		}
		c.handle(ctx, msg.Topic, msg.Value)
	}
}

// handle projects a single event. Any error is logged and swallowed so a poison
// message never stalls the partition (mirrors DownloadEventConsumer.onEvent).
func (c *Consumer) handle(ctx context.Context, topic string, payload []byte) {
	if len(payload) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("download-event consumer: recovered from panic projecting %s: %v", topic, r)
		}
	}()

	var e map[string]json.RawMessage
	if err := json.Unmarshal(payload, &e); err != nil {
		log.Printf("download-event consumer: failed to parse event from %s: %v", topic, err)
		return
	}

	kind := topic
	if i := strings.LastIndex(topic, "."); i >= 0 {
		kind = topic[i+1:]
	}

	adapter := text(e, "adapter")
	clientID := text(e, "client_id")
	if adapter == nil || clientID == nil {
		log.Printf("download-event consumer: dropping %s event with no adapter/client_id", kind)
		return
	}

	up := store.DownloadUpsert{
		ID:          derivedID(*adapter, *clientID),
		Adapter:     *adapter,
		ClientJobID: *clientID,
	}

	switch kind {
	case "started":
		// state=queued; title, wanted_item_id, size_bytes; started_at -> startedat + lasteventat.
		ts := tsMs(e, "started_at")
		up.State = "queued"
		zero := 0.0
		up.ProgressPct = &zero // Java upsertCore inserts progressPct=0 for a fresh queued row
		up.Title = text(e, "title")
		up.WantedItemID = text(e, "wanted_item_id")
		up.SizeBytes = longOrNil(e, "size_bytes")
		up.StartedAt = ts
		up.LastEventAt = ts
	case "progress":
		// state defaults to downloading; sticky terminal handled in the store SQL.
		state := "downloading"
		if s := text(e, "state"); s != nil {
			state = *s
		}
		now := tsMs(e, "emitted_at")
		up.State = state
		up.ProgressPct = doubleOrZeroPtr(e, "progress_pct")
		up.DownloadedBytes = longOrZeroPtr(e, "downloaded_bytes")
		up.SizeBytes = longOrNil(e, "size_bytes")
		up.SpeedBps = longOrNil(e, "speed_bps")
		up.EtaSec = intOrNil(e, "eta_sec")
		up.LastEventAt = now
	case "completed":
		// state=completed, pct=100, files (raw JSON array text, default "[]"),
		// wanted_item_id + size_bytes coalesced; completed_at -> completedat + lasteventat.
		now := tsMs(e, "completed_at")
		files := "[]"
		if raw, ok := e["files"]; ok && len(raw) > 0 {
			files = string(raw)
		}
		pct := 100.0
		up.State = "completed"
		up.ProgressPct = &pct
		up.WantedItemID = text(e, "wanted_item_id")
		up.SizeBytes = longOrNil(e, "size_bytes")
		up.Files = &files
		up.CompletedAt = now
		up.LastEventAt = now
	case "failed":
		// state=failed, error -> errormessage; failed_at -> lasteventat.
		now := tsMs(e, "failed_at")
		up.State = "failed"
		up.ErrorMessage = text(e, "error")
		up.LastEventAt = now
	default:
		// Unknown kinds ignored.
		return
	}

	if err := c.st.UpsertDownloadJob(ctx, up); err != nil {
		log.Printf("download-event consumer: failed to project %s event for %s:%s: %v",
			kind, *adapter, *clientID, err)
	}
}

// derivedID reproduces Java UUID.nameUUIDFromBytes(("adapter:clientId").bytes):
// an MD5-based name-based UUID (version 3, no namespace).
func derivedID(adapter, clientID string) string {
	h := md5.Sum([]byte(adapter + ":" + clientID))
	h[6] = (h[6] & 0x0f) | 0x30 // version 3
	h[8] = (h[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// loadPEMTLS reads user.crt/user.key/ca.crt from dir and builds a native-PEM
// mTLS config (security.protocol=SSL, ssl.keystore.type=PEM equivalent). Returns
// an error if any file is missing/unreadable so the caller can stay inactive.
func loadPEMTLS(dir string) (*tls.Config, error) {
	certPath := filepath.Join(dir, "user.crt")
	keyPath := filepath.Join(dir, "user.key")
	caPath := filepath.Join(dir, "ca.crt")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", caPath, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca.crt contains no valid certificate")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}, nil
}

// splitBrokers turns a comma-separated bootstrap list into a slice, dropping blanks.
func splitBrokers(s string) []string {
	var out []string
	for _, b := range strings.Split(s, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// ----------------------------------------------------------------- JSON helpers
// These mirror DownloadEventConsumer's text/longOrNull/intOrNull/doubleOrZero/
// tsMs: snake_case JSON, treating empty strings as absent (for text), and
// epoch-millis numeric timestamps falling back to now() when absent/<=0.

func text(e map[string]json.RawMessage, f string) *string {
	raw, ok := e[f]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	if s == "" {
		return nil
	}
	return &s
}

func longOrNil(e map[string]json.RawMessage, f string) *int64 {
	raw, ok := e[f]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return &n
}

func longOrZeroPtr(e map[string]json.RawMessage, f string) *int64 {
	if v := longOrNil(e, f); v != nil {
		return v
	}
	z := int64(0)
	return &z
}

func intOrNil(e map[string]json.RawMessage, f string) *int32 {
	raw, ok := e[f]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var n int32
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return &n
}

func doubleOrZeroPtr(e map[string]json.RawMessage, f string) *float64 {
	raw, ok := e[f]
	if ok && !isJSONNull(raw) {
		var d float64
		if err := json.Unmarshal(raw, &d); err == nil {
			return &d
		}
	}
	z := 0.0
	return &z
}

// tsMs reads an epoch-millis numeric field, falling back to now() when absent or
// <= 0 (DownloadEventConsumer.tsMs). Always returns a non-nil time.
func tsMs(e map[string]json.RawMessage, f string) *time.Time {
	var ms int64
	if raw, ok := e[f]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &ms)
	}
	var t time.Time
	if ms > 0 {
		t = time.UnixMilli(ms)
	} else {
		t = time.Now()
	}
	return &t
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
