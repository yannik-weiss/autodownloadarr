package movies

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shyper/autodownloadarr/internal/events"
	"github.com/shyper/autodownloadarr/internal/fetch"
	"github.com/shyper/autodownloadarr/internal/mediaserver"
	"github.com/shyper/autodownloadarr/internal/notify"
	"github.com/shyper/autodownloadarr/internal/scheduler"
	"github.com/shyper/autodownloadarr/internal/store"
)

// available records a finished download and tells the media server.
func (m *Manager) available(ctx context.Context, mv store.Movie, rel store.Release, video string) error {
	info, err := os.Stat(video)
	if err != nil {
		return err
	}
	if err := m.store.TrackFile(ctx, mv.ID, video, info.Size()); err != nil {
		return err
	}
	now := m.now()
	mv.State = store.MovieAvailable
	mv.Folder = filepath.Dir(video)
	mv.ReleaseKey, mv.ReleaseTitle, mv.ReleaseFingerprint = rel.Identity(), rel.Title, rel.Fingerprint
	mv.AvailableAt = &now
	mv.Attempts, mv.LastError, mv.NextSearchAt = 0, "", nil
	mv.StartedAt, mv.WatchedAt, mv.DeleteAt, mv.DeleteNotBefore = nil, nil, nil, nil
	mv.Progress, mv.Base = 0, store.WatchBase{} // a new file: the next poll takes the baseline
	mv.HoldUntil, mv.DeleteReason = nil, ""
	if err := m.store.UpdateMovie(ctx, mv); err != nil {
		return err
	}
	if err := m.store.ResetProgressBase(ctx, store.ItemMovie, mv.ID); err != nil {
		return err
	}
	m.log.Info("movie available", "movie", mv.Title, "file", video)
	m.publish(mv)
	m.notify(notify.MovieMessage(events.NotifyMovieAvailable, mv, rel.Title))
	m.scan(ctx, mv.Folder)
	return m.scheduleDeletion(ctx, mv)
}

// Observe compares a media server item with the baseline of a file. On the
// first sighting the baseline is taken, unless the item was played after
// since (when the file appeared; it is playable while downloading): then
// that playback counts. Marking the item unwatched in Plex lowers the
// baseline again. It returns whether the file was watched since and how
// far (0..1).
func Observe(base *store.WatchBase, it mediaserver.Item, since time.Time) (watched bool, progress float64) {
	account := base.Account
	defer func() { base.Account = account }()
	switch {
	case !base.Set && !since.IsZero() && !it.LastViewedAt.Before(since.Truncate(time.Second)): // Plex has seconds
		views := it.ViewCount
		if views > 0 && it.ViewOffset == 0 {
			views-- // just watched to the end
		}
		*base = store.WatchBase{Set: true, Views: views}
	case !base.Set || it.ViewCount < base.Views:
		*base = store.WatchBase{Set: true, Views: it.ViewCount, Offset: it.ViewOffset.Milliseconds()}
	}
	b := mediaserver.Baseline{Views: base.Views, Offset: time.Duration(base.Offset) * time.Millisecond}
	return it.WatchedSince(b), it.ProgressSince(b)
}

// fileSince is when the current file of a movie or episode started to
// appear (the start of its last download), needed only while no baseline
// has been taken.
func (m *Manager) fileSince(ctx context.Context, o store.Owner, base store.WatchBase) time.Time {
	return FileSince(ctx, m.store, o, base)
}

// FileSince is fileSince for other packages.
func FileSince(ctx context.Context, st *store.Store, o store.Owner, base store.WatchBase) time.Time {
	if base.Set {
		return time.Time{}
	}
	attempts, err := st.AttemptsOf(ctx, o)
	if err != nil || len(attempts) == 0 {
		return time.Time{}
	}
	return attempts[0].StartedAt
}

// scan asks the media server to look at a folder; failures are retried.
func (m *Manager) scan(ctx context.Context, folder string) { m.svc.Scan(ctx, false, folder) }

func (m *Manager) handleDelete(ctx context.Context, t scheduler.Task) error {
	m.requestMu.Lock()
	defer m.requestMu.Unlock()
	p, err := decode[deletePayload](t)
	if err != nil {
		return err
	}
	mv, err := m.store.Movie(ctx, p.MovieID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if mv.Keep {
		return nil
	}
	switch {
	case p.Reason == "watched" && mv.State == store.MovieWatched:
	case p.Reason == "expired" && mv.State == store.MovieAvailable && mv.StartedAt == nil:
	case p.Reason == "started" && mv.State == store.MovieAvailable && mv.StartedAt != nil:
	default:
		return nil // the state changed meanwhile
	}
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	at, reason := movieDeletion(mv, g)
	if at.IsZero() || reason != p.Reason || at.After(m.now()) {
		return m.scheduleDeletion(ctx, mv)
	}
	if err := m.removeFiles(ctx, &mv); err != nil {
		return err
	}
	mv.State, mv.DeleteAt, mv.DeleteReason = store.MovieDeleted, nil, p.Reason
	if p.Reason != "watched" {
		mv.State = store.MovieExpired // nobody watched it to the end
	}
	if err := m.store.UpdateMovie(ctx, mv); err != nil {
		return err
	}
	m.log.Info("movie deleted", "movie", mv.Title, "reason", p.Reason)
	m.publish(mv)
	return nil
}

// removeFiles deletes the tracked files of a movie. Only files inside the
// movies folder are touched, so a wrong record can never delete anything
// else (e.g. the Radarr library).
func (m *Manager) removeFiles(ctx context.Context, mv *store.Movie) error {
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	files, err := m.store.Files(ctx, mv.ID)
	if err != nil {
		return err
	}
	root := g.MoviesPath
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
	}
	if mv.Folder != "" && root != "" && within(root, mv.Folder) {
		fetch.Cleanup(filepath.Join(root, fetch.WorkDirName, "incomplete", strconv.FormatInt(mv.ID, 10)), mv.Folder, nil)
		m.scan(ctx, root) // let Plex notice the removal
	}
	mv.Folder, mv.MediaKey = "", ""
	return nil
}

// within reports whether p lies strictly inside root.
func within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pollWatched reads the watch state from the media server. It runs every minute and
// does the actual work at the configured interval.
func (m *Manager) pollWatched(ctx context.Context) error {
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	due := m.now().Sub(m.lastPoll) >= time.Duration(max(g.WatchPollMinutes, 1))*time.Minute-time.Second
	if due {
		m.lastPoll = m.now()
	}
	m.mu.Unlock()
	if !due {
		return nil
	}
	return m.PollWatched(ctx)
}

// PollWatched compares the available and watched movies with the media server now.
func (m *Manager) PollWatched(ctx context.Context) error {
	m.requestMu.Lock()
	defer m.requestMu.Unlock()
	target, err := m.svc.Media(ctx)
	if err != nil || target == nil {
		return err
	}
	list, err := m.store.Movies(ctx, store.MovieAvailable, store.MovieWatched)
	if err != nil || len(list) == 0 {
		return err
	}
	// Matched by our own file only: another copy of the movie (e.g. from
	// Radarr) has a watch state of its own.
	libs := NewAccountLibraries(m.svc, target, m.log,
		func(ctx context.Context, srv mediaserver.Server) ([]mediaserver.Item, error) {
			return srv.Movies(ctx, target.Library)
		},
		func(_ mediaserver.Item, f string) string { return filepath.Dir(target.Mapper.ToLocal(f)) })
	if _, err := libs.Index(ctx, ""); err != nil {
		return err
	}
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}

	for _, mv := range list {
		missing, err := FilesMissing(ctx, m.store, store.ForMovie(mv.ID), g.MoviesPath)
		if err != nil {
			return err
		}
		if missing {
			files, err := m.store.Files(ctx, mv.ID)
			if err != nil {
				return err
			}
			for _, f := range files {
				if err := m.store.UntrackFile(ctx, f); err != nil {
					return err
				}
			}
			mv.State, mv.DeleteAt, mv.Folder, mv.MediaKey, mv.DeleteReason = store.MovieDeleted, nil, "", "", store.DeleteMissing
			if err := m.store.UpdateMovie(ctx, mv); err != nil {
				return err
			}
			if err := m.sched.Cancel(ctx, deleteKey(mv.ID)); err != nil {
				return err
			}
			m.publish(mv)
			continue
		}
		changed, err := m.observeViewers(ctx, libs, target, g, &mv)
		if err != nil {
			return err
		}
		if changed {
			if err := m.store.UpdateMovie(ctx, mv); err != nil {
				return err
			}
			m.publish(mv)
		}
		if err := m.scheduleDeletion(ctx, mv); err != nil {
			return err
		}
	}
	return nil
}

// observeViewers updates the watch state of every viewer of a movie. The
// primary viewer (see Participants) decides whether the movie counts as
// watched; other viewers who are still watching hold back its deletion.
// Profiles that played the movie without being viewers join as viewers.
func (m *Manager) observeViewers(ctx context.Context, libs *AccountLibraries, target *Target, g store.General, mv *store.Movie) (bool, error) {
	viewers, err := m.store.Viewers(ctx, store.ViewerMovie, mv.ID)
	if err != nil {
		return false, err
	}
	rows, err := m.store.ItemProgress(ctx, store.ItemMovie, mv.ID)
	if err != nil {
		return false, err
	}
	users, primary := Participants(viewers, mv.RequestedBy)
	owner := store.ForMovie(mv.ID)
	lookup := func(ix *ItemIndex) (mediaserver.Item, bool) { return ix.Lookup(filepath.Clean(mv.Folder), mv.MediaKey) }
	now := m.now()
	before := *mv
	changed := false
	var hold *time.Time
	incomplete := false
	extend := func(started *time.Time) {
		until := holdUntil(*started, g, now)
		if hold == nil || until.After(*hold) {
			hold = &until
		}
	}
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
			incomplete = true // the profile cannot be read right now
			continue
		}
		it, ok := lookup(ix)
		if !ok {
			continue // not scanned yet
		}
		p, had := rows[mv.ID][u]
		if !had {
			p = store.Progress{ItemKind: store.ItemMovie, ItemID: mv.ID, UserID: u}
			if u == primary {
				p.Base = mv.Base // state from before viewers existed
			}
		}
		if u == primary {
			// The movie itself carries the primary viewer's state.
			p.Progress, p.StartedAt, p.WatchedAt = mv.Progress, mv.StartedAt, mv.WatchedAt
			if mv.State != store.MovieWatched {
				p.WatchedAt = nil
			}
		}
		prev := p
		PrepareBase(&p, account, it)
		cur := *mv
		cur.Base, cur.Progress, cur.StartedAt, cur.WatchedAt = p.Base, p.Progress, p.StartedAt, p.WatchedAt
		if u != primary {
			cur.State = store.MovieAvailable
			if p.WatchedAt != nil {
				cur.State = store.MovieWatched
			}
		}
		observeMovie(&cur, it, m.fileSince(ctx, owner, cur.Base), now)
		p.Base, p.Progress, p.StartedAt, p.WatchedAt = cur.Base, cur.Progress, cur.StartedAt, nil
		if cur.State == store.MovieWatched {
			p.WatchedAt = cur.WatchedAt
		}
		if !had || prev != p {
			if err := m.store.SaveProgress(ctx, p); err != nil {
				return false, err
			}
		}
		if u == primary {
			if cur.WatchedAt != nil && mv.WatchedAt == nil {
				at, _ := movieDeletion(cur, g)
				m.log.Info("movie watched", "movie", mv.Title, "delete_at", at)
			}
			mv.State, mv.WatchedAt, mv.StartedAt, mv.Progress = cur.State, cur.WatchedAt, cur.StartedAt, cur.Progress
			mv.Base, mv.MediaKey, mv.DeleteNotBefore = cur.Base, cur.MediaKey, cur.DeleteNotBefore
			changed = movieObserved(before, *mv)
		} else if p.StartedAt != nil && p.WatchedAt == nil {
			extend(p.StartedAt)
		}
	}

	known := map[int64]bool{}
	for _, v := range viewers {
		known[v.UserID] = true
	}
	since := m.fileSince(ctx, owner, store.WatchBase{})
	detected, err := libs.Detect(ctx, known, since, lookup)
	if err != nil {
		return false, err
	}
	for _, d := range detected {
		active, err := m.store.AddViewer(ctx, store.Viewer{Kind: store.ViewerMovie, TitleID: mv.ID, UserID: d.UserID, Source: store.SourceAuto}, false)
		if err != nil || !active {
			return false, err
		}
		p := store.Progress{ItemKind: store.ItemMovie, ItemID: mv.ID, UserID: d.UserID, StartedAt: &now}
		watched, progress := Observe(&p.Base, d.Item, since)
		p.Base.Account, p.Progress = d.Account, progress
		if watched {
			p.WatchedAt = &now
		} else {
			extend(p.StartedAt)
		}
		if err := m.store.SaveProgress(ctx, p); err != nil {
			return false, err
		}
		m.log.Info("viewer joined by watching", "movie", mv.Title, "user", d.UserID)
	}

	if incomplete && mv.HoldUntil != nil && (hold == nil || mv.HoldUntil.After(*hold)) {
		hold = mv.HoldUntil // keep what is known until the profile can be read again
	}
	if !sameTime(hold, mv.HoldUntil) {
		mv.HoldUntil, changed = hold, true
	}
	return changed, nil
}

// holdUntil is how long a viewer who started at started keeps a movie: the
// expiry for started movies, else the one for unwatched movies, else one
// more day each time it is checked.
func holdUntil(started time.Time, g store.General, now time.Time) time.Time {
	switch {
	case g.StartedExpiryDays > 0:
		return started.Add(time.Duration(g.StartedExpiryDays) * 24 * time.Hour)
	case g.UnwatchedExpiryDays > 0:
		return started.Add(time.Duration(g.UnwatchedExpiryDays) * 24 * time.Hour)
	}
	return now.Add(24 * time.Hour)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// movieObserved reports whether an observation changed what is stored.
func movieObserved(before, after store.Movie) bool {
	return before.MediaKey != after.MediaKey || before.Base != after.Base || before.State != after.State ||
		!sameTime(before.StartedAt, after.StartedAt) || !sameTime(before.WatchedAt, after.WatchedAt) ||
		!sameTime(before.DeleteNotBefore, after.DeleteNotBefore) ||
		math.Abs(before.Progress-after.Progress) >= 0.01 ||
		(before.Progress != after.Progress && (after.Progress == 0 || after.Progress == 1))
}

// observeMovie tracks individual viewing cycles. For watched movies Base.Offset
// follows the resume position; a nonzero position starting over begins a replay.
// Series continue to use Observe's file-wide baseline unchanged.
func observeMovie(mv *store.Movie, it mediaserver.Item, since, now time.Time) bool {
	before := *mv
	replay := mv.State == store.MovieWatched && it.ViewCount == mv.Base.Views+1 &&
		it.ViewOffset > 0 && (mv.Base.Offset == 0 || it.ViewOffset.Milliseconds() < mv.Base.Offset)
	completedAgain := mv.State == store.MovieWatched && it.ViewCount > mv.Base.Views+1
	if replay {
		mv.DeleteNotBefore = mv.DeleteAt
		mv.State, mv.WatchedAt, mv.StartedAt = store.MovieAvailable, nil, &now
		mv.Base = store.WatchBase{Set: true, Views: it.ViewCount, Account: mv.Base.Account}
	}
	watched, progress := Observe(&mv.Base, it, since)
	mv.MediaKey, mv.Progress = it.Key, progress
	if progress > 0 && mv.StartedAt == nil {
		mv.StartedAt = &now
	}
	switch {
	case watched && (mv.State == store.MovieAvailable || completedAgain):
		if completedAgain || mv.DeleteNotBefore != nil {
			mv.DeleteNotBefore = mv.DeleteAt
		}
		mv.State, mv.WatchedAt = store.MovieWatched, &now
		// Keep exactly one completed viewing above the cycle baseline so an
		// increased play count also detects a replay completed between polls.
		mv.Base.Views = it.ViewCount - 1
	case !watched && mv.State == store.MovieWatched:
		mv.State, mv.WatchedAt = store.MovieAvailable, nil
	}
	if mv.State == store.MovieWatched {
		mv.Base.Offset = it.ViewOffset.Milliseconds()
	}
	return before.MediaKey != mv.MediaKey || before.Base != mv.Base || before.State != mv.State ||
		before.StartedAt != mv.StartedAt || before.WatchedAt != mv.WatchedAt ||
		math.Abs(before.Progress-mv.Progress) >= 0.01 ||
		(before.Progress != mv.Progress && (mv.Progress == 0 || mv.Progress == 1))
}

// FilesMissing only confirms removal when all tracked files are absent and the
// library root is accessible. Scan delays and filesystem errors are not deletions.
func FilesMissing(ctx context.Context, st *store.Store, owner store.Owner, root string) (bool, error) {
	if root == "" {
		return false, nil
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return false, nil
	}
	files, err := st.FilesOf(ctx, owner)
	if err != nil {
		return false, err
	}
	if len(files) == 0 {
		return false, nil
	}
	for _, f := range files {
		if !within(root, f) {
			return false, nil
		}
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			return false, nil
		}
	}
	return true, nil
}
