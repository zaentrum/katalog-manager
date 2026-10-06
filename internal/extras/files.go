package extras

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
)

// QH1 is the size of the file at path and its quick hash, as the library
// records a file's fixity (library.QH1).
func QH1(path string) (int64, string, error) { return library.QH1(path) }

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
// an absolute path of an existing regular video file, inside a root of the
// layout (fileRoots) and outside the package store, as written and with its
// links followed (a link inside a root that leads out of it is refused).
func (s *Service) file(ctx context.Context, path string) (string, *graph.ExtraRefused) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "an extra needs its file: path is required")
	}
	if !filepath.IsAbs(path) {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s is no absolute path", path)
	}
	path = filepath.Clean(path)
	v2, roots, named, err := s.fileRoots(ctx)
	if err != nil {
		return "", graph.Refused(http.StatusServiceUnavailable, codeNoTable, "the library's settings cannot be read: %v", err)
	}
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
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s is not under %s (or is under the package store)", path, named)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", graph.Refused(http.StatusBadRequest, codeRefused, "there is no file at %s", path)
		}
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s cannot be read: %v", path, err)
	}
	if !inside(real, true) {
		return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s leads out of %s", path,
			strings.Replace(named, " or ", " and ", 1))
	}
	if v2 {
		for _, d := range []string{library.MoviesDir, library.SeriesDir, library.PeopleDir} {
			if r := filepath.Join(s.cfg.Roots(true).Library, d); within(r, path) || within(resolved(r), real) {
				return "", graph.Refused(http.StatusBadRequest, codeRefused, "%s is in the library's record, which holds no original", path)
			}
		}
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

// layout reads whether the environment runs the library's v2 layout.
func (s *Service) layout(ctx context.Context) (bool, library.Settings, error) {
	if s.st == nil {
		return false, library.Defaults(), nil
	}
	set, err := library.ReadSettings(ctx, s.st.Pool())
	return set.V2(), set, err
}

// fileRoots are whether the layout is v2, where an extra's file may lie, and
// how a refusal names them (config.Roots): with library.layout=v2,
// ARRIVALS_ROOT (beside its title, as the scanner's convention finds it) and
// EXTRAS_ROOT; with the legacy layout the media root, LIBRARY_ROOT and
// EXTRAS_ROOT, as before.
func (s *Service) fileRoots(ctx context.Context) (bool, []string, string, error) {
	v2, _, err := s.layout(ctx)
	if err != nil {
		return false, nil, "", err
	}
	r := s.cfg.Roots(v2)
	if v2 {
		return true, []string{r.Arrivals, r.Extras}, "ARRIVALS_ROOT or EXTRAS_ROOT", nil
	}
	return false, []string{s.cfg.NFSRoot, r.Library, r.Extras}, "the media root, LIBRARY_ROOT or EXTRAS_ROOT", nil
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
