package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The kinds of event katalog-manager writes into an item's folder.
const (
	EventOriginalDeleted   = "original-deleted"
	EventVersionRemoved    = "version-removed"
	EventPackageSuperseded = "package-superseded"
	EventExtraRemoved      = "extra-removed"
)

// EventFile is an event's record, written once in a folder of its own.
const EventFile = "event.json"

// Successor is the package that takes over from a superseded one.
type Successor struct{ VersionID, PackageID string }

// Event is a fact that arose after an item's records were written: its id,
// its moment (to the second: the folder is named by it), who acted, its kind
// and the records it names.
type Event struct {
	ID        string
	At        time.Time
	By        string
	Kind      string
	VersionID string
	SourceID  string
	PackageID string
	Successor *Successor // package-superseded
	ExtraID   string     // extra-removed
	Reason    *string
	Accepted  []string // original-deleted: what the deletion gave up; written also when empty
}

// Timestamp writes t as a record does: RFC 3339, UTC, to the second.
func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// Doc is the event's record, event.json.
func (ev Event) Doc() Doc {
	d := Doc{{"schema", "zaentrum.library.event/2"}, {"eventId", ev.ID}, {"at", Timestamp(ev.At)}}
	if ev.By != "" {
		d = append(d, Field{"by", ev.By})
	}
	d = append(d, Field{"kind", ev.Kind})
	for _, f := range []Field{{"versionId", ev.VersionID}, {"sourceId", ev.SourceID}, {"packageId", ev.PackageID}} {
		if f.Value != "" {
			d = append(d, f)
		}
	}
	if ev.Successor != nil {
		d = append(d, Field{"supersededBy", Doc{{"versionId", ev.Successor.VersionID}, {"packageId", ev.Successor.PackageID}}})
	}
	if ev.ExtraID != "" {
		d = append(d, Field{"extraId", ev.ExtraID})
	}
	if ev.Reason != nil {
		d = append(d, Field{"reason", *ev.Reason})
	}
	if ev.Kind == EventOriginalDeleted {
		accepted := make([]any, 0, len(ev.Accepted))
		for _, a := range ev.Accepted {
			accepted = append(accepted, a)
		}
		d = append(d, Field{"accepted", accepted})
	}
	return d
}

// Dir is the event's folder in the item folder itemDir.
func (ev Event) Dir(itemDir string) string { return EventDir(itemDir, ev.At, ev.ID, ev.Kind) }

// WriteEvent records ev in the item folder itemDir: its folder gets
// event.json, then the checksums.sha256 that lists exactly it, which says the
// event is recorded. A folder whose checksums are written holds the event
// already and is left as it is; one without is written again, the same
// bytes. It answers the folder.
func WriteEvent(itemDir string, ev Event) (string, error) {
	if !ValidID(ev.ID) {
		return "", fmt.Errorf("an event's id is a lower-case UUID, not %q", ev.ID)
	}
	if ev.At.IsZero() {
		return "", errors.New("an event needs its moment")
	}
	dir := ev.Dir(itemDir)
	if _, err := os.Stat(filepath.Join(dir, SumsFile)); err == nil {
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b, err := Encode(ev.Doc())
	if err != nil {
		return "", err
	}
	if err := WriteCovered(dir, map[string][]byte{EventFile: b}); err != nil {
		return "", fmt.Errorf("record the %s event in %s: %w", ev.Kind, itemDir, err)
	}
	return dir, nil
}

// FindEvent finds the folder of the event id of kind among itemDir's events,
// recorded or not: "" when there is none.
func FindEvent(itemDir, id, kind string) (string, error) {
	if len(id) < 8 {
		return "", fmt.Errorf("no event id: %q", id)
	}
	entries, err := os.ReadDir(filepath.Join(itemDir, "events"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-"+id[:8]+"-"+kind) {
			dir := filepath.Join(itemDir, "events", e.Name())
			b, err := os.ReadFile(filepath.Join(dir, EventFile))
			if err != nil {
				continue
			}
			if d, err := DecodeDoc(b); err == nil {
				if v, _ := d.Get("eventId"); v == id {
					return dir, nil
				}
			}
		}
	}
	return "", nil
}

// ReadEvents reads the events recorded in the item folder itemDir, in the
// order of their folders (their moment): those whose checksums are written.
func ReadEvents(itemDir string) ([]Doc, error) {
	entries, err := os.ReadDir(filepath.Join(itemDir, "events"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Doc
	for _, e := range entries {
		dir := filepath.Join(itemDir, "events", e.Name())
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, SumsFile)); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, EventFile))
		if err != nil {
			return nil, err
		}
		d, err := DecodeDoc(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		out = append(out, d)
	}
	return out, nil
}
