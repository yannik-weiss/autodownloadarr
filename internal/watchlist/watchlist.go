// Package watchlist imports the Plex watchlists: the signed-in account's and
// those of Plex Home profiles assigned to users. Movies and series on them
// are requested automatically for that user, so adding a title in Plex is
// enough to get it.
package watchlist

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shyper/autodownloadarr/internal/mediaserver"
	"github.com/shyper/autodownloadarr/internal/mediaserver/plex"
	"github.com/shyper/autodownloadarr/internal/movies"
	"github.com/shyper/autodownloadarr/internal/series"
	"github.com/shyper/autodownloadarr/internal/store"
)

// Manager polls the watchlist and requests new titles.
type Manager struct {
	store  *store.Store
	svc    *movies.Services
	movies *movies.Manager
	series *series.Manager
	log    *slog.Logger
	now    func() time.Time

	mu   sync.Mutex
	last time.Time
}

// NewManager returns a watchlist poller.
func NewManager(st *store.Store, svc *movies.Services, mv *movies.Manager, se *series.Manager, log *slog.Logger) *Manager {
	return &Manager{store: st, svc: svc, movies: mv, series: se, log: log, now: time.Now}
}

// Poll checks the watchlist if the configured interval has passed.
func (m *Manager) Poll(ctx context.Context) error {
	return m.poll(ctx, false)
}

// PollNow checks the watchlist regardless of the interval.
func (m *Manager) PollNow(ctx context.Context) error {
	return m.poll(ctx, true)
}

func (m *Manager) poll(ctx context.Context, force bool) error {
	g, err := m.store.General(ctx)
	if err != nil {
		return err
	}
	if !g.Watchlist {
		return nil
	}
	interval := time.Duration(max(g.WatchlistPollMinutes, 1)) * time.Minute
	m.mu.Lock()
	due := force || m.last.IsZero() || m.now().Sub(m.last) >= interval-time.Second
	if due {
		m.last = m.now()
	}
	m.mu.Unlock()
	if !due {
		return nil
	}

	ms, found, err := m.store.MediaServer(ctx)
	if err != nil {
		return err
	}
	if !found || mediaserver.Kind(ms.Kind) != mediaserver.Plex {
		return nil
	}
	acc, err := m.svc.PlexAccount(ctx)
	if err != nil {
		return err
	}
	profiles, err := m.store.MediaProfiles(ctx)
	if err != nil {
		return err
	}
	// The owner's watchlist (the account that signed in) belongs to the user
	// its profile is assigned to; other profiles only when assigned and
	// switched on.
	sources := []source{}
	if ms.AccountToken != "" {
		owner := source{token: ms.AccountToken}
		for _, p := range profiles {
			if p.Owner {
				owner.userID = p.UserID
			}
		}
		sources = append(sources, owner)
	}
	for _, p := range profiles {
		if !p.Owner && p.Watchlist && p.UserID > 0 && p.AccountToken != "" {
			sources = append(sources, source{token: p.AccountToken, userID: p.UserID, profileID: p.ID, account: p.AccountKey()})
		}
	}
	var firstErr error
	for _, src := range sources {
		err := m.pollSource(ctx, acc, src)
		if src.profileID > 0 {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if serr := m.store.SetMediaProfileError(ctx, src.profileID, msg); serr != nil {
				return serr
			}
		}
		if err != nil {
			m.log.Warn("Plex watchlist import failed", "profile", src.profileID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// source is one watchlist: the owner's or that of an assigned profile.
type source struct {
	token     string
	userID    int64  // requests are attributed to this user (0: the first user)
	profileID int64  // 0 for the owner
	account   string // watch account, see store.WatchBase.Account
}

func (m *Manager) pollSource(ctx context.Context, acc *plex.Account, src source) error {
	items, err := acc.Watchlist(ctx, src.token)
	if err != nil {
		return err
	}
	for _, it := range items {
		id := tmdbID(it.GUIDs)
		if id == 0 {
			m.log.Debug("watchlist item without TMDB id", "title", it.Title, "guids", it.GUIDs)
			continue
		}
		switch it.Type {
		case "movie":
			mv, lookupErr := m.store.MovieByTMDB(ctx, id)
			watched := false
			if lookupErr == nil {
				if watched, err = m.watchedBy(ctx, mv, src); err != nil {
					return err
				}
			}
			if watched {
				key := it.Key
				if it.PlexGUID != "" {
					key = it.PlexGUID[strings.LastIndex(it.PlexGUID, "/")+1:]
				}
				if key == "" {
					continue
				}
				if err := acc.RemoveFromWatchlist(ctx, src.token, key); err != nil {
					return err // the next scheduled poll retries the removal
				}
				m.log.Info("watched movie removed from Plex watchlist", "movie", mv.Title, "profile", src.profileID)
				continue
			}
			m.requestMovie(ctx, id, it.Title, src.userID)
		case "show":
			m.requestSeries(ctx, id, it.Title, src.userID)
		}
	}
	return nil
}

// watchedBy reports whether the owner of a watchlist has seen a movie: the
// viewer's own progress, or for the signed-in account without an assigned
// user the movie's state when it follows that account.
func (m *Manager) watchedBy(ctx context.Context, mv store.Movie, src source) (bool, error) {
	if src.userID > 0 {
		rows, err := m.store.ItemProgress(ctx, store.ItemMovie, mv.ID)
		if err != nil {
			return false, err
		}
		if p, ok := rows[mv.ID][src.userID]; ok {
			return p.WatchedAt != nil, nil
		}
		if mv.RequestedBy != src.userID {
			return false, nil
		}
	}
	if mv.WatchedAt == nil {
		return false, nil
	}
	account, err := m.svc.AccountForUser(ctx, mv.RequestedBy)
	return account == src.account, err
}

func (m *Manager) requestMovie(ctx context.Context, tmdbID int, title string, userID int64) {
	if mv, err := m.store.MovieByTMDB(ctx, tmdbID); err == nil {
		// Already there: the watchlist owner watches it too.
		if _, err := m.store.AddViewer(ctx, store.Viewer{Kind: store.ViewerMovie, TitleID: mv.ID, UserID: userID, Source: store.SourceWatchlist}, false); err != nil {
			m.log.Warn("add watchlist viewer failed", "movie", mv.Title, "err", err)
		}
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		return
	}
	mv, err := m.movies.RequestFromWatchlist(ctx, tmdbID, userID)
	switch {
	case err == nil:
		m.log.Info("movie requested from the Plex watchlist", "movie", mv.Title)
	case errors.Is(err, movies.ErrAlreadyRequested), errors.Is(err, movies.ErrWatchlistExcluded):
	default:
		m.log.Warn("request movie from the Plex watchlist failed", "title", title, "tmdb", tmdbID, "err", err)
	}
}

func (m *Manager) requestSeries(ctx context.Context, tmdbID int, title string, userID int64) {
	if se, err := m.store.SeriesByTMDB(ctx, tmdbID); err == nil {
		// Already there: the watchlist owner watches it too, with a window
		// of their own.
		active, err := m.store.AddViewer(ctx, store.Viewer{Kind: store.ViewerSeries, TitleID: se.ID, UserID: userID, Source: store.SourceWatchlist}, false)
		if err == nil && active {
			err = m.series.Reconcile(ctx, se.ID)
		}
		if err != nil {
			m.log.Warn("add watchlist viewer failed", "series", se.Title, "err", err)
		}
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		return
	}
	se, err := m.series.Request(ctx, tmdbID, userID, series.RequestOptions{})
	switch {
	case err == nil:
		m.log.Info("series requested from the Plex watchlist", "series", se.Title)
	case errors.Is(err, series.ErrAlreadyRequested):
	default:
		m.log.Warn("request series from the Plex watchlist failed", "title", title, "tmdb", tmdbID, "err", err)
	}
}

// tmdbID extracts the TMDB ID from Plex GUIDs (tmdb://693134).
func tmdbID(guids []string) int {
	for _, g := range guids {
		if strings.HasPrefix(g, "tmdb://") {
			id, err := strconv.Atoi(strings.TrimPrefix(g, "tmdb://"))
			if err == nil {
				return id
			}
		}
	}
	return 0
}
