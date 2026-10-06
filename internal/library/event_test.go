package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An event is recorded in a folder of its own, named by its moment, its id's
// first eight characters and its kind: event.json and the checksums that
// list exactly it. Recorded again, nothing changes; found again by its id.
func TestAnEventIsRecordedOnce(t *testing.T) {
	item := t.TempDir()
	reason := "originals are not kept: the package is the record"
	ev := Event{ID: "1f2e3d4c-0000-4000-8000-000000000001", At: time.Date(2026, 10, 6, 10, 30, 0, 750, time.UTC),
		By: "katalog-manager (library.originals=delete-after-package)", Kind: EventOriginalDeleted,
		VersionID: "9a2e0000-0000-4000-8000-000000000002", SourceID: "0b6c0000-0000-4000-8000-000000000003",
		Reason: &reason, Accepted: []string{"maxAudioChannels", "surround"}}
	dir, err := WriteEvent(item, ev)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(item, "events", "20261006T103000Z-1f2e3d4c-original-deleted"); dir != want {
		t.Errorf("the folder: %s, want %s", dir, want)
	}
	want := `{
  "schema": "zaentrum.library.event/2",
  "eventId": "1f2e3d4c-0000-4000-8000-000000000001",
  "at": "2026-10-06T10:30:00Z",
  "by": "katalog-manager (library.originals=delete-after-package)",
  "kind": "original-deleted",
  "versionId": "9a2e0000-0000-4000-8000-000000000002",
  "sourceId": "0b6c0000-0000-4000-8000-000000000003",
  "reason": "originals are not kept: the package is the record",
  "accepted": [
    "maxAudioChannels",
    "surround"
  ]
}
`
	b, _ := os.ReadFile(filepath.Join(dir, EventFile))
	if string(b) != want {
		t.Errorf("event.json:\n%s\nwant:\n%s", b, want)
	}
	sums, _ := os.ReadFile(filepath.Join(dir, SumsFile))
	if string(sums) != SHA256([]byte(want))+"  event.json\n" {
		t.Errorf("checksums: %q", sums)
	}
	ev.Reason = nil
	if again, err := WriteEvent(item, ev); err != nil || again != dir {
		t.Fatalf("again: %s, %v", again, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, EventFile)); string(b) != want {
		t.Error("a recorded event was written again")
	}
	if found, err := FindEvent(item, ev.ID, EventOriginalDeleted); err != nil || found != dir {
		t.Errorf("found: %s, %v", found, err)
	}
	if found, err := FindEvent(item, "1f2e3d4c-0000-4000-8000-000000000009", EventOriginalDeleted); err != nil || found != "" {
		t.Errorf("another id with the same eight characters found: %s, %v", found, err)
	}

	// The others: a supersession names its successor, a removal its version,
	// an extra's its extra; none accepts anything.
	sup := Event{ID: "2a2a2a2a-0000-4000-8000-000000000004", At: ev.At.Add(time.Hour), By: "katalog-manager",
		Kind: EventPackageSuperseded, VersionID: ev.VersionID, PackageID: "4f1d0000-0000-4000-8000-000000000005",
		Successor: &Successor{VersionID: "77c10000-0000-4000-8000-000000000006", PackageID: "88c10000-0000-4000-8000-000000000007"}}
	if got := string(must(Encode(sup.Doc()))); got != `{
  "schema": "zaentrum.library.event/2",
  "eventId": "2a2a2a2a-0000-4000-8000-000000000004",
  "at": "2026-10-06T11:30:00Z",
  "by": "katalog-manager",
  "kind": "package-superseded",
  "versionId": "9a2e0000-0000-4000-8000-000000000002",
  "packageId": "4f1d0000-0000-4000-8000-000000000005",
  "supersededBy": {
    "versionId": "77c10000-0000-4000-8000-000000000006",
    "packageId": "88c10000-0000-4000-8000-000000000007"
  }
}
` {
		t.Errorf("package-superseded:\n%s", got)
	}
	x := Event{ID: "3b3b3b3b-0000-4000-8000-000000000008", At: ev.At, Kind: EventExtraRemoved, ExtraID: "16aa0000-0000-4000-8000-000000000009"}
	if got := string(must(Encode(x.Doc()))); got != "{\n  \"schema\": \"zaentrum.library.event/2\",\n  \"eventId\": \"3b3b3b3b-0000-4000-8000-000000000008\",\n"+
		"  \"at\": \"2026-10-06T10:30:00Z\",\n  \"kind\": \"extra-removed\",\n  \"extraId\": \"16aa0000-0000-4000-8000-000000000009\"\n}\n" {
		t.Errorf("extra-removed:\n%s", got)
	}
	if _, err := WriteEvent(item, Event{ID: "nope", At: ev.At, Kind: "note"}); err == nil {
		t.Error("an event without a valid id was recorded")
	}
	events, err := ReadEvents(item)
	if err != nil || len(events) != 1 {
		t.Errorf("read: %d events, %v", len(events), err)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
