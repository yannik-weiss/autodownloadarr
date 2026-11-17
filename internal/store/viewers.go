package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Kinds of titles viewers watch and of items their progress belongs to.
const (
	ViewerMovie  = "movie"
	ViewerSeries = "series"

	ItemMovie   = "movie"
	ItemEpisode = "episode"
)

// Sources of a viewer.
const (
	SourceRequest   = "request"
	SourceWatchlist = "watchlist"
	SourceTrakt     = "trakt"
	SourceAuto      = "auto" // started watching without a request
)

// Viewer is a user who watches a movie or series: its requester and everyone
// who joined later. A viewer who left keeps the row (LeftAt set), so
// automatic imports do not add them again.
type Viewer struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	TitleID  int64  `json:"titleId"`
	UserID   int64  `json:"userId"`
	UserName string `json:"userName"`
	Source   string `json:"source"`
	// StartSeason/StartEpisode is where a series viewer begins (0: the
	// series start).
	StartSeason  int        `json:"startSeason"`
	StartEpisode int        `json:"startEpisode"`
	AddedAt      time.Time  `json:"addedAt"`
	LeftAt       *time.Time `json:"leftAt,omitempty"`
}

// Active reports whether the viewer still watches the title.
func (v Viewer) Active() bool { return v.LeftAt == nil }

// Progress is the watch state of one viewer for a movie or episode, read
// from the viewer's media account. UserID 0 is the default account of a
// title without viewers.
type Progress struct {
	ItemKind  string     `json:"-"`
	ItemID    int64      `json:"itemId"`
	UserID    int64      `json:"userId"`
	Base      WatchBase  `json:"-"`
	Progress  float64    `json:"progress"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	WatchedAt *time.Time `json:"watchedAt,omitempty"`
}

// Consumed reports whether the viewer is done with the item.
func (p Progress) Consumed(threshold float64) bool {
	return p.WatchedAt != nil || p.Progress >= threshold
}

const viewerColumns = `v.id, v.kind, v.title_id, v.user_id, COALESCE(u.username, ''), v.source, v.start_season, v.start_episode,
	v.added_at, v.left_at`

func scanViewer(row interface{ Scan(...any) error }) (Viewer, error) {
	var v Viewer
	var added int64
	var left sql.NullInt64
	err := row.Scan(&v.ID, &v.Kind, &v.TitleID, &v.UserID, &v.UserName, &v.Source, &v.StartSeason, &v.StartEpisode, &added, &left)
	if errors.Is(err, sql.ErrNoRows) {
		return Viewer{}, ErrNotFound
	}
	v.AddedAt, v.LeftAt = time.UnixMilli(added), nullTime(left)
	return v, err
}

// Viewers lists the viewers of a title, including those who left, in the
// order they joined.
func (s *Store) Viewers(ctx context.Context, kind string, titleID int64) ([]Viewer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+viewerColumns+` FROM viewers v LEFT JOIN users u ON u.id = v.user_id
		WHERE v.kind = ? AND v.title_id = ? ORDER BY v.added_at, v.id`, kind, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Viewer{}
	for rows.Next() {
		v, err := scanViewer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ActiveViewers returns only the viewers who still watch a title.
func (s *Store) ActiveViewers(ctx context.Context, kind string, titleID int64) ([]Viewer, error) {
	all, err := s.Viewers(ctx, kind, titleID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, v := range all {
		if v.Active() {
			out = append(out, v)
		}
	}
	return out, nil
}

// Viewer returns one viewer row of a title.
func (s *Store) Viewer(ctx context.Context, kind string, titleID, userID int64) (Viewer, error) {
	return scanViewer(s.db.QueryRowContext(ctx, `SELECT `+viewerColumns+` FROM viewers v LEFT JOIN users u ON u.id = v.user_id
		WHERE v.kind = ? AND v.title_id = ? AND v.user_id = ?`, kind, titleID, userID))
}

// AddViewer adds a user as viewer of a title. A viewer who left is only
// added again with rejoin (an explicit request); automatic imports keep
// respecting that they left. It reports whether the user is an active
// viewer afterwards.
func (s *Store) AddViewer(ctx context.Context, v Viewer, rejoin bool) (bool, error) {
	if v.UserID <= 0 || v.TitleID <= 0 {
		return false, nil
	}
	if v.Source == "" {
		v.Source = SourceRequest
	}
	now := s.ms()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO viewers (kind, title_id, user_id, source, start_season, start_episode, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, title_id, user_id) DO NOTHING`,
		v.Kind, v.TitleID, v.UserID, v.Source, v.StartSeason, v.StartEpisode, now); err != nil {
		return false, err
	}
	if rejoin {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE viewers SET left_at = NULL, added_at = ?, source = ?, start_season = ?, start_episode = ?
			WHERE kind = ? AND title_id = ? AND user_id = ? AND left_at IS NOT NULL`,
			now, v.Source, v.StartSeason, v.StartEpisode, v.Kind, v.TitleID, v.UserID); err != nil {
			return false, err
		}
	}
	cur, err := s.Viewer(ctx, v.Kind, v.TitleID, v.UserID)
	if err != nil {
		return false, err
	}
	return cur.Active(), nil
}

// LeaveViewer marks a viewer as gone. Their progress is kept, so joining
// again continues where they were.
func (s *Store) LeaveViewer(ctx context.Context, kind string, titleID, userID int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE viewers SET left_at = ? WHERE kind = ? AND title_id = ? AND user_id = ? AND left_at IS NULL`,
		s.ms(), kind, titleID, userID))
}

// IsViewer reports whether a user is or was a viewer of a title.
func (s *Store) IsViewer(ctx context.Context, kind string, titleID, userID int64) (bool, error) {
	_, err := s.Viewer(ctx, kind, titleID, userID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// WatchedTitles returns the titles of a kind a user actively watches.
func (s *Store) WatchedTitles(ctx context.Context, kind string, userID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT title_id FROM viewers WHERE kind = ? AND user_id = ? AND left_at IS NULL`, kind, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ItemProgress returns the progress of all viewers of the given items:
// item ID → user ID → progress.
func (s *Store) ItemProgress(ctx context.Context, itemKind string, itemIDs ...int64) (map[int64]map[int64]Progress, error) {
	out := map[int64]map[int64]Progress{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	args := []any{itemKind}
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT item_id, user_id, base_set, base_views, base_offset, base_account, progress, started_at, watched_at
		FROM viewer_progress WHERE item_kind = ? AND item_id IN (?`+strings.Repeat(`, ?`, len(itemIDs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p := Progress{ItemKind: itemKind}
		var started, watched sql.NullInt64
		if err := rows.Scan(&p.ItemID, &p.UserID, &p.Base.Set, &p.Base.Views, &p.Base.Offset, &p.Base.Account, &p.Progress, &started, &watched); err != nil {
			return nil, err
		}
		p.StartedAt, p.WatchedAt = nullTime(started), nullTime(watched)
		if out[p.ItemID] == nil {
			out[p.ItemID] = map[int64]Progress{}
		}
		out[p.ItemID][p.UserID] = p
	}
	return out, rows.Err()
}

// SaveProgress stores the progress of one viewer.
func (s *Store) SaveProgress(ctx context.Context, p Progress) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO viewer_progress (item_kind, item_id, user_id, base_set, base_views, base_offset, base_account, progress, started_at, watched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (item_kind, item_id, user_id) DO UPDATE SET
			base_set = excluded.base_set, base_views = excluded.base_views, base_offset = excluded.base_offset,
			base_account = excluded.base_account, progress = excluded.progress, started_at = excluded.started_at,
			watched_at = excluded.watched_at`,
		p.ItemKind, p.ItemID, p.UserID, p.Base.Set, p.Base.Views, p.Base.Offset, p.Base.Account, p.Progress,
		timeArg(p.StartedAt), timeArg(p.WatchedAt))
	return err
}

// ResetProgressBase drops the watch baselines of an item after a new file
// appeared. Progress and watched times stay: a viewer who had seen the item
// keeps it as seen.
func (s *Store) ResetProgressBase(ctx context.Context, itemKind string, itemID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE viewer_progress SET base_set = 0, base_views = 0, base_offset = 0
		WHERE item_kind = ? AND item_id = ?`, itemKind, itemID)
	return err
}

// DeleteProgress forgets all watch state of an item, e.g. when a deleted
// movie is requested again to be watched anew.
func (s *Store) DeleteProgress(ctx context.Context, itemKind string, itemID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM viewer_progress WHERE item_kind = ? AND item_id = ?`, itemKind, itemID)
	return err
}
