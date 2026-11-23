package library

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The forms the gate takes a source record, its stored probe and a package
// in: those the platform's writers write, and no other. Every writer of the
// three is the record logic's json_bytes (the schemas' tools/libv2_records.py,
// which the packager vendors and the schemas' migration tools share):
// json.dumps(doc, indent=2, ensure_ascii=False) + "\n", in UTF-8; the probe
// is ffprobe's output loaded and dumped so (scrub_probe). katalog-manager
// writes none of them. So a text is as Python's encoder writes it (its quote,
// backslash and control characters escaped as \", \\, \n, \r, \t, \b, \f and
// \u00xx, nothing else, no other character escaped); a whole number is in
// digits, a minus its only sign, never -0, within 64 bits; a float is as
// Python's repr writes it (the shortest digits that read back, positional
// from 1e-4 to 1e16, else 1e-05 or 1e+16: those are the only exponents a
// writer writes); and no key has space about it (Python's str.strip). A
// record in any other form holds, with the reason: a form nobody writes is
// not worth reading, and every unusual form has been a place where two
// readers differ. The layout (the indent, the line breaks between tokens)
// is not held to: no reader reads it apart.

// readWritten reads the record at path (LoadRecord) in a form its writers
// write (writtenForm), and, of a source record or a package (rules), as the
// validator's own rules of its texts and keys take it (textRules). A record
// that is JSON in any other form - its bytes no UTF-8, a key given twice, a
// number no reader holds among them - is no failure of the library's: it
// says so as unwritten, which the gate keeps the original for, saying why.
// One that is no JSON can't be read, as ever.
func readWritten(path string, rules bool) (Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := LoadRecord(b)
	if err != nil {
		if _, no := DecodeDoc(b); no != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return nil, &unwritten{path, "it is in no form the platform's writers write: " + err.Error()}
	}
	if why := writtenForm(b); why != "" {
		return nil, &unwritten{path, "it is in no form the platform's writers write: " + why}
	}
	if rules {
		if why := textRules(d, ""); why != "" {
			return nil, &unwritten{path, "the contract's validator refuses it: " + why}
		}
	}
	return d, nil
}

// unwritten is a record at path in no form its writers write, or one the
// validator's own rules take for none (readWritten), and why.
type unwritten struct{ path, why string }

func (e *unwritten) Error() string { return e.path + ": " + e.why }

// unwrittenWhy is why err says a record is unwritten, "" of any other err.
func unwrittenWhy(err error) string {
	var u *unwritten
	if errors.As(err, &u) {
		return u.why
	}
	return ""
}

// writtenForm says why the JSON text b (one LoadRecord reads) is in no form
// the platform's writers write, "" when it is in theirs.
func writtenForm(b []byte) string {
	if !utf8.Valid(b) {
		return "its bytes are no UTF-8"
	}
	for i := 0; i < len(b); {
		switch c := b[i]; {
		case c == '"':
			j := i + 1
			for j < len(b) && b[j] != '"' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			j++ // past the closing quote
			if j > len(b) {
				return "a text of it is not closed"
			}
			raw := b[i:j]
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return "a text of it can't be read: " + err.Error()
			}
			var w bytes.Buffer
			writeString(&w, s)
			if !bytes.Equal(w.Bytes(), raw) {
				return fmt.Sprintf("a text of it is written %s, which its writers write %s", clipForm(raw), clipForm(w.Bytes()))
			}
			if k := j; isKeyAt(b, k) && pyStrip(s) != s {
				return fmt.Sprintf("its key %q has space about it", s)
			}
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < len(b) && strings.IndexByte("+-.eE0123456789", b[j]) >= 0 {
				j++
			}
			if why := numberForm(string(b[i:j])); why != "" {
				return why
			}
			i = j
		default:
			i++
		}
	}
	return ""
}

// isKeyAt reports whether the text that ends at j is a key: a colon is the
// next of its tokens.
func isKeyAt(b []byte, j int) bool {
	for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
		j++
	}
	return j < len(b) && b[j] == ':'
}

// numberForm says why the number t is in no form a writer writes, "" when
// it is in one.
func numberForm(t string) string {
	if !strings.ContainsAny(t, ".eE") {
		if t == "-0" {
			return "it gives -0, a whole number no writer writes"
		}
		if _, err := strconv.ParseInt(t, 10, 64); err != nil {
			return fmt.Sprintf("it gives %s, a whole number past 64 bits", clipForm([]byte(t)))
		}
		return ""
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return fmt.Sprintf("it gives %s, a number past what a float holds", clipForm([]byte(t)))
	}
	if s, err := pyFloat(f); err != nil || s != t {
		return fmt.Sprintf("a number of it is written %s, which its writers write %s", clipForm([]byte(t)), s)
	}
	return ""
}

// clipForm is b to say in a reason: at most 40 bytes of it, quoted.
func clipForm(b []byte) string {
	if len(b) > 40 {
		return strconv.Quote(string(b[:40])) + "..."
	}
	return strconv.Quote(string(b))
}

// The validator's own rules of a record's texts and keys, beside its schema
// (validate-library-v2.py's schema_ok): a text under an id-like key
// (validatorIDKeys) with a line break anywhere in it, or under any key but
// the free-text ones (validatorFreeText) ending in one, and a key with space
// about it, keep it for none. A text in a list is under the list's key.
var (
	validatorIDKeys = map[string]bool{"schema": true, "itemId": true, "seriesId": true, "sourceId": true,
		"versionId": true, "packageId": true, "eventId": true, "personId": true, "file": true, "path": true, "dir": true,
		"vttPath": true, "manifestPath": true, "originalFile": true, "name": true, "language": true, "type": true,
		"kind": true, "tmdbMovie": true, "tmdbTv": true, "tmdbSeason": true, "tmdbEpisode": true, "tmdbCollection": true,
		"imdb": true, "tvdb": true, "tmdbPerson": true, "sha256": true, "qh1": true, "ref": true}
	validatorFreeText = map[string]bool{"overview": true, "tagline": true, "notes": true, "note": true, "detail": true,
		"reason": true}
)

// textRules says why the validator's own rules take the decoded record v,
// under the key key, for none, "" when they take it.
func textRules(v any, key string) string {
	switch x := v.(type) {
	case Doc:
		for _, f := range x {
			if pyStrip(f.Key) != f.Key {
				return fmt.Sprintf("its key %q has space about it", f.Key)
			}
			if why := textRules(f.Value, f.Key); why != "" {
				return why
			}
		}
	case []any:
		for _, e := range x {
			if why := textRules(e, key); why != "" {
				return why
			}
		}
	case string:
		if (validatorIDKeys[key] && strings.ContainsAny(x, "\r\n")) ||
			(!validatorFreeText[key] && strings.TrimRight(x, "\r\n") != x) {
			return fmt.Sprintf("its %s %q contains a line break", key, x)
		}
	}
	return ""
}
