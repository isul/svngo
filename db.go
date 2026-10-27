package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// dbtx is satisfied by both *sql.DB and *sql.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const schemaV1 = `
CREATE TABLE admins (
	id INTEGER PRIMARY KEY,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_login INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE users (
	id INTEGER PRIMARY KEY,
	username TEXT NOT NULL UNIQUE,
	full_name TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE groups (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE group_members (
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (group_id, user_id)
);
CREATE INDEX idx_group_members_user ON group_members(user_id);
CREATE TABLE repos (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT -1,
	size_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE rules (
	id INTEGER PRIMARY KEY,
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	path TEXT NOT NULL,
	subject_type TEXT NOT NULL CHECK (subject_type IN ('user','group','all','authenticated')),
	subject_id INTEGER NOT NULL DEFAULT 0,
	access TEXT NOT NULL CHECK (access IN ('r','rw','none')),
	created_at INTEGER NOT NULL,
	UNIQUE (repo_id, path, subject_type, subject_id)
);
CREATE INDEX idx_rules_subject ON rules(subject_type, subject_id);
CREATE TABLE sessions (
	token_hash TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK (kind IN ('admin','user')),
	principal_id INTEGER NOT NULL,
	username TEXT NOT NULL,
	csrf TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX idx_sessions_principal ON sessions(kind, principal_id);
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE audit (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE TABLE settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

var migrations = []string{schemaV1}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("DB 연결 실패: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var ver int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&ver); err != nil {
		return err
	}
	for i := ver; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("DB 마이그레이션 %d 실패: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() int64 { return time.Now().Unix() }

func audit(ctx context.Context, q dbtx, actor, action, detail string) error {
	_, err := q.ExecContext(ctx, "INSERT INTO audit(at, actor, action, detail) VALUES(?,?,?,?)",
		now(), actor, action, clip(detail, 1000))
	if err != nil {
		return err
	}
	// keep the audit table bounded without a background job
	_, err = q.ExecContext(ctx, `DELETE FROM audit WHERE id <= (SELECT MAX(id) FROM audit) - 20000`)
	return err
}

// Settings editable from the web UI.
var settingDefaults = map[string]string{
	"svn_base_url":   "",   // e.g. https://svn.example.com/svn ; empty = derive from request
	"default_layout": "1",  // create trunk/branches/tags on repository creation
	"page_size":      "30", // rows per page
	"login_attempts": "10", // failed logins per 15 minutes before blocking

	"svn_block_attempts": "20", // failed SVN (HTTP Basic) logins per IP before blocking; 0 = off
	"svn_block_minutes":  "15", // counting window and block duration
}

func getSetting(ctx context.Context, q dbtx, key string) string {
	var v string
	if err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v); err != nil {
		return settingDefaults[key]
	}
	return v
}

func getSettingInt(ctx context.Context, q dbtx, key string, min, max int) int {
	n, err := strconv.Atoi(getSetting(ctx, q, key))
	if err != nil {
		n, _ = strconv.Atoi(settingDefaults[key])
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

func setSetting(ctx context.Context, q dbtx, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
