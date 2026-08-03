// Package store owns every read and write against the local SQLite database.
//
// The central design rule: the events table is the source of truth and the
// commands table is a materialized view of it. Whenever an event lands we
// recompute that command's row from *all* of its events rather than applying a
// delta. That makes replaying an event idempotent and makes any arrival order
// converge to the same row — which matters because sync will deliver `end`
// before `start` sooner or later.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shellcrumbs/shcr/internal/event"

	_ "modernc.org/sqlite"
)

const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusOrphaned  = "orphaned"
)

type Store struct {
	db *sql.DB
}

// Command is the materialized view of one command's event history.
type Command struct {
	ID           string  `json:"id"`
	Command      string  `json:"command"`
	Hostname     string  `json:"hostname"`
	DeviceID     string  `json:"device_id"`
	SessionID    string  `json:"session_id"`
	Cwd          string  `json:"cwd"`
	GitBranch    *string `json:"git_branch,omitempty"`
	Shell        string  `json:"shell"`
	StartTime    int64   `json:"start_time"`
	EndTime      *int64  `json:"end_time,omitempty"`
	ExitCode     *int    `json:"exit_code,omitempty"`
	DurationMS   *int64  `json:"duration_ms,omitempty"`
	Status       string  `json:"status"`
	PGID         int     `json:"pgid"`
	IsBackground bool    `json:"is_background"`
	// Imported means this came from a shell history file, so its exit code is
	// unknown and its time may be approximate.
	Imported bool `json:"imported"`
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc's driver is not safe to hammer with concurrent writers; a small
	// pool plus WAL is plenty for a single-daemon workload.
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// SQLite creates its files 0644, so the history was readable by anyone with
	// a login on the machine the moment it left the 0700 directory — a backup, a
	// copy, a directory whose mode drifted. The write-ahead log holds the most
	// recent commands and needs the same treatment.
	restrict(path)
	return s, nil
}

func restrict(dbPath string) {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() != 0o600 {
			_ = os.Chmod(p, 0o600)
		}
	}
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return err
		}
	}
	return nil
}

// AppendEvent records a locally-produced event and rebuilds the affected
// command row. It reports whether the event was new; replaying a known event_id
// is a silent no-op, which is what makes sync merging safe.
func (s *Store) AppendEvent(ev event.Event) (bool, error) {
	return s.appendEvent(ev, 0)
}

// InsertRemoteEvent records an event that arrived from a peer. It is marked
// already-synced: this device did not produce it and must never upload it,
// because every device writes only its own events to its own prefix. Losing
// that invariant would have each device echoing every other device's history
// back into the bucket.
func (s *Store) InsertRemoteEvent(ev event.Event) (bool, error) {
	return s.appendEvent(ev, 1)
}

func (s *Store) appendEvent(ev event.Event, synced int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO events
		   (event_id, command_id, device_id, type, payload, created_at, synced)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.EventID, ev.CommandID, ev.DeviceID, string(ev.Type), string(ev.Payload), ev.CreatedAt, synced)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, tx.Commit() // already had it; row is already correct
	}
	if err := recompute(tx, ev.CommandID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// recompute rebuilds one command row from the full history of its events.
func recompute(tx *sql.Tx, commandID string) error {
	rows, err := tx.Query(
		`SELECT device_id, type, payload FROM events
		  WHERE command_id = ?
		  ORDER BY created_at ASC, event_id ASC`, commandID)
	if err != nil {
		return err
	}
	defer rows.Close()

	c := Command{ID: commandID, Status: StatusRunning}
	var (
		end      *event.EndPayload
		orphaned bool
		redacted bool
		seen     bool
		imported bool
	)
	for rows.Next() {
		var deviceID, typ, payload string
		if err := rows.Scan(&deviceID, &typ, &payload); err != nil {
			return err
		}
		seen = true
		switch event.Type(typ) {
		case event.TypeStart:
			var p event.StartPayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return fmt.Errorf("start payload for %s: %w", commandID, err)
			}
			c.Command = p.Command
			c.Hostname = p.Hostname
			c.DeviceID = deviceID
			c.SessionID = p.SessionID
			c.Cwd = p.Cwd
			if p.GitBranch != "" {
				b := p.GitBranch
				c.GitBranch = &b
			}
			c.Shell = p.Shell
			c.StartTime = p.StartTime
			c.PGID = p.PGID
			c.IsBackground = p.IsBackground
		case event.TypeEnd:
			var p event.EndPayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return fmt.Errorf("end payload for %s: %w", commandID, err)
			}
			end = &p
		case event.TypeImport:
			var p event.ImportPayload
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return fmt.Errorf("import payload for %s: %w", commandID, err)
			}
			c.Command = p.Command
			c.Hostname = p.Hostname
			c.DeviceID = deviceID
			c.Shell = p.Shell
			c.StartTime = p.StartTime
			if p.DurationMS > 0 {
				d := p.DurationMS
				c.DurationMS = &d
			}
			imported = true
		case event.TypeOrphan:
			orphaned = true
		case event.TypeRedact:
			redacted = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !seen {
		return nil
	}
	if c.DeviceID == "" {
		// `end` arrived before `start`: keep the row anchored to the device we
		// heard from so it is not orphan-swept by someone else.
		_ = tx.QueryRow(`SELECT device_id FROM events WHERE command_id = ? LIMIT 1`, commandID).Scan(&c.DeviceID)
	}

	c.Imported = imported
	// An end event always wins over an orphan guess — if the command reported a
	// result, it finished, regardless of what the sweep concluded meanwhile.
	switch {
	case end != nil:
		c.EndTime = &end.EndTime
		code := end.ExitCode
		c.ExitCode = &code
		if c.StartTime > 0 {
			d := end.EndTime - c.StartTime
			c.DurationMS = &d
		}
		if code == 0 {
			c.Status = StatusCompleted
		} else {
			c.Status = StatusFailed
		}
	case orphaned:
		c.Status = StatusOrphaned
	case imported:
		// It completed at some point — the shell wrote it to its history — but
		// nothing recorded how. The exit code stays unknown rather than being
		// assumed to be zero.
		c.Status = StatusCompleted
	default:
		c.Status = StatusRunning
	}
	if redacted {
		c.Command = event.RedactedMarker
	}

	if _, err := tx.Exec(
		`INSERT INTO commands
		   (id, command, hostname, device_id, session_id, cwd, git_branch, shell,
		    start_time, end_time, exit_code, duration_ms, status, pgid, is_background, imported)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   command=excluded.command, hostname=excluded.hostname,
		   device_id=excluded.device_id, session_id=excluded.session_id,
		   cwd=excluded.cwd, git_branch=excluded.git_branch, shell=excluded.shell,
		   start_time=excluded.start_time, end_time=excluded.end_time,
		   exit_code=excluded.exit_code, duration_ms=excluded.duration_ms,
		   status=excluded.status, pgid=excluded.pgid,
		   is_background=excluded.is_background, imported=excluded.imported`,
		c.ID, c.Command, c.Hostname, c.DeviceID, c.SessionID, c.Cwd, c.GitBranch,
		c.Shell, c.StartTime, c.EndTime, c.ExitCode, c.DurationMS, c.Status,
		c.PGID, boolToInt(c.IsBackground), boolToInt(c.Imported)); err != nil {
		return err
	}

	// The FTS row is keyed by the command's rowid, so searching yields integer
	// primary keys the outer query can look up directly.
	var rowid int64
	if err := tx.QueryRow(`SELECT rowid FROM commands WHERE id = ?`, commandID).Scan(&rowid); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM commands_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO commands_fts(rowid, command) VALUES (?, ?)`, rowid, c.Command)
	return err
}

// Filter selects a slice of history. Zero values mean "no constraint".
type Filter struct {
	Text      string // FTS match over the command text
	Hostname  string
	Status    string
	SessionID string
	Cwd       string
	Since     int64 // unix millis, inclusive
	Until     int64 // unix millis, exclusive
	Limit     int
	Offset    int
}

// buildWhere turns a Filter into a WHERE clause. Shared with the export path so
// `shcr export --status failed` selects exactly what `shcr list --status failed`
// shows, rather than two implementations that drift.
func buildWhere(f Filter) (string, []any) {
	var (
		where []string
		args  []any
	)
	if q := ftsQuery(f.Text); q != "" {
		where = append(where, `rowid IN (SELECT rowid FROM commands_fts WHERE commands_fts MATCH ?)`)
		args = append(args, q)
	}
	if f.Hostname != "" {
		where = append(where, `hostname = ?`)
		args = append(args, f.Hostname)
	}
	if f.Status != "" {
		where = append(where, `status = ?`)
		args = append(args, f.Status)
	}
	if f.SessionID != "" {
		where = append(where, `session_id = ?`)
		args = append(args, f.SessionID)
	}
	if f.Cwd != "" {
		where = append(where, `cwd = ?`)
		args = append(args, f.Cwd)
	}
	if f.Since > 0 {
		where = append(where, `start_time >= ?`)
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, `start_time < ?`)
		args = append(args, f.Until)
	}

	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

const commandColumns = `id, command, hostname, device_id, session_id, cwd, git_branch,
	shell, start_time, end_time, exit_code, duration_ms, status,
	pgid, is_background, imported`

func (s *Store) QueryCommands(f Filter) ([]Command, error) {
	clause, args := buildWhere(f)
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q := "SELECT " + commandColumns + " FROM commands" + clause +
		fmt.Sprintf(" ORDER BY start_time DESC, id DESC LIMIT %d OFFSET %d", limit, f.Offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommands(rows)
}

// EachCommand streams every command matching the filter, oldest first, without
// holding the result set in memory. Export has no business loading half a
// million rows at once just to write them out again.
func (s *Store) EachCommand(f Filter, fn func(Command) error) error {
	clause, args := buildWhere(f)
	rows, err := s.db.Query("SELECT "+commandColumns+" FROM commands"+clause+
		" ORDER BY start_time ASC, id ASC", args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	return rows.Err()
}

// EachEvent streams the raw log in the order it was written. This is the
// lossless form: command rows are derived from these, so an export of events
// can reconstruct everything, while an export of commands cannot.
func (s *Store) EachEvent(fn func(event.Event) error) error {
	rows, err := s.db.Query(
		`SELECT event_id, command_id, device_id, type, payload, created_at
		   FROM events ORDER BY created_at ASC, event_id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var ev event.Event
		var typ, payload string
		if err := rows.Scan(&ev.EventID, &ev.CommandID, &ev.DeviceID, &typ, &payload, &ev.CreatedAt); err != nil {
			return err
		}
		ev.Type = event.Type(typ)
		ev.Payload = json.RawMessage(payload)
		if err := fn(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LiveCommands returns commands this device still believes are running, which
// is the input to the daemon's orphan sweep.
func (s *Store) LiveCommands(deviceID string) ([]Command, error) {
	rows, err := s.db.Query(
		"SELECT "+commandColumns+
			" FROM commands WHERE status = ? AND device_id = ?", StatusRunning, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommands(rows)
}

func (s *Store) CommandByID(id string) (*Command, error) {
	rows, err := s.db.Query(
		"SELECT "+commandColumns+" FROM commands WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cs, err := scanCommands(rows)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}
	return &cs[0], nil
}

// SessionContext returns the commands run immediately before the given one in
// the same shell session, newest first. It answers "what was I doing when I ran
// this", which is usually more useful than the command on its own.
func (s *Store) SessionContext(sessionID string, before int64, limit int) ([]Command, error) {
	if sessionID == "" {
		return nil, nil
	}
	return s.QueryCommands(Filter{SessionID: sessionID, Until: before, Limit: limit})
}

func (s *Store) CountCommands() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT count(*) FROM commands`).Scan(&n)
	return n, err
}

// UnsyncedEvents returns this device's own not-yet-uploaded events, oldest
// first. The device filter is a second line of defence behind
// InsertRemoteEvent: a peer's events must never end up in our prefix.
func (s *Store) UnsyncedEvents(deviceID string, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT event_id, command_id, device_id, type, payload, created_at
		   FROM events WHERE synced = 0 AND device_id = ?
		  ORDER BY created_at ASC, event_id ASC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []event.Event
	for rows.Next() {
		var ev event.Event
		var typ, payload string
		if err := rows.Scan(&ev.EventID, &ev.CommandID, &ev.DeviceID, &typ, &payload, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Type = event.Type(typ)
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) MarkSynced(eventIDs []string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE events SET synced = 1 WHERE event_id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range eventIDs {
		if _, err := stmt.Exec(id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// scanCommand reads one row in commandColumns order. Every query that selects
// those columns goes through it, so adding a column is one edit here rather
// than a hunt for the copies that were missed.
func scanCommand(rows *sql.Rows) (Command, error) {
	var c Command
	var bg, imported int
	err := rows.Scan(&c.ID, &c.Command, &c.Hostname, &c.DeviceID, &c.SessionID,
		&c.Cwd, &c.GitBranch, &c.Shell, &c.StartTime, &c.EndTime, &c.ExitCode,
		&c.DurationMS, &c.Status, &c.PGID, &bg, &imported)
	c.IsBackground = bg != 0
	c.Imported = imported != 0
	return c, err
}

func scanCommands(rows *sql.Rows) ([]Command, error) {
	var out []Command
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ftsQuery turns a user's free-text search into an FTS5 expression. Every token
// is emitted as a quoted string so shell punctuation can never be read as FTS
// operator syntax, and the final token gets a prefix wildcard so search feels
// live as you type.
func ftsQuery(text string) string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		if isFTSWordChar(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	var parts []string
	for i, t := range tokens {
		q := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		if i == len(tokens)-1 {
			q += "*"
		}
		parts = append(parts, q)
	}
	return strings.Join(parts, " ")
}

func isFTSWordChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r > 127: // let the tokenizer deal with unicode
		return true
	}
	return false
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- sync cursors

// Cursor records how far this device has read one peer's batch stream.
type Cursor struct {
	PeerDeviceID string `json:"peer_device_id"`
	LastBatchKey string `json:"last_batch_key"`
	LastSyncedAt int64  `json:"last_synced_at"`
	HostnameHint string `json:"hostname_hint,omitempty"`
}

func (s *Store) Cursors() ([]Cursor, error) {
	rows, err := s.db.Query(
		`SELECT peer_device_id, last_batch_key, last_synced_at, hostname_hint
		   FROM sync_cursors ORDER BY peer_device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cursor
	for rows.Next() {
		var c Cursor
		if err := rows.Scan(&c.PeerDeviceID, &c.LastBatchKey, &c.LastSyncedAt, &c.HostnameHint); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Cursor(peerDeviceID string) (Cursor, error) {
	c := Cursor{PeerDeviceID: peerDeviceID}
	err := s.db.QueryRow(
		`SELECT last_batch_key, last_synced_at, hostname_hint
		   FROM sync_cursors WHERE peer_device_id = ?`, peerDeviceID).
		Scan(&c.LastBatchKey, &c.LastSyncedAt, &c.HostnameHint)
	if err == sql.ErrNoRows {
		return c, nil // an unknown peer simply starts from the beginning
	}
	return c, err
}

func (s *Store) SaveCursor(c Cursor) error {
	_, err := s.db.Exec(
		`INSERT INTO sync_cursors (peer_device_id, last_batch_key, last_synced_at, hostname_hint)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(peer_device_id) DO UPDATE SET
		   last_batch_key = excluded.last_batch_key,
		   last_synced_at = excluded.last_synced_at,
		   hostname_hint  = excluded.hostname_hint`,
		c.PeerDeviceID, c.LastBatchKey, c.LastSyncedAt, c.HostnameHint)
	return err
}

// CountUnsynced reports how many events are waiting to be uploaded, which is
// what the push loop uses to decide whether a batch is worth sending.
func (s *Store) CountUnsynced(deviceID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM events WHERE synced = 0 AND device_id = ?`, deviceID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- dashboard

// HourBucket is one column of the activity histogram.
type HourBucket struct {
	// Hour is the start of the bucket, unix millis.
	Hour  int64 `json:"hour"`
	Count int   `json:"count"`
}

// Stats is the dashboard summary: what happened today and what is happening now.
type Stats struct {
	Total     int64        `json:"total"`
	Today     int64        `json:"today"`
	Running   int64        `json:"running"`
	Completed int64        `json:"completed"`
	Failed    int64        `json:"failed"`
	Orphaned  int64        `json:"orphaned"`
	Hourly    []HourBucket `json:"hourly"`
}

// Stats summarises the database as of now. The histogram always covers a full
// 24 hours, including the quiet ones — a rhythm with gaps in it is the point,
// so empty buckets are returned rather than omitted.
func (s *Store) Stats(now time.Time) (Stats, error) {
	var out Stats

	if err := s.db.QueryRow(`SELECT count(*) FROM commands`).Scan(&out.Total); err != nil {
		return out, err
	}

	rows, err := s.db.Query(`SELECT status, count(*) FROM commands GROUP BY status`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return out, err
		}
		switch status {
		case StatusRunning:
			out.Running = n
		case StatusCompleted:
			out.Completed = n
		case StatusFailed:
			out.Failed = n
		case StatusOrphaned:
			out.Orphaned = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if err := s.db.QueryRow(
		`SELECT count(*) FROM commands WHERE start_time >= ?`, midnight.UnixMilli()).Scan(&out.Today); err != nil {
		return out, err
	}

	// 24 buckets ending with the hour in progress.
	start := now.Truncate(time.Hour).Add(-23 * time.Hour)
	counts := map[int64]int{}
	hist, err := s.db.Query(
		`SELECT start_time FROM commands WHERE start_time >= ?`, start.UnixMilli())
	if err != nil {
		return out, err
	}
	defer hist.Close()
	for hist.Next() {
		var ts int64
		if err := hist.Scan(&ts); err != nil {
			return out, err
		}
		counts[time.UnixMilli(ts).Truncate(time.Hour).UnixMilli()]++
	}
	if err := hist.Err(); err != nil {
		return out, err
	}
	out.Hourly = make([]HourBucket, 0, 24)
	for i := range 24 {
		h := start.Add(time.Duration(i) * time.Hour).UnixMilli()
		out.Hourly = append(out.Hourly, HourBucket{Hour: h, Count: counts[h]})
	}
	return out, nil
}

// EventsSince returns the command ids touched by events after the given rowid,
// along with the new high-water mark. This is how a separate process — the web
// server — notices that the daemon has recorded something, without either of
// them having to know about the other.
func (s *Store) EventsSince(rowID int64) (commandIDs []string, newRowID int64, err error) {
	rows, err := s.db.Query(
		`SELECT rowid, command_id FROM events WHERE rowid > ? ORDER BY rowid LIMIT 500`, rowID)
	if err != nil {
		return nil, rowID, err
	}
	defer rows.Close()

	newRowID = rowID
	seen := map[string]bool{}
	for rows.Next() {
		var rid int64
		var cmdID string
		if err := rows.Scan(&rid, &cmdID); err != nil {
			return nil, rowID, err
		}
		newRowID = rid
		if !seen[cmdID] {
			seen[cmdID] = true
			commandIDs = append(commandIDs, cmdID)
		}
	}
	return commandIDs, newRowID, rows.Err()
}

// MaxEventRowID is the current high-water mark, so a new subscriber can start
// from "now" rather than replaying history.
func (s *Store) MaxEventRowID() (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT max(rowid) FROM events`).Scan(&id)
	return id.Int64, err
}

// CommandsPerDevice counts what each machine has contributed, which is how the
// dashboard shows a device list that means something.
func (s *Store) CommandsPerDevice() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT device_id, count(*) FROM commands GROUP BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// SessionAfter returns the commands that ran next in the same shell session,
// oldest first. Paired with SessionContext it gives the detail view a timeline
// with the selected command in the middle rather than at the end.
func (s *Store) SessionAfter(sessionID string, after int64, limit int) ([]Command, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		"SELECT "+commandColumns+
			" FROM commands WHERE session_id = ? AND start_time > ?"+
			" ORDER BY start_time ASC LIMIT ?", sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommands(rows)
}

// RunInfo says where a command sits among today's repeats of the same text.
type RunInfo struct {
	Ordinal int `json:"ordinal"`
	Total   int `json:"total"`
}

// RunsToday counts how often each command text has been run since the given
// time and which run each row is. Answering "3rd run today" per row would
// otherwise mean a query per row; a single window function does the lot.
func (s *Store) RunsToday(since int64) (map[string]RunInfo, error) {
	rows, err := s.db.Query(
		`SELECT id,
		        row_number() OVER (PARTITION BY command ORDER BY start_time) AS ordinal,
		        count(*)     OVER (PARTITION BY command)                     AS total
		   FROM commands
		  WHERE start_time >= ? AND command <> ''`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]RunInfo{}
	for rows.Next() {
		var id string
		var info RunInfo
		if err := rows.Scan(&id, &info.Ordinal, &info.Total); err != nil {
			return nil, err
		}
		// Only repeats are interesting; a single run says nothing worth showing.
		if info.Total > 1 {
			out[id] = info
		}
	}
	return out, rows.Err()
}

// Hostnames lists every machine that appears in the history, for the
// dashboard's host filter.
func (s *Store) Hostnames() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT hostname FROM commands WHERE hostname <> '' ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
