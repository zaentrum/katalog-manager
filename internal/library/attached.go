package library

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// An attached subtitle is one attached to a title once its version is in the
// library (migration 048, POST /api/library/items/{itemId}/subtitles): after
// its package completed, or while the version is taken in, its original and
// no package (migration 044). Its file lies in the subs/ folder of the
// version it was attached to, beside the package's (a version taken in gets
// its package beside it), as
//
//	subs/attached-<sha256>.<language>[.forced][.sdh].vtt
//
// <sha256> the hex sha256 of the file's bytes, <language> its language tag
// in lower case. The package's chain does not cover it: package.json and its
// checksums are written once, when the package completes, of the package's
// files alone, and a subtitle attached before or after changes neither. Its
// name is its check instead, as a projected image's is (VerifyVersion).
const AttachedPrefix = "attached-"

var attachedRE = regexp.MustCompile(`^attached-([0-9a-f]{64})\.[a-z0-9]+(-[a-z0-9]+)*(\.forced)?(\.sdh)?\.vtt$`)

// AttachedName is the name of the file of an attached subtitle whose bytes
// hash to sum.
func AttachedName(sum, language string, forced, sdh bool) string {
	name := AttachedPrefix + sum + "." + strings.ToLower(language)
	if forced {
		name += ".forced"
	}
	if sdh {
		name += ".sdh"
	}
	return name + ".vtt"
}

// AttachedSum is the hash an attached subtitle's file name gives its bytes;
// false for a name that is no attached subtitle's.
func AttachedSum(name string) (string, bool) {
	m := attachedRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// AttachedPath is where the file of an attached subtitle lies in the version
// folder versionDir.
func AttachedPath(versionDir, name string) string {
	return filepath.Join(versionDir, "subs", name)
}

// isAttachedFile reports whether rel, a path in a version's folder, is the
// file of an attached subtitle: directly in subs/, named as one is.
func isAttachedFile(rel string) bool {
	dir, name := filepath.Split(filepath.ToSlash(rel))
	_, ok := AttachedSum(name)
	return ok && dir == "subs/"
}

// sum64Chunk is how much of each end of a file Sum64 reads.
const sum64Chunk = 64 << 10

// ErrTooSmall is why a file has no Sum64: it is smaller than both ends it
// reads.
var ErrTooSmall = errors.New("the file is smaller than the 128 KiB its fingerprint reads")

// Sum64 is the fingerprint subtitle sources match a file by: the file's size
// plus its first and its last 64 KiB read as little-endian 64-bit words, all
// summed modulo 2^64, as 16 hex digits. It reads those 128 KiB and nothing
// else; a file smaller than that has none (ErrTooSmall).
func Sum64(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	if !fi.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%s is no file", path)
	}
	size := fi.Size()
	if size < 2*sum64Chunk {
		return size, "", ErrTooSmall
	}
	sum := uint64(size)
	buf := make([]byte, sum64Chunk)
	for _, at := range []int64{0, size - sum64Chunk} {
		if _, err := f.ReadAt(buf, at); err != nil && !errors.Is(err, io.EOF) {
			return size, "", err
		}
		for i := 0; i+8 <= len(buf); i += 8 {
			sum += binary.LittleEndian.Uint64(buf[i:])
		}
	}
	return size, fmt.Sprintf("%016x", sum), nil
}
