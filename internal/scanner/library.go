package scanner

import (
	"context"
	"log"
	"os"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// With the setting library.layout=v2 a title's original is a source the
// library knows by its size and quick hash (migration 040). A file the walk
// meets at a path the catalog does not know is told by them first:
//   - an original still there (present) whose file is gone from its place has
//     moved: its title follows it, and nothing new is created;
//   - one still there whose file is in its place too is copied: the copy is no
//     title, and is left alone, said in the log;
//   - one deleted after packaging (or being deleted) arrived again: it is in
//     the library already, so it is no new title either, and is left alone,
//     said in the log: a better file is given to its title with
//     replaceSource;
//   - any other is a new title, taken in as before, with its source.
// A file known at its path whose size changed before its record was written
// gets its quick hash again.

// fixity is what tells a file: its size and its quick hash.
type fixity struct {
	size int64
	qh1  string
}

// knownArrival tells the new file at path among the originals the library
// knows (see above). It answers the file's fixity, and whether the file is
// taken care of: moved, or left alone; nil with false when it cannot be read,
// and the walk passes over it for now.
func (s *Scanner) knownArrival(ctx context.Context, lib library.Paths, path string, res *scanResult) (*fixity, bool) {
	size, qh1, err := library.QH1(path)
	if err != nil {
		log.Printf("scanner: %s cannot be read, and is passed over: %v", path, err)
		return nil, false
	}
	f := &fixity{size: size, qh1: qh1}
	pool := s.st.Pool()
	known, err := library.SourcesByFixity(ctx, pool, size, qh1)
	if err != nil {
		log.Printf("scanner: the originals %s may be could not be read, and it is passed over: %v", path, err)
		return f, true
	}
	for _, k := range known {
		switch k.State {
		case library.SourcePresent:
			if k.ArrivalPath != nil && fileThere(*k.ArrivalPath) {
				log.Printf("scanner: %s is a copy of the original of item %s at %s, and is no title of its own", path,
					k.ItemID, *k.ArrivalPath)
				return f, true
			}
			s.moved(ctx, lib, k, path, res)
			return f, true
		case library.SourceRetiring, library.SourceDeleted:
			when := "now"
			if k.DeletedAt != nil {
				when = "at " + library.Timestamp(*k.DeletedAt)
			} else if k.RetireEventAt != nil {
				when = "at " + library.Timestamp(*k.RetireEventAt)
			}
			log.Printf("scanner: %s is already in the library as item %s (retired %s); use replaceSource to make it a new version",
				path, k.ItemID, when)
			return f, true
		}
	}
	return f, false
}

// moved has the title of the source k follow its original to path: its
// primary asset and its source name the new place, and the subtitle files
// beside it are paired.
func (s *Scanner) moved(ctx context.Context, lib library.Paths, k *library.Source, path string, res *scanResult) {
	pool := s.st.Pool()
	old := ""
	if k.ArrivalPath != nil {
		old = *k.ArrivalPath
	}
	if _, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET path = $3, sourceid = $2
		WHERE item_id = $1 AND COALESCE(kind, 'primary') = 'primary' AND (sourceid = $2 OR (sourceid IS NULL AND path = $4))`,
		k.ItemID, k.ID, path, old); err != nil {
		log.Printf("scanner: the original of item %s moved to %s, and its asset could not follow: %v", k.ItemID, path, err)
		return
	}
	if err := lib.MoveSource(ctx, pool, k, path); err != nil {
		log.Printf("scanner: the original of item %s moved to %s, and its source could not follow: %v", k.ItemID, path, err)
		return
	}
	log.Printf("scanner: the original of item %s moved from %s to %s", k.ItemID, old, path)
	res.itemsUpdated++
	s.scanSidecars(ctx, pool, path, k.ItemID)
}

// addSource takes the new title's file in as its source, which its asset
// names.
func (s *Scanner) addSource(ctx context.Context, lib library.Paths, itemID, path string, f fixity) {
	pool := s.st.Pool()
	src, err := lib.AddSource(ctx, pool, itemID, path, f.size, f.qh1)
	if err != nil {
		log.Printf("scanner: the source of item %s at %s could not be kept: %v", itemID, path, err)
		return
	}
	if _, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $3 WHERE item_id = $1 AND path = $2`,
		itemID, path, src.ID); err != nil {
		log.Printf("scanner: the asset of item %s could not name its source: %v", itemID, err)
	}
}

// refix gives the original at path of a title the scanner knows its source
// (one from before the v2 layout gets one) and, when the file's size changed
// before its record was written, its quick hash again.
func (s *Scanner) refix(ctx context.Context, lib library.Paths, itemID, path string, size int64) {
	pool := s.st.Pool()
	src, err := library.SourceAt(ctx, pool, path)
	if err == nil && src == nil {
		src, err = lib.EnsureSource(ctx, pool, itemID)
	}
	if err != nil || src == nil || src.SizeBytes == size || src.RecordedAt != nil {
		if err != nil {
			log.Printf("scanner: the source of item %s at %s: %v", itemID, path, err)
		}
		return
	}
	sz, qh1, err := library.QH1(path)
	if err != nil {
		log.Printf("scanner: %s cannot be read: %v", path, err)
		return
	}
	if err := library.Refix(ctx, pool, src.ID, sz, qh1); err != nil {
		log.Printf("scanner: the source of item %s at %s: %v", itemID, path, err)
	}
}

// fileThere reports whether a file is at path.
func fileThere(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
