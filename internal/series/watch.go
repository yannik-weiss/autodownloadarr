package series

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shyper/autodownloadarr/internal/fetch"
	"github.com/shyper/autodownloadarr/internal/mediaserver"
	"github.com/shyper/autodownloadarr/internal/movies"
	"github.com/shyper/autodownloadarr/internal/store"
)

// pollWatched reads progress and watch state of the episodes from the media server. It
// runs every minute so that the next episode is fetched soon after the
// current one was started.
func (m *Manager) pollWatched(ctx context.Context) error {
	return m.PollWatched(ctx)
}

// PollWatched compares the episodes on disk with the media server now.
func (m *Manager) PollWatched(ctx context.Context) error {
	m.requestMu.Lock()
	defer m.requestMu.Unlock()
	target, err := m.svc.MediaTV(ctx)
	if err != nil || target == nil {
		return err
	}
	eps, err := m.store.EpisodesInState(ctx, store.MovieAvailable, store.MovieWatched)
	if err != nil || len(eps) == 0 {
		return err
	}
	libs := movies.NewAccountLibraries(m.svc, target, m.log,
		func(ctx context.Context, srv mediaserver.Server) ([]mediaserver.Item, error) {
			return srv.Episodes(ctx, target.Library)
		},
		func(_ mediaserver.Item, f string) string { return filepath.Clean(target.Mapper.ToLocal(f)) })
	if _, err := libs.Index(ctx, ""); err != nil {
		return err
	}
	series := map[int64]store.Series{}
	viewers := map[int64][]store.Viewer{}

	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	changedSeries := map[int64]bool{}
	for _, e := range eps {
		missing, err := movies.FilesMissing(ctx, m.store, store.ForEpisode(e.ID), g.SeriesPath)
		if err != nil {
			return err
		}
		if missing {
			files, err := m.store.FilesOf(ctx, store.ForEpisode(e.ID))
			if err != nil {
				return err
			}
			for _, f := range files {
				if err := m.store.UntrackFile(ctx, f); err != nil {
					return err
				}
			}
			e.State, e.File, e.MediaKey, e.DeleteAt, e.DeleteReason = store.MovieDeleted, "", "", nil, store.DeleteMissing
			if err := m.store.UpdateEpisode(ctx, e); err != nil {
				return err
			}
			m.publishEpisode(e)
			changedSeries[e.SeriesID] = true
			continue
		}
		se, ok := series[e.SeriesID]
		if !ok {
			if se, err = m.store.Series(ctx, e.SeriesID); err != nil {
				return err
			}
			series[e.SeriesID] = se
			if viewers[e.SeriesID], err = m.store.Viewers(ctx, store.ViewerSeries, e.SeriesID); err != nil {
				return err
			}
		}
		changed, err := m.observeEpisode(ctx, libs, se, viewers[e.SeriesID], &e, now)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		if err := m.store.UpdateEpisode(ctx, e); err != nil {
			return err
		}
		m.publishEpisode(e)
		changedSeries[e.SeriesID] = true
	}
	for id := range changedSeries {
		if err := m.Reconcile(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// observeEpisode updates the watch state of every viewer of an episode and
// the episode's summary of it: the furthest progress, the first start and
// the first viewing. Profiles that played it without being viewers of the
// series join as viewers.
func (m *Manager) observeEpisode(ctx context.Context, libs *movies.AccountLibraries, se store.Series, viewers []store.Viewer,
	e *store.Episode, now time.Time,
) (bool, error) {
	rows, err := m.store.ItemProgress(ctx, store.ItemEpisode, e.ID)
	if err != nil {
		return false, err
	}
	progress := rows[e.ID]
	if progress == nil {
		progress = map[int64]store.Progress{}
	}
	users, primary := movies.Participants(viewers, se.RequestedBy)
	owner := store.ForEpisode(e.ID)
	lookup := func(ix *movies.ItemIndex) (mediaserver.Item, bool) {
		return ix.Lookup(filepath.Clean(e.File), e.MediaKey)
	}
	mediaKey := e.MediaKey
	viewerChanged := false // a viewer moved on: their window may change
	for _, u := range users {
		account, err := libs.Account(ctx, u)
		if err != nil {
			return false, err
		}
		ix, err := libs.Index(ctx, account)
		if err != nil {
			return false, err
		}
		if ix == nil {
			continue // the profile cannot be read right now; its last state counts
		}
		it, ok := lookup(ix)
		if !ok {
			continue // not scanned yet
		}
		mediaKey = it.Key
		p, had := progress[u]
		if !had {
			p = store.Progress{ItemKind: store.ItemEpisode, ItemID: e.ID, UserID: u}
			if u == primary {
				// State from before viewers existed.
				p.Base, p.Progress, p.StartedAt, p.WatchedAt = e.Base, e.Progress, e.StartedAt, e.WatchedAt
			}
		}
		prev := p
		movies.PrepareBase(&p, account, it)
		watched, pr := movies.Observe(&p.Base, it, movies.FileSince(ctx, m.store, owner, p.Base))
		observeProgress(&p, watched, pr, now)
		if !had || prev != p {
			if err := m.store.SaveProgress(ctx, p); err != nil {
				return false, err
			}
			viewerChanged = viewerChanged || prev.Progress != p.Progress || !sameTime(prev.WatchedAt, p.WatchedAt)
		}
		progress[u] = p
	}

	known := map[int64]bool{}
	for _, v := range viewers {
		known[v.UserID] = true
	}
	since := movies.FileSince(ctx, m.store, owner, store.WatchBase{})
	detected, err := libs.Detect(ctx, known, since, lookup)
	if err != nil {
		return false, err
	}
	for _, d := range detected {
		active, err := m.store.AddViewer(ctx, store.Viewer{Kind: store.ViewerSeries, TitleID: se.ID, UserID: d.UserID,
			Source: store.SourceAuto, StartSeason: e.Season, StartEpisode: e.Episode}, false)
		if err != nil {
			return false, err
		}
		if !active {
			continue
		}
		p := store.Progress{ItemKind: store.ItemEpisode, ItemID: e.ID, UserID: d.UserID}
		watched, pr := movies.Observe(&p.Base, d.Item, since)
		p.Base.Account = d.Account
		observeProgress(&p, watched, pr, now)
		if err := m.store.SaveProgress(ctx, p); err != nil {
			return false, err
		}
		progress[d.UserID] = p
		viewerChanged = true
		m.log.Info("viewer joined by watching", "series", se.Title, "season", e.Season, "episode", e.Episode, "user", d.UserID)
	}

	// The episode's own fields summarise its viewers.
	before := *e
	e.MediaKey, e.Progress, e.StartedAt, e.WatchedAt = mediaKey, 0, nil, nil
	for _, u := range append(users, detectedUsers(detected)...) {
		p, ok := progress[u]
		if !ok {
			continue
		}
		e.Progress = max(e.Progress, p.Progress)
		e.StartedAt = earlier(e.StartedAt, p.StartedAt)
		e.WatchedAt = earlier(e.WatchedAt, p.WatchedAt)
	}
	switch {
	case e.WatchedAt != nil:
		e.State = store.MovieWatched
	case e.State == store.MovieWatched:
		e.State, e.DeleteAt = store.MovieAvailable, nil
	}
	return viewerChanged || before.MediaKey != e.MediaKey || before.Progress != e.Progress || before.State != e.State ||
		!sameTime(before.StartedAt, e.StartedAt) || !sameTime(before.WatchedAt, e.WatchedAt), nil
}

// observeProgress applies an observation to a viewer's progress.
func observeProgress(p *store.Progress, watched bool, progress float64, now time.Time) {
	p.Progress = progress
	if progress > 0 && p.StartedAt == nil {
		p.StartedAt = &now
	}
	switch {
	case watched && p.WatchedAt == nil:
		p.WatchedAt = &now
	case !watched && p.WatchedAt != nil:
		p.WatchedAt = nil // marked unwatched again
	}
}

func detectedUsers(d []movies.Detected) []int64 {
	out := make([]int64, 0, len(d))
	for _, x := range d {
		out = append(out, x.UserID)
	}
	return out
}

func earlier(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.Before(*a)) {
		return b
	}
	return a
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// scan asks the media server to look at a folder of the TV library.
func (m *Manager) scan(ctx context.Context, folder string) { m.svc.Scan(ctx, true, folder) }

// removeFiles deletes the tracked files of an episode, only inside the
// series folder.
func (m *Manager) removeFiles(ctx context.Context, se store.Series, e *store.Episode) error {
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	files, err := m.store.FilesOf(ctx, store.ForEpisode(e.ID))
	if err != nil {
		return err
	}
	root := g.SeriesPath
	for _, f := range files {
		if root == "" || !within(root, f) {
			return fmt.Errorf("refusing to delete tracked file outside the library: %s", f)
		}
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := m.store.UntrackFile(ctx, f); err != nil {
			return err
		}
		_ = os.Remove(filepath.Dir(f)) // season folder, if now empty
	}
	if root != "" {
		// The download may have been cancelled before it tracked a file.
		_ = os.RemoveAll(filepath.Join(root, fetch.WorkDirName, "incomplete", "e"+strconv.FormatInt(e.ID, 10)))
		if len(files) > 0 {
			m.scan(ctx, filepath.Join(root, SeriesFolder(se)))
		}
	}
	e.File, e.MediaKey = "", ""
	return nil
}

// cleanupFolder removes the empty folders of a deleted series.
func (m *Manager) cleanupFolder(ctx context.Context, se store.Series) {
	g, err := m.store.General(ctx)
	if err != nil || g.SeriesPath == "" {
		return
	}
	dir := filepath.Join(g.SeriesPath, SeriesFolder(se))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	_ = os.Remove(dir)
	m.scan(ctx, g.SeriesPath)
}

func within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
