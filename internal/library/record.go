package library

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A record is written as the library's tools write one (and as Python's
// json.dumps(doc, indent=2, ensure_ascii=False) does): two-space indent, UTF-8
// as it is, keys in the order the record gives them, and one trailing
// newline. The same record makes the same bytes, whichever writer wrote it.

// Doc is a JSON object whose keys keep their order.
type Doc []Field

// Field is one key of a Doc and its value.
type Field struct {
	Key   string
	Value any
}

// Get is the value of key, and whether d has it.
func (d Doc) Get(key string) (any, bool) {
	for _, f := range d {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// With is d with key set to value: in its place when d has it, else last.
func (d Doc) With(key string, value any) Doc {
	for i, f := range d {
		if f.Key == key {
			out := append(Doc(nil), d...)
			out[i].Value = value
			return out
		}
	}
	return append(append(Doc(nil), d...), Field{key, value})
}

// Without is d without key.
func (d Doc) Without(key string) Doc {
	out := make(Doc, 0, len(d))
	for _, f := range d {
		if f.Key != key {
			out = append(out, f)
		}
	}
	return out
}

// MarshalJSON writes d compactly, its keys in order.
func (d Doc) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, d, "", false); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Encode writes v as a record: indented by two spaces, one trailing newline.
// v is built of Doc, []any (or a slice of any of these), string, bool, nil,
// integers, float64 (written as Python writes a float: 8.0, 0.1, 1e-05),
// json.Number, and pointers to them.
func Encode(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, v, "\n", true); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func encode(b *bytes.Buffer, v any, nl string, indent bool) error {
	in := nl + "  "
	if !indent {
		in = ""
	}
	sep, colon := ",", ": "
	if !indent {
		colon = ":"
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case Doc:
		if len(x) == 0 {
			b.WriteString("{}")
			return nil
		}
		b.WriteByte('{')
		for i, f := range x {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(in)
			writeString(b, f.Key)
			b.WriteString(colon)
			if err := encode(b, f.Value, in, indent); err != nil {
				return err
			}
		}
		b.WriteString(nl)
		b.WriteByte('}')
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(in)
			if err := encode(b, e, in, indent); err != nil {
				return err
			}
		}
		b.WriteString(nl)
		b.WriteByte(']')
	case string:
		writeString(b, x)
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(x.String())
	case float64:
		s, err := pyFloat(x)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case float32:
		return encode(b, float64(x), nl, indent)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	default:
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Pointer:
			if rv.IsNil() {
				b.WriteString("null")
				return nil
			}
			return encode(b, rv.Elem().Interface(), nl, indent)
		case reflect.Slice:
			if rv.IsNil() {
				b.WriteString("[]")
				return nil
			}
			out := make([]any, rv.Len())
			for i := range out {
				out[i] = rv.Index(i).Interface()
			}
			return encode(b, out, nl, indent)
		}
		return fmt.Errorf("a record holds no %T", v)
	}
	return nil
}

// writeString writes s as JSON does with ensure_ascii off: every character
// as it is, but the quote, the backslash and the control characters.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(`�`)
			i++
			continue
		}
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	b.WriteByte('"')
}

// pyFloat writes f as Python's repr does: the shortest digits that read back
// as f, in positional notation with at least one digit after the point while
// its exponent is from -4 to 15, else in scientific notation.
func pyFloat(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("a record holds no %v", f)
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0", nil
		}
		return "0.0", nil
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expPart, _ := strings.Cut(e, "e")
	exp, err := strconv.Atoi(expPart)
	if err != nil {
		return "", err
	}
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp), nil
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits, nil
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0", nil
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:], nil
}

// Decode reads JSON keeping what Encode needs to write it again as it was:
// an object's keys in their order (a Doc), numbers as they are written
// (json.Number).
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			d := Doc{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := k.(string)
				if !ok {
					return nil, errors.New("an object's key is no string")
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				d = append(d, Field{key, v})
			}
			_, err := dec.Token()
			return d, err
		case '[':
			out := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			_, err := dec.Token()
			return out, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	}
	return tok, nil
}

// DecodeDoc reads a JSON object (Decode).
func DecodeDoc(data []byte) (Doc, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	d, ok := v.(Doc)
	if !ok {
		return nil, errors.New("no JSON object")
	}
	return d, nil
}

// SHA256 is the hex sha256 of b.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// SHA256File is the hex sha256 of the file at path, and its size.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// MkdirAll makes dir and its parents, as the library's writers make folders.
func MkdirAll(dir string) error { return os.MkdirAll(dir, DirMode) }

// WriteFile writes data to path as a reader never sees half of it: into a
// temporary file in its folder, synced, then renamed into place; the folder
// is made when it is missing.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := MkdirAll(dir); err != nil {
		return err
	}
	tag := make([]byte, 4)
	if _, err := rand.Read(tag); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(tag))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, FileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir syncs a folder, so a rename in it is on storage; where a folder
// cannot be synced, the rename is as durable as the filesystem makes it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// WriteRecord writes doc to path as a record (Encode, WriteFile).
func WriteRecord(path string, doc any) error {
	b, err := Encode(doc)
	if err != nil {
		return err
	}
	return WriteFile(path, b)
}

// SumsFile is the checksums file of a folder written once.
const SumsFile = "checksums.sha256"

// Checksums is a checksums.sha256 listing digests (a path relative to its
// folder: its hex sha256): "<hex>  <path>" lines, by path.
func Checksums(digests map[string]string) []byte {
	names := make([]string, 0, len(digests))
	for n := range digests {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		b.WriteString(digests[n] + "  " + n + "\n")
	}
	return b.Bytes()
}

// ReadChecksums reads a checksums.sha256: each path it lists with its hex
// digest. A line that is no "<64 hex>  <path>", or a path listed twice, is an
// error.
func ReadChecksums(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || name == "" {
			return nil, fmt.Errorf("%s: line %d is no checksum: %q", path, i+1, line)
		}
		if _, twice := out[name]; twice {
			return nil, fmt.Errorf("%s lists %s twice", path, name)
		}
		out[name] = digest
	}
	return out, nil
}

// WriteCovered writes files into dir, each once, then the checksums.sha256
// that lists exactly them: the checksums file last, so its presence says the
// folder is complete.
func WriteCovered(dir string, files map[string][]byte) error {
	digests := map[string]string{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := WriteFile(filepath.Join(dir, n), files[n]); err != nil {
			return err
		}
		digests[n] = SHA256(files[n])
	}
	return WriteFile(filepath.Join(dir, SumsFile), Checksums(digests))
}
