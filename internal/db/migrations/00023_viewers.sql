-- Several users can watch the same movie or series. viewers lists who
-- watches a title (the requester and everyone who joined later);
-- viewer_progress keeps the watch state of each viewer per movie or
-- episode, read from that viewer's media account. user_id 0 in
-- viewer_progress is the default account of a title without viewers.
-- delete_reason tells automatic deletions (which may be fetched again for
-- a slower viewer) apart from removals by the user. hold_until is how long
-- viewers who are still watching a movie keep it.

-- +goose Up
CREATE TABLE viewers (
    id            INTEGER PRIMARY KEY,
    kind          TEXT    NOT NULL CHECK (kind IN ('movie', 'series')),
    title_id      INTEGER NOT NULL,
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source        TEXT    NOT NULL DEFAULT 'request',
    start_season  INTEGER NOT NULL DEFAULT 0,
    start_episode INTEGER NOT NULL DEFAULT 0,
    added_at      INTEGER NOT NULL,
    left_at       INTEGER,
    UNIQUE (kind, title_id, user_id)
);
CREATE INDEX viewers_user ON viewers (user_id);

CREATE TABLE viewer_progress (
    item_kind    TEXT    NOT NULL CHECK (item_kind IN ('movie', 'episode')),
    item_id      INTEGER NOT NULL,
    user_id      INTEGER NOT NULL,
    base_set     INTEGER NOT NULL DEFAULT 0,
    base_views   INTEGER NOT NULL DEFAULT 0,
    base_offset  INTEGER NOT NULL DEFAULT 0,
    base_account TEXT    NOT NULL DEFAULT '',
    progress     REAL    NOT NULL DEFAULT 0,
    started_at   INTEGER,
    watched_at   INTEGER,
    PRIMARY KEY (item_kind, item_id, user_id)
);

ALTER TABLE movies ADD COLUMN delete_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE movies ADD COLUMN hold_until INTEGER;
ALTER TABLE episodes ADD COLUMN delete_reason TEXT NOT NULL DEFAULT '';

INSERT INTO viewers (kind, title_id, user_id, source, added_at)
SELECT 'movie', id, requested_by, 'request', created_at FROM movies WHERE requested_by IS NOT NULL;
INSERT INTO viewers (kind, title_id, user_id, source, start_season, start_episode, added_at)
SELECT 'series', id, requested_by, 'request', start_season, start_episode, created_at FROM series WHERE requested_by IS NOT NULL;

INSERT INTO viewer_progress (item_kind, item_id, user_id, base_set, base_views, base_offset, base_account, progress, started_at, watched_at)
SELECT 'movie', id, COALESCE(requested_by, 0), base_set, base_views, base_offset, base_account, progress, started_at, watched_at
FROM movies WHERE base_set = 1 OR progress > 0 OR started_at IS NOT NULL OR watched_at IS NOT NULL;
INSERT INTO viewer_progress (item_kind, item_id, user_id, base_set, base_views, base_offset, base_account, progress, started_at, watched_at)
SELECT 'episode', e.id, COALESCE(s.requested_by, 0), e.base_set, e.base_views, e.base_offset, e.base_account, e.progress, e.started_at, e.watched_at
FROM episodes e JOIN series s ON s.id = e.series_id
WHERE e.base_set = 1 OR e.progress > 0 OR e.started_at IS NOT NULL OR e.watched_at IS NOT NULL;

UPDATE movies SET delete_reason = CASE state WHEN 'deleted' THEN 'watched' WHEN 'expired' THEN 'expired' ELSE '' END;
UPDATE episodes SET delete_reason = CASE state WHEN 'deleted' THEN 'watched' WHEN 'expired' THEN 'expired' ELSE '' END;

-- +goose StatementBegin
CREATE TRIGGER movies_viewers_cleanup AFTER DELETE ON movies BEGIN
    DELETE FROM viewers WHERE kind = 'movie' AND title_id = OLD.id;
    DELETE FROM viewer_progress WHERE item_kind = 'movie' AND item_id = OLD.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER series_viewers_cleanup AFTER DELETE ON series BEGIN
    DELETE FROM viewers WHERE kind = 'series' AND title_id = OLD.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER episodes_progress_cleanup AFTER DELETE ON episodes BEGIN
    DELETE FROM viewer_progress WHERE item_kind = 'episode' AND item_id = OLD.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER users_progress_cleanup AFTER DELETE ON users BEGIN
    DELETE FROM viewer_progress WHERE user_id = OLD.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER users_progress_cleanup;
DROP TRIGGER episodes_progress_cleanup;
DROP TRIGGER series_viewers_cleanup;
DROP TRIGGER movies_viewers_cleanup;
ALTER TABLE episodes DROP COLUMN delete_reason;
ALTER TABLE movies DROP COLUMN hold_until;
ALTER TABLE movies DROP COLUMN delete_reason;
DROP TABLE viewer_progress;
DROP TABLE viewers;
