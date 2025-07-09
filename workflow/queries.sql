-- name: GetRunVersion :one
SELECT version FROM runs WHERE id = $1;

-- name: GetRunVersionForUpdate :one
SELECT version FROM runs WHERE id = $1 FOR UPDATE;

-- name: UpsertRun :one
INSERT INTO runs (id, version)
VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET version = excluded.version
RETURNING version;

-- name: InsertEvent :exec
INSERT INTO events (run_id, version, data)
VALUES ($1, $2, $3);

-- name: GetEvents :many
SELECT data FROM events WHERE run_id = $1 ORDER BY version ASC;

-- name: GetLastEvent :one
SELECT data, version FROM events WHERE run_id = $1 ORDER BY version DESC LIMIT 1;

-- name: GetFirstEvent :one
SELECT data FROM events WHERE run_id = $1 ORDER BY version ASC LIMIT 1;

-- name: PutScript :exec
INSERT INTO scripts (hash, data)
VALUES ($1, $2)
ON CONFLICT (hash) DO UPDATE SET data = excluded.data;

-- name: GetScript :one
SELECT data FROM scripts WHERE hash = $1; 