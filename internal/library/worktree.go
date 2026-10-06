// Package library is the catalog's side of the library record on storage
// (platform-library/1): with the setting library.layout=v2 the share's root
// holds movies/, series/ and people/, the record, beside .work/, everything
// that is not the record. katalog-manager decides every path in it: the
// workers get theirs in their worker records and compute none themselves.
//
// It holds the path rules, the writer of the records katalog-manager writes
// (item.json, the events) and of the projections (metadata.json,
// person.json), the checks of a version's chain, and the deletion gate.
package library

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// The folders of the work folder: the files that arrive (incoming, the scan
// root, and extras, the extras' files taken in by the API), the files handed
// to replaceSource (replace), the transcoder's handoffs to the packager
// (inbox), the packager's staging, the retired originals during their grace
// (trash), the sweep's quarantine, the migration's runs and what is left of
// the old layout (legacy).
const (
	WorkIncoming   = "incoming"
	WorkExtras     = "extras"
	WorkReplace    = "replace"
	WorkInbox      = "inbox"
	WorkStaging    = "staging"
	WorkTrash      = "trash"
	WorkQuarantine = "quarantine"
	WorkMigration  = "migration"
	WorkLegacy     = "legacy"
)

// DirMode and FileMode are what the library's writers create, under the
// umask 0002 every writer of the share runs with: the group writes too.
const (
	DirMode  os.FileMode = 0o775
	FileMode os.FileMode = 0o664
)

// EnsureWorkTree creates the work folder and its folders where they are
// missing, and the library's root with it: ARRIVALS_ROOT and EXTRAS_ROOT
// where the configuration puts them, the others under WORK_ROOT.
func EnsureWorkTree(cfg config.Config) error {
	dirs := []string{cfg.ArrivalsRoot, cfg.ExtrasRoot}
	for _, d := range []string{WorkReplace, WorkInbox, WorkStaging, WorkTrash, WorkQuarantine, WorkMigration, WorkLegacy} {
		dirs = append(dirs, filepath.Join(cfg.WorkRoot, d))
	}
	for _, d := range dirs {
		if d == "" || !filepath.IsAbs(d) {
			return fmt.Errorf("the work folder %q is no absolute path", d)
		}
		if err := os.MkdirAll(d, DirMode); err != nil {
			return err
		}
	}
	return nil
}
