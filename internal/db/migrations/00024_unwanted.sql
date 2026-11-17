-- unwanted_at is when the last viewer of a movie or series left it. A
-- title nobody wants to watch any more is removed soon after.

-- +goose Up
ALTER TABLE movies ADD COLUMN unwanted_at INTEGER;
ALTER TABLE series ADD COLUMN unwanted_at INTEGER;

-- +goose Down
ALTER TABLE series DROP COLUMN unwanted_at;
ALTER TABLE movies DROP COLUMN unwanted_at;
