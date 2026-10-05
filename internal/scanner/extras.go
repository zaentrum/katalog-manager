package scanner

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/extras"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The scanner's extras convention, behind the setting extras.scan (off by
// default, so a library does not start packaging its bonus material by
// itself). A file the convention takes for an extra never becomes an item.
// It is an extra of a title when:
//   - its name is the name of the file of a title beside it, then a kind and
//     a label maybe (extranames.go): "Sintel-trailer.mkv", "Sintel - Behind
//     the Scenes - Music.mkv" beside "Sintel.mkv";
//   - it lies in a folder of extras (trailers/, extras/, featurettes/, …)
//     beside the title's file, and the folder that holds the extras' folder
//     holds that one title's file and no other (a file named as an episode,
//     and a show's folder under series/, are no extras: a show may be called
//     Extras);
//   - its name is a kind alone ("trailer.mkv", "teaser-2.mp4"; not
//     interview, short, other or extra, which may be titles' names), or names
//     a trailer as the scanner always took one ("… - trailer.mkv"), and its
//     folder holds one title's file and no other;
//   - a folder of extras of a show (series/<Show>/trailers/, or
//     series/<Show>/Season 01/extras/ for that season) is the extras of the
//     one series the episodes under series/<Show>/ belong to.
//
// Anything ambiguous (a flat folder of many titles, a folder of no title's,
// two series) is skipped, and said in the log. The scanner then takes each in
// as an extra of its title (registered by the scanner), or finds it again:
// a file found at a new path whose size and quick hash are those of an extra
// of its title whose file is gone is that extra moved, and keeps its id and
// its package. After a walk that went through, an extra the scanner took in
// whose file is gone is missing, and hidden, until the file is back.
//
// With the setting off, a file the scanner always took for a trailer is
// skipped and no extra is taken in. Whatever the setting, the scanner writes
// no trailer asset rows (kind trailer) any more, and deletes the one a file
// it meets has: they were its own, attached to whatever title's file lay in
// the trailer's folder.

// ExtrasSetting is the setting that turns the convention on: true, on, yes
// or 1.
const ExtrasSetting = "extras.scan"

// ExtraSender sends the triggers of extras (extras.Service).
type ExtraSender interface {
	Send(ctx context.Context, ids []string, source string) (sent, notSent int, err error)
}

// WithExtras has the scanner send the triggers of the extras it takes in
// through x; without it they wait, pending, for the sweep.
func (s *Scanner) WithExtras(x ExtraSender) *Scanner {
	s.extras = x
	return s
}

// extrasOn reads the setting.
func (s *Scanner) extrasOn(ctx context.Context) bool {
	if s.st == nil {
		return false
	}
	set, err := s.st.GetSettingByKey(ctx, ExtrasSetting)
	if err != nil || set == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(set.ValueText)) {
	case "true", "on", "yes", "1":
		return true
	}
	return false
}

// How an extra's file was told one.
const (
	byName    = "name"    // its name is a title's file's, then a kind
	byFolder  = "folder"  // it lies in a folder of extras
	byKind    = "kind"    // its name is a kind alone
	byTrailer = "trailer" // its name names a trailer, as the scanner always took one
)

// extraFile is a file the walk took for an extra.
type extraFile struct {
	path     string // absolute
	by       string
	kind     string
	title    string
	mainStem string // byName: the stem of the title's file it names
	anchor   string // the folder the title is told by
}

// walkState is what a walk keeps of the extras it meets.
type walkState struct {
	on    bool
	found []extraFile
	// videos are the video files of each folder by their stems, read once.
	videos map[string][]string
}

func newWalkState(on bool) *walkState { return &walkState{on: on, videos: map[string][]string{}} }

// stems are the stems of the video files directly in dir.
func (w *walkState) stems(dir string) []string {
	if v, ok := w.videos[dir]; ok {
		return v
	}
	var out []string
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, ".") || !videoExts[strings.ToLower(filepath.Ext(name))] {
				continue
			}
			out = append(out, stripExt(name))
		}
	}
	w.videos[dir] = out
	return out
}

// extraFileOf says whether the video file at absPath is an extra, and how:
// with the convention off only a file the scanner always took for a trailer.
func (w *walkState) extraFileOf(absPath, name string) (extraFile, bool) {
	dir := filepath.Dir(absPath)
	stem := stripExt(name)
	if !w.on {
		if isTrailerPath(absPath, name) {
			return extraFile{path: absPath, by: byTrailer, kind: "trailer"}, true
		}
		return extraFile{}, false
	}
	// A file named as an episode (S01E02) is one, also in a folder named as
	// a folder of extras: a show may be called Extras.
	episode := episodePattern.MatchString(name)
	if kind, ok := folderKind(filepath.Base(dir)); ok && !episode && !seriesFolderNames[strings.ToLower(filepath.Base(filepath.Dir(dir)))] {
		anchor := filepath.Dir(dir)
		kind, title := folderExtra(stem, kind, w.stems(anchor))
		return extraFile{path: absPath, by: byFolder, kind: kind, title: title, anchor: anchor}, true
	}
	if main, kind, label, ok := nameOfAnExtra(stem, w.stems(dir)); ok {
		return extraFile{path: absPath, by: byName, kind: kind, title: extraTitle(kind, label), mainStem: main, anchor: dir}, true
	}
	if kind, label, ok := parseKind(stem); ok && bareKinds[kind] && !episode {
		return extraFile{path: absPath, by: byKind, kind: kind, title: extraTitle(kind, label), anchor: dir}, true
	}
	if isTrailerPath(absPath, name) {
		return extraFile{path: absPath, by: byTrailer, kind: "trailer", title: model.ExtraKindTitle("trailer"), anchor: dir}, true
	}
	return extraFile{}, false
}

// dropTrailerRow deletes the trailer asset row (kind trailer) the scanner
// once wrote for the file at path.
func (s *Scanner) dropTrailerRow(ctx context.Context, path string) {
	if tag, err := s.st.Pool().Exec(ctx, `DELETE FROM com_nalet_katalog_playbackassets WHERE path = $1 AND kind = 'trailer'`,
		path); err != nil {
		log.Printf("scanner: the trailer row of %s could not be deleted: %v", path, err)
	} else if tag.RowsAffected() > 0 {
		log.Printf("scanner: deleted the trailer row the scanner once wrote for %s", path)
	}
}

// primaryFile is a title's file.
type primaryFile struct {
	itemID, path, typ string
}

// likePrefix is a LIKE pattern of everything under dir.
func likePrefix(dir string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(dir) + "/%"
}

// primariesIn are the titles' files directly in dir.
func (s *Scanner) primariesIn(ctx context.Context, dir string) ([]primaryFile, error) {
	rows, err := s.st.Pool().Query(ctx, `SELECT p.item_id, p.path, i.type FROM com_nalet_katalog_playbackassets p
		JOIN com_nalet_katalog_items i ON i.id = p.item_id
		WHERE p.isprimary = true AND p.path LIKE $1 ESCAPE '\' ORDER BY p.path`, likePrefix(dir))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []primaryFile
	for rows.Next() {
		var p primaryFile
		if err := rows.Scan(&p.itemID, &p.path, &p.typ); err != nil {
			return nil, err
		}
		if filepath.Dir(p.path) == dir {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// showOf reads an anchor folder under root as a show's: the show's folder,
// series/<Show> (under a folder series, tv, shows or tvshows), or a season's
// folder in it, with its season.
func showOf(root, anchor string) (show string, season *int32, ok bool) {
	rel, err := filepath.Rel(root, anchor)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", nil, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := 0; i+1 < len(parts); i++ {
		if !seriesFolderNames[strings.ToLower(parts[i])] {
			continue
		}
		show = filepath.Join(append([]string{root}, parts[:i+2]...)...)
		switch rest := parts[i+2:]; len(rest) {
		case 0:
			return show, nil, true
		case 1:
			if n, ok := seasonOf(rest[0]); ok {
				return show, &n, true
			}
		}
		return "", nil, false
	}
	return "", nil, false
}

// titleOf finds the title the extra file x is of: the movie whose file it
// names, the one title whose file its anchor folder holds, or the series of
// a show's folder. why says why there is none.
func (s *Scanner) titleOf(ctx context.Context, root string, x extraFile) (itemID string, season *int32, why string, err error) {
	if x.by != byName {
		if show, n, ok := showOf(root, x.anchor); ok {
			return s.seriesOf(ctx, show, n)
		}
	}
	files, err := s.primariesIn(ctx, x.anchor)
	if err != nil {
		return "", nil, "", err
	}
	if x.by == byName {
		var named []primaryFile
		for _, f := range files {
			if strings.EqualFold(stripExt(filepath.Base(f.path)), x.mainStem) {
				named = append(named, f)
			}
		}
		files = named
	}
	switch {
	case len(files) == 0:
		return "", nil, "no title's file is " + map[bool]string{true: "named " + x.mainStem + " in its folder",
			false: "in " + x.anchor}[x.by == byName], nil
	case len(files) > 1:
		return "", nil, "it could be an extra of any of the titles whose files are in " + x.anchor, nil
	}
	switch strings.ToLower(files[0].typ) {
	case "movie":
		return files[0].itemID, nil, "", nil
	case "episode":
		return "", nil, "its title's file is an episode's, and an episode has no extras: a show's go in series/<Show>/extras/", nil
	}
	return "", nil, "its title is a " + files[0].typ + ", and only a movie or a series has extras", nil
}

// seriesOf is the one series the episodes under show belong to, and the
// season, which must be one of its seasons with episodes.
func (s *Scanner) seriesOf(ctx context.Context, show string, season *int32) (string, *int32, string, error) {
	rows, err := s.st.Pool().Query(ctx, `SELECT DISTINCT s.id FROM com_nalet_katalog_playbackassets p
		JOIN com_nalet_katalog_items e ON e.id = p.item_id AND e.type = 'episode'
		JOIN com_nalet_katalog_items par ON par.id = e.parent_id
		LEFT JOIN com_nalet_katalog_items gp ON gp.id = par.parent_id
		JOIN com_nalet_katalog_items s ON s.type = 'series' AND s.id = CASE WHEN par.type = 'series' THEN par.id ELSE gp.id END
		WHERE p.isprimary = true AND p.path LIKE $1 ESCAPE '\' ORDER BY s.id`, likePrefix(show))
	if err != nil {
		return "", nil, "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", nil, "", err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, "", err
	}
	switch {
	case len(ids) == 0:
		return "", nil, "no series has episodes under " + show, nil
	case len(ids) > 1:
		return "", nil, "the episodes under " + show + " belong to " + strings.Join(ids, " and "), nil
	}
	if season != nil {
		var has bool
		if err := s.st.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items e
			WHERE e.type = 'episode' AND e.seasonnumber = $2
			  AND (e.parent_id = $1 OR e.parent_id IN (SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1)))`,
			ids[0], *season).Scan(&has); err != nil {
			return "", nil, "", err
		}
		if !has {
			return "", nil, "the series " + ids[0] + " has no episode in the season its folder names", nil
		}
	}
	return ids[0], season, "", nil
}

// takeExtras takes the extra files a walk met in as their titles' extras,
// or finds them again, and sends the triggers of those that wait to be
// packaged.
func (s *Scanner) takeExtras(ctx context.Context, root string, found []extraFile) {
	var queue []string
	taken := 0
	for _, x := range found {
		itemID, season, why, err := s.titleOf(ctx, root, x)
		if err != nil {
			log.Printf("scanner: the title of the extra %s could not be found: %v", x.path, err)
			continue
		}
		if why != "" {
			log.Printf("scanner: %s is skipped, as no title's extra: %s", x.path, why)
			continue
		}
		ex, err := s.takeExtra(ctx, x, itemID, season)
		if err != nil {
			log.Printf("scanner: %s could not be taken in as an extra of item %s: %v", x.path, itemID, err)
			continue
		}
		if ex == nil {
			continue
		}
		taken++
		if ex.State == model.ExtraPending {
			queue = append(queue, ex.ID)
		}
		s.dropItemOfTheFile(ctx, x.path, itemID)
	}
	if taken > 0 {
		log.Printf("scanner: %d extras found, %d of them to be packaged", taken, len(queue))
	}
	if s.extras != nil && len(queue) > 0 {
		if _, _, err := s.extras.Send(ctx, queue, model.ExtraByScanner); err != nil {
			log.Printf("scanner: the triggers of %d extras could not be sent: %v; the sweep sends them", len(queue), err)
		}
	}
}

// takeExtra takes the file x in as an extra of the title itemID, or finds it
// again: the extra it is already (its file changed, or back after it went
// missing), or an extra of the title moved here (its size and quick hash,
// its old file gone). A file that is another title's extra already is left
// as it is. It answers the extra, nil when it is left alone.
func (s *Scanner) takeExtra(ctx context.Context, x extraFile, itemID string, season *int32) (*model.Extra, error) {
	cur, err := s.st.ExtraAt(ctx, x.path)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.ItemID != itemID {
		log.Printf("scanner: %s is extra %s of item %s already, and stays so", x.path, cur.ID, cur.ItemID)
		return nil, nil
	}
	// A file found again as it was is not read again: its size tells it.
	if cur != nil && cur.State != model.ExtraMissing && cur.SourceSize != nil && cur.SourceQH1 != nil {
		if fi, err := os.Stat(x.path); err == nil && fi.Size() == *cur.SourceSize {
			return cur, nil
		}
	}
	size, qh1, err := extras.QH1(x.path)
	if err != nil {
		return nil, err
	}
	src := store.ExtraSource{Path: x.path, Size: size, QH1: qh1}
	if cur != nil {
		return s.st.UpdateExtraSource(ctx, cur.ID, src, deletedByScanner)
	}
	moved, err := s.st.MovableExtras(ctx, itemID, size, qh1)
	if err != nil {
		return nil, err
	}
	for _, m := range moved {
		if m.SourcePath == nil {
			continue
		}
		if _, err := os.Stat(*m.SourcePath); errors.Is(err, os.ErrNotExist) {
			log.Printf("scanner: extra %s moved from %s to %s", m.ID, *m.SourcePath, x.path)
			return s.st.UpdateExtraSource(ctx, m.ID, src, deletedByScanner)
		}
	}
	ex, _, err := s.st.AddExtra(ctx, store.ExtraWrite{ItemID: itemID, Kind: x.kind, Title: x.title, SeasonNumber: season,
		SourcePath: x.path, SourceSize: size, SourceQH1: qh1, RegisteredBy: model.ExtraByScanner, By: deletedByScanner})
	var conflict *store.ExtraConflict
	if errors.As(err, &conflict) {
		log.Printf("scanner: %v", err)
		return nil, nil
	}
	return ex, err
}

// dropItemOfTheFile takes back what the catalog held of the extra's file at
// path as a file of a title of its own (one scanned before the scanner told
// extras apart): its asset row goes, and the title, left without a file,
// goes too, recorded in the deletion log.
func (s *Scanner) dropItemOfTheFile(ctx context.Context, path, extraOf string) {
	rows, err := s.st.Pool().Query(ctx, `DELETE FROM com_nalet_katalog_playbackassets WHERE path = $1 AND item_id <> $2
		RETURNING item_id`, path, extraOf)
	if err != nil {
		log.Printf("scanner: the asset rows of the extra %s could not be deleted: %v", path, err)
		return
	}
	var owners []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			owners = append(owners, id)
		}
	}
	rows.Close()
	for _, id := range owners {
		var left int
		if err := s.st.Pool().QueryRow(ctx, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, id).
			Scan(&left); err != nil || left > 0 {
			continue
		}
		if _, err := s.st.DeleteItem(ctx, id, store.Deletion{By: deletedByScanner,
			Reason: "its file is an extra of item " + extraOf}); err != nil {
			log.Printf("scanner: item %s has no files left after its file became an extra of item %s, but could not be removed: %v",
				id, extraOf, err)
		}
	}
}

// reconcileExtras marks the extras the scanner took in under root whose file
// is gone: missing, and hidden. A file that cannot be told gone (an error
// other than its absence) is left as it is.
func (s *Scanner) reconcileExtras(ctx context.Context, root string) {
	xs, err := s.st.ScannerExtras(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrNoExtras) {
			log.Printf("scanner: the extras could not be reconciled: %v", err)
		}
		return
	}
	var gone []string
	for _, x := range xs {
		if p := *x.SourcePath; underScanRoot(root, p) {
			if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
				gone = append(gone, x.ID)
			}
		}
	}
	sort.Strings(gone)
	if n, err := s.st.MarkExtrasMissing(ctx, gone, deletedByScanner); err != nil {
		log.Printf("scanner: %d extras whose file is gone could not be marked missing: %v", len(gone), err)
	} else if n > 0 {
		log.Printf("scanner: %d extras' files are gone: missing, until they are back", n)
	}
}

// underScanRoot reports whether path lies inside root.
func underScanRoot(root, path string) bool {
	root = filepath.Clean(root)
	return root != "/" && strings.HasPrefix(filepath.Clean(path), root+string(filepath.Separator))
}
