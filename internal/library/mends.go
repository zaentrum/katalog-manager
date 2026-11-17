package library

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MendKinds are the kinds of mend package.schema.json lists (mend.kind): a
// closed list, as a new kind is a change of the format.
var MendKinds = map[string]bool{"avsync-editlist": true}

// mendFields are the fields of a mend entry, all required and no other
// (package.schema.json, $defs/mend).
var mendFields = []string{"kind", "file", "box", "old", "new", "sha256Before", "sha256After", "at", "by"}

// mendFile is the file a mend changes: a rendition's init segment.
var mendFile = regexp.MustCompile(`^hls/[^/]+/init\.mp4$`)

var mendDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// mendBox is the box an avsync-editlist changes, and mendNew the value it
// writes: the first media edit's media_time of the track's edit list, 0.
const (
	mendBox = "moov/trak/edts/elst"
	mendNew = 0
)

// initMax is how much of an init segment a mend's check reads, as the
// schemas' validator does: a file larger is no init segment.
const initMax = 1 << 22

// mend is one entry of a package's mends: a change made to one file of the
// package after it completed, which only boxes of a rendition's init
// segment take part in. A mend writes the file's line in checksums.sha256
// again; the entries are its history, the first one's before the hash the
// file had as the package was written.
type mend struct {
	kind, file, before, after string
	old, new                  int64
}

// integer is v as a whole number.
func integer(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case float64:
		return int64(x), x == float64(int64(x))
	case int:
		return int64(x), true
	case int64:
		return x, true
	}
	return 0, false
}

// mendsOf is the entries of package.json's mends by the file each changed,
// in the order they were made, or why they are no mends: each entry of the
// fields the format lists and no other, of a kind it lists, an
// avsync-editlist the init segment of one of the package's video
// renditions, its box the edit list and its new value 0, and the entries in
// the order they were made (at). These are the schema's rules, which the
// schemas' validator holds a package to.
func mendsOf(pkg Doc) (map[string][]mend, error) {
	v, ok := pkg.Get("mends")
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("its mends is no list of entries")
	}
	video := map[string]bool{}
	if r, ok := pkg.Get("renditions"); ok {
		if rd, ok := r.(Doc); ok {
			if vs, ok := rd.Get("video"); ok {
				vl, _ := vs.([]any)
				for _, x := range vl {
					if d, ok := x.(Doc); ok {
						video[strings.TrimSuffix(str(d, "dir"), "/")] = true
					}
				}
			}
		}
	}
	out := map[string][]mend{}
	var last time.Time
	for i, x := range list {
		e, ok := x.(Doc)
		if !ok {
			return nil, fmt.Errorf("its mends[%d] is no entry", i)
		}
		for _, f := range e {
			if !contains(mendFields, f.Key) {
				return nil, fmt.Errorf("its mends[%d] has the field %q, which the format does not list", i, f.Key)
			}
		}
		for _, k := range mendFields {
			if _, ok := e.Get(k); !ok {
				return nil, fmt.Errorf("its mends[%d] has no %s", i, k)
			}
		}
		m := mend{kind: str(e, "kind"), file: str(e, "file"), before: str(e, "sha256Before"), after: str(e, "sha256After")}
		ov, _ := e.Get("old")
		nv, _ := e.Get("new")
		old, okOld := integer(ov)
		nw, okNew := integer(nv)
		m.old, m.new = old, nw
		at, err := time.Parse(time.RFC3339, str(e, "at"))
		switch {
		case !MendKinds[m.kind]:
			return nil, fmt.Errorf("its mends[%d] is of the kind %q, which the format does not list", i, m.kind)
		case !mendFile.MatchString(m.file):
			return nil, fmt.Errorf("its mends[%d] changes %q, which is no rendition's init segment (hls/<rendition>/init.mp4)", i, m.file)
		case !mendDigest.MatchString(m.before) || !mendDigest.MatchString(m.after):
			return nil, fmt.Errorf("its mends[%d] does not name the hashes before and after", i)
		case !okOld || !okNew:
			return nil, fmt.Errorf("its mends[%d] gives no whole numbers for old and new", i)
		case str(e, "by") == "":
			return nil, fmt.Errorf("its mends[%d] does not say who made it", i)
		case err != nil:
			return nil, fmt.Errorf("its mends[%d] does not say when it was made", i)
		case i > 0 && at.Before(last):
			return nil, fmt.Errorf("its mends[%d] is dated before the one before it: mends are in the order they were made", i)
		}
		last = at
		if m.kind == "avsync-editlist" {
			switch {
			case str(e, "box") != mendBox:
				return nil, fmt.Errorf("its mends[%d] changes the box %q, and an avsync-editlist changes %s", i, str(e, "box"), mendBox)
			case m.new != mendNew:
				return nil, fmt.Errorf("its mends[%d] sets the edit to %d, and an avsync-editlist sets it to 0", i, m.new)
			case !video[strings.TrimSuffix(m.file, "/init.mp4")]:
				return nil, fmt.Errorf("its mends[%d] changes the edit list of %s, the init segment of no video rendition of the package", i, m.file)
			}
		}
		out[m.file] = append(out[m.file], m)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// mendsProblem is what is wrong with the entries of a package's mends
// against its checksums (listed: path → hex digest), "" when nothing is: a
// mend writes the mended file's line in checksums.sha256 again, so each
// file's entries run with no gap — each made to the bytes the one before left
// — and the last one ends at the digest the checksums list now.
func mendsProblem(mended map[string][]mend, listed map[string]string) string {
	for _, f := range mendedFiles(mended) {
		entries := mended[f]
		digest, ok := listed[f]
		if !ok {
			return fmt.Sprintf("its mends name %s, which checksums.sha256 does not list", f)
		}
		for i := 1; i < len(entries); i++ {
			if entries[i].before != entries[i-1].after {
				return fmt.Sprintf("its mends of %s have a gap: one was made to %s, and the one before left %s",
					f, entries[i].before, entries[i-1].after)
			}
		}
		if last := entries[len(entries)-1].after; last != "sha256:"+digest {
			return fmt.Sprintf("the last of its mends of %s left %s, and checksums.sha256 lists sha256:%s: "+
				"the checksums were not written again after it", f, last, digest)
		}
	}
	return ""
}

func mendedFiles(mended map[string][]mend) []string {
	files := make([]string, 0, len(mended))
	for f := range mended {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// mendedBytesProblem holds each file of the folder dir a mend changed to
// its mends, whatever the level of the verification: it is the bytes its
// last mend left and its checksums list, and each mend, newest first,
// changed the one field it names and nothing else — its old value written
// back into the bytes it left gives the bytes it was made to (sha256Before).
// "" when every file holds.
func mendedBytesProblem(dir string, mended map[string][]mend) (string, string) {
	for _, f := range mendedFiles(mended) {
		p := dir + string(os.PathSeparator) + f
		buf, err := readUpTo(p, initMax+1)
		if err != nil {
			return p, err.Error()
		}
		entries := mended[f]
		for i := len(entries) - 1; i >= 0; i-- {
			m := entries[i]
			if "sha256:"+SHA256(buf) != m.after {
				if i == len(entries)-1 {
					return p, fmt.Sprintf("its hash is sha256:%s, and its last mend left %s: changed since, with no entry in mends", SHA256(buf), m.after)
				}
				return p, fmt.Sprintf("its mends do not lead back to the bytes it had: one left %s, and the one after it was made to other bytes", m.after)
			}
			edit, why := initEdit(buf)
			switch {
			case why != "":
				return p, why + ", and its mends change its edit list"
			case m.kind == "avsync-editlist" && edit.handler != "vide":
				return p, fmt.Sprintf("its track's handler is %q, not a video track's (vide), and its mend changes its edit list as a video rendition's", edit.handler)
			case edit.off < 0 || edit.mediaTime != m.new:
				return p, fmt.Sprintf("its edit list's media_time is %d, and its mend wrote %d", edit.mediaTime, m.new)
			}
			back, ok := withField(buf, edit.off, edit.width, m.old)
			if !ok || "sha256:"+SHA256(back) != m.before {
				return p, fmt.Sprintf("its edit list's media_time put back to %d, it hashes to other bytes than its mend's sha256Before: "+
					"the mend changed more of the file than the field it names", m.old)
			}
			buf = back
		}
	}
	return "", ""
}

func readUpTo(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

// initEdit is what a mend's check reads of an init segment of one track as
// a packager writes it — an ftyp and a moov at its top, every edit inside
// its box: its handler, and the offset, the width and the value of its
// first media edit's media_time (off -1 when it has no media edit), or why
// buf is none.
type initEditField struct {
	handler    string
	off, width int
	mediaTime  int64
}

func initEdit(buf []byte) (initEditField, string) {
	out := initEditField{off: -1}
	if len(buf) > initMax {
		return out, fmt.Sprintf("it is larger than an init segment (%d bytes)", initMax)
	}
	top := mp4Boxes(buf, 0, len(buf))
	moovs := 0
	for i, b := range top {
		switch {
		case i == 0 && b.kind != "ftyp":
			return out, "it is no init segment: an ftyp and a moov at its top"
		case b.kind == "moov":
			moovs++
		case b.kind != "ftyp" && b.kind != "free" && b.kind != "skip":
			return out, "it is no init segment: an ftyp and a moov at its top"
		}
	}
	if len(top) == 0 || moovs != 1 {
		return out, "it is no init segment: an ftyp and a moov at its top"
	}
	var moov mp4Box
	for _, b := range top {
		if b.kind == "moov" {
			moov = b
		}
	}
	var traks []mp4Box
	for _, b := range mp4Boxes(buf, moov.start, moov.end) {
		if b.kind == "trak" {
			traks = append(traks, b)
		}
	}
	if len(traks) != 1 {
		return out, fmt.Sprintf("it has %d tracks, one expected", len(traks))
	}
	inTrak := mp4Boxes(buf, traks[0].start, traks[0].end)
	for _, b := range inTrak {
		if b.kind != "mdia" {
			continue
		}
		for _, h := range mp4Boxes(buf, b.start, b.end) {
			if h.kind == "hdlr" && h.start+12 <= h.end {
				out.handler = string(buf[h.start+8 : h.start+12])
			}
		}
	}
	for _, b := range inTrak {
		if b.kind != "edts" {
			continue
		}
		for _, l := range mp4Boxes(buf, b.start, b.end) {
			if l.kind != "elst" || out.off >= 0 {
				continue
			}
			if l.start+8 > l.end {
				return out, "it has an edit list too short"
			}
			version, count := buf[l.start], int(binary.BigEndian.Uint32(buf[l.start+4:]))
			width := 4
			if version == 1 {
				width = 8
			}
			if count < 0 || l.start+8+count*(2*width+4) > l.end {
				return out, "it has an edit list whose entries run past its box"
			}
			p := l.start + 8
			for k := 0; k < count; k++ {
				var mt int64
				if width == 8 {
					mt = int64(binary.BigEndian.Uint64(buf[p+8:]))
				} else {
					mt = int64(int32(binary.BigEndian.Uint32(buf[p+4:])))
				}
				if mt != -1 {
					out.off, out.width, out.mediaTime = p+width, width, mt
					break
				}
				p += 2*width + 4
			}
		}
	}
	return out, ""
}

// withField is buf with the signed width-byte field at off set to v.
func withField(buf []byte, off, width int, v int64) ([]byte, bool) {
	out := bytes.Clone(buf)
	if width == 8 {
		binary.BigEndian.PutUint64(out[off:], uint64(v))
		return out, true
	}
	if v < -1<<31 || v > 1<<31-1 {
		return nil, false
	}
	binary.BigEndian.PutUint32(out[off:], uint32(int32(v)))
	return out, true
}

type mp4Box struct {
	kind       string
	start, end int // its payload
}

// mp4Boxes is the ISO BMFF boxes of buf[off:end], up to one that runs past
// its parent.
func mp4Boxes(buf []byte, off, end int) []mp4Box {
	var out []mp4Box
	for off+8 <= end {
		size, hdr := int64(binary.BigEndian.Uint32(buf[off:])), 8
		kind := string(buf[off+4 : off+8])
		if size == 1 {
			if off+16 > end {
				return out
			}
			size, hdr = int64(binary.BigEndian.Uint64(buf[off+8:])), 16
		} else if size == 0 {
			size = int64(end - off)
		}
		if size < int64(hdr) || int64(off)+size > int64(end) {
			return out
		}
		out = append(out, mp4Box{kind, off + hdr, off + int(size)})
		off += int(size)
	}
	return out
}

// mendSettle is how long the retire job waits before it reads a version
// whose chain did not hold again: more than a mend of one version takes to
// write its files (a few small ones, each written through).
var mendSettle = 5 * time.Second

// mendMayBeUnderWay is whether err, a version's chain not holding, is what a
// mend under way leaves for a moment: a file a mend writes (the chain's
// heads, a rendition's init segment) not matching, or a mend's file not yet
// put in place.
func mendMayBeUnderWay(err error) bool {
	var b *Broken
	if !errors.As(err, &b) {
		return false
	}
	name := filepath.Base(b.File)
	switch {
	case name == CompleteFile, name == PackageFile, name == SumsFile, name == "init.mp4":
		return true
	case strings.HasSuffix(name, MendingSuffix):
		return true
	}
	return false
}
