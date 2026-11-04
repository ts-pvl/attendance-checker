// Package store opens the SQLite database and owns the schema.
//
// All timestamps are stored as INTEGER Unix milliseconds (UTC), taken from
// the server clock. Times are converted to the school's time zone only for
// display and for splitting at midnight.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

// Open opens (and if needed creates and migrates) the database at path.
func Open(path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// BEGIN IMMEDIATE takes the write lock up front, so concurrent writers
	// wait on busy_timeout instead of failing with SQLITE_BUSY on upgrade.
	q.Set("_txlock", "immediate")
	sqldb, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(8)
	sqldb.SetConnMaxIdleTime(5 * time.Minute)
	db := &DB{sqldb}
	if err := db.migrate(context.Background()); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return db, nil
}

// migrations are applied in order; the index+1 is stored in PRAGMA user_version.
// Never edit an applied migration; append a new one.
var migrations = []string{
	`
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE hosts (
  id     INTEGER PRIMARY KEY,
  name   TEXT NOT NULL UNIQUE,
  ip     TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE students (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  grp         TEXT NOT NULL DEFAULT '',
  external_id TEXT UNIQUE,
  archived    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);

CREATE TABLE enroll_codes (
  code       TEXT PRIMARY KEY,
  student_id INTEGER NOT NULL REFERENCES students(id),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  used_by_tg INTEGER,
  revoked_at INTEGER
);
CREATE INDEX enroll_codes_student ON enroll_codes(student_id);

-- One Telegram account per student and one student per account.
CREATE TABLE telegram_links (
  student_id  INTEGER PRIMARY KEY REFERENCES students(id),
  tg_user_id  INTEGER NOT NULL UNIQUE,
  tg_username TEXT NOT NULL DEFAULT '',
  tg_name     TEXT NOT NULL DEFAULT '',
  linked_at   INTEGER NOT NULL
);

-- Append-only raw input. Sessions and presence are derived from it and can
-- be rebuilt with "attendance replay".
CREATE TABLE events (
  id         INTEGER PRIMARY KEY,
  at         INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  host       TEXT NOT NULL DEFAULT '',
  sid        TEXT NOT NULL DEFAULT '',
  idle_ms    INTEGER,
  tg_user_id INTEGER,
  student_id INTEGER,
  detail     TEXT NOT NULL DEFAULT '',
  result     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX events_at ON events(at);
CREATE INDEX events_kind_at ON events(kind, at);

-- A PC login, identified by (host, sid). id is the id of the opening event.
CREATE TABLE sessions (
  id               INTEGER PRIMARY KEY,
  host             TEXT NOT NULL,
  sid              TEXT NOT NULL,
  started_at       INTEGER NOT NULL,
  last_seen_at     INTEGER NOT NULL,
  last_activity_at INTEGER NOT NULL,
  last_idle_ms     INTEGER,
  implicit         INTEGER NOT NULL DEFAULT 0,
  ended_at         INTEGER,
  end_reason       TEXT
);
CREATE UNIQUE INDEX sessions_open_host ON sessions(host) WHERE ended_at IS NULL;
CREATE INDEX sessions_host_started ON sessions(host, started_at);

-- A visit: a confirmed scan tied to a PC session. id is the scan event id,
-- so it is stable across replays and corrections can refer to it.
CREATE TABLE presence (
  id              INTEGER PRIMARY KEY,
  student_id      INTEGER NOT NULL REFERENCES students(id),
  host            TEXT NOT NULL,
  session_id      INTEGER NOT NULL REFERENCES sessions(id),
  confirmed_at    INTEGER NOT NULL,
  ended_at        INTEGER,
  end_reason      TEXT,
  scan_latency_ms INTEGER NOT NULL DEFAULT 0,
  stale_token     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX presence_open_student ON presence(student_id) WHERE ended_at IS NULL;
CREATE INDEX presence_session ON presence(session_id);
CREATE INDEX presence_confirmed ON presence(confirmed_at);
CREATE INDEX presence_student_confirmed ON presence(student_id, confirmed_at);

-- Manual corrections never touch presence; they are layered on top.
-- target is 'p<presence id>' or 'm<correction id of an add>'.
CREATE TABLE corrections (
  id         INTEGER PRIMARY KEY,
  action     TEXT NOT NULL CHECK (action IN ('add','edit','delete')),
  target     TEXT NOT NULL DEFAULT '',
  student_id INTEGER NOT NULL REFERENCES students(id),
  host       TEXT NOT NULL DEFAULT '',
  start_at   INTEGER,
  end_at     INTEGER,
  reason     TEXT NOT NULL,
  actor      TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX corrections_student ON corrections(student_id);
CREATE INDEX corrections_target ON corrections(target);

CREATE TABLE used_nonces (
  nonce      TEXT PRIMARY KEY,
  expires_at INTEGER NOT NULL
);

CREATE TABLE staff (
  id         INTEGER PRIMARY KEY,
  username   TEXT NOT NULL UNIQUE,
  pw_hash    TEXT NOT NULL,
  role       TEXT NOT NULL CHECK (role IN ('admin','teacher')),
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE staff_sessions (
  token_hash   TEXT PRIMARY KEY,
  staff_id     INTEGER NOT NULL REFERENCES staff(id),
  csrf         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);

CREATE TABLE audit_log (
  id      INTEGER PRIMARY KEY,
  at      INTEGER NOT NULL,
  actor   TEXT NOT NULL,
  action  TEXT NOT NULL,
  target  TEXT NOT NULL DEFAULT '',
  details TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log(at);
`,
}

func (db *DB) migrate(ctx context.Context) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Querier is satisfied by *sql.DB, *sql.Tx and *DB.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// InTx runs fn in a write transaction.
func (db *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func Ms(t time.Time) int64 { return t.UnixMilli() }

func FromMs(ms int64) time.Time { return time.UnixMilli(ms) }

func GetSetting(ctx context.Context, q Querier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func SetSetting(ctx context.Context, q Querier, key, value string) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}

// Audit appends an entry to the audit log.
func Audit(ctx context.Context, q Querier, at time.Time, actor, action, target, details string) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO audit_log(at, actor, action, target, details) VALUES(?, ?, ?, ?, ?)",
		Ms(at), actor, action, target, details)
	return err
}

// Backup writes a consistent copy of the database to dst using VACUUM INTO.
func (db *DB) Backup(ctx context.Context, dst string) error {
	_, err := db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}
