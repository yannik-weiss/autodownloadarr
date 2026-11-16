package series

import (
	"context"
	"time"

	"github.com/shyper/autodownloadarr/internal/movies"
	"github.com/shyper/autodownloadarr/internal/scheduler"
	"github.com/shyper/autodownloadarr/internal/store"
)

// consumedProgress is the share of an episode after which it counts as seen
// even if the media server has not marked it as watched yet (end credits).
const consumedProgress = 0.9

// Reconcile brings a series in line with its window: episodes that have
// been watched far enough are deleted, and the next WindowSize episodes
// from the current position are searched (or wait for their broadcast).
func (m *Manager) Reconcile(ctx context.Context, id int64) error {
	m.reconciling.Lock()
	defer m.reconciling.Unlock()

	se, err := m.store.Series(ctx, id)
	if err != nil {
		return err
	}
	all, err := m.store.Episodes(ctx, id)
	if err != nil {
		return err
	}
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	if se.ExpiredAt != nil {
		m.bus.Publish(EventSeriesUpdated, se)
		return nil // nothing to keep ready until it is restored
	}
	var nextCheck time.Time
	if at := expiryAt(se, g, all); !at.IsZero() {
		if !now.Before(at) {
			// Expire waits for running downloads, which reconcile when they
			// end, so it runs as its own task.
			_, err := m.sched.Schedule(ctx, scheduler.Spec{Type: taskExpire, Key: expireKey(id), Payload: idPayload{ID: id}})
			return err
		}
		nextCheck = at
	}
	var list []store.Episode
	for _, e := range all {
		if se.Wants(e.Season) && e.State != store.EpisodeSkipped {
			list = append(list, e)
		}
	}

	windows, err := m.viewerWindows(ctx, se, list)
	if err != nil {
		return err
	}

	// 1. Delete episodes every viewer is done with: their trigger fired, the
	// episode lies before where they started, or it is ahead of them beyond
	// their window (it is fetched again once they get there).
	for i := range list {
		e := &list[i]
		if (e.State != store.MovieAvailable && e.State != store.MovieWatched) || se.Keep {
			continue
		}
		due, at, next := episodeDue(se, g, windows, i, now)
		if due {
			if err := m.removeFiles(ctx, se, e); err != nil {
				return err
			}
			e.State, e.DeleteAt, e.DeleteReason = store.MovieDeleted, nil, store.DeleteWatched
			if err := m.store.UpdateEpisode(ctx, *e); err != nil {
				return err
			}
			m.log.Info("episode deleted", "series", se.Title, "season", e.Season, "episode", e.Episode)
			m.publishEpisode(*e)
			continue
		}
		if !at.IsZero() && (e.DeleteAt == nil || !e.DeleteAt.Equal(at)) {
			e.DeleteAt = &at
			if err := m.store.UpdateEpisode(ctx, *e); err != nil {
				return err
			}
			m.publishEpisode(*e)
		}
		if !next.IsZero() && (nextCheck.IsZero() || next.Before(nextCheck)) {
			nextCheck = next
		}
	}

	// 2. Fill the windows of all viewers. Episodes removed automatically are
	// fetched again for a viewer who has not seen them yet.
	if !se.Paused {
		minStart := len(list)
		for _, w := range windows {
			minStart = min(minStart, w.start)
		}
		for i := range list {
			e := &list[i]
			in := false
			for _, w := range windows {
				in = in || w.window[i]
			}
			switch {
			case in && e.State == store.EpisodePending:
				if err := m.want(ctx, e, now, false); err != nil {
					return err
				}
			case in && e.State == store.MovieDeleted && store.Refetchable(e.DeleteReason):
				m.log.Info("fetching episode again for a viewer", "series", se.Title, "season", e.Season, "episode", e.Episode)
				e.File, e.MediaKey, e.DeleteAt, e.AvailableAt, e.Attempts = "", "", nil, nil, 0
				if err := m.want(ctx, e, now, true); err != nil {
					return err
				}
			case !in && i >= minStart && (e.State == store.MovieSearching || e.State == store.MovieWaiting || e.State == store.MovieQueued):
				// The windows moved or shrank: not needed for now.
				if err := m.sched.Cancel(ctx, searchKey(e.ID)); err != nil {
					return err
				}
				e.State, e.NextSearchAt, e.LastError = store.EpisodePending, nil, ""
				if err := m.store.UpdateEpisode(ctx, *e); err != nil {
					return err
				}
				m.publishEpisode(*e)
			}
		}
	}

	if !nextCheck.IsZero() {
		if _, err := m.sched.Schedule(ctx, scheduler.Spec{Type: taskCheck, Key: checkKey(id), RunAt: nextCheck,
			Payload: idPayload{ID: id}}); err != nil {
			return err
		}
	}
	m.bus.Publish(EventSeriesUpdated, se)
	return nil
}

// want puts an episode of the window into the queue: search now if it has
// aired, else wait for the broadcast.
// With reuse, the release the episode was downloaded from before is tried
// first.
func (m *Manager) want(ctx context.Context, e *store.Episode, now time.Time, reuse bool) error {
	e.LastError = ""
	if e.Aired(now) {
		e.State, e.NextSearchAt = store.MovieSearching, nil
		if err := m.store.UpdateEpisode(ctx, *e); err != nil {
			return err
		}
		m.publishEpisode(*e)
		if reuse {
			return m.scheduleReuse(ctx, e.ID)
		}
		return m.scheduleSearch(ctx, e.ID, 0, time.Time{})
	}
	e.State = store.MovieWaiting
	e.NextSearchAt = nil
	if d, err := time.ParseInLocation(time.DateOnly, e.AirDate, now.Location()); err == nil {
		// Releases usually appear within hours after the broadcast.
		at := d.Add(24 * time.Hour)
		e.NextSearchAt = &at
	}
	if err := m.store.UpdateEpisode(ctx, *e); err != nil {
		return err
	}
	m.publishEpisode(*e)
	if e.NextSearchAt != nil {
		return m.scheduleSearch(ctx, e.ID, 0, *e.NextSearchAt)
	}
	return nil // unknown date: the daily refresh will bring one
}

// Position is where the viewer stands in a series: the current episode
// (being watched, or the next one up) and the next episode still to be
// broadcast. Either can be nil.
func Position(se store.Series, eps []store.Episode, now time.Time) (current, upcoming *store.Episode) {
	var list []store.Episode
	for _, e := range eps {
		if se.Wants(e.Season) && e.State != store.EpisodeSkipped {
			list = append(list, e)
		}
	}
	if start := windowStart(list); start < len(list) {
		current = &list[start]
	}
	for i := range list {
		if list[i].AirDate != "" && !list[i].Aired(now) {
			upcoming = &list[i]
			break
		}
	}
	return current, upcoming
}

// windowStart is the index of the current episode: the last one started,
// or the one after it when it has been seen.
func windowStart(list []store.Episode) int {
	last := -1
	for i, e := range list {
		if e.Progress > 0 || e.WatchedAt != nil {
			last = i
		}
	}
	start := max(last, 0)
	if last >= 0 && consumed(list[last]) {
		start = last + 1
	}
	for start < len(list) && list[start].State == store.MovieDeleted {
		start++
	}
	return start
}

func consumed(e store.Episode) bool {
	return e.WatchedAt != nil || e.Progress >= consumedProgress
}

// deletionDue decides whether episode i of the list may be deleted now,
// or else when it will be (zero if not yet known).
func deletionDue(se store.Series, g store.General, list []store.Episode, i int, now time.Time) (bool, time.Time) {
	e := list[i]
	var at time.Time
	earliest := func(t time.Time) {
		if at.IsZero() || t.Before(at) {
			at = t
		}
	}
	for _, later := range list[i+1:] {
		switch se.DeleteTrigger {
		case store.TriggerProgress:
			if later.Progress*100 >= float64(se.TriggerValue) || later.WatchedAt != nil {
				return true, time.Time{}
			}
		case store.TriggerMinutes:
			if later.StartedAt != nil {
				earliest(later.StartedAt.Add(time.Duration(se.TriggerValue) * time.Minute))
			}
		case store.TriggerWatched:
			// Skipped (or left unfinished): it counts as seen once a later
			// episode has been watched.
			if e.WatchedAt == nil && later.WatchedAt != nil {
				earliest(later.WatchedAt.Add(time.Duration(se.TriggerValue) * time.Minute))
			}
		}
	}
	if e.WatchedAt != nil {
		if se.DeleteTrigger == store.TriggerWatched {
			earliest(e.WatchedAt.Add(time.Duration(se.TriggerValue) * time.Minute))
		}
		// Without a next episode (season or series finale) the movie delay
		// applies.
		earliest(e.WatchedAt.Add(time.Duration(g.DeleteAfterHours) * time.Hour))
	}
	if !at.IsZero() && !now.Before(at) {
		return true, time.Time{}
	}
	return false, at
}

// viewerWindow is one viewer's view of a series: the episodes with the
// viewer's own progress, where they started, where they are and which
// episodes their window holds.
type viewerWindow struct {
	userID int64
	eps    []store.Episode
	first  int // index of the episode the viewer started the series at
	start  int // index of the viewer's current episode
	window map[int]bool
}

// viewerWindows builds the window of every active viewer (or of the
// requester when nobody watches the series any more).
func (m *Manager) viewerWindows(ctx context.Context, se store.Series, list []store.Episode) ([]viewerWindow, error) {
	viewers, err := m.store.Viewers(ctx, store.ViewerSeries, se.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(list))
	for i, e := range list {
		ids[i] = e.ID
	}
	rows, err := m.store.ItemProgress(ctx, store.ItemEpisode, ids...)
	if err != nil {
		return nil, err
	}
	starts := map[int64][2]int{}
	for _, v := range viewers {
		starts[v.UserID] = [2]int{v.StartSeason, v.StartEpisode}
	}
	users, _ := movies.Participants(viewers, se.RequestedBy)
	out := make([]viewerWindow, 0, len(users))
	for _, u := range users {
		w := viewerWindow{userID: u, eps: make([]store.Episode, len(list)), window: map[int]bool{}}
		for i, e := range list {
			p, ok := rows[e.ID][u]
			if !ok && len(rows[e.ID]) == 0 && u == se.RequestedBy {
				// No viewer data yet (e.g. before the first poll): the
				// episode's own state is the requester's.
				p = store.Progress{Progress: e.Progress, StartedAt: e.StartedAt, WatchedAt: e.WatchedAt}
			}
			e.Progress, e.StartedAt, e.WatchedAt = p.Progress, p.StartedAt, p.WatchedAt
			w.eps[i] = e
		}
		if st := starts[u]; st[0] > 0 {
			for w.first < len(list) && (list[w.first].Season < st[0] || list[w.first].Season == st[0] && list[w.first].Episode < st[1]) {
				w.first++
			}
		}
		w.start = viewerStart(w.eps, w.first)
		for i, n := w.start, 0; i < len(list) && n < se.WindowSize; i++ {
			if skipForViewer(w.eps[i]) {
				continue
			}
			w.window[i] = true
			n++
		}
		out = append(out, w)
	}
	return out, nil
}

// viewerStart is the index of a viewer's current episode: the last one
// they started, or the one after it when they have seen it; without any,
// where they started the series.
func viewerStart(eps []store.Episode, first int) int {
	last := -1
	for i, e := range eps {
		if e.Progress > 0 || e.WatchedAt != nil {
			last = i
		}
	}
	start := first
	if last >= 0 {
		start = last
		if consumed(eps[last]) {
			start = last + 1
		}
	}
	for start < len(eps) && skipForViewer(eps[start]) {
		start++
	}
	return start
}

// skipForViewer reports whether a removed episode stays out of a viewer's
// window: removed by the user or outside AutoDownloadarr, or already seen
// by this viewer.
func skipForViewer(e store.Episode) bool {
	if e.State != store.MovieDeleted && e.State != store.MovieExpired {
		return false
	}
	return !store.Refetchable(e.DeleteReason) || consumed(e)
}

// episodeDue decides whether episode i may be deleted now for all viewers.
// at is when it will be, if known for every viewer still holding it; next
// is when to check again.
func episodeDue(se store.Series, g store.General, windows []viewerWindow, i int, now time.Time) (due bool, at, next time.Time) {
	someoneDone, holding, unknown := false, false, false
	for _, w := range windows {
		e := w.eps[i]
		vDue, vAt := deletionDue(se, g, w.eps, i, now)
		switch {
		case vDue:
			someoneDone = true
			continue
		case i < w.first && e.Progress == 0 && e.WatchedAt == nil:
			continue // before where they started
		case i > w.start && !w.window[i] && e.Progress == 0 && e.WatchedAt == nil:
			continue // ahead of them, beyond their window
		}
		holding = true
		if vAt.IsZero() {
			unknown = true
			continue
		}
		if vAt.After(at) {
			at = vAt
		}
		if next.IsZero() || vAt.Before(next) {
			next = vAt
		}
	}
	if someoneDone && !holding {
		return true, time.Time{}, time.Time{}
	}
	if unknown {
		at = time.Time{}
	}
	return false, at, next
}

// ViewerPosition is where a viewer of a series stands.
type ViewerPosition struct {
	UserID  int64
	Current *store.Episode // the episode they watch or watch next (nil: done)
	Window  []int64        // episode IDs their window holds
}

// ViewerPositions returns where each viewer of a series stands.
func (m *Manager) ViewerPositions(ctx context.Context, id int64) ([]ViewerPosition, error) {
	se, err := m.store.Series(ctx, id)
	if err != nil {
		return nil, err
	}
	all, err := m.store.Episodes(ctx, id)
	if err != nil {
		return nil, err
	}
	var list []store.Episode
	for _, e := range all {
		if se.Wants(e.Season) && e.State != store.EpisodeSkipped {
			list = append(list, e)
		}
	}
	windows, err := m.viewerWindows(ctx, se, list)
	if err != nil {
		return nil, err
	}
	out := make([]ViewerPosition, 0, len(windows))
	for _, w := range windows {
		p := ViewerPosition{UserID: w.userID}
		if w.start < len(list) {
			e := list[w.start]
			p.Current = &e
		}
		for i := range list {
			if w.window[i] {
				p.Window = append(p.Window, list[i].ID)
			}
		}
		out = append(out, p)
	}
	return out, nil
}
