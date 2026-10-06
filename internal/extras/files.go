package extras

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// qh1Chunk is how much of each end of a file its quick hash reads.
const qh1Chunk = 64 << 10

// QH1 is the size of the file at path and its quick hash, as the library
// records a file's fixity: "sha256:" and the hex SHA-256 of its first 64 KiB,
// its last 64 KiB (the second read only for a file larger than 64 KiB) and
// its size as a big-endian uint64. Two reads however large the file is; it
// tells the file again after a move, and is no hash of its content.
func QH1(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	size := fi.Size()
	h := sha256.New()
	buf := make([]byte, qh1Chunk)
	read := func() error {
		n, err := io.ReadFull(f, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		h.Write(buf[:n])
		return nil
	}
	if err := read(); err != nil {
		return 0, "", err
	}
	if size > qh1Chunk {
		if _, err := f.Seek(size-qh1Chunk, io.SeekStart); err != nil {
			return 0, "", err
		}
		if err := read(); err != nil {
			return 0, "", err
		}
	}
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(size))
	h.Write(n[:])
	return size, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// videoExts are the files an extra may be, by their extension: the
// containers the transcoder probes and encodes.
var videoExts = map[string]bool{".avi": true, ".m2ts": true, ".m4v": true, ".mkv": true, ".mov": true, ".mp4": true,
	".mpeg": true, ".mpg": true, ".ogv": true, ".ts": true, ".webm": true, ".wmv": true}

// IsVideoFile reports whether name is a file an extra may be, by its
// extension.
func IsVideoFile(name string) bool { return videoExts[strings.ToLower(filepath.Ext(name))] }

func videoExtList() string {
	out := make([]string, 0, len(videoExts))
	for e := range videoExts {
		out = append(out, e)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// within reports whether path lies strictly inside root, both clean and
// absolute.
func within(root, path string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "/" || root == "." || !filepath.IsAbs(root) {
		return false
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolved is root with its links followed, root itself when it cannot be
// (a root that does not exist yet).
func resolved(root string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		return r
	}
	return filepath.Clean(root)
}

// file checks that path names a file an extra may be, and answers it clean:
// an absolute path of an existing regular video file, inside the media root,
// the share's library or extras folder (the legacy layout's) or EXTRAS_ROOT
// and outside the package store, as written and with its links followed (a
// link inside a root that leads out of it is refused).
func (s *Service) file(path string) (string, *graph.ExtraRefused) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "an extra needs its file: path is required")
	}
	if !filepath.IsAbs(path) {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s is no absolute path", path)
	}
	path = filepath.Clean(path)
	roots := []string{s.cfg.NFSRoot, s.cfg.LegacyLibraryRoot, s.cfg.LegacyExtrasRoot, s.cfg.ExtrasRoot}
	inside := func(p string, resolve bool) bool {
		pkgs := s.cfg.PackagesRoot
		if resolve {
			pkgs = resolved(pkgs)
		}
		if within(pkgs, p) {
			return false
		}
		for _, r := range roots {
			if resolve {
				r = resolved(r)
			}
			if within(r, p) {
				return true
			}
		}
		return false
	}
	if !inside(path, false) {
		return "", graph.Refused(http.StatusBadRequest, codeRefused,
			"%s is not under the media root, the library's or the extras' folder or EXTRAS_ROOT (or is under the package store)", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", graph.Refused(http.StatusBadRequest, codeRefused, "there is no file at %s", path)
		}
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s cannot be read: %v", path, err)
	}
	if !inside(real, true) {
		return "", graph.Refused(http.StatusBadRequest, codeRefused,
			"%s leads out of the media root, the library's and the extras' folder and EXTRAS_ROOT", path)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s cannot be read: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s is no file", path)
	}
	if !IsVideoFile(path) {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s is no video file (%s)", path, videoExtList())
	}
	return path, nil
}

// packageDir is the folder of the extra id's package in the package store:
// packages/extras/<the id's first two characters>/<id>.
func packageDir(packagesRoot, id string) string {
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(packagesRoot, "extras", shard, id)
}

// PackageDir is the folder of the extra id's package under packagesRoot.
func PackageDir(packagesRoot, id string) string { return packageDir(packagesRoot, id) }

// inboxDir is where the transcoder hands the extra id's encode to the
// packager: _inbox/extra-<id>/ in the package store, beside the items'.
func inboxDir(packagesRoot, id string) string {
	return filepath.Join(packagesRoot, "_inbox", "extra-"+id)
}
