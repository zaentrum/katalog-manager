package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
)

// A title's original lives in the library from its first version on, in that
// version's folder: versions/<versionId>/original.<ext> (names.go), from
// where the retire job deletes it after packaging. katalog-manager decides
// what a run of the packager does with the source it works on (the worker
// record's build.mode):
//   - establish: the source has no version yet, and the pipeline packages it:
//     its first version, the package and the original renamed into the
//     version's folder together;
//   - takein: the source has no version yet and gets no package now (its
//     transcode was refused, its transcode or its package failed with no
//     attempt left, an admin took it in): a version of its original alone,
//     taken (VersionTaken), from which the title plays as a title without a
//     package does;
//   - add: the source's version holds its original alone (taken): the
//     package is added to that folder, and the version is complete;
//   - repackage: the title has a packaged version of the source: a new
//     version, its package alone, the original staying where it is (in an
//     older version's folder, or where it arrived).
const (
	ModeEstablish = "establish"
	ModeTakeIn    = "takein"
	ModeAdd       = "add"
	ModeRepackage = "repackage"
)

// RunOf decides the mode of a run of the packager on the source s; takeIn
// says the run takes the title in (its step takein waits or runs). For add
// it answers the version taken in, the one whose folder holds the original.
func RunOf(ctx context.Context, q Querier, s *Source, takeIn bool) (string, *Version, error) {
	versions, err := VersionsOf(ctx, q, s.ItemID)
	if err != nil {
		return "", nil, err
	}
	var taken *Version
	established := false
	for _, v := range versions {
		if !slices.Contains(v.SourceIDs, s.ID) {
			continue
		}
		switch v.State {
		case VersionTaken:
			if taken == nil || (v.Dir != nil && s.ArrivalPath != nil && filepath.Dir(*s.ArrivalPath) == filepath.Clean(*v.Dir)) {
				taken = v
			}
			established = true
		case VersionComplete, VersionSuperseded:
			established = true
		}
	}
	switch {
	case taken != nil:
		return ModeAdd, taken, nil
	case established:
		return ModeRepackage, nil, nil
	case takeIn:
		return ModeTakeIn, nil, nil
	}
	return ModeEstablish, nil, nil
}

// TakenOf is the version taken in of the source s, whose folder holds its
// original alone: the one a run adds its package to; nil when it has none.
func TakenOf(ctx context.Context, q Querier, s *Source) (*Version, error) {
	mode, v, err := RunOf(ctx, q, s, false)
	if err != nil || mode != ModeAdd {
		return nil, err
	}
	return v, nil
}

// VersionFolderOf says whether path lies directly in a version folder of the
// item folder itemDir (versions/<versionId>/), and of which version.
func VersionFolderOf(itemDir, path string) (string, bool) {
	dir := filepath.Dir(filepath.Clean(path))
	if itemDir == "" || filepath.Dir(dir) != filepath.Join(filepath.Clean(itemDir), "versions") || !ValidID(filepath.Base(dir)) {
		return "", false
	}
	return filepath.Base(dir), true
}

// OriginalsIn lists the originals the version folder dir holds, by name: its
// files named as the library names an original; none when it is not there.
func OriginalsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && IsOriginalName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ArrivalOf is where the original of the source s arrived: its place while it
// lies outside the library's record, else (in its version's folder) the
// arrivals joined with its library path, the place it had among them; ""
// when neither says. The files that came with it (its sidecars) lie there.
func (p Paths) ArrivalOf(s *Source) string {
	if s.ArrivalPath != nil && !p.InRecord(*s.ArrivalPath) {
		return filepath.Clean(*s.ArrivalPath)
	}
	if s.LibraryPath != nil && *s.LibraryPath != "" && p.Arrivals != "" {
		path := filepath.Join(p.Arrivals, filepath.FromSlash(*s.LibraryPath))
		if Within(p.Arrivals, path) {
			return path
		}
	}
	return ""
}

// InRecord reports whether path lies in the library's record: under movies/,
// series/ or people/.
func (p Paths) InRecord(path string) bool {
	for _, d := range []string{MoviesDir, SeriesDir, PeopleDir} {
		if Within(filepath.Join(p.Root, d), path) {
			return true
		}
	}
	return false
}

// IsTakenRefused reports whether err is Postgres refusing a version taken in:
// the check on a version's state that migration 044 widens.
func IsTakenRefused(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23514" && pe.TableName == "com_nalet_katalog_itemversions"
}
