package store

// migrations are applied in order; the DB's PRAGMA user_version records how
// many have run. Never edit an existing entry — append a new one.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE IF NOT EXISTS events (
    event_id   TEXT PRIMARY KEY,
    command_id TEXT NOT NULL,
    device_id  TEXT NOT NULL,
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    synced     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_events_command  ON events(command_id);
CREATE INDEX IF NOT EXISTS idx_events_unsynced ON events(synced, created_at);

CREATE TABLE IF NOT EXISTS commands (
    id            TEXT PRIMARY KEY,
    command       TEXT NOT NULL DEFAULT '',
    hostname      TEXT NOT NULL DEFAULT '',
    device_id     TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT '',
    cwd           TEXT NOT NULL DEFAULT '',
    git_branch    TEXT,
    shell         TEXT NOT NULL DEFAULT '',
    start_time    INTEGER NOT NULL DEFAULT 0,
    end_time      INTEGER,
    exit_code     INTEGER,
    duration_ms   INTEGER,
    status        TEXT NOT NULL DEFAULT 'running',
    pgid          INTEGER NOT NULL DEFAULT 0,
    is_background INTEGER NOT NULL DEFAULT 0
);
-- Every index pairs its filter column with start_time DESC. Results are always
-- ordered newest-first, so an index on the filter alone would still leave
-- SQLite sorting the whole match set before returning a page of it, turning
-- every filtered query into work proportional to the history rather than to the
-- page. TestQueryPerformanceAtScale holds the resulting budget.
CREATE INDEX IF NOT EXISTS idx_commands_start   ON commands(start_time DESC);
CREATE INDEX IF NOT EXISTS idx_commands_status  ON commands(status, start_time DESC);
CREATE INDEX IF NOT EXISTS idx_commands_session ON commands(session_id, start_time DESC);
CREATE INDEX IF NOT EXISTS idx_commands_host    ON commands(hostname, start_time DESC);
CREATE INDEX IF NOT EXISTS idx_commands_cwd     ON commands(cwd, start_time DESC);

-- Keyed by rowid rather than a TEXT command_id: matching rows are then found
-- through the integer primary key instead of the string unique index, which is
-- roughly 3x cheaper on a broad match set.
CREATE VIRTUAL TABLE IF NOT EXISTS commands_fts USING fts5(command);
`,
	// 2: sync bookkeeping — how far we have read each peer's batch stream.
	`
CREATE TABLE IF NOT EXISTS sync_cursors (
    peer_device_id TEXT PRIMARY KEY,
    last_batch_key TEXT NOT NULL DEFAULT '',
    last_synced_at INTEGER NOT NULL DEFAULT 0,
    hostname_hint  TEXT NOT NULL DEFAULT ''
);
`,
	// 3: history brought in from a shell's own history file, which carries no
	// exit code and often no clock time. Flagged so it is never mistaken for
	// something this tool actually watched run.
	`
ALTER TABLE commands ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_commands_imported ON commands(imported, start_time DESC);
`,
	// 4: per-command statistics for ranking.
	//
	// Ranking needs one row per distinct command, not per execution, and it
	// needs it fast enough to run between keystrokes. Deriving it on demand is
	// not an option: GROUP BY command over 500k executions measures 815ms, and
	// even a bare DISTINCT is 644ms, because the cost is the scan rather than
	// the result. So it is materialised here and refreshed from the executions
	// that have arrived since the watermark.
	//
	// It is a cache. Nothing here cannot be rebuilt from the commands table,
	// and RefreshCommandStats(0) does exactly that.
	`
CREATE TABLE IF NOT EXISTS command_stats (
    command        TEXT PRIMARY KEY,
    runs           INTEGER NOT NULL DEFAULT 0,
    last_time      INTEGER NOT NULL DEFAULT 0,
    -- The decayed use count, and when it was last brought up to date.
    weight         REAL    NOT NULL DEFAULT 0,
    weight_at      INTEGER NOT NULL DEFAULT 0,
    -- Exit codes bucketed by what they mean; see rank.ClassifyExit, which is
    -- the authority for these ranges.
    succeeded      INTEGER NOT NULL DEFAULT 0,
    failed         INTEGER NOT NULL DEFAULT 0,
    never_ran      INTEGER NOT NULL DEFAULT 0,
    interrupted    INTEGER NOT NULL DEFAULT 0,
    unfinished     INTEGER NOT NULL DEFAULT 0,
    imported_runs  INTEGER NOT NULL DEFAULT 0,
    last_failed_at INTEGER NOT NULL DEFAULT 0
);

-- Refreshing looks up executions by their command text, which without this is
-- the same full scan the table exists to avoid.
CREATE INDEX IF NOT EXISTS idx_commands_command ON commands(command);

CREATE TABLE IF NOT EXISTS stats_meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    watermark INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO stats_meta (id, watermark) VALUES (1, 0);
`,
}
