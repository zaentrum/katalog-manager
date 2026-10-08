package library

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The records of a version folder and of an extra's folder, and the chain
// above them: .complete holds the hash of package.json, package.json the hash
// of checksums.sha256, and checksums.sha256 lists the record (version.json,
// extra.json) and every file of the package.
const (
	CompleteFile = ".complete"
	PackageFile  = "package.json"
	VersionFile  = "version.json"
	ExtraFile    = "extra.json"
)

// The folders of a version's package (a trailer of a version from before
// extras too), and of an extra's.
var (
	versionPackageDirs = []string{"hls", "subs", "trickplay", "trailers"}
	extraPackageDirs   = []string{"hls", "subs", "trickplay"}
)

// osArtefacts are the files operating systems and NAS software drop into
// shared folders, which no package holds (as the library's tools read one).
var osArtefacts = regexp.MustCompile(`^(\.DS_Store|\._.*|Thumbs\.db|desktop\.ini|@eaDir|\.@__thumb|#recycle|\.AppleDouble)$`)

// IsArtefact reports whether name is that of a file operating systems and
// NAS software drop into shared folders.
func IsArtefact(name string) bool { return osArtefacts.MatchString(name) }

// Broken says which file of a chain does not hold, and how.
type Broken struct{ File, Reason string }

func (e *Broken) Error() string { return e.File + ": " + e.Reason }

func broken(file, format string, args ...any) error {
	return &Broken{File: file, Reason: fmt.Sprintf(format, args...)}
}

// Chain is a version's or an extra's package as its chain records it.
type Chain struct {
	Package   Doc               // package.json
	Complete  string            // "sha256:<hex>" of package.json
	Listed    map[string]string // what checksums.sha256 lists: path → hex sha256
	SizeBytes int64             // the listed files' bytes, as found
}

// VerifyVersion checks the chain of the version folder dir: .complete is the
// hash of package.json, package.json's checksums the hash of
// checksums.sha256, which lists exactly version.json and the files under
// hls/, subs/ and trickplay/, as many as it says and of the bytes it says,
// and version.json's digest. full hashes every listed file too.
func VerifyVersion(dir string, full bool) (*Chain, error) {
	return verifyChain(dir, VersionFile, versionPackageDirs, full)
}

// VerifyExtra checks the chain of an extra's folder as VerifyVersion checks a
// version's, its record extra.json.
func VerifyExtra(dir string, full bool) (*Chain, error) {
	return verifyChain(dir, ExtraFile, extraPackageDirs, full)
}

func verifyChain(dir, record string, dirs []string, full bool) (*Chain, error) {
	c := &Chain{}
	at := func(name string) string { return filepath.Join(dir, name) }
	complete, err := os.ReadFile(at(CompleteFile))
	if err != nil {
		return nil, broken(at(CompleteFile), "%v", err)
	}
	c.Complete = strings.TrimSpace(string(complete))
	pkg, err := os.ReadFile(at(PackageFile))
	if err != nil {
		return nil, broken(at(PackageFile), "%v", err)
	}
	if c.Complete != "sha256:"+SHA256(pkg) {
		return nil, broken(at(CompleteFile), "it says %s, and package.json's hash is sha256:%s", c.Complete, SHA256(pkg))
	}
	if c.Package, err = DecodeDoc(pkg); err != nil {
		return nil, broken(at(PackageFile), "no JSON object: %v", err)
	}
	cs, _ := c.Package.Get("checksums")
	sums, _ := cs.(Doc)
	file := str(sums, "file")
	if file != SumsFile {
		return nil, broken(at(PackageFile), "its checksums name the file %q, not %s", file, SumsFile)
	}
	listing, err := os.ReadFile(at(SumsFile))
	if err != nil {
		return nil, broken(at(SumsFile), "%v", err)
	}
	if want := str(sums, "sha256"); want != "sha256:"+SHA256(listing) {
		return nil, broken(at(SumsFile), "package.json says its hash is %s, and it is sha256:%s", want, SHA256(listing))
	}
	if c.Listed, err = ReadChecksums(at(SumsFile)); err != nil {
		return nil, broken(at(SumsFile), "%v", err)
	}
	present := map[string]bool{}
	if _, err := os.Stat(at(record)); err == nil {
		present[record] = true
	}
	for _, d := range dirs {
		root := at(d)
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && path == root {
					return nil
				}
				return err
			}
			if osArtefacts.MatchString(e.Name()) {
				if e.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !e.IsDir() {
				rel, err := filepath.Rel(dir, path)
				if err != nil {
					return err
				}
				present[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			return nil, broken(root, "%v", err)
		}
	}
	var missing, unlisted []string
	for name := range c.Listed {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	for name := range present {
		if _, ok := c.Listed[name]; !ok {
			unlisted = append(unlisted, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(unlisted)
	if len(missing) > 0 {
		return nil, broken(at(missing[0]), "checksums.sha256 lists it, and it is not there (%d listed files missing)", len(missing))
	}
	if len(unlisted) > 0 {
		return nil, broken(at(unlisted[0]), "it is not listed in checksums.sha256 (%d files unlisted)", len(unlisted))
	}
	if n, ok := number(sums, "files"); ok && n != float64(len(c.Listed)) {
		return nil, broken(at(SumsFile), "it lists %d files, and package.json says %v", len(c.Listed), n)
	}
	names := make([]string, 0, len(c.Listed))
	for name := range c.Listed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := at(name)
		if name == record || full {
			digest, size, err := SHA256File(path)
			if err != nil {
				return nil, broken(path, "%v", err)
			}
			if digest != c.Listed[name] {
				return nil, broken(path, "its hash is sha256:%s, and checksums.sha256 lists %s", digest, c.Listed[name])
			}
			c.SizeBytes += size
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			return nil, broken(path, "%v", err)
		}
		c.SizeBytes += fi.Size()
	}
	if n, ok := number(sums, "bytes"); ok && n != float64(c.SizeBytes) {
		return nil, broken(at(SumsFile), "the files it lists hold %d bytes, and package.json says %v", c.SizeBytes, n)
	} else if !ok {
		return nil, broken(at(PackageFile), "its checksums say no bytes")
	}
	return c, nil
}

// str is d's key as a string, "" when it is none.
func str(d Doc, key string) string {
	v, _ := d.Get(key)
	s, _ := v.(string)
	return s
}

// number is d's key as a number.
func number(d Doc, key string) (float64, bool) {
	v, _ := d.Get(key)
	return toFloat(v)
}
