package movies

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shyper/autodownloadarr/internal/db"
	"github.com/shyper/autodownloadarr/internal/events"
	"github.com/shyper/autodownloadarr/internal/mediaserver/pathmap"
	"github.com/shyper/autodownloadarr/internal/scheduler"
	"github.com/shyper/autodownloadarr/internal/secrets"
	"github.com/shyper/autodownloadarr/internal/store"
)

// fakeViews is a Plex movie library whose watch state differs per token.
type fakeViews struct {
	mu    sync.Mutex
	views map[string][2]int // token → view count, resume position in ms
}

func (f *fakeViews) set(token string, views, offset int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views[token] = [2]int{views, offset}
}

func (f *fakeViews) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/library/sections/1/all" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	v, ok := f.views[r.Header.Get("X-Plex-Token")]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	viewed := int64(0)
	if v[0] > 0 || v[1] > 0 {
		viewed = time.Now().Unix()
	}
	_, _ = fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"7","title":"Film","viewCount":%d,"viewOffset":%d,"lastViewedAt":%d,"duration":6000000,
		"Media":[{"Part":[{"file":"/remote/Film (2024)/Film.mkv"}]}]}]}}`, v[0], v[1], viewed)
}

// A viewer who is still watching keeps a movie the requester has finished;
// a profile that starts watching joins as viewer.
func TestViewersHoldMovieDeletion(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sqldb, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	must(t, err)
	t.Cleanup(func() { _ = sqldb.Close() })
	box, _ := secrets.New(make([]byte, 32))
	st := store.New(sqldb, box)
	root := t.TempDir()
	g := store.DefaultGeneral()
	g.MoviesPath, g.DeleteAfterHours, g.StartedExpiryDays = root, 1, 2
	must(t, st.SetGeneral(ctx, g))
	m := NewManager(st, &Services{Store: st, Log: log}, scheduler.New(sqldb, log), events.New(log), log)

	plex := &fakeViews{views: map[string][2]int{"owner": {}, "bob": {}, "carol": {}, "dave": {}}}
	srv := httptest.NewServer(plex)
	defer srv.Close()
	_, err = st.SaveMediaServer(ctx, store.MediaServer{Kind: "plex", Name: "Plex", URL: srv.URL, Token: "owner", MovieLibrary: "1",
		Enabled: true, Mappings: []pathmap.Mapping{{Local: root, Remote: "/remote"}}})
	must(t, err)
	users := map[int64]string{2: "bob", 3: "carol", 4: "dave"}
	infos := []store.MediaProfileInfo{{ExternalID: "owner", Name: "Owner", Owner: true}}
	for id, name := range users {
		_, err := sqldb.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(?,?,'x','user',0,0)`, id, name)
		must(t, err)
		infos = append(infos, store.MediaProfileInfo{ExternalID: name, Name: name})
	}
	must(t, st.SyncMediaProfiles(ctx, infos))
	profiles, err := st.MediaProfiles(ctx)
	must(t, err)
	for _, p := range profiles {
		for id, name := range users {
			if p.ExternalID == name {
				must(t, st.AssignMediaProfile(ctx, p.ID, id, false))
				must(t, st.SetMediaProfileTokens(ctx, p.ID, name, name+"-account"))
			}
		}
	}

	mv, err := st.CreateMovie(ctx, store.Movie{TMDBID: 42, Title: "Film", Year: 2024, State: store.MovieAvailable}, 2)
	must(t, err)
	_, err = st.StartAttempt(ctx, mv.ID, store.Release{Title: "Film.2024"}) // when the file appeared
	must(t, err)
	now := time.Now()
	mv.Folder, mv.AvailableAt = filepath.Join(root, "Film (2024)"), &now
	must(t, st.UpdateMovie(ctx, mv))
	_, err = st.AddViewer(ctx, store.Viewer{Kind: store.ViewerMovie, TitleID: mv.ID, UserID: 3}, true)
	must(t, err)
	poll := func() store.Movie {
		t.Helper()
		must(t, m.PollWatched(ctx))
		cur, err := st.Movie(ctx, mv.ID)
		must(t, err)
		return cur
	}
	poll()

	// carol is half way, bob finishes: the movie counts as watched but is
	// kept for carol until her started-expiry.
	plex.set("carol", 0, 3_000_000)
	plex.set("bob", 1, 0)
	cur := poll()
	if cur.State != store.MovieWatched {
		t.Fatalf("state %s, want watched", cur.State)
	}
	if cur.HoldUntil == nil {
		t.Fatal("no hold for carol")
	}
	at, _ := movieDeletion(cur, g)
	if want := cur.WatchedAt.Add(time.Hour); !at.After(want) || at.Before(cur.HoldUntil.Add(-time.Second)) {
		t.Fatalf("deletion at %v, hold until %v", at, cur.HoldUntil)
	}

	// dave starts watching without being a viewer: he joins.
	plex.set("dave", 0, 1_000_000)
	poll()
	dave, err := st.Viewer(ctx, store.ViewerMovie, mv.ID, 4)
	if err != nil || dave.Source != store.SourceAuto || !dave.Active() {
		t.Fatalf("dave = %+v, %v", dave, err)
	}

	// carol and dave finish: the hold is gone.
	plex.set("carol", 1, 0)
	plex.set("dave", 1, 0)
	if cur = poll(); cur.HoldUntil != nil {
		t.Fatalf("hold until %v after everyone finished", cur.HoldUntil)
	}
	rows, err := st.ItemProgress(ctx, store.ItemMovie, mv.ID)
	must(t, err)
	for _, u := range []int64{2, 3, 4} {
		if rows[mv.ID][u].WatchedAt == nil {
			t.Errorf("user %d not recorded as watched", u)
		}
	}

	// Leaving releases a hold as well.
	must(t, st.DeleteProgress(ctx, store.ItemMovie, mv.ID))
	cur.State, cur.WatchedAt = store.MovieAvailable, nil
	must(t, st.UpdateMovie(ctx, cur))
	plex.set("bob", 1, 0)
	plex.set("carol", 1, 1_000_000)
	poll()
	if cur = poll(); cur.HoldUntil == nil {
		t.Fatal("no hold for carol's second viewing")
	}
	must(t, st.LeaveViewer(ctx, store.ViewerMovie, mv.ID, 3))
	must(t, st.LeaveViewer(ctx, store.ViewerMovie, mv.ID, 4))
	if cur = poll(); cur.HoldUntil != nil {
		t.Fatal("hold survived the viewer leaving")
	}
}
