// Package store holds every piece of persistent state: configuration,
// known senders and the invoices flowing through the pipeline.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite handle used by all modules.
type DB struct{ *sql.DB }

// Open opens (and creates, if needed) the SQLite database at path and applies
// the schema migrations.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single writer keeps "database is locked" away without extra machinery.
	sqlDB.SetMaxOpenConns(1)
	db := &DB{sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS senders (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	email    TEXT NOT NULL UNIQUE,
	name     TEXT NOT NULL DEFAULT '',
	skr04    TEXT NOT NULL DEFAULT '',
	active   INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS invoices (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	file_hash      TEXT NOT NULL UNIQUE,
	file_name      TEXT NOT NULL,
	file_path      TEXT NOT NULL,
	mail_from      TEXT NOT NULL DEFAULT '',
	mail_subject   TEXT NOT NULL DEFAULT '',
	received_at    TEXT NOT NULL DEFAULT '',

	sender_name    TEXT NOT NULL DEFAULT '',
	sender_address TEXT NOT NULL DEFAULT '',
	sender_vat_id  TEXT NOT NULL DEFAULT '',
	number         TEXT NOT NULL DEFAULT '',
	date           TEXT NOT NULL DEFAULT '',
	total_cents    INTEGER NOT NULL DEFAULT 0,
	currency       TEXT NOT NULL DEFAULT '',
	vat_cents      INTEGER NOT NULL DEFAULT 0,
	vat_rate       REAL NOT NULL DEFAULT 0,
	skr04          TEXT NOT NULL DEFAULT '',

	method         TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL DEFAULT 'new',
	datev_state    TEXT NOT NULL DEFAULT '',
	sevdesk_id     TEXT NOT NULL DEFAULT '',
	sevdesk_state  TEXT NOT NULL DEFAULT '',
	archive_path   TEXT NOT NULL DEFAULT '',
	last_error     TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL DEFAULT (datetime('now')),
	updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS invoices_status ON invoices(status);

CREATE TABLE IF NOT EXISTS runs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	trigger     TEXT NOT NULL,
	started_at  TEXT NOT NULL DEFAULT (datetime('now')),
	finished_at TEXT,
	status      TEXT NOT NULL DEFAULT 'running',
	found       INTEGER NOT NULL DEFAULT 0,
	processed   INTEGER NOT NULL DEFAULT 0,
	failed      INTEGER NOT NULL DEFAULT 0,
	summary     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id     INTEGER,
	invoice_id INTEGER,
	ts         TEXT NOT NULL DEFAULT (datetime('now')),
	module     TEXT NOT NULL DEFAULT '',
	level      TEXT NOT NULL DEFAULT 'info',
	message    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_run ON events(run_id);
CREATE INDEX IF NOT EXISTS events_invoice ON events(invoice_id);
`

func (db *DB) migrate() error {
	_, err := db.Exec(schema)
	return err
}
