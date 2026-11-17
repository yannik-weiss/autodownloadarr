package api

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/shyper/autodownloadarr/internal/store"
)

func TestViewerEndpoints(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	e.completeSetup(t, c)
	ctx := context.Background()
	resp, body := e.do(t, c, "POST", "/api/v1/users", map[string]any{"username": "bob", "password": "longenough", "role": "user"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	bob := int64(body["id"].(float64))
	mv, err := e.store.CreateMovie(ctx, store.Movie{TMDBID: 42, Title: "Film", State: store.MovieAvailable}, 0) // the admin's
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/movies/" + strconv.FormatInt(mv.ID, 10)

	bc := e.client(t)
	if resp, body := e.do(t, bc, "POST", "/api/v1/auth/login", map[string]string{"username": "bob", "password": "longenough"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	resp, body = e.do(t, bc, "POST", path+"/watch", nil)
	if list, _ := body["viewers"].([]any); resp.StatusCode != http.StatusOK || len(list) != 2 {
		t.Fatalf("join: %d %v", resp.StatusCode, body)
	}
	// A viewer may not delete the movie; only its owner or an admin.
	if resp, _ := e.do(t, bc, "DELETE", path, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer delete: %d", resp.StatusCode)
	}
	resp, body = e.do(t, bc, "DELETE", path+"/watch", nil)
	if list, _ := body["viewers"].([]any); resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("leave: %d %v", resp.StatusCode, body)
	}
	// Removing someone else is for administrators.
	if _, err := e.store.AddViewer(ctx, store.Viewer{Kind: store.ViewerMovie, TitleID: mv.ID, UserID: bob}, true); err != nil {
		t.Fatal(err)
	}
	removePath := path + "/viewers/" + strconv.FormatInt(bob, 10)
	if resp, _ := e.do(t, bc, "DELETE", path+"/viewers/1", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin removal: %d", resp.StatusCode)
	}
	resp, body = e.do(t, c, "DELETE", removePath, nil)
	if list, _ := body["viewers"].([]any); resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("admin removal: %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, c, "GET", "/api/v1/movies/999/viewers", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown movie: %d", resp.StatusCode)
	}
}

// Each viewer sees their own watch state of a shared series, and with
// several viewers every present episode says for whom it is there.
func TestSeriesViewPerViewer(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	e.completeSetup(t, c)
	ctx := context.Background()
	if resp, body := e.do(t, c, "POST", "/api/v1/users", map[string]any{"username": "bob", "password": "longenough", "role": "user"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	se, err := e.store.CreateSeries(ctx, store.Series{TMDBID: 7, Title: "Show", WindowSize: 2, Seasons: []int{1}}, 0) // the admin's
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpsertEpisodes(ctx, se.ID, []store.Episode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2},
		{Season: 1, Episode: 3}, {Season: 1, Episode: 4}}); err != nil {
		t.Fatal(err)
	}
	eps, err := e.store.Episodes(ctx, se.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Episode 3 is there for the admin only, episode 4 was removed after
	// the admin saw it; bob's window holds episodes 1 and 2.
	for i := range eps {
		eps[i].State, eps[i].AvailableAt = store.MovieAvailable, &now
		switch i {
		case 0:
			eps[i].State, eps[i].WatchedAt = store.MovieWatched, &now
		case 3:
			eps[i].State, eps[i].DeleteReason = store.MovieDeleted, store.DeleteWatched
		}
		if err := e.store.UpdateEpisode(ctx, eps[i]); err != nil {
			t.Fatal(err)
		}
	}
	// The admin (user 1) has seen episodes 1 and 4; bob joins and has not.
	for _, ep := range []store.Episode{eps[0], eps[3]} {
		if err := e.store.SaveProgress(ctx, store.Progress{ItemKind: store.ItemEpisode, ItemID: ep.ID, UserID: 1, Progress: 1, WatchedAt: &now}); err != nil {
			t.Fatal(err)
		}
	}
	bc := e.client(t)
	if resp, body := e.do(t, bc, "POST", "/api/v1/auth/login", map[string]string{"username": "bob", "password": "longenough"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	path := "/api/v1/series/" + strconv.FormatInt(se.ID, 10)
	if resp, _ := e.do(t, bc, "POST", path+"/watch", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	first := func(body map[string]any) map[string]any {
		list, _ := body["episodes"].([]any)
		if len(list) == 0 {
			t.Fatalf("no episodes: %v", body)
		}
		return list[0].(map[string]any)
	}
	_, body := e.do(t, bc, "GET", path, nil)
	ep := first(body)
	if ep["state"] != store.MovieAvailable {
		t.Fatalf("bob sees episode 1 as %v", ep["state"])
	}
	if cur, _ := body["current"].(map[string]any); cur["episode"] != float64(1) {
		t.Fatalf("bob's current = %v", body["current"])
	}
	if viewers, _ := ep["viewers"].([]any); len(viewers) != 2 {
		t.Fatalf("episode viewers = %v", ep["viewers"])
	}
	states := func(body map[string]any) []any {
		var out []any
		list, _ := body["episodes"].([]any)
		for _, e := range list {
			out = append(out, e.(map[string]any)["state"])
		}
		return out
	}
	if got := states(body); got[2] != store.EpisodePending || got[3] != store.EpisodePending {
		t.Fatalf("bob sees episodes 3 and 4 as %v", got)
	}
	ep3 := body["episodes"].([]any)[2].(map[string]any)
	if viewers, _ := ep3["viewers"].([]any); len(viewers) != 1 {
		t.Fatalf("episode 3 is listed for %v", ep3["viewers"])
	}
	_, body = e.do(t, c, "GET", path, nil)
	if ep := first(body); ep["state"] != store.MovieWatched {
		t.Fatalf("the admin sees episode 1 as %v", ep["state"])
	}
	if got := states(body); got[2] != store.MovieAvailable || got[3] != store.MovieDeleted {
		t.Fatalf("the admin sees episodes 3 and 4 as %v", got)
	}
}

// A viewer of someone else's movie sees their own progress, not the
// requester's.
func TestMovieViewPerViewer(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	e.completeSetup(t, c)
	ctx := context.Background()
	if resp, body := e.do(t, c, "POST", "/api/v1/users", map[string]any{"username": "bob", "password": "longenough", "role": "user"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: %d %v", resp.StatusCode, body)
	}
	now := time.Now()
	mv, err := e.store.CreateMovie(ctx, store.Movie{TMDBID: 42, Title: "Film", State: store.MovieAvailable}, 0) // the admin's
	if err != nil {
		t.Fatal(err)
	}
	mv.State, mv.WatchedAt, mv.Progress = store.MovieWatched, &now, 1
	if err := e.store.UpdateMovie(ctx, mv); err != nil {
		t.Fatal(err)
	}
	bc := e.client(t)
	if resp, body := e.do(t, bc, "POST", "/api/v1/auth/login", map[string]string{"username": "bob", "password": "longenough"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %v", resp.StatusCode, body)
	}
	path := "/api/v1/movies/" + strconv.FormatInt(mv.ID, 10)
	if resp, _ := e.do(t, bc, "POST", path+"/watch", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	viewers, err := e.store.ActiveViewers(ctx, store.ViewerMovie, mv.ID)
	if err != nil || len(viewers) != 2 {
		t.Fatalf("viewers = %+v, %v", viewers, err)
	}
	if err := e.store.SaveProgress(ctx, store.Progress{ItemKind: store.ItemMovie, ItemID: mv.ID, UserID: viewers[1].UserID, Progress: 0.3, StartedAt: &now}); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, bc, "GET", path, nil)
	if body["state"] != store.MovieAvailable || body["progress"] != 0.3 {
		t.Fatalf("bob sees %v at %v", body["state"], body["progress"])
	}
	_, body = e.do(t, c, "GET", path, nil)
	if body["state"] != store.MovieWatched {
		t.Fatalf("the admin sees %v", body["state"])
	}
}

// Joining a movie whose files were removed brings it back; only former
// viewers, its owner and administrators may.
func TestJoinRestoresRemovedMovie(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	e.completeSetup(t, c)
	ctx := context.Background()
	for _, name := range []string{"bob", "eve"} {
		if resp, body := e.do(t, c, "POST", "/api/v1/users", map[string]any{"username": name, "password": "longenough", "role": "user"}); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create user: %d %v", resp.StatusCode, body)
		}
	}
	mv, err := e.store.CreateMovie(ctx, store.Movie{TMDBID: 42, Title: "Film", State: store.MovieAvailable}, 0)
	if err != nil {
		t.Fatal(err)
	}
	login := func(name string) *http.Client {
		cl := e.client(t)
		if resp, body := e.do(t, cl, "POST", "/api/v1/auth/login", map[string]string{"username": name, "password": "longenough"}); resp.StatusCode != http.StatusOK {
			t.Fatalf("login: %d %v", resp.StatusCode, body)
		}
		return cl
	}
	bob, eve := login("bob"), login("eve")
	path := "/api/v1/movies/" + strconv.FormatInt(mv.ID, 10) + "/watch"
	if resp, _ := e.do(t, bob, "POST", path, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	mv, _ = e.store.Movie(ctx, mv.ID)
	mv.State, mv.DeleteReason = store.MovieExpired, store.DeleteUnwanted
	if err := e.store.UpdateMovie(ctx, mv); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do(t, eve, "POST", path, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger restored: %d", resp.StatusCode)
	}
	if resp, body := e.do(t, bob, "POST", path, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: %d %v", resp.StatusCode, body)
	}
	if cur, _ := e.store.Movie(ctx, mv.ID); cur.State != store.MovieSearching {
		t.Fatalf("state after joining = %s", cur.State)
	}
}
