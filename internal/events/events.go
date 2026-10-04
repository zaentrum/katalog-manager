// Package events is the catalog pipeline's Kafka trigger spine. Each stage
// handoff is a Kafka event keyed by item_id (per-item ordering), replacing the
// old poll/ticker/DB-promotion triggers:
//
//	scan INSERT  -> stube.catalog.item.discovered -> enricher
//	enrich done  -> stube.catalog.item.enriched   -> analyzer
//	analyze done -> stube.catalog.item.analyzed    -> transcoder
//	transcode    -> stube.catalog.item.transcoded  -> packager
//
// katalog-manager PRODUCES discovered + enriched and CONSUMES discovered (the
// enricher). The Python workers consume/produce the rest. The DB processing-step
// rows still record STATE (the Activity monitor reads them); events only drive
// the HANDOFF. Delivery is at-least-once — consumers are idempotent via the
// unique (item_id, step) index on com_nalet_katalog_itemprocessingsteps.
//
// TLS is optional: mTLS when KAFKA_CERT_DIR holds user.crt/user.key/ca.crt (the
// prod Strimzi profile), PLAINTEXT otherwise (the bundled single-node demo
// broker at kafka:9092). Everything is defensive — no brokers / unreadable certs
// => a logged no-op, never a crash.
package events

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// Catalog pipeline topics, named <prefix><domain>.<entity>.<event>. The prefix
// is CONFIGURABLE (KAFKA_TOPIC_PREFIX, default "stube.") so multiple platform
// instances — e.g. beta and prod — can share one Kafka cluster without topic
// collisions. Call Configure once at boot, before any producer/consumer starts.
var (
	TopicDiscovered = "stube.catalog.item.discovered"
	TopicEnriched   = "stube.catalog.item.enriched"
	TopicAnalyzed   = "stube.catalog.item.analyzed"
	TopicTranscoded = "stube.catalog.item.transcoded"
	// TopicPackaged marks the pipeline's END: the packaged (playable) asset is
	// recorded in the catalog. Emitted by the packaging-complete REST sink — the
	// packager itself stays Kafka-producer-free. Nothing downstream consumes it
	// as a trigger; it exists for live-refresh fan-outs ("became watchable").
	TopicPackaged = "stube.catalog.item.packaged"
	// TopicRemoved announces a catalog deletion (fan-out only, like packaged):
	// live-refresh surfaces drop the item without a reload.
	TopicRemoved = "stube.catalog.item.removed"
)

// Configure derives the topic names from a tenant prefix. Blank falls back to
// "stube."; a missing trailing dot is added. Not goroutine-safe by design —
// boot-time only.
func Configure(prefix string) {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = "stube."
	}
	if !strings.HasSuffix(p, ".") {
		p += "."
	}
	TopicDiscovered = p + "catalog.item.discovered"
	TopicEnriched = p + "catalog.item.enriched"
	TopicAnalyzed = p + "catalog.item.analyzed"
	TopicTranscoded = p + "catalog.item.transcoded"
	TopicPackaged = p + "catalog.item.packaged"
	TopicRemoved = p + "catalog.item.removed"
}

// ItemEvent is the minimal envelope carried on the catalog topics: identity +
// the step this event unblocks + provenance. Consumers that need the media path
// or details read the DB by ItemID (notification-plus-identity, not full state).
type ItemEvent struct {
	EventID    string `json:"eventId"`
	ItemID     string `json:"itemId"`
	Type       string `json:"type,omitempty"`
	Step       string `json:"step,omitempty"`
	Status     string `json:"status,omitempty"`
	OccurredAt string `json:"occurredAt"`
	Source     string `json:"source,omitempty"`
}

// NewItemEvent stamps a fresh event (random id + now()).
func NewItemEvent(itemID string) ItemEvent {
	return ItemEvent{
		EventID:    newEventID(),
		ItemID:     itemID,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------- Producer

// Producer writes catalog events. A nil *Producer is a valid no-op (so callers
// need no nil-checks); NewProducer returns nil when no brokers are configured.
type Producer struct {
	w messageWriter
}

// messageWriter is what a Producer writes through: a *kafka.Writer.
type messageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// NewProducer builds a key-hashing writer over brokers. tlsCfg nil => PLAINTEXT.
// Returns nil (no-op producer) when brokers is empty.
func NewProducer(brokers []string, tlsCfg *tls.Config) *Producer {
	if len(brokers) == 0 {
		log.Printf("catalog events: no Kafka brokers configured; producer disabled (service stays healthy)")
		return nil
	}
	transport := kafka.DefaultTransport
	scheme := "PLAINTEXT"
	if tlsCfg != nil {
		transport = &kafka.Transport{TLS: tlsCfg}
		scheme = "SSL"
	}
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Balancer:               &kafka.Hash{}, // Key=item_id => deterministic partition => per-item ordering
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: true, // demo broker has auto-create on; prod topics are pre-provisioned
		Transport:              transport,
	}
	log.Printf("catalog events: producer active (brokers=%s, %s) topics=stube.catalog.item.*", strings.Join(brokers, ","), scheme)
	return &Producer{w: w}
}

// emitAttempts / emitBackoff bound the produce-retry loop. The bundled demo
// broker is ephemeral (emptyDir) and topics auto-create, so the FIRST produce
// after a broker restart (or a cold topic) returns UNKNOWN_TOPIC_OR_PARTITION
// while the broker creates the topic — a transient that clears within a second
// or two. Without a retry that first event is silently lost (a stuck item);
// retrying makes the pipeline self-heal across a broker restart / topic
// auto-create race. Total worst-case wait ~5.5s (0.3+0.6+..+1.8).
const emitAttempts = 6

var emitBackoff = 300 * time.Millisecond // a variable for the tests

// Emit publishes payload to topic keyed by key (the item_id), retrying transient
// produce errors (topic auto-create window, leader election) with a bounded
// backoff. A final failure is logged, never propagated — the DB step row is
// still the source of truth and a stuck item stays recoverable by re-emitting.
func (p *Producer) Emit(ctx context.Context, topic, key string, payload any) {
	if p == nil || p.w == nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("catalog events: marshal %s failed: %v", topic, err)
		return
	}
	msg := kafka.Message{Topic: topic, Key: []byte(key), Value: b}
	var lastErr error
	for attempt := 1; attempt <= emitAttempts; attempt++ {
		if lastErr = p.w.WriteMessages(ctx, msg); lastErr == nil {
			log.Printf("catalog events: emitted %s key=%s", topic, key)
			return
		}
		if ctx.Err() != nil {
			return
		}
		// AllowAutoTopicCreation means the failed write also triggers the
		// broker to create the topic; the next attempt lands once it exists.
		time.Sleep(time.Duration(attempt) * emitBackoff)
	}
	log.Printf("catalog events: emit to %s (key=%s) FAILED after %d attempts: %v", topic, key, emitAttempts, lastErr)
}

// EmitItem is the common case: an ItemEvent to a stage topic.
func (p *Producer) EmitItem(ctx context.Context, topic string, ev ItemEvent) {
	p.Emit(ctx, topic, ev.ItemID, ev)
}

// ErrNoBus is what Publish says of every event when the service has no Kafka
// brokers configured.
var ErrNoBus = errors.New("no event bus: KAFKA_BROKERS is not set")

// Enabled reports whether p writes events: brokers are configured.
func (p *Producer) Enabled() bool { return p != nil && p.w != nil }

// Message is an item event and the topic it goes to.
type Message struct {
	Topic string
	Event ItemEvent
}

// Publish writes msgs, each to its topic keyed by its item, in one batch (a
// write of one event waits out the writer's batch timeout; a batch waits it
// once), and writes again the ones that fail, as Emit does. It returns one
// error per message, nil for each one written. Without brokers every message
// fails with ErrNoBus.
func (p *Producer) Publish(ctx context.Context, msgs []Message) []error {
	errs := make([]error, len(msgs))
	if !p.Enabled() {
		for i := range errs {
			errs[i] = ErrNoBus
		}
		return errs
	}
	values := make([][]byte, len(msgs))
	var pending []int
	for i, m := range msgs {
		b, err := json.Marshal(m.Event)
		if err != nil {
			errs[i] = fmt.Errorf("marshal the %s event of %s: %w", m.Topic, m.Event.ItemID, err)
			continue
		}
		values[i] = b
		pending = append(pending, i)
	}
	for attempt := 1; attempt <= emitAttempts && len(pending) > 0; attempt++ {
		batch := make([]kafka.Message, len(pending))
		for j, i := range pending {
			batch[j] = kafka.Message{Topic: msgs[i].Topic, Key: []byte(msgs[i].Event.ItemID), Value: values[i]}
		}
		err := p.w.WriteMessages(ctx, batch...)
		var werr kafka.WriteErrors
		var failed []int
		for j, i := range pending {
			switch {
			case err == nil:
				errs[i] = nil
			case errors.As(err, &werr) && len(werr) == len(pending):
				if errs[i] = werr[j]; werr[j] != nil {
					failed = append(failed, i)
				}
			default:
				errs[i] = err
				failed = append(failed, i)
			}
		}
		pending = failed
		if len(pending) == 0 || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(attempt) * emitBackoff)
	}
	written := 0
	for _, e := range errs {
		if e == nil {
			written++
		}
	}
	if written < len(msgs) {
		log.Printf("catalog events: published %d of %d events; the first failure: %v", written, len(msgs), firstErr(errs))
	} else {
		log.Printf("catalog events: published %d events", written)
	}
	return errs
}

func firstErr(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Close flushes and closes the underlying writer.
func (p *Producer) Close() {
	if p != nil && p.w != nil {
		_ = p.w.Close()
	}
}

// ---------------------------------------------------------------- Consumer

// Handler processes one message. Returning an error logs it (poison-message
// safe) but does not stall the partition — offsets auto-commit.
type Handler func(ctx context.Context, topic string, ev ItemEvent) error

// Consume runs a consumer-group reader over topics until ctx is cancelled,
// invoking h per message. It starts at the EARLIEST offset (a fresh group works
// through history — right for pipeline stages, which must not miss items). TLS
// is optional (see package doc). Defensive: no brokers / unreadable certs =>
// logged no-op return (service stays healthy).
func Consume(ctx context.Context, brokers []string, certDir, groupID string, topics []string, h Handler) {
	consume(ctx, brokers, certDir, groupID, topics, h, kafka.FirstOffset)
}

// ConsumeLatest is Consume starting at the LATEST offset: a fresh group sees
// only NEW events. Right for live-tail fan-outs (the SSE catalog stream), where
// replaying history would blast stale notifications at every boot.
func ConsumeLatest(ctx context.Context, brokers []string, certDir, groupID string, topics []string, h Handler) {
	consume(ctx, brokers, certDir, groupID, topics, h, kafka.LastOffset)
}

func consume(ctx context.Context, brokers []string, certDir, groupID string, topics []string, h Handler, startOffset int64) {
	if len(brokers) == 0 {
		log.Printf("catalog events consumer(%s): no Kafka brokers; not starting (service stays healthy)", groupID)
		return
	}
	var dialer *kafka.Dialer
	scheme := "PLAINTEXT"
	if tlsCfg, err := MaybeTLS(certDir); err != nil {
		log.Printf("catalog events consumer(%s): certs present but unreadable in %s (%v); not starting", groupID, certDir, err)
		return
	} else if tlsCfg != nil {
		dialer = &kafka.Dialer{Timeout: 10 * time.Second, DualStack: true, TLS: tlsCfg}
		scheme = "SSL"
	} else {
		dialer = &kafka.Dialer{Timeout: 10 * time.Second, DualStack: true}
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     brokers,
		GroupID:     groupID,
		GroupTopics: topics,
		Dialer:      dialer,
		StartOffset: startOffset,
	})
	defer reader.Close()
	log.Printf("catalog events consumer(%s) active (%s) topics=%s", groupID, scheme, strings.Join(topics, ","))

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("catalog events consumer(%s): read error: %v", groupID, err)
			continue
		}
		var ev ItemEvent
		if err := json.Unmarshal(msg.Value, &ev); err != nil {
			log.Printf("catalog events consumer(%s): bad event on %s: %v", groupID, msg.Topic, err)
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("catalog events consumer(%s): recovered from panic on %s: %v", groupID, msg.Topic, r)
				}
			}()
			if err := h(ctx, msg.Topic, ev); err != nil {
				log.Printf("catalog events consumer(%s): handler error on %s (item=%s): %v", groupID, msg.Topic, ev.ItemID, err)
			}
		}()
	}
}

// ---------------------------------------------------------------- TLS

// MaybeTLS returns (nil, nil) when the cert dir has no user.crt (PLAINTEXT),
// a populated *tls.Config when the full mTLS triple is present, or an error when
// certs are partially present but unreadable (so the caller can stay inactive).
func MaybeTLS(dir string) (*tls.Config, error) {
	certPath := filepath.Join(dir, "user.crt")
	if _, err := os.Stat(certPath); err != nil {
		return nil, nil // no client cert => plaintext
	}
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
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool}, nil
}

// SplitBrokers turns a comma-separated bootstrap list into a slice, dropping blanks.
func SplitBrokers(s string) []string {
	var out []string
	for _, b := range strings.Split(s, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ev-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
