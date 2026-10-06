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

// WriteVersion writes the version v into dir as the packager does: the
// package's files, version.json, the checksums over both, package.json with
// their hash and size (its sizeBytes the package's files'), and .complete
// with package.json's hash, which it answers.
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
	cs := checksums(digests)
	Write(t, filepath.Join(dir, "checksums.sha256"), cs)
	pkg := map[string]any{"schema": "zaentrum.library.package/2", "packageId": v.PackageID, "createdAt": v.CreatedAt,
		"packagedBy": "packager", "state": "complete", "role": "canonical", "sizeBytes": size,
		"checksums": map[string]any{"file": "checksums.sha256", "algorithm": "sha256", "sha256": "sha256:" + sum(cs),
			"files": len(digests), "bytes": total}}
	for k, x := range v.Package {
		pkg[k] = x
	}
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
