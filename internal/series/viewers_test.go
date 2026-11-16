package series

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shyper/autodownloadarr/internal/store"
)

// present matches an episode whose file is there (available or watched).
const present = "present"

// Two viewers at different speeds: each has a window of their own, an
// episode goes only when both are done with it, and an episode removed
// between them is fetched again when the slower viewer gets there.
func TestViewersWindowsAndGaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newWorld(t)
	m, st, _ := setup(ctx, t, w, nil)
	for id, name := range map[int64]string{1: "fast", 2: "slow"} {
		_, err := w.db.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(?,?,'x','user',0,0)`, id, name)
		must(t, err)
	}
	// fast uses the server owner's account, slow a Plex Home profile.
	must(t, st.SyncMediaProfiles(ctx, []store.MediaProfileInfo{{ExternalID: "o", Name: "Owner", Owner: true}, {ExternalID: "s", Name: "Slow"}}))
	profiles, err := st.MediaProfiles(ctx)
	must(t, err)
	must(t, st.AssignMediaProfile(ctx, profiles[0].ID, 1, false))
	must(t, st.AssignMediaProfile(ctx, profiles[1].ID, 2, false))
	must(t, st.SetMediaProfileTokens(ctx, profiles[1].ID, "slow-token", "slow-account"))
	slow := map[int]float64{}
	w.byToken["slow-token"] = slow
	setSlow := func(ep int, p float64) {
		w.mu.Lock()
		defer w.mu.Unlock()
		slow[ep] = p
	}

	se, err := m.Request(ctx, 7, 1, RequestOptions{WindowSize: 2, Trigger: store.TriggerProgress, TriggerValue: 10})
	must(t, err)
	if _, err := m.Request(ctx, 7, 2, RequestOptions{}); !errors.Is(err, ErrAlreadyRequested) {
		t.Fatalf("second request: %v", err)
	}
	viewers, err := st.ActiveViewers(ctx, store.ViewerSeries, se.ID)
	if err != nil || len(viewers) != 2 {
		t.Fatalf("viewers = %+v, %v", viewers, err)
	}
	states := func() map[int]string {
		eps, _ := st.Episodes(ctx, se.ID)
		out := map[int]string{}
		for _, e := range eps {
			out[e.Episode] = e.State
		}
		return out
	}
	waitStates := func(what string, want map[int]string) {
		t.Helper()
		eventually(t, what, func() bool {
			got := states()
			for ep, s := range want {
				if s == present && (got[ep] == store.MovieAvailable || got[ep] == store.MovieWatched) {
					continue
				}
				if got[ep] != s {
					return false
				}
			}
			return true
		})
	}
	waitStates("first window", map[int]string{1: present, 2: present})

	// fast watches 1 and 2 and starts 3: slow still needs 1 and 2, fast's
	// window brings 3 and 4.
	w.setProgress(1, 1)
	w.setProgress(2, 1)
	w.setProgress(3, 0.5)
	must(t, m.PollWatched(ctx))
	waitStates("fast window", map[int]string{1: present, 2: present, 3: present, 4: present})

	// slow watches 1 and starts 2: 1 is done for both.
	setSlow(1, 1)
	setSlow(2, 0.5)
	must(t, m.PollWatched(ctx))
	waitStates("1 done for both", map[int]string{1: store.MovieDeleted, 2: present, 3: present})

	// fast races ahead to 5: 4 lies beyond slow's window and goes (a gap);
	// 3 stays for slow.
	w.setProgress(3, 1)
	w.setProgress(4, 1)
	w.setProgress(5, 0.5)
	must(t, m.PollWatched(ctx))
	waitStates("fast at 5", map[int]string{5: present})
	must(t, m.PollWatched(ctx)) // fast's progress on 5 is seen once it is there
	waitStates("gap", map[int]string{3: present, 4: store.MovieDeleted, 5: present})
	if e := episodeNo(ctx, t, st, se.ID, 4); e.DeleteReason != store.DeleteWatched {
		t.Fatalf("episode 4 deleted as %q", e.DeleteReason)
	}

	// slow reaches 3: the gap episode 4 is fetched again, 2 goes.
	setSlow(2, 1)
	setSlow(3, 0.5)
	must(t, m.PollWatched(ctx))
	waitStates("gap filled", map[int]string{2: store.MovieDeleted, 3: present, 4: present})

	// fast had watched 4 before: it stays watched for them after the new file.
	must(t, m.PollWatched(ctx))
	ep4 := episodeNo(ctx, t, st, se.ID, 4)
	rows, err := st.ItemProgress(ctx, store.ItemEpisode, ep4.ID)
	must(t, err)
	if rows[ep4.ID][1].WatchedAt == nil {
		t.Fatal("fast lost the viewing of episode 4")
	}

	positions, err := m.ViewerPositions(ctx, se.ID)
	must(t, err)
	if len(positions) != 2 || positions[1].Current == nil || positions[1].Current.Episode != 3 {
		t.Fatalf("positions = %+v", positions)
	}

	// slow leaves: only fast's window counts, so 3 and 4 may go.
	must(t, st.LeaveViewer(ctx, store.ViewerSeries, se.ID, 2))
	must(t, m.Reconcile(ctx, se.ID))
	waitStates("slow left", map[int]string{3: store.MovieDeleted, 4: store.MovieDeleted, 5: present})
}

func episodeNo(ctx context.Context, t *testing.T, st *store.Store, seriesID int64, n int) store.Episode {
	t.Helper()
	eps, err := st.Episodes(ctx, seriesID)
	must(t, err)
	for _, e := range eps {
		if e.Episode == n {
			return e
		}
	}
	t.Fatalf("episode %d missing", n)
	return store.Episode{}
}

func TestEpisodeDueNeedsEveryViewer(t *testing.T) {
	se := store.Series{DeleteTrigger: store.TriggerProgress, TriggerValue: 10, WindowSize: 1}
	g := store.DefaultGeneral()
	now := time.Now()
	ep := func(p float64) store.Episode { return store.Episode{State: store.MovieAvailable, Progress: p} }
	done := viewerWindow{eps: []store.Episode{ep(1), ep(0.5)}, start: 1, window: map[int]bool{1: true}}
	behind := viewerWindow{eps: []store.Episode{ep(0), ep(0)}, start: 0, window: map[int]bool{0: true}}
	if due, _, _ := episodeDue(se, g, []viewerWindow{done}, 0, now); !due {
		t.Fatal("a single viewer who is done does not delete")
	}
	if due, _, _ := episodeDue(se, g, []viewerWindow{done, behind}, 0, now); due {
		t.Fatal("deleted although a viewer still needs it")
	}
	// Before where the other viewer started: it does not hold the episode.
	late := viewerWindow{eps: []store.Episode{ep(0), ep(0)}, first: 1, start: 1, window: map[int]bool{1: true}}
	if due, _, _ := episodeDue(se, g, []viewerWindow{done, late}, 0, now); !due {
		t.Fatal("held by a viewer who started later")
	}
	// Nobody done: nothing is deleted.
	if due, _, _ := episodeDue(se, g, []viewerWindow{late}, 0, now); due {
		t.Fatal("deleted without any viewer being done")
	}
}

func TestSkipForViewer(t *testing.T) {
	removed := func(reason string, p float64) store.Episode {
		return store.Episode{State: store.MovieDeleted, DeleteReason: reason, Progress: p}
	}
	cases := []struct {
		e    store.Episode
		skip bool
	}{
		{store.Episode{State: store.MovieAvailable}, false},
		{removed(store.DeleteWatched, 0), false}, // fetch again for this viewer
		{removed(store.DeleteWatched, 1), true},  // they have seen it
		{removed(store.DeleteMissing, 0), true},  // removed outside AutoDownloadarr
		{removed(store.DeleteUser, 0), true},
	}
	for _, c := range cases {
		if got := skipForViewer(c.e); got != c.skip {
			t.Errorf("skipForViewer(%+v) = %v", c.e, got)
		}
	}
}
