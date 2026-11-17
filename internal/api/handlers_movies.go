package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shyper/autodownloadarr/internal/access"
	"github.com/shyper/autodownloadarr/internal/metadata/tmdb"
	"github.com/shyper/autodownloadarr/internal/movies"
	"github.com/shyper/autodownloadarr/internal/store"
)

// searchResult is a TMDB hit, annotated with the state of an existing
// request.
type searchResult struct {
	tmdb.Movie
	PosterURL string `json:"posterUrl,omitempty"`
	MovieID   int64  `json:"movieId,omitempty"`
	State     string `json:"state,omitempty"`
}

func (s *server) handleSearchMovies(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []searchResult{})
		return
	}
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	client, err := s.Services.TMDB(r.Context())
	if err != nil {
		s.writeStoreError(w, "tmdb", err)
		return
	}
	found, err := client.SearchMovies(r.Context(), q, year)
	if err != nil {
		s.writeStoreError(w, "search movies", err)
		return
	}
	out := make([]searchResult, 0, len(found))
	for _, m := range found {
		res := searchResult{Movie: m, PosterURL: tmdb.PosterURL(m.PosterPath, "w342")}
		if existing, err := s.Store.MovieByTMDB(r.Context(), m.ID); err == nil {
			res.MovieID, res.State = existing.ID, existing.State
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, out)
}

// movieView adds the poster URL for the UI.
type movieView struct {
	store.Movie
	PosterURL string `json:"posterUrl,omitempty"`
}

func view(m store.Movie) movieView {
	return movieView{Movie: m, PosterURL: tmdb.PosterURL(m.PosterPath, "w342")}
}

// personal shows movies as the signed-in user sees them: for a movie
// requested by someone else that the user watches too, the user's own
// progress and whether they have seen it.
func (s *server) personal(ctx context.Context, list []store.Movie) ([]store.Movie, error) {
	user, _, _ := currentUser(ctx)
	mine, err := s.Store.WatchedTitles(ctx, store.ViewerMovie, user.ID)
	if err != nil || len(mine) == 0 {
		return list, err
	}
	var ids []int64
	for _, m := range list {
		if mine[m.ID] && m.RequestedBy != user.ID {
			ids = append(ids, m.ID)
		}
	}
	rows, err := s.Store.ItemProgress(ctx, store.ItemMovie, ids...)
	if err != nil {
		return nil, err
	}
	for i, m := range list {
		if !mine[m.ID] || m.RequestedBy == user.ID {
			continue
		}
		p := rows[m.ID][user.ID]
		m.Progress, m.StartedAt, m.WatchedAt = p.Progress, p.StartedAt, p.WatchedAt
		if m.State == store.MovieAvailable || m.State == store.MovieWatched {
			m.State = store.MovieAvailable
			if p.WatchedAt != nil {
				m.State = store.MovieWatched
			}
		}
		list[i] = m
	}
	return list, nil
}

// personalView is view for one movie as the signed-in user sees it.
func (s *server) personalView(ctx context.Context, m store.Movie) (movieView, error) {
	list, err := s.personal(ctx, []store.Movie{m})
	if err != nil {
		return movieView{}, err
	}
	return view(list[0]), nil
}

func (s *server) handleListMovies(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.Movies(r.Context())
	if err != nil {
		s.internalError(w, "list movies", err)
		return
	}
	if list, err = s.personal(r.Context(), list); err != nil {
		s.internalError(w, "list movies", err)
		return
	}
	out := make([]movieView, len(list))
	for i, m := range list {
		out[i] = view(m)
	}
	writeJSON(w, http.StatusOK, out)
}

type requestMovie struct {
	TMDBID    int   `json:"tmdbId"`
	ProfileID int64 `json:"profileId"`
}

func (s *server) handleRequestMovie(w http.ResponseWriter, r *http.Request) {
	var req requestMovie
	if !decodeJSON(w, r, &req) {
		return
	}
	user, _, _ := currentUser(r.Context())
	if existing, err := s.Store.MovieByTMDB(r.Context(), req.TMDBID); err == nil && store.MovieFilesRemoved(existing.State) &&
		!access.MayRestore(r.Context(), s.Store, user, store.ViewerMovie, existing.ID, existing.RequestedBy) {
		writeError(w, http.StatusForbidden, "forbidden", "only the request owner or an administrator may restore this item")
		return
	}
	m, err := s.Movies.Request(r.Context(), req.TMDBID, user.ID, req.ProfileID)
	if errors.Is(err, movies.ErrAlreadyRequested) {
		writeJSON(w, http.StatusOK, view(m))
		return
	}
	if err != nil {
		s.writeStoreError(w, "request movie", err)
		return
	}
	writeJSON(w, http.StatusCreated, view(m))
}

// movieDetail is a movie with its search results and history.
type movieDetail struct {
	movieView
	Releases  []store.Release        `json:"releases"`
	Attempts  []store.Attempt        `json:"attempts"`
	Blocklist []store.BlocklistEntry `json:"blocklist"`
}

func (s *server) handleGetMovie(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	m, err := s.Store.Movie(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, "read movie", err)
		return
	}
	if m.Genres == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if client, err := s.Services.TMDB(ctx); err == nil {
			if meta, err := client.Movie(ctx, m.TMDBID); err == nil {
				m.Genres = meta.Genres
				if err := s.Store.SetMovieGenres(ctx, m.ID, meta.Genres); err != nil {
					s.Log.Warn("cache genres", "err", err)
				}
			}
		}
	}

	mv, err := s.personalView(r.Context(), m)
	if err != nil {
		s.internalError(w, "read movie", err)
		return
	}
	d := movieDetail{movieView: mv}
	if d.Releases, err = s.Store.Releases(r.Context(), id); err != nil {
		s.internalError(w, "read releases", err)
		return
	}
	if d.Attempts, err = s.Store.Attempts(r.Context(), id); err != nil {
		s.internalError(w, "read attempts", err)
		return
	}
	if d.Blocklist, err = s.Store.BlocklistEntries(r.Context(), id); err != nil {
		s.internalError(w, "read blocklist", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) handleMovieSearch(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	m, err := s.Movies.Search(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeStoreError(w, "search movie", err)
			return
		}
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view(m))
}

func (s *server) handleMovieKeep(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	var req struct {
		Keep bool `json:"keep"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := s.Movies.SetKeep(r.Context(), id, req.Keep)
	if err != nil {
		s.writeStoreError(w, "keep movie", err)
		return
	}
	writeJSON(w, http.StatusOK, view(m))
}

func (s *server) handleMovieSkip(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	if err := s.Movies.SkipRelease(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, "invalid_state", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) handleMovieDownloadRelease(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	releaseID, _ := idParam(r, "releaseId")
	m, err := s.Movies.DownloadRelease(r.Context(), id, releaseID)
	if err != nil {
		s.writeStoreError(w, "download release", err)
		return
	}
	writeJSON(w, http.StatusOK, view(m))
}

func (s *server) handleDeleteMovie(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	forget := r.URL.Query().Get("forget") == "1"
	if err := s.Movies.Delete(r.Context(), id, forget); err != nil {
		s.writeStoreError(w, "delete movie", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDeleteBlocklist(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	if err := s.Store.RemoveBlocklistEntry(r.Context(), id); err != nil {
		s.writeStoreError(w, "remove blocklist entry", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDownloads(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Movies.Downloads())
}

func (s *server) handlePlexPoll(w http.ResponseWriter, r *http.Request) {
	if err := s.Movies.PollWatched(r.Context()); err != nil {
		s.writeStoreError(w, "poll plex", err)
		return
	}
	if err := s.Series.PollWatched(r.Context()); err != nil {
		s.writeStoreError(w, "poll plex", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleQueue(w http.ResponseWriter, r *http.Request) {
	q, err := s.Store.Queue(r.Context())
	if err != nil {
		s.internalError(w, "read queue", err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

type historyPage struct {
	Items []store.HistoryEntry `json:"items"`
	Next  int64                `json:"next,omitempty"` // "before" for the next page
}

func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.HistoryFilter{Kind: q.Get("kind"), Result: q.Get("result"), Limit: 50}
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	items, err := s.Store.History(r.Context(), f)
	if err != nil {
		s.internalError(w, "read history", err)
		return
	}
	page := historyPage{Items: items}
	if len(items) == f.Limit {
		page.Next = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, page)
}

// handleMovieReport rejects a faulty release and requests its replacement.
func (s *server) handleMovieReport(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r, "id")
	var req struct {
		ReleaseKey string `json:"releaseKey"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	item, err := s.Movies.ReportRelease(r.Context(), id, req.ReleaseKey)
	if err != nil {
		s.writeStoreError(w, "report movie release", err)
		return
	}
	writeJSON(w, http.StatusAccepted, item)
}
