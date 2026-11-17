package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/shyper/autodownloadarr/internal/access"
	"github.com/shyper/autodownloadarr/internal/movies"
	"github.com/shyper/autodownloadarr/internal/series"
	"github.com/shyper/autodownloadarr/internal/store"
)

// --- viewers ----------------------------------------------------------------
//
// Several users can watch the same movie or series; see store.Viewer.

type viewerView struct {
	UserID    int64      `json:"userId"`
	UserName  string     `json:"userName"`
	Source    string     `json:"source"`
	AddedAt   time.Time  `json:"addedAt"`
	Progress  float64    `json:"progress"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	WatchedAt *time.Time `json:"watchedAt,omitempty"`
	// Holding: a movie viewer who is still watching keeps the movie.
	Holding bool `json:"holding,omitempty"`
	// Season/Episode: where a series viewer stands (0: done).
	Season  int `json:"season,omitempty"`
	Episode int `json:"episode,omitempty"`
	// Window is the number of episodes a series viewer's window holds.
	Window int `json:"window,omitempty"`
}

func (s *server) handleMovieViewers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := idParam(r, "id")
	mv, err := s.Store.Movie(ctx, id)
	if err != nil {
		s.writeStoreError(w, "read movie", err)
		return
	}
	viewers, err := s.Store.ActiveViewers(ctx, store.ViewerMovie, id)
	if err != nil {
		s.internalError(w, "list viewers", err)
		return
	}
	rows, err := s.Store.ItemProgress(ctx, store.ItemMovie, id)
	if err != nil {
		s.internalError(w, "read progress", err)
		return
	}
	out := make([]viewerView, 0, len(viewers))
	for _, v := range viewers {
		p := rows[id][v.UserID]
		vv := viewerView{UserID: v.UserID, UserName: v.UserName, Source: v.Source, AddedAt: v.AddedAt,
			Progress: p.Progress, StartedAt: p.StartedAt, WatchedAt: p.WatchedAt}
		vv.Holding = mv.HoldUntil != nil && p.StartedAt != nil && p.WatchedAt == nil && v.UserID != mv.RequestedBy
		out = append(out, vv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewers": out, "holdUntil": mv.HoldUntil})
}

func (s *server) handleSeriesViewers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := idParam(r, "id")
	if _, err := s.Store.Series(ctx, id); err != nil {
		s.writeStoreError(w, "read series", err)
		return
	}
	viewers, err := s.Store.ActiveViewers(ctx, store.ViewerSeries, id)
	if err != nil {
		s.internalError(w, "list viewers", err)
		return
	}
	positions, err := s.Series.ViewerPositions(ctx, id)
	if err != nil {
		s.internalError(w, "viewer positions", err)
		return
	}
	out := make([]viewerView, 0, len(viewers))
	for _, v := range viewers {
		vv := viewerView{UserID: v.UserID, UserName: v.UserName, Source: v.Source, AddedAt: v.AddedAt}
		for _, p := range positions {
			if p.UserID != v.UserID {
				continue
			}
			if p.Current != nil {
				vv.Season, vv.Episode = p.Current.Season, p.Current.Episode
			}
			vv.Window = len(p.Window)
		}
		out = append(out, vv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewers": out})
}

type joinRequest struct {
	StartSeason  int `json:"startSeason"`
	StartEpisode int `json:"startEpisode"`
}

// handleJoin makes the signed-in user a viewer of a movie or series.
func (s *server) handleJoin(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, _ := idParam(r, "id")
		var req joinRequest
		if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
			return
		}
		if req.StartSeason < 0 || req.StartEpisode < 0 {
			writeError(w, http.StatusBadRequest, "invalid_input", "invalid start")
			return
		}
		if !s.titleExists(w, r, kind, id) {
			return
		}
		user, _, _ := currentUser(ctx)
		// Joining a title whose files were removed brings them back, like
		// requesting it again.
		var restored bool
		var err error
		switch kind {
		case store.ViewerMovie:
			mv, _ := s.Store.Movie(ctx, id)
			if store.MovieFilesRemoved(mv.State) {
				if !access.MayRestore(ctx, s.Store, user, kind, id, mv.RequestedBy) {
					writeError(w, http.StatusForbidden, "forbidden", "only the request owner, a viewer or an administrator may restore this item")
					return
				}
				_, err = s.Movies.Request(ctx, mv.TMDBID, user.ID, 0)
				restored = true
			}
		default:
			se, _ := s.Store.Series(ctx, id)
			if se.ExpiredAt != nil {
				if !access.MayRestore(ctx, s.Store, user, kind, id, se.RequestedBy) {
					writeError(w, http.StatusForbidden, "forbidden", "only the request owner, a viewer or an administrator may restore this item")
					return
				}
				_, err = s.Series.Request(ctx, se.TMDBID, user.ID, series.RequestOptions{StartSeason: req.StartSeason, StartEpisode: req.StartEpisode})
				restored = true
			}
		}
		if err != nil && !errors.Is(err, movies.ErrAlreadyRequested) && !errors.Is(err, series.ErrAlreadyRequested) {
			s.writeStoreError(w, "restore "+kind, err)
			return
		}
		if !restored {
			v := store.Viewer{Kind: kind, TitleID: id, UserID: user.ID, Source: store.SourceRequest,
				StartSeason: req.StartSeason, StartEpisode: req.StartEpisode}
			if _, err := s.Store.AddViewer(ctx, v, true); err != nil {
				s.internalError(w, "add viewer", err)
				return
			}
		}
		s.viewersChanged(w, r, kind, id)
	}
}

// handleLeave removes the signed-in user from the viewers.
func (s *server) handleLeave(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := currentUser(r.Context())
		id, _ := idParam(r, "id")
		s.leave(w, r, kind, id, user.ID)
	}
}

// handleRemoveViewer lets an administrator remove a viewer.
func (s *server) handleRemoveViewer(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := idParam(r, "id")
		userID, ok := idParam(r, "userId")
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_input", "invalid user id")
			return
		}
		s.leave(w, r, kind, id, userID)
	}
}

func (s *server) leave(w http.ResponseWriter, r *http.Request, kind string, id, userID int64) {
	if !s.titleExists(w, r, kind, id) {
		return
	}
	if err := s.Store.LeaveViewer(r.Context(), kind, id, userID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, "remove viewer", err)
		return
	}
	s.viewersChanged(w, r, kind, id)
}

func (s *server) titleExists(w http.ResponseWriter, r *http.Request, kind string, id int64) bool {
	var err error
	if kind == store.ViewerMovie {
		_, err = s.Store.Movie(r.Context(), id)
	} else {
		_, err = s.Store.Series(r.Context(), id)
	}
	if err != nil {
		s.writeStoreError(w, "read "+kind, err)
		return false
	}
	return true
}

// viewersChanged recalculates the series windows (movies follow on the next
// poll) and answers with the current viewers.
func (s *server) viewersChanged(w http.ResponseWriter, r *http.Request, kind string, id int64) {
	if kind == store.ViewerSeries {
		if err := s.Series.Reconcile(r.Context(), id); err != nil {
			s.internalError(w, "reconcile series", err)
			return
		}
		s.handleSeriesViewers(w, r)
		return
	}
	if err := s.Movies.RescheduleDeletion(r.Context(), id); err != nil {
		s.internalError(w, "reschedule deletion", err)
		return
	}
	s.handleMovieViewers(w, r)
}
