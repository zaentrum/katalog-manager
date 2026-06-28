package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// itemBaseCols is the base com_nalet_katalog_items projection (no computed cols).
const itemBaseCols = `id, createdat, createdby, modifiedat, modifiedby, type, title, sorttitle,
	year, description, rating, durationms, parent_id, seasonnumber, episodenumber, tagline`

func scanItemBase(row pgx.Row, i *model.Item) error {
	return row.Scan(&i.ID, &i.CreatedAt, &i.CreatedBy, &i.ModifiedAt, &i.ModifiedBy, &i.Type, &i.Title,
		&i.SortTitle, &i.Year, &i.Description, &i.Rating, &i.DurationMs, &i.ParentID, &i.SeasonNumber, &i.EpisodeNumber, &i.Tagline)
}

// ChildrenByParent returns the items whose parent_id is the given id, ordered
// to mirror Episodes (seasonnumber, episodenumber) then title.
func (s *Store) ChildrenByParent(ctx context.Context, parentID string) ([]*model.Item, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+itemBaseCols+` FROM com_nalet_katalog_items
		WHERE parent_id = $1 ORDER BY seasonnumber NULLS FIRST, episodenumber NULLS FIRST, title`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Item
	for rows.Next() {
		var i model.Item
		if err := scanItemBase(rows, &i); err != nil {
			return nil, err
		}
		out = append(out, &i)
	}
	return out, rows.Err()
}

// ItemIsPackaged reproduces the view formula: a movie/episode is packaged when
// it has an hev1/hvc1 playback asset; a series is packaged when every episode
// is packaged (and it has at least one episode).
func (s *Store) ItemIsPackaged(ctx context.Context, id, typ string) (bool, error) {
	var ok bool
	switch typ {
	case "series":
		err := s.pool.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM com_nalet_katalog_items c
				WHERE c.parent_id = $1 AND c.type = 'episode')
			AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_items c
				WHERE c.parent_id = $1 AND c.type = 'episode'
				AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
					WHERE a.item_id = c.id AND (a.codec LIKE 'hev1%' OR a.codec LIKE 'hvc1%')))`, id).Scan(&ok)
		return ok, err
	case "movie", "episode":
		err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
			WHERE a.item_id = $1 AND (a.codec LIKE 'hev1%' OR a.codec LIKE 'hvc1%'))`, id).Scan(&ok)
		return ok, err
	default:
		return false, nil
	}
}

// ItemHasSegment reports whether the item has a media segment of the given kind.
func (s *Store) ItemHasSegment(ctx context.Context, id, kind string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_mediasegments
		WHERE item_id = $1 AND kind = $2)`, id, kind).Scan(&ok)
	return ok, err
}

func (s *Store) AssetsByItem(ctx context.Context, id string) ([]*model.PlaybackAsset, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, item_id, path, codec, resolution, bitratekbps, sizebytes,
		hash, isprimary, kind, audiocodec, audiolanguage, audiochannels, audiobitratekbps,
		audiotrackcount, subtitletrackcount, durationms, sizemb
		FROM katalogservice_playbackassets WHERE item_id = $1 ORDER BY isprimary DESC NULLS LAST, path`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.PlaybackAsset
	for rows.Next() {
		var a model.PlaybackAsset
		if err := rows.Scan(&a.ID, &a.ItemID, &a.Path, &a.Codec, &a.Resolution, &a.BitrateKbps, &a.SizeBytes,
			&a.Hash, &a.IsPrimary, &a.Kind, &a.AudioCodec, &a.AudioLanguage, &a.AudioChannels, &a.AudioBitrateKbps,
			&a.AudioTrackCount, &a.SubtitleTrackCount, &a.DurationMs, &a.SizeMB); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *Store) SubtitlesByItem(ctx context.Context, id string) ([]*model.SubtitleAsset, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, item_id, path, format, lang, label, isdefault
		FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY isdefault DESC NULLS LAST, lang`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.SubtitleAsset
	for rows.Next() {
		var x model.SubtitleAsset
		if err := rows.Scan(&x.ID, &x.ItemID, &x.Path, &x.Format, &x.Lang, &x.Label, &x.IsDefault); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) SegmentsByItem(ctx context.Context, id string) ([]*model.MediaSegment, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, createdat, modifiedat, item_id, kind, startms, endms, source, confidence, label
		FROM com_nalet_katalog_mediasegments WHERE item_id = $1 ORDER BY startms`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.MediaSegment
	for rows.Next() {
		var x model.MediaSegment
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.ModifiedAt, &x.ItemID, &x.Kind, &x.StartMs, &x.EndMs, &x.Source, &x.Confidence, &x.Label); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) ChaptersByItem(ctx context.Context, id string) ([]*model.ItemChapter, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, createdat, modifiedat, item_id, startms, endms, title, ordinal
		FROM com_nalet_katalog_itemchapters WHERE item_id = $1 ORDER BY ordinal NULLS LAST, startms`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemChapter
	for rows.Next() {
		var x model.ItemChapter
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.ModifiedAt, &x.ItemID, &x.StartMs, &x.EndMs, &x.Title, &x.Ordinal); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) TrailerLinksByItem(ctx context.Context, id string) ([]*model.ItemTrailerLink, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, createdat, modifiedat, item_id, source, site, externalid, url,
		title, durationsec, publishedat, downloadedat, localpath
		FROM com_nalet_katalog_itemtrailerlinks WHERE item_id = $1 ORDER BY publishedat DESC NULLS LAST`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemTrailerLink
	for rows.Next() {
		var x model.ItemTrailerLink
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.ModifiedAt, &x.ItemID, &x.Source, &x.Site, &x.ExternalID, &x.URL,
			&x.Title, &x.DurationSec, &x.PublishedAt, &x.DownloadedAt, &x.LocalPath); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) DiagnosticsByItem(ctx context.Context, id string) (*model.ItemDiagnostics, error) {
	var x model.ItemDiagnostics
	err := s.pool.QueryRow(ctx, `SELECT id, item_id, generatedat, sourcepath, sourcesize, sourcemtime,
		ffprobedata, folderlisting, notes FROM com_nalet_katalog_itemdiagnostics WHERE item_id = $1 LIMIT 1`, id).Scan(
		&x.ID, &x.ItemID, &x.GeneratedAt, &x.SourcePath, &x.SourceSize, &x.SourceMtime, &x.FfprobeData, &x.FolderListing, &x.Notes)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) StepsByItem(ctx context.Context, id string) ([]*model.ItemProcessingStep, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, createdat, modifiedat, item_id, step, status, startedat,
		finishedat, attempts, error, details, statuscriticality
		FROM katalogservice_itemprocessingsteps WHERE item_id = $1 ORDER BY modifiedat DESC NULLS LAST`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemProcessingStep
	for rows.Next() {
		var x model.ItemProcessingStep
		if err := rows.Scan(&x.ID, &x.CreatedAt, &x.ModifiedAt, &x.ItemID, &x.Step, &x.Status, &x.StartedAt,
			&x.FinishedAt, &x.Attempts, &x.Error, &x.Details, &x.StatusCriticality); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) OverallStatusByItem(ctx context.Context, id string) (*model.ItemOverallStatus, error) {
	var x model.ItemOverallStatus
	err := s.pool.QueryRow(ctx, `SELECT item_id, overallstatus, donecount, pendingcount, failedcount,
		inprogresscount, notapplicablecount, totalsteps, laststepfinishedat
		FROM katalogservice_itemoverallstatus WHERE item_id = $1`, id).Scan(
		&x.ItemID, &x.OverallStatus, &x.DoneCount, &x.PendingCount, &x.FailedCount,
		&x.InProgressCount, &x.NotApplicableCount, &x.TotalSteps, &x.LastStepFinishedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) GenresByItem(ctx context.Context, id string) ([]*model.Genre, error) {
	rows, err := s.pool.Query(ctx, `SELECT g.id, g.name FROM com_nalet_katalog_itemgenres ig
		JOIN com_nalet_katalog_genres g ON g.id = ig.genre_id WHERE ig.item_id = $1 ORDER BY g.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Genre
	for rows.Next() {
		var x model.Genre
		if err := rows.Scan(&x.ID, &x.Name); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

// PeopleByItem returns the item-person link rows and the parallel person rows.
func (s *Store) PeopleByItem(ctx context.Context, id string) ([]*model.ItemPerson, []*model.Person, error) {
	rows, err := s.pool.Query(ctx, `SELECT ip.id, ip.item_id, ip.person_id, ip.role, p.name
		FROM com_nalet_katalog_itempeople ip JOIN com_nalet_katalog_people p ON p.id = ip.person_id
		WHERE ip.item_id = $1 ORDER BY ip.role, p.name`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var links []*model.ItemPerson
	var people []*model.Person
	for rows.Next() {
		var ip model.ItemPerson
		var name string
		if err := rows.Scan(&ip.ID, &ip.ItemID, &ip.PersonID, &ip.Role, &name); err != nil {
			return nil, nil, err
		}
		links = append(links, &ip)
		people = append(people, &model.Person{ID: ip.PersonID, Name: name})
	}
	return links, people, rows.Err()
}

func (s *Store) TagsByItem(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT tag FROM com_nalet_katalog_itemtags WHERE item_id = $1 ORDER BY tag`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ExternalIDsByItem(ctx context.Context, id string) ([]*model.ItemExternalID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, item_id, source, externalid
		FROM com_nalet_katalog_itemexternalids WHERE item_id = $1 ORDER BY source`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemExternalID
	for rows.Next() {
		var x model.ItemExternalID
		if err := rows.Scan(&x.ID, &x.ItemID, &x.Source, &x.ExternalID); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) ArtworkByItem(ctx context.Context, id string) ([]*model.ItemArtwork, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, item_id, kind, url
		FROM com_nalet_katalog_itemartwork WHERE item_id = $1 ORDER BY kind`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemArtwork
	for rows.Next() {
		var x model.ItemArtwork
		if err := rows.Scan(&x.ID, &x.ItemID, &x.Kind, &x.URL); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}
