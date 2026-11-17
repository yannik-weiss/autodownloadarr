package store

import (
	"context"
	"testing"
)

func TestViewers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, id := range []int{1, 2, 3} {
		botUser(t, s, id)
	}
	mv, err := s.CreateMovie(ctx, Movie{TMDBID: 1, Title: "Film", State: MovieAvailable}, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The requester is the first viewer.
	vs, err := s.ActiveViewers(ctx, ViewerMovie, mv.ID)
	if err != nil || len(vs) != 1 || vs[0].UserID != 1 || vs[0].Source != SourceRequest {
		t.Fatalf("viewers = %+v, %v", vs, err)
	}
	if active, err := s.AddViewer(ctx, Viewer{Kind: ViewerMovie, TitleID: mv.ID, UserID: 2, Source: SourceWatchlist}, false); err != nil || !active {
		t.Fatalf("add = %v, %v", active, err)
	}
	if err := s.LeaveViewer(ctx, ViewerMovie, mv.ID, 2); err != nil {
		t.Fatal(err)
	}
	// Imports respect leaving; an explicit request rejoins.
	if active, _ := s.AddViewer(ctx, Viewer{Kind: ViewerMovie, TitleID: mv.ID, UserID: 2, Source: SourceTrakt}, false); active {
		t.Fatal("import re-added a viewer who left")
	}
	if ok, _ := s.IsViewer(ctx, ViewerMovie, mv.ID, 2); !ok {
		t.Fatal("left viewer forgotten")
	}
	if active, _ := s.AddViewer(ctx, Viewer{Kind: ViewerMovie, TitleID: mv.ID, UserID: 2}, true); !active {
		t.Fatal("request did not rejoin")
	}
	if ok, _ := s.IsViewer(ctx, ViewerMovie, mv.ID, 3); ok {
		t.Fatal("stranger is a viewer")
	}

	p := Progress{ItemKind: ItemMovie, ItemID: mv.ID, UserID: 2, Progress: 0.5, Base: WatchBase{Set: true, Views: 3, Account: "profile:1"}}
	if err := s.SaveProgress(ctx, p); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ItemProgress(ctx, ItemMovie, mv.ID)
	if err != nil || rows[mv.ID][2].Base != p.Base || rows[mv.ID][2].Progress != 0.5 {
		t.Fatalf("progress = %+v, %v", rows, err)
	}
	if err := s.ResetProgressBase(ctx, ItemMovie, mv.ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ = s.ItemProgress(ctx, ItemMovie, mv.ID); rows[mv.ID][2].Base.Set || rows[mv.ID][2].Progress != 0.5 {
		t.Fatalf("after reset = %+v", rows[mv.ID][2])
	}

	// Deleting a user or the movie removes their rows.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if rows, _ = s.ItemProgress(ctx, ItemMovie, mv.ID); len(rows[mv.ID]) != 0 {
		t.Fatal("progress of deleted user kept")
	}
	if err := s.DeleteMovie(ctx, mv.ID); err != nil {
		t.Fatal(err)
	}
	if vs, _ = s.Viewers(ctx, ViewerMovie, mv.ID); len(vs) != 0 {
		t.Fatal("viewers of deleted movie kept")
	}
}

func TestBotNoticesReachViewers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, id := range []int{1, 2} {
		botUser(t, s, id)
		code, err := s.CreateBotLinkCode(ctx, "telegram", string(rune('0'+id)), "c", "en")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ConfirmBotLink(ctx, int64(id), code); err != nil {
			t.Fatal(err)
		}
	}
	mv, err := s.CreateMovie(ctx, Movie{TMDBID: 1, Title: "Film", State: MovieSearching}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddViewer(ctx, Viewer{Kind: ViewerMovie, TitleID: mv.ID, UserID: 2}, true); err != nil {
		t.Fatal(err)
	}
	mv.State = MovieAvailable
	if err := s.UpdateMovie(ctx, mv); err != nil {
		t.Fatal(err)
	}
	notices, err := s.BotNotices(ctx)
	if err != nil || len(notices) != 2 {
		t.Fatalf("notices = %+v, %v", notices, err)
	}
	if err := s.LeaveViewer(ctx, ViewerMovie, mv.ID, 2); err != nil {
		t.Fatal(err)
	}
	if notices, _ = s.BotNotices(ctx); len(notices) != 1 {
		t.Fatalf("notices after leaving = %d", len(notices))
	}
}

func TestPreviousRelease(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mv, err := s.CreateMovie(ctx, Movie{TMDBID: 1, Title: "Film", State: MovieDeleted}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rels := []Release{{IndexerID: 0, GUID: "a", Title: "A", Fingerprint: "fa"}, {IndexerID: 0, GUID: "b", Title: "B", Fingerprint: "fb"}}
	if err := s.ReplaceReleases(ctx, mv.ID, rels); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.PreviousRelease(ctx, ForMovie(mv.ID), rels[1].Identity())
	if err != nil || !ok || r.Title != "B" {
		t.Fatalf("previous = %+v %v %v", r, ok, err)
	}
	if _, ok, _ := s.PreviousRelease(ctx, ForMovie(mv.ID), ""); ok {
		t.Fatal("found a release without a key")
	}
	if err := s.Blocklist(ctx, BlocklistEntry{MovieID: mv.ID, Fingerprint: r.Fingerprint, Title: r.Title, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PreviousRelease(ctx, ForMovie(mv.ID), rels[1].Identity()); ok {
		t.Fatal("blocklisted release reused")
	}
}

func TestUnwantedWhenLastViewerLeaves(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, id := range []int{1, 2} {
		botUser(t, s, id)
	}
	mv, err := s.CreateMovie(ctx, Movie{TMDBID: 1, Title: "Film", State: MovieAvailable}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddViewer(ctx, Viewer{Kind: ViewerMovie, TitleID: mv.ID, UserID: 2}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveViewer(ctx, ViewerMovie, mv.ID, 1); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Movie(ctx, mv.ID); cur.UnwantedAt != nil {
		t.Fatal("unwanted while a viewer is left")
	}
	if err := s.LeaveViewer(ctx, ViewerMovie, mv.ID, 2); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Movie(ctx, mv.ID); cur.UnwantedAt == nil {
		t.Fatal("not unwanted after everyone left")
	}
	// UpdateMovie leaves it alone; joining again clears it.
	cur, _ := s.Movie(ctx, mv.ID)
	if err := s.UpdateMovie(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddViewer(ctx, Viewer{Kind: ViewerMovie, TitleID: mv.ID, UserID: 2}, true); err != nil {
		t.Fatal(err)
	}
	if cur, _ = s.Movie(ctx, mv.ID); cur.UnwantedAt != nil {
		t.Fatal("still unwanted after a viewer joined")
	}

	se, err := s.CreateSeries(ctx, Series{TMDBID: 2, Title: "Show"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveViewer(ctx, ViewerSeries, se.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Series(ctx, se.ID); got.UnwantedAt == nil {
		t.Fatal("series not unwanted after its only viewer left")
	}
}
