package db

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMediaServerMigrationsUpgrade checks the table rebuild that allows
// Jellyfin/Emby on a database from before that change, including the path
// mappings' foreign key.
func TestMediaServerMigrationsUpgrade(t *testing.T) {
	ctx := context.Background()
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	dsn := "file:" + filepath.Join(t.TempDir(), "old.db") + "?" + q.Encode()
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)

	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqldb, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO media_servers
		(kind, name, url, token_enc, movie_library, tv_library, enabled, created_at, updated_at)
		VALUES ('plex', 'Plex', 'http://plex:32400', '', '1', '2', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO path_mappings (media_server_id, local_path, remote_path)
		VALUES (1, '/movies', '/data/movies')`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(ctx, sqldb); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `UPDATE media_servers SET kind = 'jellyfin' WHERE id = 1`); err != nil {
		t.Fatalf("jellyfin not accepted: %v", err)
	}
	if _, err := sqldb.ExecContext(ctx, `UPDATE media_servers SET kind = 'unknown' WHERE id = 1`); err == nil {
		t.Error("unknown kind accepted")
	}
	var account string
	if err := sqldb.QueryRowContext(ctx, `SELECT account_token_enc FROM media_servers WHERE id = 1`).Scan(&account); err != nil {
		t.Errorf("account token column missing: %v", err)
	}
	if _, err := sqldb.ExecContext(ctx, `DELETE FROM media_servers WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_mappings`).Scan(&n); err != nil || n != 0 {
		t.Errorf("path mappings after delete = %d, %v", n, err)
	}
}

// TestViewersMigration checks that existing requests become viewers with
// their watch state and that removed files get a reason.
func TestViewersMigration(t *testing.T) {
	ctx := context.Background()
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	sqldb, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db")+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqldb, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 22); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO users (id, username, password_hash, role, created_at, updated_at) VALUES (5, 'anna', 'x', 'user', 1, 1)`,
		`INSERT INTO movies (id, tmdb_id, title, state, requested_by, base_set, base_views, progress, watched_at, created_at, updated_at)
			VALUES (1, 10, 'Watched', 'deleted', 5, 1, 2, 1, 1000, 1, 1)`,
		`INSERT INTO movies (id, tmdb_id, title, state, requested_by, created_at, updated_at) VALUES (2, 11, 'Fresh', 'expired', 5, 1, 1)`,
		`INSERT INTO series (id, tmdb_id, title, requested_by, start_season, start_episode, created_at, updated_at) VALUES (1, 20, 'Show', 5, 1, 1, 1, 1)`,
		`INSERT INTO episodes (id, series_id, season, episode, state, progress, started_at, created_at, updated_at) VALUES (1, 1, 1, 1, 'available', 0.4, 500, 1, 1)`,
	} {
		if _, err := sqldb.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := migrate(ctx, sqldb); err != nil {
		t.Fatal(err)
	}
	var viewers int
	if err := sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewers WHERE user_id = 5`).Scan(&viewers); err != nil || viewers != 3 {
		t.Fatalf("viewers = %d, %v", viewers, err)
	}
	var views int
	var watched sql.NullInt64
	if err := sqldb.QueryRowContext(ctx, `SELECT base_views, watched_at FROM viewer_progress WHERE item_kind = 'movie' AND item_id = 1 AND user_id = 5`).
		Scan(&views, &watched); err != nil || views != 2 || watched.Int64 != 1000 {
		t.Fatalf("movie progress = %d %v, %v", views, watched, err)
	}
	var progress float64
	if err := sqldb.QueryRowContext(ctx, `SELECT progress FROM viewer_progress WHERE item_kind = 'episode' AND item_id = 1 AND user_id = 5`).
		Scan(&progress); err != nil || progress != 0.4 {
		t.Fatalf("episode progress = %v, %v", progress, err)
	}
	for id, want := range map[int]string{1: "watched", 2: "expired"} {
		var reason string
		if err := sqldb.QueryRowContext(ctx, `SELECT delete_reason FROM movies WHERE id = ?`, id).Scan(&reason); err != nil || reason != want {
			t.Errorf("movie %d reason %q, %v", id, reason, err)
		}
	}
	if _, err := sqldb.ExecContext(ctx, `DELETE FROM movies WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewer_progress WHERE item_kind = 'movie' AND item_id = 1`).Scan(&viewers); err != nil || viewers != 0 {
		t.Fatalf("progress of deleted movie = %d, %v", viewers, err)
	}
	if _, err := provider.DownTo(ctx, 22); err != nil {
		t.Fatalf("down: %v", err)
	}
}
