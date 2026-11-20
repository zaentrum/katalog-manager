// Package librarytest writes the library's records as the packager writes
// them, for tests: a version folder with its package and its chain, a source
// record, an extra's folder.
package librarytest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A version folder's package, as tests name its files.
var DefaultFiles = map[string]string{
	"hls/master.m3u8":           "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4000000\nv0/playlist.m3u8\n",
	"hls/v0/playlist.m3u8":      "#EXTM3U\n#EXTINF:6.0,\nseg0.m4s\n#EXT-X-ENDLIST\n",
	"hls/v0/init.mp4":           "init",
	"hls/v0/seg0.m4s":           "a segment of the picture",
	"hls/a0/playlist.m3u8":      "#EXTM3U\n#EXTINF:6.0,\nseg0.m4s\n#EXT-X-ENDLIST\n",
	"hls/a0/seg0.m4s":           "a segment of the sound",
	"subs/0.vtt":                "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello\n",
	"subs/1.vtt":                "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHallo\n",
	"trickplay/sprite-0001.jpg": "\xff\xd8 a sprite",
}

// Version is a version folder as WriteVersion writes it.
type Version struct {
	VersionID, PackageID string
	SourceIDs            []string
	CreatedAt            string            // package.json's createdAt
	Files                map[string]string // the package's files by path in the folder; DefaultFiles when nil
	Package              map[string]any    // package.json's fields besides those WriteVersion sets
	Version              map[string]any    // version.json's fields besides those WriteVersion sets
	// Pending writes the version as a migration stages one whose files it
	// did not read: no checksums.sha256, and a package.json that counts
	// the files and says its checksums are pending. Fill closes its chain.
	Pending bool
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// JSON writes v as a record is written: indented, one trailing newline.
func JSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// Write writes the file path with content, its folders first.
func Write(t testing.TB, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o664); err != nil {
		t.Fatal(err)
	}
}

// checksums lists digests by path.
func checksums(digests map[string]string) []byte {
	names := make([]string, 0, len(digests))
	for n := range digests {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(digests[n] + "  " + n + "\n")
	}
	return []byte(b.String())
}

// VideoEncoder, where a test package sets it, is the encoder WriteVersion
// gives the top video rendition of a package whose record states none, as
// the packager writes one: a package record without renditions, or without
// a video rendition, gets one of it, and a first video rendition without an
// encoder key gets it. A test that writes a package that says no encoder
// gives the key, null, or an empty list of video renditions.
var VideoEncoder string

// encoded is the package record fields pkg with VideoEncoder given its top
// video rendition where it says none.
func encoded(pkg map[string]any) map[string]any {
	if VideoEncoder == "" {
		return pkg
	}
	out := map[string]any{}
	for k, v := range pkg {
		out[k] = v
	}
	ren, _ := out["renditions"].(map[string]any)
	r := map[string]any{}
	for k, v := range ren {
		r[k] = v
	}
	video, has := r["video"].([]any)
	switch {
	case !has && r["video"] == nil:
		r["video"] = []any{map[string]any{"id": "v0", "encoder": VideoEncoder}}
	case len(video) > 0:
		if v0, ok := video[0].(map[string]any); ok {
			if _, said := v0["encoder"]; !said {
				c := map[string]any{"encoder": VideoEncoder}
				for k, v := range v0 {
					c[k] = v
				}
				r["video"] = append([]any{c}, video[1:]...)
			}
		}
	}
	out["renditions"] = r
	return out
}

// WriteVersion writes the version v into dir as the packager does: the
// package's files, version.json, the checksums over both, package.json with
// their hash and size (its sizeBytes the package's files'), and .complete
// with package.json's hash, which it answers. Its package says no scan
// unless v.Package gives one.
func WriteVersion(t testing.TB, dir string, v Version) string {
	t.Helper()
	files := v.Files
	if files == nil {
		files = DefaultFiles
	}
	digests := map[string]string{}
	var size, total int64
	for rel, content := range files {
		Write(t, filepath.Join(dir, rel), []byte(content))
		digests[rel] = sum([]byte(content))
		size += int64(len(content))
	}
	ver := map[string]any{"schema": "zaentrum.library.version/2", "versionId": v.VersionID, "createdAt": v.CreatedAt,
		"createdBy": "packager", "sourceIds": v.SourceIDs, "originalFiles": []string{}, "chapters": []any{},
		"chaptersFrom": nil, "segments": []any{}}
	for k, x := range v.Version {
		ver[k] = x
	}
	vb := JSON(t, ver)
	Write(t, filepath.Join(dir, "version.json"), vb)
	digests["version.json"] = sum(vb)
	total = size + int64(len(vb))
	sums := map[string]any{"file": "checksums.sha256", "algorithm": "sha256", "pending": true,
		"files": len(digests), "bytes": total}
	if !v.Pending {
		cs := checksums(digests)
		Write(t, filepath.Join(dir, "checksums.sha256"), cs)
		delete(sums, "pending")
		sums["sha256"] = "sha256:" + sum(cs)
	}
	pkg := map[string]any{"schema": "zaentrum.library.package/2", "packageId": v.PackageID, "createdAt": v.CreatedAt,
		"packagedBy": "packager", "state": "complete", "role": "canonical", "sizeBytes": size, "checksums": sums}
	for k, x := range encoded(v.Package) {
		pkg[k] = x
	}
	pb := JSON(t, pkg)
	Write(t, filepath.Join(dir, "package.json"), pb)
	complete := "sha256:" + sum(pb)
	Write(t, filepath.Join(dir, ".complete"), []byte(complete+"\n"))
	return complete
}

// Fill closes the chain of the version folder dir, written with its
// checksums pending, as the schemas' library-v2-fill-checksums.py does: the
// checksums over version.json and every file of the package, package.json
// again with their hash in place of pending, and .complete over that, whose
// hash it answers.
func Fill(t testing.TB, dir string) string {
	t.Helper()
	digests := map[string]string{}
	var total int64
	add := func(rel string) {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		digests[filepath.ToSlash(rel)] = sum(b)
		total += int64(len(b))
	}
	add("version.json")
	for _, sub := range []string{"hls", "subs", "trickplay", "trailers"} {
		_ = filepath.WalkDir(filepath.Join(dir, sub), func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dir, path)
				add(rel)
			}
			return nil
		})
	}
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg map[string]any
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	cs := checksums(digests)
	Write(t, filepath.Join(dir, "checksums.sha256"), cs)
	pkg["checksums"] = map[string]any{"file": "checksums.sha256", "algorithm": "sha256", "sha256": "sha256:" + sum(cs),
		"files": len(digests), "bytes": total}
	pb := JSON(t, pkg)
	Write(t, filepath.Join(dir, "package.json"), pb)
	complete := "sha256:" + sum(pb)
	Write(t, filepath.Join(dir, ".complete"), []byte(complete+"\n"))
	return complete
}

// WriteSource writes a source record into dir, sources/<id>/ of its item:
// source.json with record's fields, and its checksums.
func WriteSource(t testing.TB, dir string, record map[string]any, files map[string]string) {
	t.Helper()
	rec := map[string]any{"schema": "zaentrum.library.source/2"}
	for k, x := range record {
		rec[k] = x
	}
	b := JSON(t, rec)
	Write(t, filepath.Join(dir, "source.json"), b)
	digests := map[string]string{"source.json": sum(b)}
	for rel, content := range files {
		Write(t, filepath.Join(dir, rel), []byte(content))
		digests[rel] = sum([]byte(content))
	}
	Write(t, filepath.Join(dir, "checksums.sha256"), checksums(digests))
}

// Extra is an extra's folder as WriteExtra writes it.
type Extra struct {
	ExtraID, PackageID string
	CreatedAt          string
	Files              map[string]string // the package's files; a picture, a sound and a master when nil
	Package            map[string]any
}

// WriteExtra writes the extra x into dir as the packager does: the package's
// files, extra.json, the checksums over both, package.json and .complete,
// whose content it answers.
func WriteExtra(t testing.TB, dir string, x Extra) string {
	t.Helper()
	files := x.Files
	if files == nil {
		files = map[string]string{}
		for k, v := range DefaultFiles {
			if strings.HasPrefix(k, "hls/") {
				files[k] = v
			}
		}
	}
	digests := map[string]string{}
	var size int64
	for rel, content := range files {
		Write(t, filepath.Join(dir, rel), []byte(content))
		digests[rel] = sum([]byte(content))
		size += int64(len(content))
	}
	xb := JSON(t, map[string]any{"schema": "zaentrum.library.extra/2", "extraId": x.ExtraID, "createdAt": x.CreatedAt,
		"originalFiles": []string{}, "originals": []any{}})
	Write(t, filepath.Join(dir, "extra.json"), xb)
	digests["extra.json"] = sum(xb)
	cs := checksums(digests)
	Write(t, filepath.Join(dir, "checksums.sha256"), cs)
	pkg := map[string]any{"schema": "zaentrum.library.package/2", "packageId": x.PackageID, "createdAt": x.CreatedAt,
		"sizeBytes": size, "checksums": map[string]any{"file": "checksums.sha256", "algorithm": "sha256",
			"sha256": "sha256:" + sum(cs), "files": len(digests), "bytes": size + int64(len(xb))}}
	for k, v := range x.Package {
		pkg[k] = v
	}
	pb := JSON(t, pkg)
	Write(t, filepath.Join(dir, "package.json"), pb)
	complete := "sha256:" + sum(pb)
	Write(t, filepath.Join(dir, ".complete"), []byte(complete+"\n"))
	return complete
}

// InitSegment is an init segment of one track as a packager writes one,
// with no media: an ftyp and a moov whose track's handler is handler
// ("vide", "soun") and whose edit list holds one media edit at mediaTime,
// the boxes a mend reads and changes.
func InitSegment(mediaTime int32, handler string) []byte {
	box := func(kind string, payload ...[]byte) []byte {
		var b []byte
		for _, p := range payload {
			b = append(b, p...)
		}
		n := uint32(8 + len(b))
		return append([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), kind[0], kind[1], kind[2], kind[3]}, b...)
	}
	u32 := func(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }
	elst := box("elst", u32(0), u32(1), u32(0), u32(uint32(mediaTime)), []byte{0, 1, 0, 0})
	mdhd := box("mdhd", u32(0), make([]byte, 8), u32(16000), make([]byte, 8))
	hdlr := box("hdlr", u32(0), u32(0), []byte(handler), make([]byte, 13))
	trak := box("trak", box("tkhd", u32(3), make([]byte, 80)), box("edts", elst),
		box("mdia", mdhd, hdlr, box("minf", box("stbl"))))
	return append(box("ftyp", []byte("iso6"), u32(0), []byte("iso6cmfc")), box("moov", box("mvhd", u32(0), make([]byte, 96)), trak)...)
}

// Renditions is package.json's renditions for DefaultFiles: v0 the video,
// a0 the stereo sound.
func Renditions() map[string]any {
	return map[string]any{
		"video": []any{map[string]any{"id": "v0", "dir": "hls/v0"}},
		"audio": []any{map[string]any{"id": "a0", "dir": "hls/a0"}},
	}
}
